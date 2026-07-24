package ui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

type endMsg struct{ dismiss bool }

// control embeds the shared select form and adds playback ending as a second
// event source, keys stay with huh so the menu behaves like every other prompt
type control struct {
	form      *huh.Form
	wait      func() bool
	ended     bool
	dismissed bool
}

func (m control) Init() tea.Cmd {
	return tea.Batch(m.form.Init(), func() tea.Msg { return endMsg{m.wait()} })
}

func (m control) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if end, ok := msg.(endMsg); ok {
		m.ended = true
		if end.dismiss {
			m.dismissed = true
			return m, tea.Quit
		}
		return m, nil
	}
	f, cmd := m.form.Update(msg)
	m.form = f.(*huh.Form)
	return m, cmd
}

func (m control) View() string {
	if m.dismissed {
		return ""
	}
	return m.form.View()
}

// Control shows the action menu while playback runs
// wait blocks until playback ends and reports whether the menu dismisses
// itself, the action is "" on a dismissal and quit on an aborted form, and
// ended reports whether playback was already over when the menu closed
func Control(ctx context.Context, title string, actions []string, wait func() bool, opts ...tea.ProgramOption) (action string, ended bool, err error) {
	idx := 0
	m := control{form: menu(title, actions, func(s string) string { return s }, &idx), wait: wait}
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
	switch {
	case fm.dismissed:
		return "", true, nil
	case fm.form.State == huh.StateCompleted:
		return actions[idx], fm.ended, nil
	default:
		return "quit", fm.ended, nil
	}
}
