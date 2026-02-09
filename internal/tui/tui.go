package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type SnapshotProvider interface {
	GetSnapshot() Snapshot
}

type Model struct {
	provider        SnapshotProvider
	snapshot        Snapshot
	refreshInterval time.Duration
}

type tickMsg time.Time

func NewModel(provider SnapshotProvider, refreshInterval time.Duration) Model {
	return Model{
		provider:        provider,
		snapshot:        provider.GetSnapshot(),
		refreshInterval: refreshInterval,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		tickCmd(m.refreshInterval),
		tea.EnterAltScreen,
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r":
			// Manual refresh
			m.snapshot = m.provider.GetSnapshot()
			return m, nil
		}

	case tickMsg:
		m.snapshot = m.provider.GetSnapshot()
		return m, tickCmd(m.refreshInterval)
	}

	return m, nil
}

func (m Model) View() string {
	return renderView(m.snapshot)
}

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}
