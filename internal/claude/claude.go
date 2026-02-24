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
	model             string
	useTmux           bool
	tmuxSessionPrefix string
	runID             string // unique ID for this daemon run
	logger            *slog.Logger
}

func NewClient(model string, useTmux bool, tmuxSessionPrefix string, logger *slog.Logger) *Client {
	runID := fmt.Sprintf("%d", time.Now().Unix())

	// Clean up dangling sessions from previous runs when tmux enabled
	if useTmux {
		cleanupDanglingSessions(tmuxSessionPrefix, runID, logger)
	}

	return &Client{
		model:             model,
		useTmux:           useTmux,
		tmuxSessionPrefix: tmuxSessionPrefix,
		runID:             runID,
		logger:            logger,
	}
}

// cleanupDanglingSessions kills old auto-claude tmux sessions
func cleanupDanglingSessions(prefix, currentRunID string, logger *slog.Logger) {
	cmd := exec.Command("tmux", "ls", "-F", "#{session_name}")
	out, err := cmd.Output()
	if err != nil {
		// tmux not running or no sessions - OK
		return
	}

	sessions := strings.Split(string(out), "\n")
	for _, session := range sessions {
		session = strings.TrimSpace(session)
		if session == "" {
			continue
		}
		// Kill sessions matching our prefix but not current run
		if strings.HasPrefix(session, prefix+"-") && !strings.Contains(session, "-"+currentRunID+"-") {
			logger.Info("cleaning up dangling tmux session", "session", session)
			_ = exec.Command("tmux", "kill-session", "-t", session).Run()
		}
	}
}

// GenerateTmuxSessionName returns the tmux session name for a workdir (empty if tmux disabled)
func (c *Client) GenerateTmuxSessionName(workdir string) string {
	if !c.useTmux {
		return ""
	}
	return c.generateTmuxSessionName(workdir)
}

