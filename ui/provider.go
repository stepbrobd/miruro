package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	pending = iota
	resolved
	failed
)

type labelMsg struct {
	i     int
	label string
	ok    bool
}

type picker struct {
	title  string
	codes  []string
	labels []string
	state  []int
	cursor int
	choice int
	ch     chan tea.Msg
}

func (m picker) Init() tea.Cmd { return listen(m.ch) }

func (m picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case labelMsg:
		m.labels[msg.i] = msg.label
		if msg.ok {
			m.state[msg.i] = resolved
		} else {
			m.state[msg.i] = failed
		}
		return m, listen(m.ch)
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.codes)-1 {
				m.cursor++
			}
		case "enter":
			m.choice = m.cursor
			return m, tea.Quit
		case "esc", "ctrl+c", "q":
			m.choice = -1
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m picker) View() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(nord8)).Bold(true).Render(m.title))
	b.WriteString("\n\n")

	cursorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(nord13))
	codeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(nord4))
	for i, code := range m.codes {
		point := "  "
		if i == m.cursor {
			point = cursorStyle.Render("> ")
		}
		b.WriteString(point)
		b.WriteString(codeStyle.Render(fmt.Sprintf("%-14s", code)))
		b.WriteString("  ")
		b.WriteString(detail(m.state[i], m.labels[i]))
		b.WriteByte('\n')
	}
	return b.String()
}

func detail(state int, label string) string {
	switch state {
	case resolved:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(nord14)).Render(label)
	case failed:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(nord11)).Render(label)
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(nord3)).Render("resolving...")
	}
}

// PickProvider lists provider codes immediately and fills each row's label as
// probe returns, then returns the selected index
// probe runs once per provider in its own goroutine and reports a label and
// whether the provider is usable
func PickProvider(title string, codes []string, probe func(i int) (string, bool)) (int, error) {
	m := picker{
		title:  title,
		codes:  codes,
		labels: make([]string, len(codes)),
		state:  make([]int, len(codes)),
		choice: -1,
		ch:     make(chan tea.Msg, 2*len(codes)+1),
	}
	for i := range codes {
		go func(i int) {
			label, ok := probe(i)
			m.ch <- labelMsg{i, label, ok}
		}(i)
	}

	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return 0, err
	}
	fm, ok := final.(picker)
	if !ok || fm.choice < 0 {
		return 0, ErrAborted
	}
	return fm.choice, nil
}
