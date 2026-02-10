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
		"app/renovate":  {},
	}
)

type Worker struct {
	repo   config.RepoConfig
	pr     github.PRInfo
	gh     *github.Client
	claude *claude.Client
	git    *git.Client
	logger *slog.Logger

	cachedReviews       []github.Review
	cachedReviewThreads []github.ReviewThread

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

		// Fetch reviews and review threads for Copilot review status (only if required and not Renovate)
		if *w.repo.RequireCopilotReview && !isRenovateAuthor(w.pr.Author.Login) {
			reviews, err := w.gh.GetReviews(ctx, w.repo.Owner, w.repo.Name, w.pr.Number)
			if err != nil {
				w.logger.Error("failed to get reviews", "err", err)
				consecutiveFailures++
				w.sleep(ctx, consecutiveFailures)
				continue
			}
			w.cachedReviews = reviews

			// Only fetch review threads if Copilot review exists (ignore PENDING/DISMISSED)
			hasCopilotReview := false
			for _, r := range reviews {
				if isCopilotAuthor(r.Author) && r.State != "PENDING" && r.State != "DISMISSED" {
					hasCopilotReview = true
					break
				}
			}

			if hasCopilotReview {
				threads, err := w.gh.GetReviewThreads(ctx, w.repo.Owner, w.repo.Name, w.pr.Number)
				if err != nil {
					w.logger.Error("failed to get review threads", "err", err)
					consecutiveFailures++
					w.sleep(ctx, consecutiveFailures)
					continue
				}
				w.cachedReviewThreads = threads
			} else {
				w.cachedReviewThreads = nil
			}
		}

		// Reset counter after successful PR and thread fetch
		consecutiveFailures = 0

		s := w.evaluate()
		w.logger.Info("evaluated state", "state", stateString(s))

		var actionErr error
		switch s {
		case stateDraft:
			w.logger.Info("PR is draft, waiting for next poll")
			return nil

		case stateConflicting:
			actionErr = w.resolveConflicts(ctx, wtDir)

		case stateChecksFailing:
			actionErr = w.fixChecks(ctx, wtDir)

		case stateReviewsPending:
			actionErr = w.fixReviews(ctx, wtDir)

		case stateChecksPending:
			w.logger.Info("checks pending, waiting for next poll")
			return nil

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
			w.logger.Error("action failed, will retry on next poll", "state", stateString(s), "err", actionErr)
			return nil
		}
		// Exit after successful action, let next poll cycle evaluate fresh state
		w.logger.Info("action completed, exiting worker")
		return nil
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
	var hasCopilotReview bool
	var hasUnresolvedComment bool

	// Check top-level reviews
	for _, r := range w.cachedReviews {
		if isCopilotAuthor(r.Author) {
			hasCopilotReview = true
			break
		}
	}

	// Check review threads (inline comments)
	for _, t := range w.cachedReviewThreads {
		for _, c := range t.Comments {
			if isCopilotAuthor(c.Author) {
				hasCopilotReview = true
				if !t.IsResolved && !t.IsOutdated {
					hasUnresolvedComment = true
				}
				break
			}
		}
	}

	if !hasCopilotReview {
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
