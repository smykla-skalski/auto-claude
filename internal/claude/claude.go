package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

type Client struct {
	model  string
	logger *slog.Logger
}

func NewClient(model string, logger *slog.Logger) *Client {
	return &Client{model: model, logger: logger}
}

type Result struct {
	Success bool
	Output  string
}

type jsonResponse struct {
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// Run spawns Claude Code CLI non-interactively.
func (c *Client) Run(ctx context.Context, workdir, prompt string) (*Result, error) {
	args := []string{
		"-p", prompt,
		"--output-format", "json",
		"--no-session-persistence",
		"--dangerously-skip-permissions",
		"--model", c.model,
	}

	c.logger.Info("spawning claude", "workdir", workdir, "prompt_len", len(prompt))
	c.logger.Debug("claude prompt", "prompt", prompt)

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = workdir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &Result{
			Success: false,
			Output:  string(out),
		}, fmt.Errorf("claude: %w\n%s", err, string(out))
	}

	// Try parsing JSON response
	var resp jsonResponse
	if jsonErr := json.Unmarshal(out, &resp); jsonErr == nil {
		return &Result{
			Success: !resp.IsError,
			Output:  resp.Result,
		}, nil
	}

	// Fallback: treat raw output as success
	return &Result{
		Success: true,
		Output:  string(out),
	}, nil
}

// RunCommand spawns Claude Code CLI with a slash command.
func (c *Client) RunCommand(ctx context.Context, workdir, command string, args ...string) (*Result, error) {
	cliArgs := []string{
		"-p", fmt.Sprintf("/%s %s", command, strings.Join(args, " ")),
		"--output-format", "json",
		"--no-session-persistence",
		"--dangerously-skip-permissions",
		"--model", c.model,
	}

	c.logger.Info("spawning claude command", "command", command, "workdir", workdir)

	cmd := exec.CommandContext(ctx, "claude", cliArgs...)
	cmd.Dir = workdir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &Result{
			Success: false,
			Output:  string(out),
		}, fmt.Errorf("claude command %s: %w\n%s", command, err, string(out))
	}

	var resp jsonResponse
	if jsonErr := json.Unmarshal(out, &resp); jsonErr == nil {
		return &Result{
			Success: !resp.IsError,
			Output:  resp.Result,
		}, nil
	}

	return &Result{
		Success: true,
		Output:  string(out),
	}, nil
}
