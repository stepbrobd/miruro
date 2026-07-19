package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// styled stands in for ok(false), whose escapes depend on the detected profile
const styled = "\x1b[31mx\x1b[0m"

func TestErrLine(t *testing.T) {
	long := "ffmpeg: exit status 234: Unable to choose an output format for " +
		"'/home/x/Videos/Show - E1.mp4.part'; use a standard extension for the " +
		"filename or specify the format manually."

	cases := []struct {
		name      string
		msg       string
		term      int
		truncated bool
	}{
		{"short is untouched", "no such file", 80, false},
		{"newlines collapse", "ffmpeg failed\nError initializing the muxer\n", 80, false},
		{"crlf collapses", "ffmpeg failed\r\nError initializing the muxer", 80, false},
		{"long is truncated", long, 80, true},
		{"long fits a wide terminal", long, 400, false},
		{"unknown width falls back", long, 0, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := errLine(styled, "Show - E1", c.msg, c.term)

			if strings.ContainsAny(got, "\n\r") {
				t.Errorf("line spans more than one row:\n%q", got)
			}
			if !strings.Contains(got, styled) {
				t.Errorf("marker was altered or sliced:\n%q", got)
			}
			if !strings.Contains(got, "Show - E1") {
				t.Errorf("label was altered or sliced:\n%q", got)
			}

			want := c.term
			if want <= 0 {
				want = defaultTerm
			}
			if w := lipgloss.Width(got); w > want {
				t.Errorf("width %d exceeds terminal %d:\n%q", w, want, got)
			}

			switch {
			case c.truncated && !strings.HasSuffix(got, ellipsis):
				t.Errorf("truncated line does not end in %q:\n%q", ellipsis, got)
			case !c.truncated && !strings.HasSuffix(got, flatten.Replace(c.msg)):
				t.Errorf("message should have survived whole:\n%q", got)
			}
		})
	}
}

// the ellipsis must stay ASCII, since a lone rune renders as a blank box on
// terminals without the font for it
func TestErrLineASCIIEllipsis(t *testing.T) {
	got := errLine("x", "label", strings.Repeat("a", 200), 40)
	for _, r := range got {
		if r > 127 {
			t.Fatalf("non-ascii rune %q in rendered line:\n%q", r, got)
		}
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("want an ascii ellipsis suffix, got:\n%q", got)
	}
}

// a terminal narrower than the label column leaves no room at all, which must
// degrade instead of slicing out of range
func TestErrLineNarrowTerm(t *testing.T) {
	cases := []struct {
		name string
		term int
	}{
		{"negative", -10},
		{"one column", 1},
		{"under the ellipsis", 3},
		{"just past the head", 20},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := errLine(styled, "a very long label indeed", "boom\nsplit", c.term)
			if strings.ContainsAny(got, "\n\r") {
				t.Errorf("line spans more than one row:\n%q", got)
			}
			if !strings.Contains(got, "a very long label indeed") {
				t.Errorf("label was sliced:\n%q", got)
			}
		})
	}
}

func TestCutFitsWidth(t *testing.T) {
	cases := []struct {
		name string
		s    string
		w    int
		want string
	}{
		{"zero width", "abcdef", 0, ""},
		{"negative width", "abcdef", -3, ""},
		{"exact fit", "abcdef", 6, "abcdef"},
		{"room to spare", "abc", 10, "abc"},
		{"partial", "abcdef", 3, "abc"},
		{"wide runes stay whole", "日本語", 4, "日本"},
		{"empty", "", 5, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cut(c.s, c.w); got != c.want {
				t.Errorf("cut(%q, %d) = %q, want %q", c.s, c.w, got, c.want)
			}
		})
	}
}
