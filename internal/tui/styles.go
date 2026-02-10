package tui

import "github.com/charmbracelet/lipgloss"

var (
	// State colors
	colorDraft          = lipgloss.Color("240") // gray
	colorConflicting    = lipgloss.Color("220") // yellow
	colorChecksFailing  = lipgloss.Color("196") // red
	colorChecksPending  = lipgloss.Color("33")  // blue
	colorCopilotPending = lipgloss.Color("135") // purple
	colorFixingReviews  = lipgloss.Color("208") // orange-red
	colorReviewsPending = lipgloss.Color("214") // orange
	colorReady          = lipgloss.Color("46")  // green

	// Styles
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("39")).
			PaddingLeft(1).
			PaddingRight(1)

	sectionStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212")).
			MarginTop(1).
			MarginBottom(0)

	treeRepoStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("cyan"))

	treePRStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	sessionStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	selectedSessionStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("39")).
				Background(lipgloss.Color("237"))

	footerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			MarginTop(1)

	emptyStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Italic(true)
)

// State icons and labels
func stateIcon(state string) string {
	switch state {
	case "draft":
		return "📝"
	case "conflicting":
		return "⚠️"
	case "checks_failing":
		return "🔨"
	case "checks_pending":
		return "⚙️"
	case "copilot_pending":
		return "🤖"
	case "fixing_reviews":
		return "🔧"
	case "reviews_pending":
		return "📋"
	case "ready":
		return "✅"
	default:
		return "❓"
	}
}

func stateColor(state string) lipgloss.Color {
	switch state {
	case "draft":
		return colorDraft
	case "conflicting":
		return colorConflicting
	case "checks_failing":
		return colorChecksFailing
	case "checks_pending":
		return colorChecksPending
	case "copilot_pending":
		return colorCopilotPending
	case "fixing_reviews":
		return colorFixingReviews
	case "reviews_pending":
		return colorReviewsPending
	case "ready":
		return colorReady
	default:
		return lipgloss.Color("252")
	}
}
