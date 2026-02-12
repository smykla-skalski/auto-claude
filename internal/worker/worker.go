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

	onClaudeStart  func(action string)
	onClaudeEnd    func()
	onClaudeOutput func(line string)
}

func New(repo config.RepoConfig, pr github.PRInfo, gh *github.Client, cl *claude.Client, g *git.Client, logger *slog.Logger, onClaudeStart func(action string), onClaudeEnd func(), onClaudeOutput func(line string)) *Worker {
	return &Worker{
		repo:           repo,
		pr:             pr,
		gh:             gh,
		claude:         cl,
		git:            g,
		logger:         logger.With("pr", pr.Number, "repo", repo.Owner+"/"+repo.Name),
		onClaudeStart:  onClaudeStart,
		onClaudeEnd:    onClaudeEnd,
		onClaudeOutput: onClaudeOutput,
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
		requireCopilot := w.repo.RequireCopilotReview != nil && *w.repo.RequireCopilotReview
		isRenovate := isRenovateAuthor(w.pr.Author.Login)

		w.logger.Debug("copilot review check",
			"require_copilot_review", requireCopilot,
			"is_renovate", isRenovate,
			"author", w.pr.Author.Login,
			"config_ptr_nil", w.repo.RequireCopilotReview == nil)

		if requireCopilot && !isRenovate {
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
		} else {
			// Reviews not required or Renovate PR - clear cached reviews
			w.cachedReviews = nil
			w.cachedReviewThreads = nil
			if !isRenovate && !requireCopilot {
				w.logger.Warn("copilot review check skipped unexpectedly",
					"require_copilot_review", requireCopilot,
					"config_ptr", w.repo.RequireCopilotReview)
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
			// Check if we have Copilot reviews to fix
			hasUnresolvedCopilot := false
			for _, t := range w.cachedReviewThreads {
				if t.IsResolved || t.IsOutdated {
					continue
				}
				for _, c := range t.Comments {
					if isCopilotAuthor(c.Author) {
						hasUnresolvedCopilot = true
						break
					}
				}
				if hasUnresolvedCopilot {
					break
				}
			}

			if hasUnresolvedCopilot {
				actionErr = w.fixReviews(ctx, wtDir)
			} else {
				// No Copilot reviews, just waiting for human reviews
				actionErr = w.requestReview(ctx)
			}

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

	// Check review status before merging (if required and not Renovate)
	requireCopilot := w.repo.RequireCopilotReview != nil && *w.repo.RequireCopilotReview
	isRenovate := isRenovateAuthor(w.pr.Author.Login)

	w.logger.Debug("evaluate copilot requirement",
		"require_copilot_review", requireCopilot,
		"is_renovate", isRenovate,
		"author", w.pr.Author.Login,
		"cached_reviews_count", len(w.cachedReviews),
		"cached_threads_count", len(w.cachedReviewThreads))

	if requireCopilot && !isRenovate {
		// Safety check: ensure reviews were fetched
		if w.cachedReviews == nil {
			w.logger.Error("copilot review required but reviews not fetched - blocking merge",
				"pr", w.pr.Number,
				"author", w.pr.Author.Login)
			return stateChecksPending
		}

		copilotStatus := w.checkCopilotReviewStatus()
		w.logger.Debug("copilot review status", "status", copilotStatus)
		switch copilotStatus {
		case copilotNotReviewed:
			w.logger.Info("waiting for Copilot review to complete")
			return stateChecksPending
		case copilotUnresolved:
			return stateReviewsPending
		case copilotResolved:
			// Continue to merge readiness check
		}

		// Check if reviews are approved (only when reviews required)
		if w.pr.ReviewDecision != "APPROVED" {
			w.logger.Debug("reviews not approved; waiting before merge",
				"reviewDecision", w.pr.ReviewDecision,
				"mergeStateStatus", w.pr.MergeStateStatus,
				"mergeable", w.pr.Mergeable)
			return stateReviewsPending
		}

		// Reviews approved, continue to ready check
		w.logger.Debug("reviews approved", "reviewDecision", w.pr.ReviewDecision)
	}

	// Special handling for BEHIND: allow merge attempt which will trigger UpdateBranch
	if w.pr.MergeStateStatus == "BEHIND" {
		w.logger.Debug("PR behind base branch, will attempt merge to trigger update",
			"mergeStateStatus", w.pr.MergeStateStatus)
		return stateReady
	}

	if w.pr.MergeStateStatus != "CLEAN" {
		w.logger.Debug("PR merge state not clean",
			"mergeStateStatus", w.pr.MergeStateStatus,
			"mergeable", w.pr.Mergeable)
		return stateChecksPending
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
	var hasApproval bool
	var hasUnresolvedComment bool

	// Check top-level reviews for latest non-dismissed state
	// Process reviews in order to find most recent Copilot review
	var latestCopilotState string
	for _, r := range w.cachedReviews {
		if isCopilotAuthor(r.Author) {
			// Only consider submitted reviews (ignore PENDING/DISMISSED)
			if r.State != "PENDING" && r.State != "DISMISSED" {
				hasCopilotReview = true
				latestCopilotState = r.State
			}
		}
	}

	// Only consider APPROVED if it's the latest state
	if latestCopilotState == "APPROVED" {
		hasApproval = true
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
	if hasUnresolvedComment || !hasApproval {
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
