package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

func renderView(snap Snapshot) string {
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
	b.WriteString(renderSessions(snap.ClaudeSessions))

	// Footer
	b.WriteString("\n")
	footer := fmt.Sprintf("Last updated: %s │ q:quit r:refresh",
		snap.Timestamp.Format("15:04:05"))
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

		repoLine := fmt.Sprintf("%s 🔧 %s/%s [%d workers │ %d PRs]",
			prefix, repo.Owner, repo.Name, repo.Workers, len(repo.PRs))
		b.WriteString(treeRepoStyle.Render(repoLine))
		b.WriteString("\n")

		if len(repo.PRs) == 0 {
			childPrefix := "│  "
			if isLast {
				childPrefix = "   "
			}
			b.WriteString(emptyStyle.Render(childPrefix + "  (no open PRs)"))
			b.WriteString("\n")
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

func renderSessions(sessions []ClaudeSessionState) string {
	if len(sessions) == 0 {
		return emptyStyle.Render("  (No active Claude sessions)")
	}

	var b strings.Builder
	for _, s := range sessions {
		duration := formatDuration(s.Duration)
		line := fmt.Sprintf("• %s #%d - %s (%s)",
			s.Repo, s.PRNumber, s.Action, duration)
		b.WriteString(sessionStyle.Render(line))
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
