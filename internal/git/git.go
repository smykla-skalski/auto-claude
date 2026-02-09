package git

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Client struct {
	workdir string
	logger  *slog.Logger
}

func NewClient(workdir string, logger *slog.Logger) *Client {
	return &Client{workdir: workdir, logger: logger}
}

// CloneDir returns the bare clone directory for a repo.
func (c *Client) CloneDir(owner, repo string) string {
	return filepath.Join(c.workdir, "clones", owner+"-"+repo)
}

// WorktreeDir returns the worktree directory for a specific PR.
func (c *Client) WorktreeDir(owner, repo string, prNumber int) string {
	return filepath.Join(c.workdir, "worktrees", owner+"-"+repo, fmt.Sprintf("pr-%d", prNumber))
}

// EnsureClone clones the repo if missing, fetches if exists.
func (c *Client) EnsureClone(ctx context.Context, owner, repo string) error {
	dir := c.CloneDir(owner, repo)

	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		c.logger.Debug("fetching existing clone", "dir", dir)
		return c.run(ctx, dir, "git", "fetch", "--all", "--prune")
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	url := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	c.logger.Info("cloning repo", "url", url, "dir", dir)
	return c.run(ctx, "", "git", "clone", url, dir)
}

// AddWorktree creates a worktree for the given branch.
func (c *Client) AddWorktree(ctx context.Context, owner, repo, branch string, prNumber int) (string, error) {
	cloneDir := c.CloneDir(owner, repo)
	wtDir := c.WorktreeDir(owner, repo, prNumber)

	if err := os.MkdirAll(filepath.Dir(wtDir), 0o755); err != nil {
		return "", fmt.Errorf("mkdir worktree parent: %w", err)
	}

	// Remove stale worktree if exists
	if _, err := os.Stat(wtDir); err == nil {
		_ = c.run(ctx, cloneDir, "git", "worktree", "remove", "--force", wtDir)
	}

	c.logger.Info("adding worktree", "branch", branch, "dir", wtDir)
	if err := c.run(ctx, cloneDir, "git", "worktree", "add", wtDir, "origin/"+branch); err != nil {
		return "", fmt.Errorf("add worktree: %w", err)
	}

	// Checkout the branch (detached HEAD → actual branch)
	if err := c.run(ctx, wtDir, "git", "checkout", "-B", branch, "origin/"+branch); err != nil {
		return "", fmt.Errorf("checkout branch: %w", err)
	}

	// Set upstream
	_ = c.run(ctx, wtDir, "git", "branch", "--set-upstream-to=origin/"+branch, branch)

	return wtDir, nil
}

// RemoveWorktree removes a worktree.
func (c *Client) RemoveWorktree(ctx context.Context, owner, repo string, prNumber int) error {
	cloneDir := c.CloneDir(owner, repo)
	wtDir := c.WorktreeDir(owner, repo, prNumber)

	c.logger.Debug("removing worktree", "dir", wtDir)
	if err := c.run(ctx, cloneDir, "git", "worktree", "remove", "--force", wtDir); err != nil {
		// Fallback: just remove the directory
		_ = os.RemoveAll(wtDir)
	}
	return nil
}

// Fetch fetches all remotes in the given directory.
func (c *Client) Fetch(ctx context.Context, dir string) error {
	return c.run(ctx, dir, "git", "fetch", "origin")
}

// Push pushes the branch to the remote.
func (c *Client) Push(ctx context.Context, dir, branch string) error {
	return c.run(ctx, dir, "git", "push", "origin", branch)
}

// HasUnpushedCommits checks if there are local commits not on remote.
func (c *Client) HasUnpushedCommits(ctx context.Context, dir, branch string) (bool, error) {
	// Count commits ahead of remote
	cmd := exec.CommandContext(ctx, "git", "rev-list", "--count", "origin/"+branch+".."+branch)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		c.logger.Debug("exec", "cmd", "git rev-list --count origin/"+branch+".."+branch, "dir", dir)
		return false, fmt.Errorf("git rev-list --count origin/%s..%s: %w\n%s", branch, branch, err, string(out))
	}

	count := strings.TrimSpace(string(out))
	return count != "0", nil
}

func (c *Client) run(ctx context.Context, dir string, name string, args ...string) error {
	c.logger.Debug("exec", "cmd", name+" "+strings.Join(args, " "), "dir", dir)
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, string(out))
	}
	return nil
}