// generateTmuxSessionName creates a unique session name from workdir with run ID
func (c *Client) generateTmuxSessionName(workdir string) string {
	// Extract owner/repo/pr from workdir path
	// e.g., /tmp/auto-claude-dev/worktrees/automaat-ai-casino/pr-609
	// -> auto-claude-1234567890-automaat-ai-casino-pr-609
	parts := strings.Split(filepath.Clean(workdir), string(filepath.Separator))

	// Find worktrees index and extract repo/pr after it
	for i, part := range parts {
		if part == "worktrees" && i+2 < len(parts) {
			// parts[i+1] is repo name, parts[i+2] is pr-XXX
			repo := parts[i+1]
			prPart := parts[i+2]
			return fmt.Sprintf("%s-%s-%s-%s", c.tmuxSessionPrefix, c.runID, repo, prPart)
		}
	}

	// Fallback: use last 2 path components with run ID
	if len(parts) >= 2 {
		return fmt.Sprintf("%s-%s-%s-%s", c.tmuxSessionPrefix, c.runID, parts[len(parts)-2], parts[len(parts)-1])
	}

	// Ultimate fallback: run ID + timestamp
	return fmt.Sprintf("%s-%s-%x", c.tmuxSessionPrefix, c.runID, time.Now().UnixNano())
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

// RunInTmux spawns Claude in a tmux session in interactive mode
func (c *Client) RunInTmux(ctx context.Context, sessionName, workdir, prompt string, callback OutputCallback) (*Result, error) {
	c.logger.Info("spawning interactive claude in tmux", "session", sessionName, "workdir", workdir, "prompt_len", len(prompt))
	c.logger.Debug("claude prompt", "prompt", prompt)

	// Create log file for tmux output
	logFile := filepath.Join(workdir, ".auto-claude-logs", fmt.Sprintf("tmux-%s.log", sessionName))
	if err := os.MkdirAll(filepath.Dir(logFile), 0700); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	// Create empty log file immediately
	if err := os.WriteFile(logFile, []byte{}, 0600); err != nil {
		return nil, fmt.Errorf("create log file: %w", err)
	}

	// Check if session already exists and kill it to avoid collision
	hasSessionCmd := exec.CommandContext(ctx, "tmux", "has-session", "-t", sessionName)
	if err := hasSessionCmd.Run(); err == nil {
		// Session exists, kill it
		c.logger.Warn("tmux session already exists, killing old session", "session", sessionName)
		killCmd := exec.CommandContext(ctx, "tmux", "kill-session", "-t", sessionName)
		if killErr := killCmd.Run(); killErr != nil {
			c.logger.Warn("failed to kill existing tmux session", "session", sessionName, "err", killErr)
		}
	}

	// Build tmux command with proper terminal settings for Claude TUI
	// Start Claude in interactive mode WITHOUT -p flag to get full TUI
	// Pass session name via env var so Stop hook can create marker with correct name
	tmuxArgs := []string{
		"new-session", "-d",
		"-s", sessionName,
		"-c", workdir,
		"-x", "200", // width
		"-y", "50",  // height
		"-e", "TERM=screen-256color",
		"-e", "AUTO_CLAUDE_SESSION=" + sessionName,
		"claude", "--model", c.model, "--dangerously-skip-permissions",
	}

	// Create tmux session
	createCmd := exec.CommandContext(ctx, "tmux", tmuxArgs...)
	if out, err := createCmd.CombinedOutput(); err != nil {
		return &Result{
			Success: false,
			Output:  string(out),
		}, fmt.Errorf("create tmux session: %w\n%s", err, string(out))
	}

	// Enable remain-on-exit to capture exit status after Claude closes
	remainCmd := exec.CommandContext(ctx, "tmux", "set-option", "-t", sessionName, "remain-on-exit", "on")
	if err := remainCmd.Run(); err != nil {
		c.logger.Warn("failed to set remain-on-exit", "err", err)
	}

	// Wait for Claude TUI to initialize and show confirmation prompt
	time.Sleep(1 * time.Second)

	// Accept the "dangerously skip permissions" confirmation
	// Send Down arrow to select "Yes, I accept"
	downCmd := exec.CommandContext(ctx, "tmux", "send-keys", "-t", sessionName, "Down")
	if err := downCmd.Run(); err != nil {
		c.logger.Warn("failed to send Down to tmux", "err", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Send Enter to confirm
	confirmCmd := exec.CommandContext(ctx, "tmux", "send-keys", "-t", sessionName, "Enter")
	if err := confirmCmd.Run(); err != nil {
		c.logger.Warn("failed to send confirmation Enter to tmux", "err", err)
	}

	// Wait for Claude to finish accepting and show prompt input
	time.Sleep(500 * time.Millisecond)

	// Send the actual prompt via tmux send-keys
	sendCmd := exec.CommandContext(ctx, "tmux", "send-keys", "-t", sessionName, "-l", prompt)
	if err := sendCmd.Run(); err != nil {
		c.logger.Warn("failed to send prompt to tmux", "err", err)
	}

	// Send Enter to submit the prompt
	time.Sleep(100 * time.Millisecond)
	enterCmd := exec.CommandContext(ctx, "tmux", "send-keys", "-t", sessionName, "Enter")
	if err := enterCmd.Run(); err != nil {
		c.logger.Warn("failed to send Enter to tmux", "err", err)
	}

	// Start goroutine to capture output
	if callback != nil {
		go c.captureFromTmux(ctx, sessionName, callback)
	}

	// Save output to log file periodically
	go c.savePeriodicSnapshot(ctx, sessionName, logFile)

	// Wait for session to complete (monitors marker file from Stop hook)
	return c.waitForTmuxSession(ctx, sessionName, workdir, logFile)
}

// captureFromTmux captures output from tmux pane scrollback and streams to callback
func (c *Client) captureFromTmux(ctx context.Context, sessionName string, callback OutputCallback) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastLineCount int
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Capture pane content
			cmd := exec.Command("tmux", "capture-pane", "-t", sessionName, "-p", "-S", "-1000")
			out, err := cmd.Output()
			if err != nil {
				// Session might have ended
				continue
			}

			lines := strings.Split(string(out), "\n")
			if len(lines) > lastLineCount {
				// Send new lines to callback
				for i := lastLineCount; i < len(lines); i++ {
					if strings.TrimSpace(lines[i]) != "" {
						callback(lines[i])
					}
				}
				lastLineCount = len(lines)
			}
		}
	}
}

