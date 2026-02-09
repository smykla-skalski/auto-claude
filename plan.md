# Auto-Claude: PR Automation Daemon

## Context

Build a Go daemon that monitors GitHub repos for open PRs and autonomously manages them through their lifecycle using Claude Code CLI. Spawns one worker goroutine per PR to avoid concurrency issues. Actions: resolve merge conflicts, fix failing builds, fix copilot review comments, merge when ready.

## Project Structure

```
auto-claude/
├── cmd/auto-claude/main.go          # entry point, signal handling, config loading
├── internal/
│   ├── config/config.go             # YAML config parsing + validation
│   ├── daemon/daemon.go             # poll loop, worker lifecycle management
│   ├── github/github.go             # gh CLI wrapper (list PRs, checks, reviews, merge)
│   ├── worker/
│   │   ├── worker.go                # per-PR goroutine + state machine
│   │   └── actions.go               # conflict resolution, build fix, review fix, merge
│   ├── claude/claude.go             # Claude Code CLI wrapper (spawn, parse output)
│   └── git/git.go                   # git operations (clone, worktree, push)
├── config.yaml                      # example config
├── go.mod
└── mise.toml
```

Plus one new command file:
- `~/.claude/commands/fix-review-auto.md` — non-interactive variant of fix-review

## Implementation Order

### 1. Project scaffolding
- `go mod init github.com/marcin-skalski/auto-claude`
- `mise.toml` with Go version
- `config.yaml` example

### 2. Config (`internal/config/config.go`)

```yaml
poll_interval: 60s
workdir: /tmp/auto-claude    # base dir for clones/worktrees

claude:
  model: opus

repos:
  - owner: myorg
    name: myrepo
    base_branch: main        # default: main
    exclude_authors:          # skip PRs from these authors
      - dependabot[bot]
    merge_method: squash      # squash | merge
    max_concurrent_prs: 3

log:
  level: info                # debug | info | warn | error
```

Types: `Config`, `ClaudeConfig`, `RepoConfig`, `LogConfig`
- Parse with `gopkg.in/yaml.v3`
- Validate required fields, set defaults

### 3. GitHub client (`internal/github/github.go`)

All via `gh` CLI + JSON parsing:

- `ListOpenPRs(ctx, owner, repo) ([]PRInfo, error)` — `gh pr list -R owner/repo --json number,title,headRefName,baseRefName,url,isDraft,author,labels,mergeable,mergeStateStatus,statusCheckRollup`
- `GetPRDetail(ctx, owner, repo, number) (*PRInfo, error)` — `gh pr view N -R owner/repo --json ...`
- `GetReviewThreads(ctx, owner, repo, number) ([]ReviewThread, error)` — GraphQL query (same as fix-review.md)
- `MergePR(ctx, owner, repo, number, method) error` — `gh pr merge N -R owner/repo --squash --delete-branch`

Key types:
```go
type PRInfo struct {
    Number           int
    Title            string
    HeadRef          string
    BaseRef          string
    URL              string
    Mergeable        string    // MERGEABLE, CONFLICTING, UNKNOWN
    IsDraft          bool
    Author           string
    Checks           []Check
}

type Check struct {
    Name   string
    Status string
    Conclusion string
}

type ReviewThread struct {
    IsResolved bool
    IsOutdated bool
    Path       string
    Line       int
    Comments   []ReviewComment
}

type ReviewComment struct {
    Author string
    Body   string
}
```

### 4. Git client (`internal/git/git.go`)

Directory layout:
```
<workdir>/
  clones/<owner>-<repo>/          # bare clone
  worktrees/<owner>-<repo>/
    pr-123/                       # worktree for PR #123
```

Operations:
- `EnsureClone(ctx, owner, repo, destDir) error` — clone if missing, fetch if exists
- `AddWorktree(ctx, cloneDir, branch, worktreeDir) error` — `git worktree add`
- `RemoveWorktree(ctx, cloneDir, worktreeDir) error` — `git worktree remove --force`
- `Fetch(ctx, dir) error`
- `Push(ctx, dir, remote, branch) error` — regular push (Claude commits on top)
- `CheckoutBranch(ctx, dir, branch) error` — ensure worktree is on correct branch

