package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

func renderListView(snap Snapshot, selectedSession int) string {
	var b strings.Builder

	// Header
	prCount := 0
	for _, r := range snap.Repos {
		prCount += len(r.PRs)
	}
	header := fmt.Sprintf("auto-claude │ %d repos │ %d PRs │ %d workers",
		len(snap.Repos), prCount, snap.WorkerCount)
	b.WriteString(headerStyle.Render(header))
	b.WriteString("\n")

	// Tree view
	b.WriteString(sectionStyle.Render("📦 Monitored Repositories"))
	b.WriteString("\n")
	b.WriteString(renderTree(snap.Repos))

	// Claude sessions
	b.WriteString("\n")
	b.WriteString(sectionStyle.Render(fmt.Sprintf("🤖 Active Claude Sessions (%d)", len(snap.ClaudeSessions))))
	b.WriteString("\n")
	b.WriteString(renderSessions(snap.ClaudeSessions, selectedSession))

	// Footer
	b.WriteString("\n")
	footer := fmt.Sprintf("Last updated: %s │ q:quit r:refresh ↑↓:select enter:view",
		snap.Timestamp.Format("15:04:05"))
	b.WriteString(footerStyle.Render(footer))

	return b.String()
}

func renderDetailView(session ClaudeSessionState, scrollOffset int) string {
	var b strings.Builder

	// Header
	header := fmt.Sprintf("🤖 Claude Session: %s #%d - %s", session.Repo, session.PRNumber, session.Action)
	b.WriteString(headerStyle.Render(header))
	b.WriteString("\n")

	// Duration and tmux info
	duration := formatDuration(session.Duration)
	info := fmt.Sprintf("Running for: %s │ Output lines: %d", duration, len(session.Output))
	if session.TmuxSession != "" {
		info += fmt.Sprintf("\nTmux: tmux attach -t %s", session.TmuxSession)
	}
	b.WriteString(sectionStyle.Render(info))
	b.WriteString("\n\n")

	// Output window (last 40 lines or scrollable)
	const maxLines = 40
	startIdx := scrollOffset
	if startIdx < 0 {
		startIdx = 0
	}
	// Clamp startIdx to valid range (prevent blank view when overscrolled)
	if startIdx > len(session.Output)-maxLines && len(session.Output) > maxLines {
		startIdx = len(session.Output) - maxLines
	}
	if startIdx > len(session.Output) {
		startIdx = max(0, len(session.Output)-1)
	}
	endIdx := startIdx + maxLines
	if endIdx > len(session.Output) {
		endIdx = len(session.Output)
	}

	if len(session.Output) == 0 {
		b.WriteString(emptyStyle.Render("  (No output yet)"))
		b.WriteString("\n")
	} else {
		for i := startIdx; i < endIdx; i++ {
			b.WriteString(session.Output[i])
			b.WriteString("\n")
		}

		// Scroll indicator
		if endIdx < len(session.Output) {
			remaining := len(session.Output) - endIdx
			fmt.Fprintf(&b, "\n... %d more lines (press j/down to scroll) ...\n", remaining)
		}
	}

	// Footer
	b.WriteString("\n")
	footer := "esc:back ↑↓:scroll g:top G:bottom q:quit"
	b.WriteString(footerStyle.Render(footer))

	return b.String()
}

func renderTree(repos []RepoState) string {
	if len(repos) == 0 {
		return emptyStyle.Render("  (no repos configured)")
	}

	var b strings.Builder
	for i, repo := range repos {
		isLast := i == len(repos)-1
		prefix := "├─"
		if isLast {
			prefix = "└─"
		}

		blockedInfo := ""
		if repo.BlockedPRs > 0 {
			blockedInfo = fmt.Sprintf(" │ %d blocked", repo.BlockedPRs)
		}
		repoLine := fmt.Sprintf("%s 🔧 %s/%s [%d workers │ %d PRs%s]",
			prefix, repo.Owner, repo.Name, repo.Workers, len(repo.PRs), blockedInfo)
		b.WriteString(treeRepoStyle.Render(repoLine))
		b.WriteString("\n")

		if len(repo.PRs) == 0 && repo.BlockedPRs == 0 {
			childPrefix := "│  "
			if isLast {
				childPrefix = "   "
			}
			b.WriteString(emptyStyle.Render(childPrefix + "  (no open PRs)"))
			b.WriteString("\n")
			continue
		}

		if len(repo.PRs) == 0 && repo.BlockedPRs > 0 {
			// Don't show anything - the blocked count in the header is enough
			continue
		}

		for j, pr := range repo.PRs {
			isPRLast := j == len(repo.PRs)-1
			childPrefix := "│  "
			if isLast {
				childPrefix = "   "
			}

			prPrefix := "├─"
			if isPRLast {
				prPrefix = "└─"
			}

			title := pr.Title
			if runewidth.StringWidth(title) > 60 {
				title = runewidth.Truncate(title, 57, "...")
			}

			// PR title line
			prLine := fmt.Sprintf("%s%s #%d %s",
				childPrefix, prPrefix, pr.Number, title)
			b.WriteString(treePRStyle.Render(prLine))
			b.WriteString("\n")

			// Status lines (nested under PR)
			for k, state := range pr.States {
				isLastState := k == len(pr.States)-1
				icon := stateIcon(state)
				color := stateColor(state)

				claudeIndicator := ""
				if pr.HasWorker && isLastState {
					claudeIndicator = " (Claude)"
				}

				statusPrefix := childPrefix
				if isPRLast {
					statusPrefix = childPrefix
				}

				statePrefix := "├─"
				if isLastState {
					statePrefix = "└─"
				}

				statusLine := fmt.Sprintf("%s   %s %s %s%s",
					statusPrefix, statePrefix, icon, state, claudeIndicator)
				b.WriteString(lipgloss.NewStyle().Foreground(color).Render(statusLine))
				b.WriteString("\n")
			}
		}
	}

	return b.String()
}

func renderSessions(sessions []ClaudeSessionState, selectedIdx int) string {
	if len(sessions) == 0 {
		return emptyStyle.Render("  (No active Claude sessions)")
	}

	var b strings.Builder
	for i, s := range sessions {
		duration := formatDuration(s.Duration)
		marker := "•"
		style := sessionStyle

		if i == selectedIdx {
			marker = "▶"
			style = selectedSessionStyle
		}

		line := fmt.Sprintf("%s %s #%d - %s (%s)",
			marker, s.Repo, s.PRNumber, s.Action, duration)
		if s.TmuxSession != "" {
			line += fmt.Sprintf(" [tmux attach -t %s]", s.TmuxSession)
		}
		b.WriteString(style.Render(line))
		b.WriteString("\n")
	}

	return b.String()
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	m := d / time.Minute
	s := (d % time.Minute) / time.Second
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
