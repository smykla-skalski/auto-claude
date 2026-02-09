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
	"github.com/marcin-skalski/auto-claude/internal/worker"
)

type Daemon struct {
	cfg    *config.Config
	gh     *github.Client
	claude *claude.Client
	git    *git.Client
	logger *slog.Logger

	mu      sync.Mutex
	workers map[string]context.CancelFunc
	wg      sync.WaitGroup
}

func New(cfg *config.Config, gh *github.Client, cl *claude.Client, g *git.Client, logger *slog.Logger) *Daemon {
	return &Daemon{
		cfg:     cfg,
		gh:      gh,
		claude:  cl,
		git:     g,
		logger:  logger,
		workers: make(map[string]context.CancelFunc),
	}
}

func (d *Daemon) Run(ctx context.Context) error {
	d.logger.Info("daemon started", "poll_interval", d.cfg.PollInterval, "repos", len(d.cfg.Repos))

	// Initial poll
	d.poll(ctx)

	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

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

	d.logger.Info("polled repo", "repo", repo.Owner+"/"+repo.Name, "open_prs", len(prs))

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

	w := worker.New(repo, pr, d.gh, d.claude, d.git, d.logger)

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
