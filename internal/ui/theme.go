package ui

import (
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// nord dark palette
const (
	nord3  = "#4C566A"
	nord4  = "#D8DEE9"
	nord8  = "#88C0D0"
	nord11 = "#BF616A"
	nord13 = "#EBCB8B"
	nord14 = "#A3BE8C"
)

func theme() *huh.Theme {
	t := huh.ThemeBase()
	col := func(hex string) lipgloss.Color { return lipgloss.Color(hex) }

	f := &t.Focused
	f.Title = f.Title.Foreground(col(nord8)).Bold(true)
	f.SelectSelector = f.SelectSelector.Foreground(col(nord13))
	f.SelectedOption = f.SelectedOption.Foreground(col(nord14))
	f.Option = f.Option.Foreground(col(nord4))
	f.Description = f.Description.Foreground(col(nord3))
	f.ErrorIndicator = f.ErrorIndicator.Foreground(col(nord11))
	f.ErrorMessage = f.ErrorMessage.Foreground(col(nord11))
	f.NextIndicator = f.NextIndicator.Foreground(col(nord8))
	f.PrevIndicator = f.PrevIndicator.Foreground(col(nord8))

	b := &t.Blurred
	b.Title = b.Title.Foreground(col(nord3))
	b.Option = b.Option.Foreground(col(nord3))

	return t
}
