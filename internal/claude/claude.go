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

// streamEvent represents a single event in stream-json output
type streamEvent struct {
	Type  string                 `json:"type"`
	Event map[string]interface{} `json:"event,omitempty"`
}

// resultEvent represents the final result in stream-json output
type resultEvent struct {
	Type         string  `json:"type"`
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
	var args []string
	if callback != nil {
		// Use stream-json for real-time streaming
		args = []string{
			"-p", prompt,
			"--output-format", "stream-json",
			"--verbose",
			"--include-partial-messages",
			"--no-session-persistence",
			"--dangerously-skip-permissions",
			"--model", c.model,
		}
	} else {
		// Use regular json when no streaming needed
		args = []string{
			"-p", prompt,
			"--output-format", "json",
			"--no-session-persistence",
			"--dangerously-skip-permissions",
			"--model", c.model,
		}
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

	// Stream stdout (parse stream-json events)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB max token
		var lineBuf strings.Builder // Buffer for partial deltas
		for scanner.Scan() {
			line := scanner.Text()
			outputMu.Lock()
			outputBuf.WriteString(line)
			outputBuf.WriteString("\n")
			outputMu.Unlock()

			// Parse stream-json event and extract text for callback
			if callback != nil {
				text := extractTextFromStreamEvent(line)
				if text != "" {
					lineBuf.WriteString(text)
					// Split on newlines and send complete lines
					content := lineBuf.String()
					if idx := strings.LastIndex(content, "\n"); idx >= 0 {
						lines := strings.Split(content[:idx+1], "\n")
						for _, l := range lines {
							if l != "" {
								callback(l)
							}
						}
						lineBuf.Reset()
						lineBuf.WriteString(content[idx+1:])
					}
				}
			}
		}
		// Flush remaining partial line if any
		if callback != nil && lineBuf.Len() > 0 {
			callback(lineBuf.String())
		}
		scanErrs <- scanner.Err()
		done <- struct{}{}
	}()

	// Stream stderr (raw lines)
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB max token
		for scanner.Scan() {
			line := scanner.Text()
			outputMu.Lock()
			outputBuf.WriteString(line)
			outputBuf.WriteString("\n")
			outputMu.Unlock()

			// Send stderr directly to callback for visibility
			if callback != nil {
				callback("[stderr] " + line)
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

	return c.parseStreamResult(out)
}

func (c *Client) parseResult(out []byte) (*Result, error) {
	// Try parsing JSON response
	var resp jsonResponse
	if jsonErr := json.Unmarshal(out, &resp); jsonErr == nil {
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

// parseStreamResult parses stream-json format output
func (c *Client) parseStreamResult(out []byte) (*Result, error) {
	lines := strings.Split(string(out), "\n")
	var resultData *resultEvent

	// Scan for result event in stream-json output
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var evt resultEvent
		if err := json.Unmarshal([]byte(line), &evt); err == nil && evt.Type == "result" {
			resultData = &evt
			break
		}
	}

	if resultData != nil {
		return &Result{
			Success:      !resultData.IsError,
			Output:       resultData.Result,
			DurationMs:   resultData.DurationMs,
			TotalCostUSD: resultData.TotalCostUSD,
			SessionID:    resultData.SessionID,
			NumTurns:     resultData.NumTurns,
		}, nil
	}

	// Fallback: treat as success if no result event found
	c.logger.Warn("no result event found in stream-json output")
	return &Result{
		Success: true,
		Output:  string(out),
	}, nil
}

// extractTextFromStreamEvent extracts displayable text from a stream-json event line
func extractTextFromStreamEvent(line string) string {
	var evt streamEvent
	if err := json.Unmarshal([]byte(line), &evt); err != nil {
		return ""
	}

	// Extract content_block_delta events with text_delta
	if evt.Type == "stream_event" && evt.Event != nil {
		if eventType, ok := evt.Event["type"].(string); ok && eventType == "content_block_delta" {
			if delta, ok := evt.Event["delta"].(map[string]interface{}); ok {
				if deltaType, ok := delta["type"].(string); ok && deltaType == "text_delta" {
					if text, ok := delta["text"].(string); ok {
						return text
					}
				}
			}
		}
	}

	return ""
}

// RunCommand spawns Claude Code CLI with a slash command.
// outputDir specifies where to save full Claude output logs.
func (c *Client) RunCommand(ctx context.Context, workdir, outputDir, command string, args ...string) (*Result, error) {
	return c.RunCommandWithCallback(ctx, workdir, outputDir, command, nil, args...)
}

// RunCommandWithCallback spawns Claude command with live output streaming
func (c *Client) RunCommandWithCallback(ctx context.Context, workdir, outputDir, command string, callback OutputCallback, args ...string) (*Result, error) {
	var cliArgs []string
	if callback != nil {
		// Use stream-json for real-time streaming
		cliArgs = []string{
			"-p", fmt.Sprintf("/%s %s", command, strings.Join(args, " ")),
			"--output-format", "stream-json",
			"--verbose",
			"--include-partial-messages",
			"--no-session-persistence",
			"--dangerously-skip-permissions",
			"--model", c.model,
		}
	} else {
		// Use regular json when no streaming needed
		cliArgs = []string{
			"-p", fmt.Sprintf("/%s %s", command, strings.Join(args, " ")),
			"--output-format", "json",
			"--no-session-persistence",
			"--dangerously-skip-permissions",
			"--model", c.model,
		}
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

		// Stream stdout (parse stream-json events)
		go func() {
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB max token
			var lineBuf strings.Builder // Buffer for partial deltas
			for scanner.Scan() {
				line := scanner.Text()
				outputMu.Lock()
				outputBuf.WriteString(line)
				outputBuf.WriteString("\n")
				outputMu.Unlock()

				// Parse stream-json event and extract text for callback
				if callback != nil {
					text := extractTextFromStreamEvent(line)
					if text != "" {
						lineBuf.WriteString(text)
						// Split on newlines and send complete lines
						content := lineBuf.String()
						if idx := strings.LastIndex(content, "\n"); idx >= 0 {
							lines := strings.Split(content[:idx+1], "\n")
							for _, l := range lines {
								if l != "" {
									callback(l)
								}
							}
							lineBuf.Reset()
							lineBuf.WriteString(content[idx+1:])
						}
					}
				}
			}
			// Flush remaining partial line if any
			if callback != nil && lineBuf.Len() > 0 {
				callback(lineBuf.String())
			}
			scanErrs <- scanner.Err()
			done <- struct{}{}
		}()

		// Stream stderr (raw lines)
		go func() {
			scanner := bufio.NewScanner(stderr)
			scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB max token
			for scanner.Scan() {
				line := scanner.Text()
				outputMu.Lock()
				outputBuf.WriteString(line)
				outputBuf.WriteString("\n")
				outputMu.Unlock()

				// Send stderr directly to callback for visibility
				if callback != nil {
					callback("[stderr] " + line)
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

	// Try parsing result based on whether streaming was used
	var result *Result
	var parseErr error

	if callback != nil {
		// Stream-json format
		result, parseErr = c.parseStreamResult(out)
	} else {
		// Regular json format
		result, parseErr = c.parseResult(out)
	}

	if parseErr != nil {
		c.logger.Warn("failed to parse result", "err", parseErr)
		return &Result{
			Success:    true,
			Output:     string(out),
			OutputFile: logFile,
		}, nil
	}

	// Add output file to result
	result.OutputFile = logFile

	c.logger.Info("claude completed",
		"command", command,
		"duration_ms", result.DurationMs,
		"cost_usd", result.TotalCostUSD,
		"turns", result.NumTurns,
		"session_id", result.SessionID,
		"output_file", logFile)

	return result, nil
}
