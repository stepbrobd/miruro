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
	return Control(context.Background(), "t", []string{"next", "quit"}, wait,
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
	_, _, err := Control(ctx, "t", []string{"quit"}, wait,
		tea.WithInput(strings.NewReader("")), tea.WithoutRenderer())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Control returned %v, want context.Canceled", err)
	}
}

// the menu owns the terminal while playback runs, so a log record written
// straight to stderr lands inside a redraw and the user sees a prompt with no
// explanation
func TestControlShowsTheLog(t *testing.T) {
	m := fixture()
	m.logs = make(chan string)
	before := m.View()
	if strings.Contains(before, "abandoning") {
		t.Fatal("a menu with no log rendered one")
	}

	next, cmd := m.Update(logMsg("WARN stream refused before it played, abandoning it server=HD-1"))
	m = next.(control)
	if cmd == nil {
		t.Error("the log listener was not re-armed, so only one record would ever arrive")
	}
	view := m.View()
	if !strings.Contains(view, "HD-1") || !strings.Contains(view, "abandoning it") {
		t.Errorf("view does not carry the record:\n%s", view)
	}
	if !strings.HasPrefix(view, before) {
		t.Errorf("the record displaced the menu instead of sitting under it:\n%s", view)
	}
}

// a walk across every provider writes more records than belong under a prompt
func TestControlKeepsTheLastLines(t *testing.T) {
	var m tea.Model = fixture()
	for i := range keptLines + 3 {
		m, _ = m.Update(logMsg(fmt.Sprintf("line-%d", i)))
	}
	c := m.(control)
	if len(c.seen) != keptLines {
		t.Fatalf("kept %d lines, want %d", len(c.seen), keptLines)
	}
	view := c.View()
	if strings.Contains(view, "line-0") || !strings.Contains(view, "line-10") {
		t.Errorf("kept the wrong window:\n%s", view)
	}
}

// a dismissal clears the menu to auto-advance, and the log goes with it
func TestControlDismissedShowsNoLog(t *testing.T) {
	m, _ := fixture().Update(logMsg("gone"))
	m, _ = m.Update(endMsg{dismiss: true})
	if got := m.(control).View(); got != "" {
		t.Errorf("dismissed menu rendered %q", got)
	}
}

// the logger writes one whole record per call and holds its mutex while it
// does, so the sink must never block it
func TestSinkNeverBlocks(t *testing.T) {
	s := make(sink, 1)
	for range 50 {
		if _, err := s.Write([]byte("a record\n")); err != nil {
			t.Fatal(err)
		}
	}
	if len(s) != 1 {
		t.Errorf("sink holds %d, want it to have dropped down to its capacity of 1", len(s))
	}

	drained := make(sink, 8)
	if _, err := drained.Write([]byte("one\ntwo\n\n")); err != nil {
		t.Fatal(err)
	}
	if len(drained) != 2 {
		t.Errorf("a two-line record produced %d messages, want 2 with the blank dropped", len(drained))
	}
}
