package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func pickerFixture() picker {
	return picker{
		codes:  []string{"a"},
		labels: []string{"sub"},
		state:  []int{resolved},
		subs:   []bool{false},
		choice: -1,
	}
}

// a finished picker must blank its frame so the next prompt does not render
// under it
func TestPickerClearsOnPick(t *testing.T) {
	next, _ := pickerFixture().providerKey(tea.KeyMsg{Type: tea.KeyEnter})
	m := next.(picker)
	if m.choice != 0 {
		t.Errorf("choice = %d, want 0", m.choice)
	}
	if m.View() != "" {
		t.Error("finished picker still renders")
	}
}

func TestPickerClearsOnAbort(t *testing.T) {
	next, _ := pickerFixture().providerKey(tea.KeyMsg{Type: tea.KeyEsc})
	m := next.(picker)
	if m.choice != -1 {
		t.Errorf("choice = %d, want -1", m.choice)
	}
	if m.View() != "" {
		t.Error("aborted picker still renders")
	}
}
