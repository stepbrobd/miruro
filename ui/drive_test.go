package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// huh wires the submit and cancel commands in the Run drive replaces, and
// NewForm leaves them nil, so a form that reaches drive unwired never quits:
// the state goes to completed, the view blanks, and the loop spins with the
// terminal in raw mode and no key able to reach it
func TestDriveQuitsAFormItWasNotHanded(t *testing.T) {
	for _, tc := range []struct{ name, keys string }{
		{"a submitted prompt", "hi\r"},
		{"an aborted prompt", "\x03"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s string
			// exactly what Prompt builds, which unlike menu wires nothing itself
			form := huh.NewForm(huh.NewGroup(huh.NewInput().Title("Search anime").Value(&s))).WithTheme(theme())
			done := make(chan error, 1)
			go func() {
				done <- drive(form, tea.WithInput(strings.NewReader(tc.keys)), tea.WithOutput(discard{}))
			}()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("drive never returned, which is a run no key can exit")
			}
		})
	}
}
