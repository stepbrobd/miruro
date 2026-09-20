// Package ui is the in-process selection surface, replacing external menu tools
package ui

import (
	"errors"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/termenv"
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
// at others, which wraps the row for the reason width.go opens with
func drive(form *huh.Form, opts ...tea.ProgramOption) error {
	// huh wires these in the Run this replaces, and NewForm leaves them nil, so
	// a form that reaches them never quits: the state goes to completed, the
	// view blanks, and the loop spins with the terminal still in raw mode
	form.SubmitCmd, form.CancelCmd = tea.Quit, tea.Quit
	// the picker belongs on stderr, where huh puts it, so a run whose stdout is
	// redirected still shows it and does not write it into the file
	opts = append([]tea.ProgramOption{tea.WithOutput(screen)}, opts...)
	final, err := tea.NewProgram(bounded{form: form}, opts...).Run()
	switch {
	case errors.Is(err, tea.ErrInterrupted):
		// a signal from outside the terminal is the user aborting, not a crash
		return ErrAborted
	case err != nil:
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
	rows int
}

func (m bounded) Init() tea.Cmd { return m.form.Init() }

func (m bounded) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if w, ok := msg.(tea.WindowSizeMsg); ok {
		m.term, m.rows = w.Width, w.Height
	}
	f, cmd := m.form.Update(msg)
	m.form = f.(*huh.Form)
	return m, cmd
}

func (m bounded) View() string { return bound(m.form.View(), m.term, m.rows) }

// screen is where every view here is drawn
// huh draws a form on stderr and bubbletea defaults to stdout, which put the
// three views of one run on two streams: muting the log with 2>/dev/null threw
// the pickers away while redirecting stdout wrote the playback menu into the
// file
// stderr is the one both can use, since stdout is what a run might legitimately
// be piped for
var screen = os.Stderr

// lipgloss and bubbles read the terminal's color capability from their own
// default output, which is stdout, so a redirected stdout stripped every style
// from views this package draws to stderr
// the renderer is pointed at the same stream the views use, and Downloads hands
// the matching profile to each bar, which captures it at construction
func init() {
	r := lipgloss.NewRenderer(screen)
	// bubbletea primes its own renderer in an init for exactly this reason: the
	// background query must not run while a program owns the terminal, or it
	// waits out the whole termenv timeout
	// replacing that renderer discards the priming, so this one pays the query
	// here instead, before any program is running
	r.HasDarkBackground()
	lipgloss.SetDefaultRenderer(r)
}

// profile is the color capability of the stream the views are drawn to
func profile() termenv.Profile { return lipgloss.DefaultRenderer().ColorProfile() }

const (
	// blindRows bounds a list when there is no terminal to measure
	blindRows = 16
	// spareRows is what the legend and the shell prompt need under a list
	spareRows = 4
)

// rows is the row count of the terminal the views are drawn into, zero when
// there is none
func rows() int {
	_, h, err := term.GetSize(screen.Fd())
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
			Height(fit(len(items), rows())).
			Filtering(true),
	)).WithTheme(theme())
	// only Run wires these, an embedded form must quit the host program itself
	f.SubmitCmd = tea.Quit
	f.CancelCmd = tea.Quit
	return f
}
