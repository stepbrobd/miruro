// Package ui is the in-process selection surface, replacing external menu tools
package ui

import (
	"errors"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
)

// ErrAborted is returned when the user cancels a selection
var ErrAborted = huh.ErrUserAborted

func Prompt(title string) (string, error) {
	var s string
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title(title).Value(&s),
	)).WithTheme(theme())
	if err := drive(form); err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// Select shows a filterable list and returns the chosen item
// it selects by index so T need not be comparable
func Select[T any](title string, items []T, label func(T) string) (T, error) {
	var zero T
	if len(items) == 0 {
		return zero, errors.New("nothing to select")
	}
	idx := 0
	if err := drive(menu(title, items, label, &idx)); err != nil {
		return zero, err
	}
	return items[idx], nil
}

// drive runs a form as its own program, so every row it paints can be cut to
// the terminal
// huh's own Run gives no way to touch the view, and its key legend keeps a
// width the terminal may not have: it trims the legend at some widths and not
// at others, and a row wider than the terminal wraps, which the renderer counts
// as one row while it occupies two
func drive(form *huh.Form) error {
	final, err := tea.NewProgram(bounded{form: form}).Run()
	if err != nil {
		return err
	}
	if final.(bounded).form.State != huh.StateCompleted {
		return ErrAborted
	}
	return nil
}

// bounded is one huh form with its view cut to the terminal it is drawn into
type bounded struct {
	form *huh.Form
	term int
}

func (m bounded) Init() tea.Cmd { return m.form.Init() }

func (m bounded) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if w, ok := msg.(tea.WindowSizeMsg); ok {
		m.term = w.Width
	}
	f, cmd := m.form.Update(msg)
	m.form = f.(*huh.Form)
	return m, cmd
}

func (m bounded) View() string { return bound(m.form.View(), m.term) }

const (
	// blindRows bounds a list when there is no terminal to measure
	blindRows = 16
	// spareRows is what the legend and the shell prompt need under a list
	spareRows = 4
)

// screen is the row count of the attached terminal, zero when there is none
func screen() int {
	_, h, err := term.GetSize(os.Stdout.Fd())
	if err != nil {
		return 0
	}
	return h
}

// fit is how tall a list of n items should be drawn on a terminal of h rows,
// where h is zero when there is none to measure
// a list that fits on screen is drawn whole, so moving the selector never
// scrolls what is already visible out of the way
// one row goes to the filter line above the options, and a catalog of a
// thousand episodes still stops at the height of the terminal
func fit(n, h int) int {
	limit := blindRows
	if h > 0 {
		limit = max(h-spareRows, 2)
	}
	return min(n+1, limit)
}

// menu is the one select form, so every list prompt shares keymap and theme
func menu[T any](title string, items []T, label func(T) string, idx *int) *huh.Form {
	opts := make([]huh.Option[int], len(items))
	for i, it := range items {
		opts[i] = huh.NewOption(label(it), i)
	}
	f := huh.NewForm(huh.NewGroup(
		huh.NewSelect[int]().
			Title(title).
			Options(opts...).
			Value(idx).
			Height(fit(len(items), screen())).
			Filtering(true),
	)).WithTheme(theme())
	// only Run wires these, an embedded form must quit the host program itself
	f.SubmitCmd = tea.Quit
	f.CancelCmd = tea.Quit
	return f
}
