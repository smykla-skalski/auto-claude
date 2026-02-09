# auto-claude

PR automation daemon that monitors GitHub repos, manages PR lifecycle autonomously using Claude Code CLI. Spawns worker goroutine per PR for conflict resolution, build fixes, review comments, and merging.

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
├── config.yaml                      # production config
├── config-dev.yaml                  # development config for local testing
├── mise.toml                        # tool versions + dev task
├── go.mod
└── .github/workflows/ci.yaml
```

## Tech Stack

**Language:** Go 1.24.9
**Dependency Mgmt:** go mod + mise
**External Tools:** gh CLI, git, claude CLI
**Config:** YAML (gopkg.in/yaml.v3)

## Development Workflow

### Testing Changes

**Primary method:** Manual testing with config-dev.yaml

1. Create/configure test repo with sample PRs
2. Update `config-dev.yaml` with test repo details
3. Run daemon: `mise dev`
4. Monitor logs for expected behavior
5. Verify PR state changes in GitHub

**Log levels:**
- DEBUG: full state transitions, gh CLI calls
- INFO: worker start/stop, PR actions taken
- WARN: recoverable errors, skipped PRs
- ERROR: critical failures

### Adding New PR Action

1. Define state in `worker/worker.go` state machine
2. Implement action in `worker/actions.go`
3. Add transition logic in worker loop
4. Test with `config-dev.yaml` against test PR
5. Verify logs show: action start → Claude invocation outcome → state change

### Modifying GitHub Integration

1. Read `internal/github/github.go` for existing patterns
2. Use `gh` CLI with `--json` output, parse response
3. Add error handling for rate limits, network failures
4. Test with DEBUG logging enabled
5. Verify GraphQL queries against API docs if needed

### Updating Config Schema

1. Modify types in `internal/config/config.go`
2. Update validation logic in `Load()`
3. Update `config.yaml` example with new fields + comments
4. Test with invalid configs to verify validation
5. Update this CLAUDE.md if behavior changes

## Configuration

### config.yaml Structure

```yaml
poll_interval: 60s                   # how often to check for PR updates
workdir: /tmp/auto-claude            # base dir for git worktrees

claude:
  model: opus                        # opus | sonnet | haiku

repos:
  - owner: myorg
    name: myrepo
    base_branch: main                # target branch for PRs
    exclude_authors:                 # skip PRs from these users
      - dependabot[bot]
    merge_method: squash             # squash | merge
    max_concurrent_prs: 3            # per-repo worker limit
    require_copilot_review: true     # wait for Copilot review completion

log:
  level: info                        # debug | info | warn | error
```

### config-dev.yaml

Use for local development:
- Point to test repository
- Lower poll_interval (e.g., 10s)
- Set log level to debug
- Use separate workdir to avoid conflicts

## Logging

### Colors

Log levels colored when stderr is TTY:
- DEBUG: gray
- INFO: cyan
- WARN: yellow
- ERROR: red

Auto-disabled when piped or NO_COLOR set.

Disable manually:
```bash
NO_COLOR=1 auto-claude --config config.yaml
```

## Worker State Machine

Each PR worker progresses through states:

1. **IDLE** → Check PR readiness (skip if draft, on-hold label, pending checks)
2. **NEEDS_CONFLICT_RESOLUTION** → Invoke Claude with merge conflict context
3. **NEEDS_BUILD_FIX** → Invoke Claude with failing CI logs
4. **NEEDS_REVIEW_FIX** → Invoke Claude with unresolved review threads
5. **READY_TO_MERGE** → Verify Copilot review, merge with configured method
6. **MERGED** → Worker exits

**Skip conditions (defer to next poll):**
- `isDraft: true`
- Labels: `on-hold`, `blocked`
- Status checks: any pending/in_progress

**State persistence:** In-memory only, rebuilt on each poll cycle

## Claude Code Integration

### Invocation Pattern

```go
// Summary-only logging
cmd := exec.CommandContext(ctx, "claude", args...)
output, err := cmd.CombinedOutput()
if err != nil {
    logger.Error("claude invocation failed", "err", err, "summary", extractSummary(output))
    return err
}
logger.Info("claude action completed", "summary", extractSummary(output))
```

**Output handling:**
- Log outcome + error message only (not full stdout)
- Save full output to `{workdir}/{repo}/{pr}/claude-{action}-{timestamp}.log`
- Parse output for success indicators (exit code, specific strings)

### Timeout Strategy

- Set context timeout per action type
- Conflict resolution: 10 min
- Build fix: 15 min
- Review fix: 20 min
- Kill process on timeout, log as failed

## Error Handling

### Worker Goroutine Panics

**Critical priority:** Prevent one PR failure from crashing daemon

```go
defer func() {
    if r := recover(); r != nil {
        logger.Error("worker panic recovered", "pr", prNum, "panic", r, "stack", string(debug.Stack()))
        // Mark worker as failed, continue daemon operation
    }
}()
```

**Actions on panic:**
1. Log panic with full stack trace
2. Remove worker from active workers map
3. Continue polling other PRs
4. Next poll cycle will retry PR if still open

### Graceful Degradation

**GitHub API failures:**
- Log error, skip repo in current poll cycle
- Resume on next poll (API may recover)

**Claude CLI failures:**
- Log error with summary
- Mark PR action as failed
- Retry on next poll (manual intervention may fix)

**Git operation failures:**
- Retry once with backoff
- If persistent, log and skip PR
- Manual cleanup required in workdir

## Quality Gates

Before committing:

- [ ] Builds successfully: `go build ./cmd/auto-claude`
- [ ] Code follows Go conventions (run `gofmt -s -w .`)
- [ ] Changes tested with `mise dev` + config-dev.yaml
- [ ] Logs reviewed at INFO level (no noise, no missing context)
- [ ] Panic recovery tested if modifying worker lifecycle

## Common Commands

```bash
# Start daemon with dev config
mise dev

