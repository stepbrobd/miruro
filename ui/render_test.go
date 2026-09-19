package ui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// bars are what Downloads builds, so a test measures the row a user sees
func bars(n int) []progress.Model {
	out := make([]progress.Model, n)
	for i := range out {
		out[i] = progress.New(progress.WithWidth(maxBar), progress.WithoutPercentage())
	}
	return out
}

// widths span every terminal a view might be drawn into
// sampling a handful misses the overflows, since huh truncates its own rows at
// some widths and not others
func widths() []int {
	var out []int
	for w := 10; w <= 200; w++ {
		out = append(out, w)
	}
	return out
}

// the catalog carries titles this long, and an episode label carries the
// episode name after the number
const longTitle = "Saijo no Osewa: Takane no Hanadarake na Meimonkou de, Gakuin Ichi no Ojousama (Seikatsu Nouryoku Kaimu) wo Kagenagara Osewa suru Koto ni Narimashita"

// a japanese title is two columns per rune, which is what tells a byte count
// from a display width
const cjkTitle = "鬼滅の刃 無限列車編 " + "劇場版アニメーション作品"

// fits reports every row of a view that is wider than the terminal it was
// drawn for
// a row that overflows soft-wraps, and the renderer counts rows by newline, so
// a wrapped row is drawn once and counted once while occupying two, which is
// what paints the next frame over the wrong lines
func fits(t *testing.T, what string, view string, width int) {
	t.Helper()
	for i, line := range strings.Split(view, "\n") {
		if w := lipgloss.Width(line); w > width {
			t.Errorf("%s at width %d: row %d is %d columns\n%q", what, width, i, w, line)
		}
	}
}

func TestSelectFitsEveryWidth(t *testing.T) {
	titles := []string{longTitle, cjkTitle, "Short", strings.Repeat("x", 300)}
	for _, width := range widths() {
		idx := 0
		var m tea.Model = bounded{form: menu("Select anime", titles, func(s string) string { return s }, &idx)}
		m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		fits(t, "anime select", m.View(), width)
	}
}

// the provider rows are padded to line their variants up, which is a second
// way for a row to outgrow the terminal
func TestProviderRowsFitEveryWidth(t *testing.T) {
	rows := []string{
		"ally      hardsub",
		"vidstreaming-beta-2  softsub",
		strings.Repeat("provider", 20) + "  softsub",
	}
	for _, width := range widths() {
		idx := 0
		var m tea.Model = bounded{form: menu("Select provider", rows, func(s string) string { return s }, &idx)}
		m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		fits(t, "provider select", m.View(), width)
	}
}

// the download view draws a bar and a byte count beside every label, and a
// failed row draws the whole upstream error
func TestDownloadsFitEveryWidth(t *testing.T) {
	labels := []string{"E1", "E2", "E3"}
	for _, width := range widths() {
		m := downloads{
			labels: labels,
			width:  2,
			bars:   bars(3),
			done:   []int64{1 << 20, 0, 0},
			total:  []int64{1 << 22, 0, 0},
			errs:   []error{nil, nil, errors.New("download http://127.0.0.1:55636/2b5b440f4e857cf2267552a94a268888/eyJ1IjoiaHR0cHM6Ly9obHMuYW5pZGIuYXBwL3N0cmVhbS90NEx3WGVYellMY1VqcEh5Tk53OU9DWjlMRHNQQkZrQWlNSUtFNmoxdGV0STE3di05U3RrRFhXVXVnRFZJRlVML21hc3Rlci5tM3U4OiBzdGF0dXMgNDI5")},
			fin:    []bool{false, false, true},
			seen:   []string{"WARN download failed, trying the next stream episode=3 provider=pewe err=\"status 429\""},
			term:   width,
		}
		var model tea.Model = m
		model, _ = model.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		fits(t, "downloads", model.(downloads).View(), width)
	}
}

// capture renders each view once at a readable width, so a run with -v shows
// what a user actually sees rather than only asserting about it
func TestCaptureViews(t *testing.T) {
	if !testing.Verbose() {
		t.Skip("run with -v to print the views")
	}
	const width = 80

	idx := 0
	var m tea.Model = bounded{form: menu("Select anime", []string{longTitle, cjkTitle, "Short"}, func(s string) string { return s }, &idx)}
	m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
	t.Logf("anime select:\n%s", m.View())

	var c tea.Model = fixture()
	c, _ = c.Update(tea.WindowSizeMsg{Width: width, Height: 24})
	c, _ = c.Update(logMsg("WARN stream refused before it played, abandoning it server=stream refused=30"))
	c, _ = c.Update(logMsg("WARN no provider left to try provider=hop resolve=\"ally: upstream unreachable: miruro status 444\""))
	t.Logf("control menu:\n%s", c.(control).View())

	d := downloads{
		labels: []string{"E1", "E2"},
		width:  2,
		bars:   bars(2),
		done:   []int64{3 << 20, 1 << 20},
		total:  []int64{8 << 20, 0},
		errs:   make([]error, 2),
		fin:    make([]bool, 2),
		term:   width,
	}
	var dm tea.Model = d
	dm, _ = dm.Update(tea.WindowSizeMsg{Width: width, Height: 24})
	t.Logf("downloads:\n%s", dm.(downloads).View())
}

// the control menu is the view a user reads while playback runs, and huh's key
// legend keeps a fixed width whatever the terminal is, so it is the row that
// overflows a narrow one
func TestControlFitsEveryWidthScanned(t *testing.T) {
	actions := []string{"next", "replay", "previous", "select", "change provider", "quit"}
	for _, width := range widths() {
		idx := 0
		m := control{
			form: menu("Episode 3 of "+longTitle, actions, func(s string) string { return s }, &idx),
			term: width,
			seen: []string{"WARN stream refused before it played, abandoning it server=stream refused=30"},
		}
		var model tea.Model = m
		model, _ = model.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		fits(t, "control menu", model.(control).View(), width)
	}
}

// Select and Prompt run their form through the same bounding wrapper, so the
// rows a user sees are the wrapper's, not huh's
func TestBoundedFormFitsEveryWidth(t *testing.T) {
	for _, width := range widths() {
		idx := 0
		var model tea.Model = bounded{form: menu("Select provider", []string{"ally  hardsub", "hop  softsub"}, func(s string) string { return s }, &idx)}
		model, _ = model.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		fits(t, "bounded select", model.View(), width)

		var long tea.Model = bounded{form: menu("Select anime", []string{longTitle, cjkTitle}, func(s string) string { return s }, &idx)}
		long, _ = long.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		fits(t, "bounded anime select", long.View(), width)
	}
}

// an aborted form is what Ctrl-C does, and it must reach the caller as
// ErrAborted rather than as a pick the user never made
func TestBoundedFormReportsAnAbort(t *testing.T) {
	idx := 0
	f := menu("Select anime", []string{"a", "b"}, func(s string) string { return s }, &idx)
	var model tea.Model = bounded{form: f}
	model.Init()
	model, _ = model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if got := model.(bounded).form.State; got == huh.StateCompleted {
		t.Errorf("an interrupted form reported state %v, want anything but completed", got)
	}
}
