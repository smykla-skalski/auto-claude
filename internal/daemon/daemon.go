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
	mu       sync.Mutex
	output   []string // Live output lines (thread-safe with mu)
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

	// Fetch Copilot review status outside lock to avoid blocking cache reads during network calls
	type copilotStatus struct {
		prKey                string
		hasCopilotReview     bool
		hasUnresolvedCopilot bool
	}
	var copilotStatuses []copilotStatus

	if *repo.RequireCopilotReview {
		for _, pr := range prs {
			prKey := workerKey(repo.Owner, repo.Name, pr.Number)

			// Skip fetching review threads for Renovate PRs (mirrors worker behavior)
			if isRenovateAuthor(pr.Author.Login) {
				copilotStatuses = append(copilotStatuses, copilotStatus{
					prKey:                prKey,
					hasCopilotReview:     false,
					hasUnresolvedCopilot: false,
				})
				continue
			}

			// Fetch reviews to check Copilot status
			hasCopilotReview := false
			hasUnresolvedCopilot := false

			// Check top-level reviews
			reviews, err := d.gh.GetReviews(ctx, repo.Owner, repo.Name, pr.Number)
			if err != nil {
				d.logger.Warn("failed to get reviews for Copilot status", "pr", pr.Number, "err", err)
				// Skip cache update for this PR on error - preserve previous value
				continue
			}

			for _, r := range reviews {
				if isCopilotAuthor(r.Author) && r.State != "PENDING" && r.State != "DISMISSED" {
					hasCopilotReview = true
					break
				}
			}

			// Only fetch review threads if Copilot review exists
			if hasCopilotReview {
				threads, err := d.gh.GetReviewThreads(ctx, repo.Owner, repo.Name, pr.Number)
				if err != nil {
					d.logger.Warn("failed to get review threads for Copilot status", "pr", pr.Number, "err", err)
					// Skip cache update for this PR on error - preserve previous value
					continue
				}

				for _, t := range threads {
					hasCopilotInThread := false
					for _, c := range t.Comments {
						if isCopilotAuthor(c.Author) {
							hasCopilotInThread = true
							break
						}
					}
					if hasCopilotInThread && !t.IsResolved && !t.IsOutdated {
						hasUnresolvedCopilot = true
					}
				}
			}

			copilotStatuses = append(copilotStatuses, copilotStatus{
				prKey:                prKey,
				hasCopilotReview:     hasCopilotReview,
				hasUnresolvedCopilot: hasUnresolvedCopilot,
			})
		}
	}

	// Update cache with lock held only for writes
	d.prCacheMu.Lock()
	d.prCache[repoKey] = prs
	for _, status := range copilotStatuses {
		d.copilotReviewCache[status.prKey] = status.hasCopilotReview
		d.copilotUnresolvedCache[status.prKey] = status.hasUnresolvedCopilot
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

		// Skip blocked/on-hold PRs at poll level
		if hasBlockingLabel(pr) {
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
	onClaudeOutput := func(line string) {
		d.trackClaudeOutput(key, line)
	}

	w := worker.New(repo, pr, d.gh, d.claude, d.git, d.logger, onClaudeStart, onClaudeEnd, onClaudeOutput)

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
		output:   make([]string, 0, 100),
	}
}

func (d *Daemon) trackClaudeOutput(key string, line string) {
	d.sessionsMu.Lock()
	session, ok := d.claudeSessions[key]
	d.sessionsMu.Unlock()
	if !ok {
		return
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	session.output = append(session.output, line)
	// Keep last 1000 lines to avoid memory growth
	if len(session.output) > 1000 {
		session.output = session.output[len(session.output)-1000:]
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
		s.mu.Lock()
		outputCopy := make([]string, len(s.output))
		copy(outputCopy, s.output)
		s.mu.Unlock()

		sessions = append(sessions, tui.ClaudeSessionState{
			Repo:     s.repo,
			PRNumber: s.prNumber,
			Action:   s.action,
			Duration: time.Since(s.started).Round(time.Second),
			Output:   outputCopy,
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
		blockedCount := 0
		for _, pr := range prs {
			wk := workerKey(repo.Owner, repo.Name, pr.Number)
			hasWorker := workersCopy[wk]
			if hasWorker {
				repoWorkers++
			}

			// Skip PRs with blocked/on-hold labels
			if hasBlockingLabel(pr) {
				blockedCount++
				continue
			}

			hasCopilotReview := copilotCacheCopy[wk]
			hasUnresolvedCopilot := copilotUnresolvedCacheCopy[wk]
			prStates = append(prStates, tui.PRState{
				Number:    pr.Number,
				Title:     pr.Title,
				States:    inferStatesFromPR(pr, *repo.RequireCopilotReview && !isRenovateAuthor(pr.Author.Login), hasCopilotReview, hasUnresolvedCopilot),
				Author:    pr.Author.Login,
				HasWorker: hasWorker,
			})
		}

		repos = append(repos, tui.RepoState{
			Owner:      repo.Owner,
			Name:       repo.Name,
			PRs:        prStates,
			BlockedPRs: blockedCount,
			Workers:    repoWorkers,
		})
	}

	return tui.Snapshot{
		Timestamp:      time.Now(),
		Repos:          repos,
		ClaudeSessions: sessions,
		WorkerCount:    workerCount,
	}
}

var blockingLabels = map[string]bool{
	"blocked": true,
	"on-hold": true,
}

func hasBlockingLabel(pr github.PRInfo) bool {
	for _, label := range pr.Labels {
		if blockingLabels[label.Name] {
			return true
		}
	}
	return false
}

var copilotAuthors = map[string]bool{
	"Copilot":                       true,
	"copilot":                       true,
	"github-copilot[bot]":           true,
	"copilot-pull-request-reviewer": true,
}

var renovateAuthors = map[string]bool{
	"renovate":      true,
	"renovate[bot]": true,
	"renovate-bot":  true,
	"app/renovate":  true,
}

func isCopilotAuthor(author string) bool {
	return copilotAuthors[author]
}

func isRenovateAuthor(author string) bool {
	return renovateAuthors[author]
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

	// Use ReviewDecision (consistent with worker merge gating in worker.go:290)
	// instead of MergeStateStatus which can be BLOCKED for non-review reasons
	if pr.ReviewDecision != "" && pr.ReviewDecision != "APPROVED" {
		states = append(states, "reviews_pending")
	}

	if len(states) == 0 {
		states = append(states, "ready")
	}

	return states
}