# Build binary
go build -o auto-claude ./cmd/auto-claude

# Run with custom config
go run ./cmd/auto-claude --config /path/to/config.yaml

# Test config parsing
go run ./cmd/auto-claude --config config.yaml  # exits after validation if invalid

# Check dependencies
go mod tidy
go mod verify

# Format code
gofmt -s -w .

# Update Go version
mise set go@1.25.9  # updates mise.toml + downloads
```

## Anti-Patterns

**AVOID:**

- ❌ Force-pushing to PR branches (breaks review history, conflicts with developer pushes)
- ❌ Merging without Copilot review completion when `require_copilot_review: true` (bypasses quality gate)
- ❌ Concurrent operations on same branch (multiple workers on same PR causes race conditions)
- ❌ Retrying failed actions without backoff (spam GitHub API, waste Claude credits, flood logs)
- ❌ Blocking daemon on slow operations (use timeouts, async patterns)
- ❌ Logging full Claude output at INFO level (log noise, slow performance)
- ❌ Ignoring worker panics (one failure crashes daemon)
- ❌ Committing with hardcoded credentials or API tokens

**REASON:**
- Force-push destroys review comments, conflicts with concurrent developer work
- Skipping Copilot review reduces code quality, violates team process
- Same-branch concurrency causes git conflicts, wasted Claude invocations
- No-backoff retries exhaust rate limits, degrade service
- Blocking operations freeze all workers, miss new PRs
- Verbose logs hide important events, fill disk
- Unhandled panics crash entire daemon, stop all PR automation
- Hardcoded secrets leak in version control, security vulnerability

## Daemon Lifecycle

### Startup

1. Parse config file (`--config` flag, default: config.yaml)
2. Validate repos accessible via `gh` CLI
3. Create workdir if not exists
4. Start poll loop with initial immediate poll
5. Log startup: poll_interval, repos count, workdir

### Poll Cycle

1. For each configured repo:
   - List open PRs via GitHub client
   - Filter by exclude_authors
   - For each PR: spawn/update worker goroutine
2. Track active workers in map (key: "owner/repo#PR")
3. Enforce max_concurrent_prs per repo
4. Sleep for poll_interval

### Shutdown

1. Receive SIGINT or SIGTERM signal
2. Cancel context → stops all workers
3. Wait for workers to finish current action (with timeout)
4. Log shutdown: PRs in-progress, aborted actions
5. Exit cleanly

## Extending Functionality

### Adding New PR Action

**When to add:** New automated task beyond current scope (conflict, build, review, merge)

**Steps:**
1. Define new state constant in `worker/worker.go`
2. Add detection logic in state machine (when to enter state)
3. Implement action function in `worker/actions.go`
4. Add Claude CLI invocation with appropriate context
5. Handle action success/failure transitions
6. Update this CLAUDE.md with new action details

**Example:** Add auto-update of outdated dependencies
- State: `NEEDS_DEPENDENCY_UPDATE`
- Detection: Parse PR description for "outdated" label
- Action: Invoke Claude with "update dependencies in go.mod"
- Transition: → NEEDS_BUILD_FIX (run CI) or READY_TO_MERGE

### Adding New Skip Condition

**When to add:** New criteria for deferring PR automation

**Steps:**
1. Modify `shouldSkipPR()` in `worker/worker.go`
2. Add check for label, PR state, or external signal
3. Log skip reason at DEBUG level
4. Test with config-dev.yaml
5. Document condition in "Worker State Machine" section above

**Example:** Skip PRs with "needs-human-review" label
```go
for _, label := range pr.Labels {
    if label == "needs-human-review" {
        return true, "human review requested"
    }
}
```

### Customizing Merge Strategy

**When to change:** Different repos need different merge methods

**Steps:**
1. Config already supports `merge_method: squash | merge` per repo
2. Modify `github.MergePR()` to handle new methods (e.g., rebase)
3. Update config.yaml comment with new option
4. Test with PRs in each merge mode
5. Update "Configuration" section above

## Copilot Review Integration

### Behavior

When `require_copilot_review: true` in repo config:

1. Worker enters READY_TO_MERGE state
2. Check for Copilot review completion via GitHub API
3. If Copilot review pending or unresolved comments exist:
   - Log: "waiting for Copilot review"
   - Stay in READY_TO_MERGE, check on next poll
4. Once Copilot review complete + all threads resolved:
   - Proceed with merge

### Implementation Details

Check review threads from Copilot user:
```go
threads, err := gh.GetReviewThreads(ctx, owner, repo, prNum)
for _, thread := range threads {
    if thread.Author == "github-copilot[bot]" && !thread.IsResolved {
        return false, nil  // not ready
    }
}
return true, nil  // ready to merge
```

### Override

Set `require_copilot_review: false` to skip this check for specific repos (e.g., personal projects, test repos).

## Troubleshooting

### Worker not starting for PR

**Check:**
1. Is PR from excluded author? (see config.yaml exclude_authors)
2. Is PR draft? (workers skip drafts)
3. Does PR have on-hold or blocked label?
4. Are status checks pending? (workers wait for completion)
5. Is max_concurrent_prs reached for repo? (check logs for "max workers")

**Fix:** Adjust config, remove label, or wait for checks to finish

### Claude invocation hanging

**Symptoms:** Worker stuck, no log updates for >15 min

**Fix:**
1. Check Claude CLI timeout in worker action code
2. Kill hung claude process manually: `pkill -f claude`
3. Next poll cycle will retry PR
4. Consider lowering timeout if frequent

### Merge conflicts not auto-resolving

**Symptoms:** PR stuck in NEEDS_CONFLICT_RESOLUTION state

**Debug:**
1. Check Claude output log: `{workdir}/{repo}/{pr}/claude-conflict-*.log`
2. Verify git worktree created: `ls {workdir}/{repo}/{pr}`
3. Check GitHub for recent base branch changes (new conflicts)

**Fix:**
- If Claude failed: review logs, may need manual resolution
- If git operation failed: check auth, disk space, git version
- If new conflicts: next poll will retry

### Daemon consuming too much disk

**Cause:** Git worktrees accumulate in workdir

**Fix:**
1. Implement cleanup job: delete worktrees for merged/closed PRs
2. Add to daemon poll cycle: after worker map update, scan workdir
3. Remove directories not in active workers map

### Logs flooding with DEBUG messages

**Symptoms:** Disk usage high, hard to find relevant logs

**Fix:**
1. Set `log.level: info` in config.yaml
2. Reserve DEBUG for local development only
3. Use structured logging: log.Info("event", "key", value) not log.Debug(fmt.Sprintf(...))

## Decision Matrix: When to Act on PR

Use this matrix to determine if worker should process PR in current poll cycle:

| Condition | Action |
|-----------|--------|
| PR is draft | SKIP (defer) |
| PR has "on-hold" label | SKIP (defer) |
| PR has "blocked" label | SKIP (defer) |
| Status checks pending | SKIP (defer) |
| Author in exclude_authors | SKIP (never process) |
| Mergeable = CONFLICTING | PROCESS (resolve conflicts) |
| CI checks failed | PROCESS (fix build) |
| Unresolved review threads | PROCESS (fix review) |
| Copilot review incomplete + required | SKIP (defer) |
| All checks pass + no conflicts + reviews resolved | PROCESS (merge) |

**Priority order:**
1. Skip conditions (fastest, avoid unnecessary work)
2. Conflict resolution (blocks all other work)
3. Build fixes (required before review)
4. Review fixes (final quality gate)
5. Merge (terminal state)

## Extensibility

To add sections as project evolves:

1. Add heading in appropriate location (keep daemon, worker, config together)
2. Follow section structure: concept → implementation → examples
3. Keep concrete and actionable (commands, code snippets, file paths)
4. Include troubleshooting subsection if complex feature

New sections to consider:
- **Metrics Collection** (Prometheus, GitHub Actions telemetry)
- **Alerting** (Slack, PagerDuty on repeated failures)
- **Multi-fork Support** (handle PRs from forks with different auth)
- **PR Priority Queue** (high-priority labels processed first)
