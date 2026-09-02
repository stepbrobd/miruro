package ui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

type endMsg struct{ dismiss bool }

type control struct {
	form *huh.Form
	wait func() bool
	logs <-chan string
	// seen holds the last keptLines written, oldest first
	seen []string
	// term is the terminal width, which bounds the log lines under the menu
	term      int
	ended     bool
	dismissed bool
}

func (m control) Init() tea.Cmd {
	return tea.Batch(m.form.Init(), func() tea.Msg { return endMsg{m.wait()} }, listenLog(m.logs))
}

func (m control) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case endMsg:
		m.ended = true
		if msg.dismiss {
			m.dismissed = true
			return m, tea.Quit
		}
		return m, nil
	case logMsg:
		m.seen = keep(m.seen, string(msg))
		return m, listenLog(m.logs)
	case tea.WindowSizeMsg:
		// the form needs the size too, so it is not consumed here
		m.term = msg.Width
	}
	f, cmd := m.form.Update(msg)
	m.form = f.(*huh.Form)
	return m, cmd
}

func (m control) View() string {
	if m.dismissed {
		return ""
	}
	view := m.form.View()
	if len(m.seen) == 0 {
		return view
	}
	var b strings.Builder
	b.WriteString(view)
	if !strings.HasSuffix(view, "\n") {
		b.WriteByte('\n')
	}
	writeLines(&b, m.seen, m.term)
	return b.String()
}

// Control shows the action menu while playback runs
// wait blocks until playback ends and reports whether the menu dismisses
// itself, the action is "" on a dismissal and quit on an aborted form, and
// ended reports whether playback was already over when the menu closed
// the menu owns the terminal while it runs, so the log is routed under it
// rather than left to land in the middle of a redraw
func Control(ctx context.Context, title string, actions []string, wait func() bool, opts ...tea.ProgramOption) (action string, ended bool, err error) {
	lines, restore := captureLog()
	defer restore()

	idx := 0
	m := control{form: menu(title, actions, func(s string) string { return s }, &idx), wait: wait, logs: lines}
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