// savePeriodicSnapshot saves tmux pane content to log file periodically
func (c *Client) savePeriodicSnapshot(ctx context.Context, sessionName, logFile string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final save on exit
			cmd := exec.Command("tmux", "capture-pane", "-t", sessionName, "-p", "-S", "-1000")
			if out, err := cmd.Output(); err == nil {
				_ = os.WriteFile(logFile, out, 0600)
			}
			return
		case <-ticker.C:
			cmd := exec.Command("tmux", "capture-pane", "-t", sessionName, "-p", "-S", "-1000")
			if out, err := cmd.Output(); err == nil {
				_ = os.WriteFile(logFile, out, 0600)
			}
		}
	}
}

// waitForTmuxSession polls for Stop hook marker file, then exits Claude and captures status
func (c *Client) waitForTmuxSession(ctx context.Context, sessionName, workdir, logFile string) (*Result, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Marker written to ~/.auto-claude/markers/{session-name}.marker by Stop hook
	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = os.Getenv("HOME")
	}
	markerPath := filepath.Join(homeDir, ".auto-claude", "markers", sessionName+".marker")
	var exitSent bool

	for {
		select {
		case <-ctx.Done():
			// Kill session on context cancellation
			_ = exec.Command("tmux", "kill-session", "-t", sessionName).Run()
			_ = os.Remove(markerPath)
			return &Result{Success: false, Output: "cancelled"}, ctx.Err()
		case <-ticker.C:
			// Check for completion marker from Stop hook
			if !exitSent {
				if _, err := os.Stat(markerPath); err == nil {
					// Marker exists - Claude finished processing
					c.logger.Debug("claude completed (stop hook fired), sending exit", "session", sessionName, "marker", markerPath)

					// Send Ctrl-D to exit Claude gracefully
					exitCmd := exec.CommandContext(ctx, "tmux", "send-keys", "-t", sessionName, "C-d")
					if err := exitCmd.Run(); err != nil {
						c.logger.Warn("failed to send exit to tmux", "err", err)
					}

					exitSent = true
					_ = os.Remove(markerPath) // Clean up marker
				}
				continue
			}

			// After exit sent, wait for pane to die
			statusCmd := exec.Command("tmux", "display-message", "-t", sessionName, "-p", "#{pane_dead}")
			statusOut, err := statusCmd.Output()
			if err != nil {
				// Session might be completely gone - expected after exit
				return c.readFinalResult(logFile, 0)
			}

			isDead := strings.TrimSpace(string(statusOut)) == "1"
			if isDead {
				// Pane is dead, get exit status
				exitCodeCmd := exec.Command("tmux", "display-message", "-t", sessionName, "-p", "#{pane_dead_status}")
				exitOut, err := exitCodeCmd.Output()

				var exitCode int
				if err == nil {
					fmt.Sscanf(string(exitOut), "%d", &exitCode)
				}

				// Clean up session
				_ = exec.Command("tmux", "kill-session", "-t", sessionName).Run()

				return c.readFinalResult(logFile, exitCode)
			}
		}
	}
}

// readFinalResult reads log file and determines success based on exit code
func (c *Client) readFinalResult(logFile string, exitCode int) (*Result, error) {
	out, readErr := os.ReadFile(logFile)
	if readErr != nil {
		return &Result{
			Success:    false,
			Output:     "",
			OutputFile: logFile,
		}, fmt.Errorf("read log file: %w", readErr)
	}

	// Success if exit code is 0
	success := exitCode == 0

	return &Result{
		Success:    success,
		Output:     string(out),
		OutputFile: logFile,
	}, nil
}

// RunWithCallback spawns Claude with live output streaming via callback
func (c *Client) RunWithCallback(ctx context.Context, workdir, prompt string, callback OutputCallback) (*Result, error) {
	// Use tmux mode if enabled
	if c.useTmux {
		sessionName := c.generateTmuxSessionName(workdir)
		c.logger.Info("using tmux mode", "session", sessionName)
		return c.RunInTmux(ctx, sessionName, workdir, prompt, callback)
	}

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
		var lineBuf strings.Builder                      // Buffer for partial deltas
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
	// Use tmux mode if enabled
	if c.useTmux {
		sessionName := c.generateTmuxSessionName(workdir)
		c.logger.Info("using tmux mode for command", "session", sessionName, "command", command)
		prompt := fmt.Sprintf("/%s %s", command, strings.Join(args, " "))
		return c.RunInTmux(ctx, sessionName, workdir, prompt, callback)
	}

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
			var lineBuf strings.Builder                      // Buffer for partial deltas
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
