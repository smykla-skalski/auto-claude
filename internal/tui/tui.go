package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

type SnapshotProvider interface {
	GetSnapshot() Snapshot
}

type viewMode int

const (
	viewModeList viewMode = iota
	viewModeDetail
)

type Model struct {
	provider        SnapshotProvider
	snapshot        Snapshot
	refreshInterval time.Duration
	mode            viewMode
	selectedSession int // -1 = none, otherwise index in snapshot.ClaudeSessions
	scrollOffset    int // For scrolling in detail view
}

type tickMsg time.Time

func NewModel(provider SnapshotProvider, refreshInterval time.Duration) Model {
	return Model{
		provider:        provider,
		snapshot:        provider.GetSnapshot(),
		refreshInterval: refreshInterval,
		mode:            viewModeList,
		selectedSession: -1,
		scrollOffset:    0,
	}
}

func (m Model) Init() tea.Cmd {
	return tickCmd(m.refreshInterval)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch m.mode {
		case viewModeList:
			switch msg.String() {
			case "q", "ctrl+c":
				return m, func() tea.Msg { return tea.QuitMsg{} }
			case "r":
				// Manual refresh
				m.snapshot = m.provider.GetSnapshot()
				return m, nil
			case "up", "k":
				if m.selectedSession > 0 {
					m.selectedSession--
				}
			case "down", "j":
				if m.selectedSession < len(m.snapshot.ClaudeSessions)-1 {
					m.selectedSession++
				}
			case "1", "2", "3", "4", "5", "6", "7", "8", "9":
				// Quick select by number
				idx := int(msg.String()[0] - '1')
				if idx < len(m.snapshot.ClaudeSessions) {
					m.selectedSession = idx
				}
			case "enter", " ":
				// Enter detail view if session selected
				if m.selectedSession >= 0 && m.selectedSession < len(m.snapshot.ClaudeSessions) {
					m.mode = viewModeDetail
					m.scrollOffset = 0
				}
			}

		case viewModeDetail:
			switch msg.String() {
			case "q", "ctrl+c":
				return m, func() tea.Msg { return tea.QuitMsg{} }
			case "esc":
				// Return to list view
				m.mode = viewModeList
				return m, nil
			case "up", "k":
				if m.scrollOffset > 0 {
					m.scrollOffset--
				}
			case "down", "j":
				if m.selectedSession >= 0 && m.selectedSession < len(m.snapshot.ClaudeSessions) {
					outputLen := len(m.snapshot.ClaudeSessions[m.selectedSession].Output)
					const maxLines = 40
					maxOffset := max(0, outputLen-maxLines)
					m.scrollOffset = min(m.scrollOffset+1, maxOffset)
				}
			case "pageup":
				m.scrollOffset -= 10
				if m.scrollOffset < 0 {
					m.scrollOffset = 0
				}
			case "pagedown":
				if m.selectedSession >= 0 && m.selectedSession < len(m.snapshot.ClaudeSessions) {
					outputLen := len(m.snapshot.ClaudeSessions[m.selectedSession].Output)
					const maxLines = 40
					maxOffset := max(0, outputLen-maxLines)
					m.scrollOffset = min(m.scrollOffset+10, maxOffset)
				}
			case "home", "g":
				m.scrollOffset = 0
			case "end", "G":
				// Scroll to bottom (clamped to valid range)
				if m.selectedSession >= 0 && m.selectedSession < len(m.snapshot.ClaudeSessions) {
					outputLen := len(m.snapshot.ClaudeSessions[m.selectedSession].Output)
					const maxLines = 40
					m.scrollOffset = max(0, outputLen-maxLines)
				}
			}
		}

	case tickMsg:
		m.snapshot = m.provider.GetSnapshot()
		// Auto-select first session if none selected
		if m.selectedSession == -1 && len(m.snapshot.ClaudeSessions) > 0 {
			m.selectedSession = 0
		}
		// Clear selection if session disappeared
		if m.selectedSession >= len(m.snapshot.ClaudeSessions) {
			m.selectedSession = len(m.snapshot.ClaudeSessions) - 1
			if m.selectedSession < 0 {
				m.selectedSession = -1
				m.mode = viewModeList
			}
		}
		return m, tickCmd(m.refreshInterval)
	}

	return m, nil
}

func (m Model) View() tea.View {
	var content string
	switch m.mode {
	case viewModeList:
		content = renderListView(m.snapshot, m.selectedSession)
	case viewModeDetail:
		if m.selectedSession >= 0 && m.selectedSession < len(m.snapshot.ClaudeSessions) {
			content = renderDetailView(m.snapshot.ClaudeSessions[m.selectedSession], m.scrollOffset)
		} else {
			content = renderListView(m.snapshot, m.selectedSession)
		}
	default:
		content = renderListView(m.snapshot, m.selectedSession)
	}
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}
