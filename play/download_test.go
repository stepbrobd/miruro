package play

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeNameStaysOneComponent(t *testing.T) {
	for _, in := range []string{
		"../../../home/ysun/.bashrc",
		"..",
		"a/b\\c",
		"Fate/stay night - E1",
		"Re:ZERO - E3",
		"\x00\x01evil",
	} {
		got := safeName(in)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("safeName(%q) = %q still holds a separator", in, got)
		}
		if dir := filepath.Dir(filepath.Join("/dl", got)); dir != "/dl" {
			t.Errorf("safeName(%q) = %q escapes the dir (parent %q)", in, got, dir)
		}
	}
}

func TestSafeNameDefaultsEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "  ..  ", "."} {
		if got := safeName(in); got != "untitled" {
			t.Errorf("safeName(%q) = %q, want untitled", in, got)
		}
	}
}

func TestSafeNameKeepsPlainTitles(t *testing.T) {
	if got := safeName("Frieren - E5"); got != "Frieren - E5" {
		t.Errorf("safeName mangled a plain title: %q", got)
	}
}