### 5. Claude client (`internal/claude/claude.go`)

Spawn Claude Code CLI non-interactively:

```go
func (c *client) Run(ctx context.Context, workdir, prompt string) (*Result, error) {
    args := []string{
        "-p", prompt,
        "--output-format", "json",
        "--no-session-persistence",
        "--dangerously-skip-permissions",
        "--model", c.model,
    }
    cmd := exec.CommandContext(ctx, "claude", args...)
    cmd.Dir = workdir
    output, err := cmd.CombinedOutput()
    // parse JSON response
}
```

Result type:
```go
type Result struct {
    Success bool
    Output  string
}
```

### 6. Worker state machine (`internal/worker/worker.go` + `actions.go`)

**State priority (evaluated top-to-bottom):**
1. Draft → skip, sleep
2. Mergeable == "CONFLICTING" → `resolveConflicts()`
3. Any check with conclusion "failure" → `fixChecks()`
4. Unresolved review thread from `copilot`/`github-copilot[bot]` → `fixReviews()`
5. Any check pending → sleep, wait
6. All checks pass + no unresolved reviews → `merge()`

**Worker loop:**
```
setup worktree → evaluate state → take action → sleep 30s → re-evaluate → ...
```

- `consecutiveFailures` counter with exponential backoff: `min(30s * 2^failures, 5m)`
- Max retries: 3 per action type, then give up on that PR
- Context cancellation for graceful shutdown
- Cleanup worktree on exit (defer)

**Actions:**

#### resolveConflicts()
- `git fetch origin`
- Spawn Claude: "This branch has conflicts with {base}. Run `git merge origin/{base}`, resolve all conflicts, then `git add . && git commit -s -S -m 'resolve merge conflicts'`"
- Push after Claude succeeds

#### fixChecks()
- Get failing check names from PRInfo
- Spawn Claude: "CI checks failing: {names}. Investigate failures, fix code, commit with -s -S flags. Run relevant tests locally to verify."
- Push after Claude succeeds

#### fixReviews()
- Spawn Claude with prompt from `fix-review-auto.md` command, passing PR URL
- Push after Claude succeeds

#### merge()
- `gh pr merge N -R owner/repo --squash --delete-branch`
- Worker exits after successful merge

### 7. Daemon (`internal/daemon/daemon.go`)

- Poll loop on `time.Ticker` with configured interval
- For each repo: `ListOpenPRs()`, filter excluded authors, check concurrency limit
- Track active workers in `map[string]context.CancelFunc` (key: `owner/repo#number`)
- Start new worker goroutine for new PRs
- Cancel workers for PRs no longer open (closed/merged externally)
- Graceful shutdown: cancel all workers on SIGINT/SIGTERM, wait for cleanup

```go
type Daemon struct {
    cfg     config.Config
    gh      github.Client
    claude  claude.Client
    git     git.Client
    logger  *slog.Logger
    mu      sync.Mutex
    workers map[string]context.CancelFunc
}
```

### 8. Entry point (`cmd/auto-claude/main.go`)

- Flag: `--config` path (default `config.yaml`)
- Load config, setup `slog` logger
- `signal.NotifyContext` for SIGINT/SIGTERM
- Create daemon, run

### 9. Non-interactive fix-review command

Create `~/.claude/commands/fix-review-auto.md`:
- Same analysis logic as fix-review.md (fetch threads, research, categorize)
- Key differences:
  - No `EnterPlanMode` — work directly
  - Auto-apply all valid fixes
  - Skip questionable comments (log them)
  - Skip invalid comments (log them)
  - Auto-commit with `-s -S` flags after applying fixes
  - No user interaction

### 10. Dependencies

- `gopkg.in/yaml.v3` — config parsing
- stdlib only for everything else (`os/exec`, `encoding/json`, `log/slog`, `context`, `sync`)

## Verification

1. `go build ./cmd/auto-claude/` compiles
2. Unit tests for state detection logic (table-driven with mock PRInfo)
3. Manual test: create a test repo with a PR that has conflicts, run daemon, verify it resolves + pushes
4. Manual test: create a PR with failing CI, verify daemon spawns Claude to fix
5. Check logs for proper worker lifecycle (start, action, sleep, re-evaluate, merge/cleanup)
