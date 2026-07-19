// Package ui is the in-process selection surface, replacing external menu tools.
package ui

import (
	"errors"
	"strings"

	"github.com/charmbracelet/huh"
)

// ErrAborted is returned when the user cancels a selection.
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

// Select shows a filterable list and returns the chosen item. it selects by
// index so T need not be comparable.
func Select[T any](title string, items []T, label func(T) string) (T, error) {
	var zero T
	if len(items) == 0 {
		return zero, errors.New("nothing to select")
	}

	opts := make([]huh.Option[int], len(items))
	for i, it := range items {
		opts[i] = huh.NewOption(label(it), i)
	}

	idx := 0
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[int]().
			Title(title).
			Options(opts...).
			Value(&idx).
			Height(16).
			Filtering(true),
	)).WithTheme(theme())

	if err := form.Run(); err != nil {
		return zero, err
	}
	return items[idx], nil
}

func Menu(title string, actions []string) (string, error) {
	return Select(title, actions, func(s string) string { return s })
}
