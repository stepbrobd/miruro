package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// every view here is drawn into a terminal of a width it is told about, and a
// row wider than that soft-wraps
// the renderer counts rows by newline, so a wrapped row is counted once while
// occupying two, and the next frame paints over the wrong lines

// columns is the width to draw for, the assumed one until a size message
// arrives
func columns(w int) int {
	if w <= 0 {
		return defaultTerm
	}
	return w
}

const (
	// minBar is the narrowest bar worth drawing, under which the row carries
	// its byte counter alone
	minBar = 8
	// maxBar is what a bar takes once the terminal has room to spare
	maxBar = 30
	// counterRoom is the widest byte counter a row can carry, two sizes and
	// their separator
	counterRoom = len("1023.9 TB / 1023.9 TB")
	// gutters are the two leading spaces, the one after the label, and the two
	// before the counter
	gutters = 5
)

// barRoom is what the terminal leaves a progress bar once the label and the
// byte counter beside it have theirs, zero when it leaves too little
// a bar that keeps its full width on a narrow terminal pushes the row past the
// edge, and a row that wraps is drawn once and counted once while occupying
// two, which is what makes the bars paint over each other
func barRoom(width, label int) int {
	return min(columns(width)-label-gutters-counterRoom, maxBar)
}

// defaultTerm is assumed until the first tea.WindowSizeMsg reports the real width
const defaultTerm = 80

const ellipsis = "..."

// flatten keeps a multi-line error on one row
// ffmpeg reports its failures across several lines, which would otherwise push
// the still-live rows below out of place
var flatten = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// cut returns the longest prefix of a plain string fitting w display columns
func cut(s string, w int) string {
	used := 0
	for i, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > w {
			return s[:i]
		}
		used += rw
	}
	return s
}

// bound cuts every row of a rendered view to the terminal it is drawn for
// huh sizes its own rows to the width it is given, except its key legend, which
// keeps a fixed width whatever the terminal is, so a narrow terminal wraps it
// the cut is ansi aware because a row carries the styling huh put in it, and
// cutting a row mid escape would leak the sequence into the rows below
func bound(view string, width int) string {
	term := columns(width)
	lines := strings.Split(view, "\n")
	for i, line := range lines {
		if lipgloss.Width(line) > term {
			lines[i] = ansi.Truncate(line, term, "")
		}
	}
	return strings.Join(lines, "\n")
}
