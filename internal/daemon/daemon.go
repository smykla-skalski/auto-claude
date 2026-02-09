package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/marcin-skalski/auto-claude/internal/claude"
	"github.com/marcin-skalski/auto-claude/internal/config"
	"github.com/marcin-skalski/auto-claude/internal/git"
	"github.com/marcin-skalski/auto-claude/internal/github"
	"github.com/marcin-skalski/auto-claude/internal/tui"
	"github.com/marcin-skalski/auto-claude/internal/worker"
)

type claudeSession struct {
	repo     string
	prNumber int
	action   string
	started  time.Time
}

type Daemon struct {
	cfg    *config.Config
	gh     *github.Client
	claude *claude.Client
	git    *git.Client
	logger *slog.Logger

	mu      sync.Mutex
	workers map[string]context.CancelFunc
	wg      sync.WaitGroup

	sessionsMu     sync.Mutex
	claudeSessions map[string]*claudeSession

	prCacheMu              sync.Mutex
	prCache                map[string][]github.PRInfo // key: owner/repo
	copilotReviewCache     map[string]bool            // key: owner/repo#number, value: has Copilot reviewed
	copilotUnresolvedCache map[string]bool            // key: owner/repo#number, value: has unresolved Copilot threads
}

func New(cfg *config.Config, gh *github.Client, cl *claude.Client, g *git.Client, logger *slog.Logger) *Daemon {
	return &Daemon{
		cfg:                    cfg,
		gh:                     gh,
		claude:                 cl,
		git:                    g,
		logger:                 logger,
		workers:                make(map[string]context.CancelFunc),
		claudeSessions:         make(map[string]*claudeSession),
		prCache:                make(map[string][]github.PRInfo),
		copilotReviewCache:     make(map[string]bool),
		copilotUnresolvedCache: make(map[string]bool),
	}
}

func (d *Daemon) Run(ctx context.Context) error {
	d.logger.Info("daemon started", "poll_interval", d.cfg.PollInterval, "repos", len(d.cfg.Repos))

	// Initial poll
	d.poll(ctx)

	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	statusTicker := time.NewTicker(5 * time.Second)
	defer statusTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.logger.Info("shutting down, waiting for workers")
			d.cancelAll()
			d.wg.Wait()
			d.logger.Info("all workers stopped")
			return nil
		case <-ticker.C:
			d.poll(ctx)
		case <-statusTicker.C:
			d.logClaudeStatus()
		}
	}
}

func (d *Daemon) poll(ctx context.Context) {
	for _, repo := range d.cfg.Repos {
		if err := d.pollRepo(ctx, repo); err != nil {
			d.logger.Error("poll repo failed", "repo", repo.Owner+"/"+repo.Name, "err", err)
		}
	}
}

func (d *Daemon) pollRepo(ctx context.Context, repo config.RepoConfig) error {
	prs, err := d.gh.ListOpenPRs(ctx, repo.Owner, repo.Name)
	if err != nil {
		return fmt.Errorf("list PRs: %w", err)
	}

	// Cache PR data for TUI snapshot
	repoKey := repo.Owner + "/" + repo.Name
	d.prCacheMu.Lock()
	d.prCache[repoKey] = prs

	// Cache Copilot review status for each PR if required
	if *repo.RequireCopilotReview {
		for _, pr := range prs {
			prKey := workerKey(repo.Owner, repo.Name, pr.Number)

			// Fetch review threads to check Copilot status
			hasCopilotReview := false
			hasUnresolvedCopilot := false
			threads, err := d.gh.GetReviewThreads(ctx, repo.Owner, repo.Name, pr.Number)
			if err == nil {
				// Check if Copilot has commented and if threads are unresolved
				for _, t := range threads {
					hasCopilotInThread := false
					for _, c := range t.Comments {
						if isCopilotAuthor(c.Author) {
							hasCopilotInThread = true
							hasCopilotReview = true
							break
						}
					}
					if hasCopilotInThread && !t.IsResolved && !t.IsOutdated {
						hasUnresolvedCopilot = true
					}
				}
			}

			d.copilotReviewCache[prKey] = hasCopilotReview
			d.copilotUnresolvedCache[prKey] = hasUnresolvedCopilot
		}
	}
	d.prCacheMu.Unlock()

	d.logger.Info("polled repo", "repo", repoKey, "open_prs", len(prs))

	// Track which PRs are still open
	openKeys := make(map[string]bool)

	activeCount := d.countActiveForRepo(repo)

	for _, pr := range prs {
		key := workerKey(repo.Owner, repo.Name, pr.Number)
		openKeys[key] = true

		// Skip excluded authors
		if isExcluded(pr.Author.Login, repo.ExcludeAuthors) {
			continue
		}

		// Skip drafts at poll level
		if pr.IsDraft {
			continue
		}

		d.mu.Lock()
		_, running := d.workers[key]
		d.mu.Unlock()

		if running {
			continue
		}

		if activeCount >= repo.MaxConcurrentPRs {
			d.logger.Debug("max concurrent PRs reached", "repo", repo.Owner+"/"+repo.Name)
			break
		}

		d.startWorker(ctx, repo, pr)
		activeCount++
	}

	// Cancel workers for PRs no longer open
	d.mu.Lock()
	prefix := repo.Owner + "/" + repo.Name + "#"
	for key, cancel := range d.workers {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			if !openKeys[key] {
				d.logger.Info("PR closed externally, cancelling worker", "key", key)
				cancel()
				delete(d.workers, key)
			}
		}
	}
	d.mu.Unlock()

	return nil
}

