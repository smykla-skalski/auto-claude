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
		"This branch has conflicts with %s. Run `git merge origin/%s`, resolve all conflicts, then `git add . && git commit -s -S -m 'resolve merge conflicts'`.",
		w.repo.BaseBranch, w.repo.BaseBranch,
	)

	result, err := w.claude.Run(ctx, wtDir, prompt)
	if err != nil {
		return fmt.Errorf("claude resolve conflicts: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("claude failed: %s", result.Output)
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
		"CI checks failing: %s. Investigate failures, fix code, commit with -s -S flags. Run relevant tests locally to verify.",
		strings.Join(failing, ", "),
	)

	result, err := w.claude.Run(ctx, wtDir, prompt)
	if err != nil {
		return fmt.Errorf("claude fix checks: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("claude failed: %s", result.Output)
	}

	if err := w.git.Push(ctx, wtDir, w.pr.HeadRef); err != nil {
		return fmt.Errorf("push: %w", err)
	}

	w.logger.Info("checks fixed and pushed")
	return nil
}

func (w *Worker) fixReviews(ctx context.Context, wtDir string) error {
	w.logger.Info("fixing review comments")

	threads, err := w.gh.GetReviewThreads(ctx, w.repo.Owner, w.repo.Name, w.pr.Number)
	if err != nil {
		return fmt.Errorf("get review threads: %w", err)
	}

	// Filter unresolved, non-outdated threads from copilot
	var unresolvedCount int
	for _, t := range threads {
		if t.IsResolved || t.IsOutdated {
			continue
		}
		for _, c := range t.Comments {
			if c.Author == "copilot" || c.Author == "github-copilot[bot]" {
				unresolvedCount++
				break
			}
		}
	}

	if unresolvedCount == 0 {
		w.logger.Info("no unresolved copilot reviews found")
		return nil
	}

	w.logger.Info("found unresolved copilot reviews", "count", unresolvedCount)

	if err := w.git.Fetch(ctx, wtDir); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", w.repo.Owner, w.repo.Name, w.pr.Number)
	result, err := w.claude.RunCommand(ctx, wtDir, "fix-review-auto", prURL)
	if err != nil {
		return fmt.Errorf("claude fix reviews: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("claude failed: %s", result.Output)
	}

	if err := w.git.Push(ctx, wtDir, w.pr.HeadRef); err != nil {
		return fmt.Errorf("push: %w", err)
	}

	w.logger.Info("reviews fixed and pushed")
	return nil
}

func (w *Worker) merge(ctx context.Context) error {
	return w.gh.MergePR(ctx, w.repo.Owner, w.repo.Name, w.pr.Number, w.repo.MergeMethod)
}
