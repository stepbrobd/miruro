// Package ui is the in-process selection surface, replacing external menu tools
package ui

import (
	"errors"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
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

// menuRows bounds how tall a list grows before it scrolls instead
const menuRows = 16

// menu is the one select form, so every list prompt shares keymap and theme
// the height follows the list, since a fixed one leaves a short list sitting
// above a block of blank rows and the legend stranded under them
// one row goes to the filter line above the options
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
			Height(min(len(items)+1, menuRows)).
			Filtering(true),
	)).WithTheme(theme())
	// only Run wires these, an embedded form must quit the host program itself
	f.SubmitCmd = tea.Quit
	f.CancelCmd = tea.Quit
	return f
}