func (d *Daemon) startWorker(ctx context.Context, repo config.RepoConfig, pr github.PRInfo) {
	key := workerKey(repo.Owner, repo.Name, pr.Number)
	workerCtx, cancel := context.WithCancel(ctx)

	d.mu.Lock()
	d.workers[key] = cancel
	d.mu.Unlock()

	repoFullName := repo.Owner + "/" + repo.Name
	onClaudeStart := func(action string) {
		d.trackClaudeStart(key, repoFullName, pr.Number, action)
	}
	onClaudeEnd := func() {
		d.trackClaudeEnd(key)
	}

	w := worker.New(repo, pr, d.gh, d.claude, d.git, d.logger, onClaudeStart, onClaudeEnd)

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.mu.Lock()
			delete(d.workers, key)
			d.mu.Unlock()
		}()

		d.logger.Info("starting worker", "key", key, "title", pr.Title)
		if err := w.Run(workerCtx); err != nil {
			if ctx.Err() == nil {
				d.logger.Error("worker failed", "key", key, "err", err)
			}
		}
	}()
}

func (d *Daemon) cancelAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, cancel := range d.workers {
		d.logger.Debug("cancelling worker", "key", key)
		cancel()
	}
}

func (d *Daemon) countActiveForRepo(repo config.RepoConfig) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	prefix := repo.Owner + "/" + repo.Name + "#"
	count := 0
	for key := range d.workers {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			count++
		}
	}
	return count
}

func workerKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

func isExcluded(author string, excluded []string) bool {
	for _, e := range excluded {
		if author == e {
			return true
		}
	}
	return false
}

func (d *Daemon) trackClaudeStart(key string, repo string, prNumber int, action string) {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	d.claudeSessions[key] = &claudeSession{
		repo:     repo,
		prNumber: prNumber,
		action:   action,
		started:  time.Now(),
	}
}

func (d *Daemon) trackClaudeEnd(key string) {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	delete(d.claudeSessions, key)
}

func (d *Daemon) logClaudeStatus() {
	d.sessionsMu.Lock()
	sessions := make([]*claudeSession, 0, len(d.claudeSessions))
	for _, s := range d.claudeSessions {
		sessions = append(sessions, s)
	}
	d.sessionsMu.Unlock()

	if len(sessions) == 0 {
		return
	}

	d.logger.Info("active claude sessions", "count", len(sessions))
	for _, s := range sessions {
		duration := time.Since(s.started).Round(time.Second)
		d.logger.Info("→ claude session",
			"repo", s.repo,
			"pr", s.prNumber,
			"action", s.action,
			"duration", duration)
	}
}

