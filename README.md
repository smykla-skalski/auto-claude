# 🤖 auto-claude

[![CI](https://github.com/marcin-skalski/auto-claude/actions/workflows/ci.yaml/badge.svg)](https://github.com/marcin-skalski/auto-claude/actions/workflows/ci.yaml)
[![Go Version](https://img.shields.io/badge/go-1.26.0-blue.svg)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

GitHub PR automation that never sleeps. A daemon that monitors pull requests and autonomously manages their lifecycle using Claude Code CLI: resolves merge conflicts, fixes failing builds, addresses review comments, and merges when ready.

Born from frustration with Renovate PRs piling up. Built for teams that want PRs to land without manual babysitting.

## ✨ Features

**Automated PR Actions**

- 🔀 Resolves merge conflicts by invoking Claude with conflict context
- 🛠️ Fixes failing CI/tests using build logs and error messages
- 💬 Addresses unresolved review comments from Copilot or team members
- ✅ Auto-merges when all checks pass and reviews are resolved

**Smart PR Management**

- 🤖 Copilot review gating (waits for Copilot approval before merging)
- ⚡ Concurrent worker goroutines per PR (configurable limit)
- 🎯 Intelligent skipping (drafts, on-hold/blocked labels, pending checks)
- 🔄 Graceful error handling for worker failures

**Developer Experience**

- 📊 Interactive TUI dashboard with real-time Claude output streaming
- 📝 Dual logging: structured logfmt (tint) for production, colored for development
- 🔧 Per-repo configuration (exclude authors, merge methods, worker limits)
- 🧹 Automatic worktree cleanup after worker completion

## 🚀 Quick Start

### Prerequisites

```bash
# Install dependencies via Homebrew (macOS)
brew install gh git mise

# Or via package manager on Linux
# apt-get install gh git  # Debian/Ubuntu
# yum install gh git      # RHEL/CentOS

# Install mise (https://mise.jdx.dev/)
curl https://mise.run | sh

# Authenticate with GitHub
gh auth login
```

### Installation

```bash
# Clone repository
git clone https://github.com/marcin-skalski/auto-claude.git
cd auto-claude

# Install Go via mise
mise install

# Build binary
go build -o auto-claude ./cmd/auto-claude

# Or use mise for development
mise dev  # Builds and runs with config-dev.yaml
```

### Configuration

Create `config.yaml`:

```yaml
poll_interval: 60s
workdir: /tmp/auto-claude
log_file: /tmp/auto-claude/logs/auto-claude.log

claude:
  model: opus  # opus | sonnet | haiku

repos:
  - owner: myorg
    name: myrepo
    base_branch: main
    exclude_authors:
      - dependabot[bot]
    merge_method: squash  # squash | merge
    max_concurrent_prs: 3
    require_copilot_review: true

log:
  level: info  # debug | info | warn | error

tui:
  refresh_interval: 3s
```

### Run

```bash
# Interactive TUI mode (default)
./auto-claude --config config.yaml

# Headless mode (for servers/CI)
./auto-claude --config config.yaml --no-tui

# Or set environment variable
AUTO_CLAUDE_TUI=0 ./auto-claude --config config.yaml
```

## 🏗️ How It Works

```
┌─────────────────────────────────────────────────────────────┐
│                        auto-claude                          │
│                         (daemon)                            │
└──────────────┬──────────────────────────────────────────────┘
               │ polls every 60s
               ├─────────────┬─────────────┬──────────────
               ▼             ▼             ▼
        ┌──────────┐  ┌──────────┐  ┌──────────┐
        │ org/repo1│  │ org/repo2│  │ org/repo3│
        │  (GitHub)│  │  (GitHub)│  │  (GitHub)│
        └─────┬────┘  └─────┬────┘  └─────┬────┘
              │             │             │
              │ spawns      │ spawns      │ spawns
              │ workers     │ workers     │ workers
              ▼             ▼             ▼
        ┌─────────┐   ┌─────────┐   ┌─────────┐
        │ PR #123 │   │ PR #456 │   │ PR #789 │
        │ worker  │   │ worker  │   │ worker  │
        │ (goro.) │   │ (goro.) │   │ (goro.) │
        └────┬────┘   └────┬────┘   └────┬────┘
             │             │             │
             │ invokes     │ invokes     │ invokes
             ▼             ▼             ▼
        ┌─────────────────────────────────────┐
        │         Claude Code CLI             │
        │  (conflict resolution, CI fixes,    │
        │   review comments, verification)    │
        └─────────────────────────────────────┘
```

### Worker State Machine

Each PR worker progresses through 6 states:

```
┌────────┐
│ Draft  │──────► Skip (wait for next poll)
└────────┘

┌──────────────┐
│ Conflicting  │──────► Invoke Claude: resolve conflicts → push
└──────────────┘

┌───────────────┐
│ Checks Failing│──────► Invoke Claude: fix CI/tests → push
└───────────────┘

┌────────────────┐
│ Checks Pending │──────► Skip (wait for CI completion)
└────────────────┘

┌─────────────────┐
│ Reviews Pending │──────► Invoke Claude: address comments → push
└─────────────────┘

┌───────┐
│ Ready │──────► Merge with configured method → worker exits
└───────┘
```

**Polling Model**: Daemon polls on a fixed interval and ensures a worker is running for each PR. If no worker is active, it starts one; each worker then loops, refreshing PR state and taking actions until it exits. This prevents stale state and allows concurrent PR processing.

## ⚙️ Configuration

### Complete Reference

```yaml
# Polling frequency (default: 60s)
poll_interval: 60s

# Base directory for git operations (default: /tmp/auto-claude)
workdir: /tmp/auto-claude

# Log file path with automatic rotation (default: {workdir}/logs/auto-claude.log)
log_file: /tmp/auto-claude/logs/auto-claude.log

# Claude Code CLI configuration
claude:
  model: opus  # opus (most capable), sonnet (balanced), haiku (fastest)

# Repository configurations (multiple repos supported)
repos:
  - owner: myorg           # GitHub organization or user
    name: myrepo           # Repository name
    base_branch: main      # Target branch for PRs (default: main)

    # Skip PRs from these authors
    exclude_authors:
      - dependabot[bot]
      - renovate[bot]

    # Merge strategy (default: squash)
    merge_method: squash   # squash (single commit) | merge (preserve history)

    # Maximum concurrent worker goroutines per repo (default: 3)
    max_concurrent_prs: 3

    # Wait for Copilot review completion before merging (default: true)
    # Set to false for personal projects or repos without Copilot
    require_copilot_review: true

# Logging configuration
log:
  level: info  # debug (verbose), info (default), warn, error

# TUI configuration
tui:
  refresh_interval: 3s  # Dashboard update frequency (default: 3s)
```

### Multi-Repo Example

```yaml
poll_interval: 30s
workdir: /var/lib/auto-claude
log_file: /var/log/auto-claude/daemon.log

claude:
  model: sonnet

repos:
  # Production service: strict review gating
  - owner: acme-corp
    name: api-service
    base_branch: main
    merge_method: squash
    max_concurrent_prs: 2
    require_copilot_review: true
    exclude_authors:
      - dependabot[bot]

  # Infrastructure: auto-merge dependency updates
  - owner: acme-corp
    name: terraform-modules
    base_branch: master
    merge_method: merge
    max_concurrent_prs: 5
    require_copilot_review: false
    exclude_authors:
      - renovate[bot]

  # Personal project: no Copilot gating
  - owner: myusername
    name: side-project
    base_branch: main
    merge_method: squash
    max_concurrent_prs: 10
    require_copilot_review: false

log:
  level: warn
```

## 📖 Usage

### Interactive TUI Mode

Default mode when running in terminal with TTY:

```bash
./auto-claude --config config.yaml
```

**TUI Dashboard Features:**

- 📂 Repository tree with PR status indicators
- 🔴🟡🟢 Color-coded PR states (red=action needed, yellow=pending, green=ready)
- 📺 Live Claude output streaming in scrollable pane
- ⌨️ Keyboard navigation:
  - `↑/↓` or `j/k`: Navigate PR list
  - `Enter`: View Claude output for selected PR
  - `PgUp/PgDn`: Scroll Claude output
  - `q` or `Ctrl+C`: Quit
- 🔄 Auto-refreshes every 3s (configurable)

**ASCII Representation:**

```
┌─────────────────────────────────────────────────────────────┐
│ auto-claude v1.0.0 │ Polling: 60s │ Next poll: 23s          │
├─────────────────────────────────────────────────────────────┤
│ Repositories                                                │
│                                                             │
│ ▼ myorg/api-service (3 PRs)                                │
│   🔴 #123 Fix auth bug                    [checks_failing] │
│   🟡 #124 Add logging                     [checks_pending] │
│   🟢 #125 Update deps                     [ready]          │
│                                                             │
│ ▼ myorg/frontend (2 PRs)                                   │
│   🔴 #456 Dark mode                       [conflicting]    │
│   🟢 #457 Fix typo                        [ready]          │
├─────────────────────────────────────────────────────────────┤
│ Claude Output: myorg/api-service #123                      │
│                                                             │
│ [2024-01-15 10:23:45] Starting CI fix action               │
│ [2024-01-15 10:23:47] Reading test failure logs...         │
│ [2024-01-15 10:24:12] Fixed null pointer in auth handler   │
│ [2024-01-15 10:24:15] Running tests locally...             │
│ [2024-01-15 10:24:45] All tests passing                    │
│ [2024-01-15 10:24:47] Pushing to PR branch                 │
│ [2024-01-15 10:24:50] Action completed successfully        │
│                                                             │
│ (PgUp/PgDn to scroll, q to quit)                           │
└─────────────────────────────────────────────────────────────┘
```

### Headless Mode

For servers, CI environments, or when piping output:

```bash
# Explicit flag
./auto-claude --config config.yaml --no-tui

# Environment variable
AUTO_CLAUDE_TUI=0 ./auto-claude --config config.yaml

# Auto-detected when not a TTY
./auto-claude --config config.yaml 2>&1 | tee daemon.log
```

**Headless Output:**

```
time="2024-01-15T10:23:45Z" level=INFO msg="auto-claude starting" config=config.yaml
time="2024-01-15T10:23:45Z" level=INFO msg="daemon started" poll_interval=60s repos=2
time="2024-01-15T10:23:46Z" level=INFO msg="worker started" repo=myorg/api-service pr=123
time="2024-01-15T10:24:50Z" level=INFO msg="action completed" repo=myorg/api-service pr=123 state=checks_failing
```

### Development Mode

Using mise for local testing:

```bash
# Edit config-dev.yaml with test repo
mise dev  # Equivalent to: go run ./cmd/auto-claude --config config-dev.yaml
```

## 🔄 Worker Lifecycle

### State Evaluation Logic

1. **Draft**: `pr.isDraft == true` → Skip
2. **Conflicting**: `pr.mergeable == "CONFLICTING"` → Resolve conflicts
3. **Checks Failing**: Any check has `conclusion == "failure"` → Fix CI
4. **Checks Pending**: Any check has `status != "COMPLETED"` → Skip (wait)
5. **Reviews Pending**: Copilot review incomplete or unresolved threads → Address comments
6. **Ready**: All checks pass, reviews resolved, not blocked → Merge

### Skip Conditions

Workers skip PRs and defer to next poll when:

- PR is draft (`isDraft: true`)
- PR has `on-hold` or `blocked` label
- Status checks are pending/in-progress
- Copilot review not yet submitted (when `require_copilot_review: true`)
- Author in `exclude_authors` list (permanently skipped)

### Copilot Review Gating

When `require_copilot_review: true`:

1. Worker reaches **Ready** state
2. Checks for Copilot review completion:
   - Searches reviews from: `github-copilot[bot]`, `Copilot`, `copilot-pull-request-reviewer`
   - Examines review threads for unresolved inline comments
3. If Copilot not reviewed or has unresolved threads → Stay in **Reviews Pending** state
4. Once Copilot review complete + all threads resolved → Proceed to merge

**Renovate Exception**: PRs from Renovate authors (`renovate`, `renovate[bot]`, `renovate-bot`, `app/renovate`) bypass Copilot review requirement and merge immediately when ready.

**Override**: Set `require_copilot_review: false` in repo config to disable for specific repos (e.g., personal projects, test repos).

## 🛠️ Development

### Project Structure

```
auto-claude/
├── cmd/auto-claude/         # Entry point, signal handling, TUI/headless mode
├── internal/
│   ├── config/              # YAML parsing, validation, defaults
│   ├── daemon/              # Poll loop, worker lifecycle management
│   ├── github/              # gh CLI wrapper (PRs, checks, reviews, merge)
│   ├── worker/              # Per-PR goroutine + state machine
│   │   ├── worker.go        # State evaluation, lifecycle
│   │   └── actions.go       # Conflict resolution, CI fix, review fix, merge
│   ├── claude/              # Claude Code CLI invocation, output parsing
│   ├── git/                 # Git operations (clone, worktree, push)
│   ├── logging/             # Structured logging with color support
│   └── tui/                 # Bubble Tea interactive dashboard
├── config.yaml              # Production configuration
├── config-dev.yaml          # Development configuration
├── mise.toml                # Tool versions + dev task
└── go.mod
```

### Build Commands

```bash
# Standard build
go build -o auto-claude ./cmd/auto-claude

# Optimized build (smaller binary)
go build -ldflags="-s -w" -o auto-claude ./cmd/auto-claude

# Cross-compile for Linux
GOOS=linux GOARCH=amd64 go build -o auto-claude-linux ./cmd/auto-claude
```

### Testing

```bash
# Run tests
go test ./...

# With coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# Lint
golangci-lint run

# Format
gofmt -s -w .
```

### Contributing

1. Fork repository
2. Create feature branch
3. Make changes with tests
4. Run `gofmt -s -w .` and `golangci-lint run`
5. Test with `mise dev` and `config-dev.yaml`
6. Open PR with clear description

## 🔍 Troubleshooting

### Worker Not Starting for PR

**Symptoms**: PR visible in GitHub but no worker spawned

**Check:**

1. Is PR from excluded author? (see `config.yaml` `exclude_authors`)
2. Is PR draft? (workers skip drafts)
3. Does PR have `on-hold` or `blocked` label?
4. Are status checks pending? (workers wait for completion)
5. Is `max_concurrent_prs` reached for repo? (check logs for "max workers")

**Fix**: Adjust config, remove label, or wait for checks to finish

### Claude Invocation Hanging

**Symptoms**: Worker stuck, no log updates for >15 minutes

**Debug:**

```bash
# Check for hung Claude processes
ps aux | grep claude

# View worker logs
tail -f /tmp/auto-claude/logs/auto-claude.log | grep "pr=123"

# Set to your configured workdir (see config.yaml)
WORKDIR=/tmp/auto-claude

# Check Claude output logs (stored inside the PR worktree)
ls -la "$WORKDIR/worktrees/myorg-myrepo/pr-123/.auto-claude-logs"
cat "$WORKDIR/worktrees/myorg-myrepo/pr-123/.auto-claude-logs"/claude-*.log 2>/dev/null
```

**Fix:**

1. Kill hung process: `pkill -f "claude.*myrepo"`
2. Next poll cycle will retry PR
3. Consider lowering timeout if frequent

### Merge Conflicts Not Resolving

**Symptoms**: PR stuck in **conflicting** state after multiple polls

**Debug:**

```bash
# Set to your configured workdir (see config.yaml)
WORKDIR=/tmp/auto-claude

# Check Claude output logs (stored inside the PR worktree)
ls -la "$WORKDIR/worktrees/myorg-myrepo/pr-123/.auto-claude-logs"
cat "$WORKDIR/worktrees/myorg-myrepo/pr-123/.auto-claude-logs"/claude-conflict-*.log 2>/dev/null

# Verify git worktree created (note: worktrees are cleaned up after the worker exits)
ls -la "$WORKDIR/worktrees/myorg-myrepo/pr-123"

# Check for new conflicts pushed to base branch
gh pr view 123 --repo myorg/myrepo
```

**Fix:**

- If Claude failed: review logs, may need manual resolution
- If git operation failed: check auth (`gh auth status`), disk space, git version
- If new conflicts: next poll will retry with updated base

### High Disk Usage

**Cause**: Large git clones or persistent log files

**Debug:**

```bash
# Check disk usage by component
du -sh /tmp/auto-claude/clones/*/*
du -sh /tmp/auto-claude/logs/*
du -sh /tmp/auto-claude/worktrees/*/*

# Note: Worktrees are automatically cleaned up after each worker exits
```

**Fix:**

- Review log rotation settings in config
- Consider cleaning up old clones if accumulating
- Check for large files in worktree during active runs

### TUI Not Rendering

**Symptoms**: Garbled output, broken UI, or automatic headless mode

**Debug:**

```bash
# Check if stdin/stdout are TTY
test -t 0 && echo "stdin is TTY" || echo "stdin not TTY"
test -t 1 && echo "stdout is TTY" || echo "stdout not TTY"

# Check terminal type
echo $TERM

# Check color support
echo $COLORTERM
```

**Fix:**

- Ensure running in real terminal (not piped or redirected)
- Set `TERM=xterm-256color` if unset
- Use `--no-tui` flag if terminal not supported

### Copilot Review Not Detected

**Symptoms**: Worker stuck in **checks_pending** waiting for Copilot

**Debug:**

```bash
# Check if Copilot actually reviewed
gh pr view 123 --repo myorg/myrepo --json reviews

# Check for unresolved review threads
gh api repos/myorg/myrepo/pulls/123/comments
```

**Fix:**

- Manually verify Copilot review submitted in GitHub UI
- If Copilot not available, set `require_copilot_review: false` in config
- If review dismissed/pending, wait for actual APPROVED/CHANGES_REQUESTED state

## 💡 Use Cases

### 1. Renovate Dependency Automation

**Scenario**: Renovate opens 20 PRs weekly, each needs CI pass + Copilot approval

**Config:**

```yaml
repos:
  - owner: myorg
    name: backend
    exclude_authors: []  # Don't exclude Renovate
    require_copilot_review: true  # Renovate bypasses this
    max_concurrent_prs: 5  # Process multiple updates in parallel
```

**Result**: Renovate PRs auto-merge when CI passes (Copilot review bypassed for Renovate)

### 2. Personal Projects

**Scenario**: Side project without team review process

**Config:**

```yaml
repos:
  - owner: myusername
    name: hobby-app
    require_copilot_review: false  # No review gating
    max_concurrent_prs: 10
    exclude_authors: []
```

**Result**: PRs merge as soon as CI passes, no waiting for reviews

### 3. Team PR Management

**Scenario**: Organization requires Copilot review before merge

**Config:**

```yaml
repos:
  - owner: acme-corp
    name: api-service
    require_copilot_review: true  # Enforce Copilot review
    max_concurrent_prs: 3  # Conservative limit
    exclude_authors:
      - dependabot[bot]  # Manual review for security updates
```

**Result**: PRs only merge after Copilot approval + all threads resolved

### 4. Multi-Repo Organization

**Scenario**: Manage 10+ repos with different policies

**Config:**

```yaml
repos:
  - owner: acme-corp
    name: frontend
    require_copilot_review: true
    merge_method: squash

  - owner: acme-corp
    name: backend
    require_copilot_review: true
    merge_method: squash

  - owner: acme-corp
    name: infra
    require_copilot_review: false  # Terraform auto-apply
    merge_method: merge
```

**Result**: Centralized automation with per-repo policies

## ❓ FAQ

**Q: Does auto-claude push commits to PR branches?**
A: Yes. When resolving conflicts, fixing CI, or addressing reviews, it commits changes and pushes to the PR's head branch. It prompts Claude to use `-s -S` for sign-off and GPG signing, but does not itself enforce or verify that these flags were used.

**Q: Can I run multiple instances for different repos?**
A: No need. Use a single instance with multiple repos in `config.yaml`. Multiple instances would conflict on git worktrees.

**Q: What happens if Claude fails to fix a PR?**
A: Worker logs error with summary, exits, and retries on next poll cycle. After repeated failures, manual intervention required.

**Q: Does it work with private repositories?**
A: Yes, if `gh auth login` has access. auto-claude uses `gh` CLI, which respects GitHub authentication.

**Q: Can I customize Claude prompts?**
A: Not via config. Prompts hardcoded in `internal/worker/actions.go`. Modify source and rebuild to customize.

**Q: What merge strategies are supported?**
A: `squash` (single commit) and `merge` (preserve history). Configure per-repo via `merge_method`.

**Q: How do I exclude specific PRs from automation?**
A: Add `on-hold` or `blocked` label to PR in GitHub. Worker will skip until label removed.

**Q: Does it respect branch protection rules?**
A: Yes. Merge attempts honor required reviews, status checks, and other GitHub protections.

**Q: Can auto-claude create PRs?**
A: No. It only manages existing PRs opened by humans or bots (Renovate, Dependabot, etc.).

**Q: What's the GitHub API overhead?**
A: Typically low. Each poll cycle makes one API call per repo to list open PRs. When `require_copilot_review: true` is enabled, the daemon also fetches reviews (and, when available, review threads) for each open PR on every poll to compute Copilot review status. Workers make additional calls only when taking action (for example, updating branches, commenting, or merging).

## 📄 License & Credits

**License**: MIT (see [LICENSE](LICENSE))

**Dependencies:**

- [Claude Code CLI](https://github.com/anthropics/claude-code) - AI-powered code assistance
- [GitHub CLI](https://cli.github.com/) - GitHub API wrapper
- [Bubble Tea](https://github.com/charmbracelet/bubbletea) - TUI framework
- [Lipgloss](https://github.com/charmbracelet/lipgloss) - Terminal styling
- [Tint](https://github.com/lmittmann/tint) - Structured logging with colors

**Support:**

- 🐛 [Report bugs](https://github.com/marcin-skalski/auto-claude/issues)
- 💬 [Discussions](https://github.com/marcin-skalski/auto-claude/discussions)
- 📧 Contact: [GitHub @marcin-skalski](https://github.com/marcin-skalski)

**Note**: This is experimental software. Test thoroughly in non-production environments before deploying to critical workflows. Always monitor daemon logs and verify PR actions in GitHub.

---

**Built with ☕ and frustration over manual PR babysitting**
