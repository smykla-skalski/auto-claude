package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// OutputCallback receives live output line by line as Claude runs
type OutputCallback func(line string)

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
	return c.RunWithCallback(ctx, workdir, prompt, nil)
}

// RunWithCallback spawns Claude with live output streaming via callback
func (c *Client) RunWithCallback(ctx context.Context, workdir, prompt string, callback OutputCallback) (*Result, error) {
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

	if callback == nil {
		// No streaming, use original behavior
		out, err := cmd.CombinedOutput()
		if err != nil {
			return &Result{
				Success: false,
				Output:  string(out),
			}, fmt.Errorf("claude: %w\n%s", err, string(out))
		}
		return c.parseResult(out)
	}

	// Stream output
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	var outputBuf strings.Builder
	var outputMu sync.Mutex
	done := make(chan struct{}, 2)
	scanErrs := make(chan error, 2)

	// Stream stdout
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB max token
		for scanner.Scan() {
			line := scanner.Text()
			outputMu.Lock()
			outputBuf.WriteString(line)
			outputBuf.WriteString("\n")
			outputMu.Unlock()
			if callback != nil {
				callback(line)
			}
		}
		scanErrs <- scanner.Err()
		done <- struct{}{}
	}()

	// Stream stderr
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB max token
		for scanner.Scan() {
			line := scanner.Text()
			outputMu.Lock()
			outputBuf.WriteString(line)
			outputBuf.WriteString("\n")
			outputMu.Unlock()
			if callback != nil {
				callback(line)
			}
		}
		scanErrs <- scanner.Err()
		done <- struct{}{}
	}()

	// Wait for both goroutines
	<-done
	<-done

	// Check scanner errors
	if scanErr := <-scanErrs; scanErr != nil {
		return nil, fmt.Errorf("scan stdout: %w", scanErr)
	}
	if scanErr := <-scanErrs; scanErr != nil {
		return nil, fmt.Errorf("scan stderr: %w", scanErr)
	}

	err = cmd.Wait()
	outputMu.Lock()
	out := []byte(outputBuf.String())
	outputMu.Unlock()

	if err != nil {
		return &Result{
			Success: false,
			Output:  string(out),
		}, fmt.Errorf("claude: %w\n%s", err, string(out))
	}

	return c.parseResult(out)
}

func (c *Client) parseResult(out []byte) (*Result, error) {
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
	return c.RunCommandWithCallback(ctx, workdir, outputDir, command, nil, args...)
}

// RunCommandWithCallback spawns Claude command with live output streaming
func (c *Client) RunCommandWithCallback(ctx context.Context, workdir, outputDir, command string, callback OutputCallback, args ...string) (*Result, error) {
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

	var out []byte
	var cmdErr error

	if callback == nil {
		// No streaming
		out, cmdErr = cmd.CombinedOutput()
	} else {
		// Stream output
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("create stdout pipe: %w", err)
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return nil, fmt.Errorf("create stderr pipe: %w", err)
		}

		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("start claude: %w", err)
		}

		var outputBuf strings.Builder
		var outputMu sync.Mutex
		done := make(chan struct{}, 2)
		scanErrs := make(chan error, 2)

		// Stream stdout
		go func() {
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB max token
			for scanner.Scan() {
				line := scanner.Text()
				outputMu.Lock()
				outputBuf.WriteString(line)
				outputBuf.WriteString("\n")
				outputMu.Unlock()
				callback(line)
			}
			scanErrs <- scanner.Err()
			done <- struct{}{}
		}()

		// Stream stderr
		go func() {
			scanner := bufio.NewScanner(stderr)
			scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB max token
			for scanner.Scan() {
				line := scanner.Text()
				outputMu.Lock()
				outputBuf.WriteString(line)
				outputBuf.WriteString("\n")
				outputMu.Unlock()
				callback(line)
			}
			scanErrs <- scanner.Err()
			done <- struct{}{}
		}()

		// Wait for both goroutines
		<-done
		<-done

		// Check scanner errors
		if scanErr := <-scanErrs; scanErr != nil {
			return nil, fmt.Errorf("scan stdout: %w", scanErr)
		}
		if scanErr := <-scanErrs; scanErr != nil {
			return nil, fmt.Errorf("scan stderr: %w", scanErr)
		}

		cmdErr = cmd.Wait()
		outputMu.Lock()
		out = []byte(outputBuf.String())
		outputMu.Unlock()
	}

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

	if cmdErr != nil {
		c.logger.Error("claude command failed", "command", command, "output_file", logFile)
		return &Result{
			Success:    false,
			Output:     string(out),
			OutputFile: logFile,
		}, fmt.Errorf("claude command %s: %w\n%s", command, cmdErr, string(out))
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
