#!/bin/bash

INPUT=$(cat)
SESSION_ID=$(echo "$INPUT" | jq -r '.session_id')

# Get session name from environment variable (set by auto-claude daemon)
TMUX_SESSION="${AUTO_CLAUDE_SESSION:-unknown}"

# Write marker to central location
MARKER_DIR="$HOME/.auto-claude/markers"
mkdir -p "$MARKER_DIR"
MARKER_FILE="$MARKER_DIR/${TMUX_SESSION}.marker"

echo "$SESSION_ID" > "$MARKER_FILE"

exit 0
