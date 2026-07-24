package ui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// End is the menu's reaction to playback stopping
type End struct {
	Dismiss bool   // drop the menu and hand control back
	Status  string // line shown under the title when the menu stays
}

type endMsg End

type control struct {
	title   string
	status  string
	actions []string
	cursor  int
	choice  string
	ended   bool
	done    bool
	wait    func() End
}

func (m control) Init() tea.Cmd {
	return func() tea.Msg { return endMsg(m.wait()) }
}

func (m control) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case endMsg:
		m.ended = true
		if msg.Dismiss {
			m.done = true
			return m, tea.Quit
		}
		m.status = msg.Status
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m control) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key := msg.String(); key {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.actions)-1 {
			m.cursor++
		}
	case "enter":
		return m.pick(m.actions[m.cursor])
	case "esc", "ctrl+c", "q":
		return m.pick("quit")
	default:
		// a first letter picks its action outright, n for next, r for replay
		for _, a := range m.actions {
			if strings.HasPrefix(a, key) {
				return m.pick(a)
			}
		}
	}
	return m, nil
}

func (m control) pick(action string) (tea.Model, tea.Cmd) {
	m.choice = action
	m.done = true
	return m, tea.Quit
}

func (m control) View() string {
	if m.done {
		return ""
	}

	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(nord8)).Bold(true).Render(m.title))
	b.WriteByte('\n')
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(nord3)).Render(m.status))
	b.WriteString("\n\n")

	cursorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(nord13))
	rowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(nord4))
	for i, a := range m.actions {
		point := "  "
		if i == m.cursor {
			point = cursorStyle.Render("> ")
		}
		b.WriteString(point)
		b.WriteString(rowStyle.Render(a))
		b.WriteByte('\n')
	}
	return b.String()
}

// Control shows the action menu while playback runs
// wait blocks until playback ends and returns how the menu reacts to it
// the action is "" when the end dismissed the menu, and ended reports whether
// playback was already over when the menu closed
func Control(ctx context.Context, title string, actions []string, wait func() End, opts ...tea.ProgramOption) (action string, ended bool, err error) {
	m := control{title: title, status: "playing", actions: actions, wait: wait}
	// the caller's context owns signal handling
	// bubbletea's own handler would swallow a SIGTERM and end the program with
	// no error, indistinguishable from a dismissal
	opts = append([]tea.ProgramOption{tea.WithContext(ctx), tea.WithoutSignalHandler()}, opts...)
	final, err := tea.NewProgram(m, opts...).Run()
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", false, err
	}
	fm := final.(control)
	return fm.choice, fm.ended, nil
}
