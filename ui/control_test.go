package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func fixture() control {
	idx := 0
	return control{form: menu("t", []string{"next", "quit"}, func(s string) string { return s }, &idx)}
}

func TestControlDismiss(t *testing.T) {
	next, cmd := fixture().Update(endMsg{dismiss: true})
	m := next.(control)
	if !m.ended || !m.dismissed {
		t.Errorf("dismiss = (ended %v, dismissed %v), want (true, true)", m.ended, m.dismissed)
	}
	if cmd == nil {
		t.Error("dismissal did not quit the program")
	}
	if m.View() != "" {
		t.Error("dismissed menu still renders")
	}
}

func TestControlStay(t *testing.T) {
	next, cmd := fixture().Update(endMsg{})
	m := next.(control)
	if !m.ended || m.dismissed {
		t.Errorf("stay = (ended %v, dismissed %v), want (true, false)", m.ended, m.dismissed)
	}
	if cmd != nil {
		t.Error("staying menu quit the program")
	}
}

func run(t *testing.T, input string, wait func() bool) (string, bool, error) {
	t.Helper()
	return Control(context.Background(), "t", []string{"next", "quit"}, wait, nil,
		tea.WithInput(strings.NewReader(input)), tea.WithoutRenderer())
}

func idle(t *testing.T) func() bool {
	t.Helper()
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	return func() bool {
		<-blocked
		return false
	}
}

func TestControlPick(t *testing.T) {
	action, ended, err := run(t, "\r", idle(t))
	if err != nil || action != "next" || ended {
		t.Errorf("Control = (%q, %v, %v), want (next, false, nil)", action, ended, err)
	}
}

func TestControlNavigate(t *testing.T) {
	action, _, err := run(t, "\x1b[B\r", idle(t))
	if err != nil || action != "quit" {
		t.Errorf("Control = (%q, %v), want (quit, nil)", action, err)
	}
}

func TestControlAbort(t *testing.T) {
	action, _, err := run(t, "\x03", idle(t))
	if err != nil || action != "quit" {
		t.Errorf("aborted Control = (%q, %v), want (quit, nil)", action, err)
	}
}

func TestControlDismissLive(t *testing.T) {
	action, ended, err := run(t, "", func() bool { return true })
	if err != nil || action != "" || !ended {
		t.Errorf("Control = (%q, %v, %v), want (\"\", true, nil)", action, ended, err)
	}
}

func TestControlCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	defer close(release)
	wait := func() bool {
		cancel()
		<-release
		return false
	}
	_, _, err := Control(ctx, "t", []string{"quit"}, wait, nil,
		tea.WithInput(strings.NewReader("")), tea.WithoutRenderer())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Control returned %v, want context.Canceled", err)
	}
}

// the menu owns the terminal while playback runs, so a failure written to the
// log lands inside a redraw and the user sees a prompt with no explanation
func TestControlShowsNotes(t *testing.T) {
	m := fixture()
	m.notes = make(chan Note)
	m.term = 100
	menu := m.View()
	if strings.Contains(menu, "abandoning") {
		t.Fatal("a menu with no notes rendered one")
	}

	next, cmd := m.Update(noteMsg{Subject: "HD-1", Reason: "refused 8 bodies before it played, abandoning it"})
	m = next.(control)
	if cmd == nil {
		t.Error("the note listener was not re-armed, so only one note would ever arrive")
	}
	view := m.View()
	if !strings.Contains(view, "HD-1") || !strings.Contains(view, "abandoning it") {
		t.Errorf("view does not carry the note:\n%s", view)
	}
	if !strings.HasPrefix(view, menu) {
		t.Errorf("the note displaced the menu instead of sitting under it:\n%s", view)
	}
}

// a walk across every provider reports more notes than fit above the prompt
func TestControlKeepsTheLastNotes(t *testing.T) {
	var m tea.Model = fixture()
	for i := range keptNotes + 3 {
		m, _ = m.Update(noteMsg{Subject: fmt.Sprintf("s%d", i), Reason: "gone"})
	}
	c := m.(control)
	if len(c.seen) != keptNotes {
		t.Fatalf("kept %d notes, want %d", len(c.seen), keptNotes)
	}
	view := c.View()
	if strings.Contains(view, "s0 ") || !strings.Contains(view, "s7") {
		t.Errorf("kept the wrong window of notes:\n%s", view)
	}
}

// a dismissal clears the menu to auto-advance, and the notes go with it
func TestControlDismissedShowsNoNotes(t *testing.T) {
	m, _ := fixture().Update(noteMsg{Subject: "HD-1", Reason: "gone"})
	m, _ = m.Update(endMsg{dismiss: true})
	if got := m.(control).View(); got != "" {
		t.Errorf("dismissed menu rendered %q", got)
	}
}
