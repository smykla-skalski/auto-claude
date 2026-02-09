---
description: Non-interactive PR review fix (auto-apply valid fixes)
argument-hint: [PR-URL]
allowed-tools: Read,Grep,Glob,Bash(gh:*,git:*)
---

# Fix PR Review Comments (Non-Interactive)

**Role**: Senior software engineer autonomously fixing PR review feedback.

**Task**: Process unresolved review comments from $1, research validity, auto-apply valid fixes, skip questionable/invalid.

**IMPORTANT**: Work directly — no EnterPlanMode. Apply fixes immediately after research.

## Phase 1: Fetch & Analyze

### 1. Extract PR Info

Parse PR URL to get owner/repo/number:

```bash
OWNER=$(echo "$1" | sed 's|.*github.com/\([^/]*\)/.*|\1|')
REPO=$(echo "$1" | sed 's|.*github.com/[^/]*/\([^/]*\)/.*|\1|')
PR=$(echo "$1" | sed 's|.*/pull/\([0-9]*\).*|\1|')
```

### 2. Fetch Unresolved Review Threads

```bash
gh api graphql -f owner="$OWNER" -f repo="$REPO" -F pr="$PR" -F query=@- << 'EOF'
query($owner: String!, $repo: String!, $pr: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      reviewThreads(first: 100) {
        nodes {
          isResolved
          isOutdated
          path
          line
          comments(first: 100) {
            nodes {
              author { login }
              body
              createdAt
            }
          }
        }
      }
    }
  }
}
EOF
```

Filter: `isResolved: false` AND `isOutdated: false`

### 3. Research Each Comment

For each unresolved comment:

#### Context Gathering
- Read affected file at specified `path:line`
- Search codebase for similar patterns (Grep)
- Check language/framework best practices

#### Categorize

**Valid** (ALL must be true):
- References specific code in PR diff
- Technically correct suggestion
- Doesn't break existing functionality
- Aligns with codebase patterns
- Sound reasoning from reviewer

**Invalid** (ANY true):
- References code not in this PR
- Already implemented
- Conflicts with existing patterns
- Technically incorrect
- Misunderstands code purpose

**Questionable** (ANY doubt):
- Ambiguous request
- Multiple valid interpretations
- Trade-offs not clear

## Phase 2: Apply Fixes

### Process Order
1. Critical (bugs, security, correctness)
2. Major (refactoring, performance)
3. Minor (style, naming)

### For Valid Fixes
- Use Edit tool to apply change
- Log what was fixed and why

### For Questionable Comments
- **SKIP** — log the comment and reason for skipping
- Do NOT attempt questionable fixes in non-interactive mode

### For Invalid Comments
- **SKIP** — log as invalid with evidence

## Phase 3: Commit

After all valid fixes applied:

```bash
git add .
git commit -s -S -m "fix: address PR review comments"
```

## Phase 4: Summary

```text
Summary: Fixed N/M unresolved review comments

Applied:
✓ N valid fixes across K files

Skipped:
? X questionable (ambiguous/trade-offs)
✗ Y invalid (already implemented/incorrect)
```

## Key Rules

**DO:**
- Research each comment before acting
- Search codebase for patterns
- Apply only clearly valid fixes
- Commit with -s -S flags
- Log all decisions

**DON'T:**
- Use EnterPlanMode
- Ask for user input
- Apply questionable fixes
- Use linter skip/disable directives
- Mark review threads as resolved
