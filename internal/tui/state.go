package tui

import "time"

type Snapshot struct {
	Timestamp      time.Time
	Repos          []RepoState
	ClaudeSessions []ClaudeSessionState
	WorkerCount    int
}

type RepoState struct {
	Owner   string
	Name    string
	PRs     []PRState
	Workers int
}

type PRState struct {
	Number    int
	Title     string
	States    []string // draft|conflicting|checks_failing|checks_pending|copilot_pending|reviews_pending|ready
	Author    string
	HasWorker bool
}

type ClaudeSessionState struct {
	Repo     string
	PRNumber int
	Action   string
	Duration time.Duration
}
