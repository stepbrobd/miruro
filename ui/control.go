package ui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

type endMsg struct{ dismiss bool }

// Note is one thing the playback reports while the menu is raised
// the menu owns the terminal, so a note written to the log would land in the
// middle of a redraw and is shown here instead
type Note struct {
	// Subject is what failed, a server or a provider
	Subject string
	// Reason is why, truncated to the terminal width
	Reason string
}

type noteMsg Note

// keptNotes bounds how many notes stay on screen
// a walk across every provider reports more than fits above the prompt, and the
// last few are the ones that say where it got to
const keptNotes = 5

func listenNote(ch <-chan Note) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		n, ok := <-ch
		if !ok {
			return nil
		}
		return noteMsg(n)
	}
}

// control embeds the shared select form and adds playback ending as a second
// event source, keys stay with huh so the menu behaves like every other prompt
type control struct {
	form  *huh.Form
	wait  func() bool
	notes <-chan Note
	// seen holds the last keptNotes reported, oldest first
	seen      []Note
	term      int
	ended     bool
	dismissed bool
}

func (m control) Init() tea.Cmd {
	return tea.Batch(m.form.Init(), func() tea.Msg { return endMsg{m.wait()} }, listenNote(m.notes))
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
	case noteMsg:
		m.seen = append(m.seen, Note(msg))
		if len(m.seen) > keptNotes {
			m.seen = m.seen[len(m.seen)-keptNotes:]
		}
		return m, listenNote(m.notes)
	case tea.WindowSizeMsg:
		// a resize arrives outside the note stream, so it must not re-arm the
		// listener and start a second reader
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
	for _, n := range m.seen {
		b.WriteString(errLine(ok(false), n.Subject, n.Reason, m.term))
		b.WriteByte('\n')
	}
	return b.String()
}

// Control shows the action menu while playback runs
// wait blocks until playback ends and reports whether the menu dismisses
// itself, the action is "" on a dismissal and quit on an aborted form, and
// ended reports whether playback was already over when the menu closed
// notes are rendered under the menu as they arrive, and a nil channel shows
// none
func Control(ctx context.Context, title string, actions []string, wait func() bool, notes <-chan Note, opts ...tea.ProgramOption) (action string, ended bool, err error) {
	idx := 0
	m := control{form: menu(title, actions, func(s string) string { return s }, &idx), wait: wait, notes: notes}
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
