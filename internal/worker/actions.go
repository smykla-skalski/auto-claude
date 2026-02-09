package worker

import (
	"context"
	"fmt"
	"strings"
)

func (w *Worker) resolveConflicts(ctx context.Context, wtDir string) error {
	w.logger.Info("resolving merge conflicts")

	if err := w.git.Fetch(ctx, wtDir); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	prompt := fmt.Sprintf(
		"This branch has conflicts with %s. Run `git merge origin/%s`, resolve all conflicts, commit with -s -S flags, and push.",
		w.repo.BaseBranch, w.repo.BaseBranch,
	)

	w.onClaudeStart("resolving_conflicts")
	endCalled := false
	defer func() {
		if !endCalled {
			w.onClaudeEnd()
		}
	}()

	result, err := w.claude.Run(ctx, wtDir, prompt)
	w.onClaudeEnd()
	endCalled = true
	if err != nil {
		return fmt.Errorf("claude resolve conflicts: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("claude failed: %s", result.Output)
	}

	// Check if Claude actually created commits
	hasChanges, err := w.git.HasUnpushedCommits(ctx, wtDir, w.pr.HeadRef)
	if err != nil {
		return fmt.Errorf("check unpushed commits: %w", err)
	}

	if !hasChanges {
		return fmt.Errorf("no commits created by claude, cannot push")
	}

	if err := w.git.Push(ctx, wtDir, w.pr.HeadRef); err != nil {
		return fmt.Errorf("push: %w", err)
	}

	w.logger.Info("conflicts resolved and pushed")
	return nil
}

func (w *Worker) fixChecks(ctx context.Context, wtDir string) error {
	var failing []string
	for _, c := range w.pr.Checks {
		if c.Conclusion == "failure" {
			failing = append(failing, c.Name)
		}
	}

	w.logger.Info("fixing failing checks", "checks", failing)

	if err := w.git.Fetch(ctx, wtDir); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	prompt := fmt.Sprintf(
		"CI checks failing: %s. Investigate failures, fix code, commit with -s -S flags, run tests locally to verify, and push.",
		strings.Join(failing, ", "),
	)

	w.onClaudeStart("fixing_checks")
	endCalled := false
	defer func() {
		if !endCalled {
			w.onClaudeEnd()
		}
	}()

	result, err := w.claude.Run(ctx, wtDir, prompt)
	w.onClaudeEnd()
	endCalled = true
	if err != nil {
		return fmt.Errorf("claude fix checks: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("claude failed: %s", result.Output)
	}

	// Check if Claude actually created commits
	hasChanges, err := w.git.HasUnpushedCommits(ctx, wtDir, w.pr.HeadRef)
	if err != nil {
		return fmt.Errorf("check unpushed commits: %w", err)
	}

	if !hasChanges {
		return fmt.Errorf("no commits created by claude, cannot push")
	}

	if err := w.git.Push(ctx, wtDir, w.pr.HeadRef); err != nil {
		return fmt.Errorf("push: %w", err)
	}

	w.logger.Info("checks fixed and pushed")
	return nil
}

func (w *Worker) fixReviews(ctx context.Context, wtDir string) error {
	w.logger.Info("fixing review comments")

	// Collect unresolved Copilot review threads
	var unresolvedThreads []string
	for _, t := range w.cachedReviewThreads {
		if t.IsResolved || t.IsOutdated {
			continue
		}
		for _, c := range t.Comments {
			if isCopilotAuthor(c.Author) {
				unresolvedThreads = append(unresolvedThreads, t.ID)
				break
			}
		}
	}

	if len(unresolvedThreads) == 0 {
		w.logger.Info("no unresolved copilot reviews found")
		return nil
	}

	w.logger.Info("found unresolved copilot reviews", "count", len(unresolvedThreads))

	if err := w.git.Fetch(ctx, wtDir); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	w.onClaudeStart("fixing_reviews")
	endCalled := false
	defer func() {
		if !endCalled {
			w.onClaudeEnd()
		}
	}()

	prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", w.repo.Owner, w.repo.Name, w.pr.Number)
	result, err := w.claude.RunCommand(ctx, wtDir, "fix-review-auto", prURL)
	w.onClaudeEnd()
	endCalled = true
	if err != nil {
		return fmt.Errorf("claude fix reviews: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("claude failed: %s", result.Output)
	}

	// Check if Claude actually created commits
	hasChanges, err := w.git.HasUnpushedCommits(ctx, wtDir, w.pr.HeadRef)
	if err != nil {
		return fmt.Errorf("check unpushed commits: %w", err)
	}

	if !hasChanges {
		return fmt.Errorf("no commits created by claude, cannot push")
	}

	if err := w.git.Push(ctx, wtDir, w.pr.HeadRef); err != nil {
		return fmt.Errorf("push: %w", err)
	}

	// Auto-resolve all Copilot review threads after successful fix
	w.logger.Info("resolving copilot review threads", "count", len(unresolvedThreads))
	for _, threadID := range unresolvedThreads {
		if err := w.gh.ResolveReviewThread(ctx, threadID); err != nil {
			w.logger.Error("failed to resolve thread", "thread_id", threadID, "err", err)
			// Continue with other threads even if one fails
		}
	}

	w.logger.Info("reviews fixed, pushed, and resolved")
	return nil
}

func (w *Worker) merge(ctx context.Context) error {
	err := w.gh.MergePR(ctx, w.repo.Owner, w.repo.Name, w.pr.Number, w.repo.MergeMethod)
	if err != nil && strings.Contains(err.Error(), "Base branch was modified") {
		w.logger.Info("base branch modified, updating PR branch")
		if updateErr := w.gh.UpdateBranch(ctx, w.repo.Owner, w.repo.Name, w.pr.Number); updateErr != nil {
			return fmt.Errorf("update branch: %w", updateErr)
		}
		w.logger.Info("PR branch updated, will retry merge on next poll after checks pass")
		return nil // Exit successfully, next poll will retry merge
	}
	return err
}
