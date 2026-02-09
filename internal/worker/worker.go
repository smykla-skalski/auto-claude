package worker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/marcin-skalski/auto-claude/internal/claude"
	"github.com/marcin-skalski/auto-claude/internal/config"
	"github.com/marcin-skalski/auto-claude/internal/git"
	"github.com/marcin-skalski/auto-claude/internal/github"
)

type state int

const (
	stateDraft state = iota
	stateConflicting
	stateChecksFailing
	stateReviewsPending
	stateChecksPending
	stateReady
)


type Worker struct {
	repo   config.RepoConfig
	pr     github.PRInfo
	gh     *github.Client
	claude *claude.Client
	git    *git.Client
	logger *slog.Logger

	cachedReviewThreads []github.ReviewThread
}

func New(repo config.RepoConfig, pr github.PRInfo, gh *github.Client, cl *claude.Client, g *git.Client, logger *slog.Logger) *Worker {
	return &Worker{
		repo:   repo,
		pr:     pr,
		gh:     gh,
		claude: cl,
		git:    g,
		logger: logger.With("pr", pr.Number, "repo", repo.Owner+"/"+repo.Name),
	}
}

// Run evaluates PR once and takes action if needed. Exits after action or if waiting required. Daemon restarts on next poll.
func (w *Worker) Run(ctx context.Context) error {
	w.logger.Info("worker started", "title", w.pr.Title, "head", w.pr.HeadRef)

	// Setup worktree
	if err := w.git.EnsureClone(ctx, w.repo.Owner, w.repo.Name); err != nil {
		return fmt.Errorf("ensure clone: %w", err)
	}

	wtDir, err := w.git.AddWorktree(ctx, w.repo.Owner, w.repo.Name, w.pr.HeadRef, w.pr.Number)
	if err != nil {
		return fmt.Errorf("add worktree: %w", err)
	}
	defer func() {
		if err := w.git.RemoveWorktree(context.Background(), w.repo.Owner, w.repo.Name, w.pr.Number); err != nil {
			w.logger.Error("failed to remove worktree", "error", err)
		}
	}()

	// Check context before proceeding
	select {
	case <-ctx.Done():
		w.logger.Info("worker cancelled")
		return ctx.Err()
	default:
	}

	// Refresh PR state
	pr, err := w.gh.GetPRDetail(ctx, w.repo.Owner, w.repo.Name, w.pr.Number)
	if err != nil {
		return fmt.Errorf("get PR detail: %w", err)
	}
	w.pr = *pr

	// Fetch review threads for Copilot review status (only if required)
	if *w.repo.RequireCopilotReview {
		threads, err := w.gh.GetReviewThreads(ctx, w.repo.Owner, w.repo.Name, w.pr.Number)
		if err != nil {
			return fmt.Errorf("get review threads: %w", err)
		}
		w.cachedReviewThreads = threads
	}

	s := w.evaluate()
	w.logger.Info("evaluated state", "state", stateString(s))

	switch s {
	case stateDraft:
		w.logger.Info("PR is draft, exiting worker")
		return nil

	case stateConflicting:
		if err := w.resolveConflicts(ctx, wtDir); err != nil {
			return fmt.Errorf("resolve conflicts: %w", err)
		}
		w.logger.Info("conflicts resolved, exiting worker")
		return nil

	case stateChecksFailing:
		if err := w.fixChecks(ctx, wtDir); err != nil {
			return fmt.Errorf("fix checks: %w", err)
		}
		w.logger.Info("checks fix attempted, exiting worker")
		return nil

	case stateReviewsPending:
		if err := w.fixReviews(ctx, wtDir); err != nil {
			return fmt.Errorf("fix reviews: %w", err)
		}
		w.logger.Info("reviews fix attempted, exiting worker")
		return nil

	case stateChecksPending:
		w.logger.Info("checks pending, exiting worker")
		return nil

	case stateReady:
		w.logger.Info("PR ready to merge")
		if err := w.merge(ctx); err != nil {
			return fmt.Errorf("merge: %w", err)
		}
		w.logger.Info("PR merged successfully")
		return nil
	}

	return nil
}

func (w *Worker) evaluate() state {
	if w.pr.IsDraft {
		return stateDraft
	}

	if w.pr.Mergeable == "CONFLICTING" {
		return stateConflicting
	}

	// Check for failing checks
	for _, c := range w.pr.Checks {
		if c.Conclusion == "failure" {
			return stateChecksFailing
		}
	}

	// Check for pending checks
	for _, c := range w.pr.Checks {
		if c.Conclusion == "" && c.Status != "COMPLETED" {
			return stateChecksPending
		}
	}

	// Check Copilot review status before merging (if required)
	if *w.repo.RequireCopilotReview {
		copilotStatus := w.checkCopilotReviewStatus()
		switch copilotStatus {
		case copilotNotReviewed:
			w.logger.Info("waiting for Copilot review to complete")
			return stateChecksPending
		case copilotUnresolved:
			return stateReviewsPending
		case copilotResolved:
			// Continue to merge readiness check
		}
	}

	// If merge state is blocked, could be other review requirements
	if w.pr.MergeStateStatus == "BLOCKED" {
		return stateReviewsPending
	}

	return stateReady
}

type copilotReviewStatus int

const (
	copilotNotReviewed copilotReviewStatus = iota
	copilotUnresolved
	copilotResolved
)

func (w *Worker) checkCopilotReviewStatus() copilotReviewStatus {
	var hasCopilotComment bool
	var hasUnresolvedComment bool

	for _, t := range w.cachedReviewThreads {
		for _, c := range t.Comments {
			if isCopilotAuthor(c.Author) {
				hasCopilotComment = true
				if !t.IsResolved && !t.IsOutdated {
					hasUnresolvedComment = true
				}
				break
			}
		}
	}

	if !hasCopilotComment {
		return copilotNotReviewed
	}
	if hasUnresolvedComment {
		return copilotUnresolved
	}
	return copilotResolved
}

func isCopilotAuthor(author string) bool {
	copilotAuthors := []string{
		"Copilot",
		"copilot",
		"github-copilot[bot]",
		"copilot-pull-request-reviewer",
	}
	for _, ca := range copilotAuthors {
		if author == ca {
			return true
		}
	}
	return false
}


func stateString(s state) string {
	switch s {
	case stateDraft:
		return "draft"
	case stateConflicting:
		return "conflicting"
	case stateChecksFailing:
		return "checks_failing"
	case stateReviewsPending:
		return "reviews_pending"
	case stateChecksPending:
		return "checks_pending"
	case stateReady:
		return "ready"
	default:
		return "unknown"
	}
}
