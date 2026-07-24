package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func controlFixture() control {
	return control{
		title:   "Episode 1 of Test",
		status:  "playing",
		actions: []string{"next", "replay", "select", "change provider", "quit"},
	}
}

func key(s string) tea.KeyMsg {
	if s == "enter" {
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	if s == "esc" {
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func step(t *testing.T, m control, msg tea.Msg) (control, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	nm, ok := next.(control)
	if !ok {
		t.Fatalf("Update returned %T, want control", next)
	}
	return nm, cmd
}

func TestControlDismiss(t *testing.T) {
	m, cmd := step(t, controlFixture(), endMsg(End{Dismiss: true}))
	if !m.ended || !m.done || m.choice != "" {
		t.Errorf("dismiss = (ended %v, done %v, choice %q), want (true, true, \"\")", m.ended, m.done, m.choice)
	}
	if cmd == nil {
		t.Error("dismiss did not quit the program")
	}
	if m.View() != "" {
		t.Error("dismissed menu still renders")
	}
}

func TestControlStay(t *testing.T) {
	m, cmd := step(t, controlFixture(), endMsg(End{Status: "finished"}))
	if !m.ended || m.done {
		t.Errorf("stay = (ended %v, done %v), want (true, false)", m.ended, m.done)
	}
	if cmd != nil {
		t.Error("staying menu quit the program")
	}
	if m.status != "finished" {
		t.Errorf("status = %q, want finished", m.status)
	}

	m, _ = step(t, m, key("enter"))
	if m.choice != "next" || !m.ended {
		t.Errorf("pick after end = (%q, ended %v), want (next, true)", m.choice, m.ended)
	}
}

func TestControlPickWhilePlaying(t *testing.T) {
	m := controlFixture()
	m, _ = step(t, m, key("j"))
	m, cmd := step(t, m, key("enter"))
	if m.choice != "replay" || m.ended {
		t.Errorf("early pick = (%q, ended %v), want (replay, false)", m.choice, m.ended)
	}
	if cmd == nil {
		t.Error("pick did not quit the program")
	}
}

func TestControlCursorBounds(t *testing.T) {
	m := controlFixture()
	m, _ = step(t, m, key("k"))
	if m.cursor != 0 {
		t.Errorf("cursor moved above the first row to %d", m.cursor)
	}
	for range len(m.actions) + 2 {
		m, _ = step(t, m, key("j"))
	}
	if m.cursor != len(m.actions)-1 {
		t.Errorf("cursor = %d, want pinned to %d", m.cursor, len(m.actions)-1)
	}
}

func TestControlAbort(t *testing.T) {
	for _, k := range []string{"esc", "q"} {
		m, _ := step(t, controlFixture(), key(k))
		if m.choice != "quit" {
			t.Errorf("%s picked %q, want quit", k, m.choice)
		}
	}
}

// letters must not pick, the other pickers only navigate and enter
func TestControlLetterKey(t *testing.T) {
	for _, k := range []string{"n", "r", "s", "c", "x"} {
		m, cmd := step(t, controlFixture(), key(k))
		if m.choice != "" || m.done || cmd != nil {
			t.Errorf("key %q = (%q, done %v), want inert", k, m.choice, m.done)
		}
	}
}

func TestControlCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	defer close(release)
	wait := func() End {
		cancel()
		<-release
		return End{}
	}
	_, _, err := Control(ctx, "t", []string{"quit"}, wait,
		tea.WithInput(strings.NewReader("")), tea.WithoutRenderer())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Control returned %v, want context.Canceled", err)
	}
}

func TestControlKeyOverPipe(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	wait := func() End {
		<-blocked
		return End{}
	}
	action, ended, err := Control(context.Background(), "t", []string{"next", "quit"}, wait,
		tea.WithInput(strings.NewReader("j\r")), tea.WithoutRenderer())
	if err != nil || action != "quit" || ended {
		t.Errorf("Control = (%q, %v, %v), want (quit, false, nil)", action, ended, err)
	}
}
