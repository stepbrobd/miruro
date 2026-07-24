package ui

import (
	"context"
	"errors"
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
