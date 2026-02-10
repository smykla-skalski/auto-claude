package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Client struct {
	model  string
	logger *slog.Logger
}

func NewClient(model string, logger *slog.Logger) *Client {
	return &Client{model: model, logger: logger}
}

type Result struct {
	Success      bool
	Output       string
	OutputFile   string
	DurationMs   int
	TotalCostUSD float64
	SessionID    string
	NumTurns     int
}

type jsonResponse struct {
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	DurationMs   int     `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	SessionID    string  `json:"session_id"`
	NumTurns     int     `json:"num_turns"`
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
		c.logger.Info("claude completed",
			"duration_ms", resp.DurationMs,
			"cost_usd", resp.TotalCostUSD,
			"turns", resp.NumTurns,
			"session_id", resp.SessionID)

		return &Result{
			Success:      !resp.IsError,
			Output:       resp.Result,
			DurationMs:   resp.DurationMs,
			TotalCostUSD: resp.TotalCostUSD,
			SessionID:    resp.SessionID,
			NumTurns:     resp.NumTurns,
		}, nil
	}

	// Fallback: treat raw output as success
	return &Result{
		Success: true,
		Output:  string(out),
	}, nil
}

// RunCommand spawns Claude Code CLI with a slash command.
// outputDir specifies where to save full Claude output logs.
func (c *Client) RunCommand(ctx context.Context, workdir, outputDir, command string, args ...string) (*Result, error) {
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

	// Save full output to file with high-resolution timestamp
	now := time.Now()
	timestamp := fmt.Sprintf("%s-%06d", now.Format("20060102-150405"), now.Nanosecond()/1000)
	logFile := filepath.Join(outputDir, fmt.Sprintf("claude-%s-%s.log", command, timestamp))

	// Create output directory with restrictive permissions
	if err := os.MkdirAll(outputDir, 0700); err != nil {
		c.logger.Warn("failed to create output directory", "dir", outputDir, "err", err)
	}

	// Write log with owner-only permissions
	if writeErr := os.WriteFile(logFile, out, 0600); writeErr != nil {
		c.logger.Warn("failed to save claude output", "err", writeErr)
	}

	if err != nil {
		c.logger.Error("claude command failed", "command", command, "output_file", logFile)
		return &Result{
			Success:    false,
			Output:     string(out),
			OutputFile: logFile,
		}, fmt.Errorf("claude command %s: %w\n%s", command, err, string(out))
	}

	var resp jsonResponse
	if jsonErr := json.Unmarshal(out, &resp); jsonErr == nil {
		c.logger.Info("claude completed",
			"command", command,
			"duration_ms", resp.DurationMs,
			"cost_usd", resp.TotalCostUSD,
			"turns", resp.NumTurns,
			"session_id", resp.SessionID,
			"output_file", logFile)

		return &Result{
			Success:      !resp.IsError,
			Output:       resp.Result,
			OutputFile:   logFile,
			DurationMs:   resp.DurationMs,
			TotalCostUSD: resp.TotalCostUSD,
			SessionID:    resp.SessionID,
			NumTurns:     resp.NumTurns,
		}, nil
	}

	c.logger.Info("claude completed (non-json response)", "output_file", logFile)
	return &Result{
		Success:    true,
		Output:     string(out),
		OutputFile: logFile,
	}, nil
}
