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

	maxRetriesPerAction = 3
)

var (
	copilotAuthors = map[string]struct{}{
		"Copilot":                       {},
		"copilot":                       {},
		"github-copilot[bot]":           {},
		"copilot-pull-request-reviewer": {},
	}

	renovateAuthors = map[string]struct{}{
		"renovate":      {},
		"renovate[bot]": {},
		"renovate-bot":  {},
	}
)

type Worker struct {
	repo   config.RepoConfig
	pr     github.PRInfo
	gh     *github.Client
	claude *claude.Client
	git    *git.Client
	logger *slog.Logger

	cachedReviewThreads []github.ReviewThread
	retries             map[state]int

	onClaudeStart func(action string)
	onClaudeEnd   func()
}

func New(repo config.RepoConfig, pr github.PRInfo, gh *github.Client, cl *claude.Client, g *git.Client, logger *slog.Logger, onClaudeStart func(action string), onClaudeEnd func()) *Worker {
	return &Worker{
		repo:          repo,
		pr:            pr,
		gh:            gh,
		claude:        cl,
		git:           g,
		logger:        logger.With("pr", pr.Number, "repo", repo.Owner+"/"+repo.Name),
		retries:       make(map[state]int),
		onClaudeStart: onClaudeStart,
		onClaudeEnd:   onClaudeEnd,
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

	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("worker cancelled")
			return ctx.Err()
		default:
		}

		// Refresh PR state
		pr, err := w.gh.GetPRDetail(ctx, w.repo.Owner, w.repo.Name, w.pr.Number)
		if err != nil {
			w.logger.Error("failed to get PR detail", "err", err)
			consecutiveFailures++
			w.sleep(ctx, consecutiveFailures)
			continue
		}
		w.pr = *pr

		// Fetch review threads for Copilot review status (only if required and not Renovate)
		if *w.repo.RequireCopilotReview && !isRenovateAuthor(w.pr.Author.Login) {
			threads, err := w.gh.GetReviewThreads(ctx, w.repo.Owner, w.repo.Name, w.pr.Number)
			if err != nil {
				w.logger.Error("failed to get review threads", "err", err)
				consecutiveFailures++
				w.sleep(ctx, consecutiveFailures)
				continue
			}
			w.cachedReviewThreads = threads
		}

		s := w.evaluate()
		w.logger.Info("evaluated state", "state", stateString(s))

		var actionErr error
		switch s {
		case stateDraft:
			w.logger.Info("PR is draft, sleeping")
			w.sleep(ctx, 0)
			continue

		case stateConflicting:
			if w.retries[stateConflicting] >= maxRetriesPerAction {
				w.logger.Warn("max retries for conflict resolution")
				return fmt.Errorf("max retries for conflict resolution")
			}
			actionErr = w.resolveConflicts(ctx, wtDir)
			if actionErr != nil {
				w.retries[stateConflicting]++
			}

		case stateChecksFailing:
			if w.retries[stateChecksFailing] >= maxRetriesPerAction {
				w.logger.Warn("max retries for fixing checks")
				return fmt.Errorf("max retries for fixing checks")
			}
			actionErr = w.fixChecks(ctx, wtDir)
			if actionErr != nil {
				w.retries[stateChecksFailing]++
			}

		case stateReviewsPending:
			if w.retries[stateReviewsPending] >= maxRetriesPerAction {
				w.logger.Warn("max retries for fixing reviews")
				return fmt.Errorf("max retries for fixing reviews")
			}
			actionErr = w.fixReviews(ctx, wtDir)
			if actionErr != nil {
				w.retries[stateReviewsPending]++
			}

		case stateChecksPending:
			w.logger.Info("checks pending, waiting")
			w.sleep(ctx, 0)
			continue

		case stateReady:
			w.logger.Info("PR ready to merge")
			if err := w.merge(ctx); err != nil {
				w.logger.Error("merge failed", "err", err)
				consecutiveFailures++
				w.sleep(ctx, consecutiveFailures)
				continue
			}
			w.logger.Info("PR merged successfully")
			return nil
		}

		if actionErr != nil {
			w.logger.Error("action failed", "state", stateString(s), "err", actionErr)
			consecutiveFailures++
		} else {
			consecutiveFailures = 0
		}

		w.sleep(ctx, consecutiveFailures)
	}
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

	// Check Copilot review status before merging (if required and not Renovate)
	if *w.repo.RequireCopilotReview && !isRenovateAuthor(w.pr.Author.Login) {
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
	_, ok := copilotAuthors[author]
	return ok
}

func isRenovateAuthor(author string) bool {
	_, ok := renovateAuthors[author]
	return ok
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

func (w *Worker) sleep(ctx context.Context, consecutiveFailures int) {
	// No actual sleep, just return - daemon controls polling
	// This method exists for compatibility with retry logic
}
