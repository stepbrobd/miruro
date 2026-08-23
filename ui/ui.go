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
	if err := form.Run(); err != nil {
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
	if err := menu(title, items, label, &idx).Run(); err != nil {
		return zero, err
	}
	return items[idx], nil
}

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