func (d *Daemon) GetSnapshot() tui.Snapshot {
	d.mu.Lock()
	workersCopy := make(map[string]bool, len(d.workers))
	for k := range d.workers {
		workersCopy[k] = true
	}
	workerCount := len(d.workers)
	d.mu.Unlock()

	d.sessionsMu.Lock()
	sessions := make([]tui.ClaudeSessionState, 0, len(d.claudeSessions))
	for _, s := range d.claudeSessions {
		sessions = append(sessions, tui.ClaudeSessionState{
			Repo:     s.repo,
			PRNumber: s.prNumber,
			Action:   s.action,
			Duration: time.Since(s.started).Round(time.Second),
		})
	}
	d.sessionsMu.Unlock()

	d.prCacheMu.Lock()
	prCacheCopy := make(map[string][]github.PRInfo, len(d.prCache))
	for k, v := range d.prCache {
		prCacheCopy[k] = append([]github.PRInfo(nil), v...)
	}
	copilotCacheCopy := make(map[string]bool, len(d.copilotReviewCache))
	for k, v := range d.copilotReviewCache {
		copilotCacheCopy[k] = v
	}
	copilotUnresolvedCacheCopy := make(map[string]bool, len(d.copilotUnresolvedCache))
	for k, v := range d.copilotUnresolvedCache {
		copilotUnresolvedCacheCopy[k] = v
	}
	d.prCacheMu.Unlock()

	repos := make([]tui.RepoState, 0, len(d.cfg.Repos))
	for _, repo := range d.cfg.Repos {
		repoKey := repo.Owner + "/" + repo.Name
		prs, ok := prCacheCopy[repoKey]
		if !ok {
			prs = []github.PRInfo{}
		}

		prStates := make([]tui.PRState, 0, len(prs))
		repoWorkers := 0
		for _, pr := range prs {
			wk := workerKey(repo.Owner, repo.Name, pr.Number)
			hasWorker := workersCopy[wk]
			if hasWorker {
				repoWorkers++
			}

			hasCopilotReview := copilotCacheCopy[wk]
			hasUnresolvedCopilot := copilotUnresolvedCacheCopy[wk]
			prStates = append(prStates, tui.PRState{
				Number:    pr.Number,
				Title:     pr.Title,
				States:    inferStatesFromPR(pr, *repo.RequireCopilotReview, hasCopilotReview, hasUnresolvedCopilot),
				Author:    pr.Author.Login,
				HasWorker: hasWorker,
			})
		}

		repos = append(repos, tui.RepoState{
			Owner:   repo.Owner,
			Name:    repo.Name,
			PRs:     prStates,
			Workers: repoWorkers,
		})
	}

	return tui.Snapshot{
		Timestamp:      time.Now(),
		Repos:          repos,
		ClaudeSessions: sessions,
		WorkerCount:    workerCount,
	}
}

func isCopilotAuthor(author string) bool {
	copilotAuthors := map[string]bool{
		"Copilot":                       true,
		"copilot":                       true,
		"github-copilot[bot]":           true,
		"copilot-pull-request-reviewer": true,
	}
	return copilotAuthors[author]
}

func inferStatesFromPR(pr github.PRInfo, requireCopilot bool, hasCopilotReview bool, hasUnresolvedCopilot bool) []string {
	var states []string

	if pr.IsDraft {
		return []string{"draft"}
	}

	if pr.Mergeable == "CONFLICTING" {
		states = append(states, "conflicting")
	}

	hasFailingChecks := false
	hasPendingChecks := false
	for _, c := range pr.Checks {
		if c.Conclusion == "failure" {
			hasFailingChecks = true
		}
		if c.Conclusion == "" && c.Status != "COMPLETED" {
			hasPendingChecks = true
		}
	}

	if hasFailingChecks {
		states = append(states, "checks_failing")
	}
	if hasPendingChecks {
		states = append(states, "checks_pending")
	}

	// Check for Copilot review status if required
	if requireCopilot {
		if !hasCopilotReview {
			states = append(states, "copilot_pending")
		} else if hasUnresolvedCopilot {
			states = append(states, "fixing_reviews")
		}
	}

	if pr.MergeStateStatus == "BLOCKED" {
		states = append(states, "reviews_pending")
	}

	if len(states) == 0 {
		states = append(states, "ready")
	}

	return states
}
