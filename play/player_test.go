package play

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"ysun.co/miruro"
)

func TestTailKeepsSuffix(t *testing.T) {
	var tl tail
	tl.Write(bytes.Repeat([]byte("x"), tailMax))
	tl.Write([]byte("\nlast words\n"))
	if len(tl.b) > tailMax {
		t.Errorf("tail holds %d bytes, cap is %d", len(tl.b), tailMax)
	}
	if got := tl.last(); got != "last words" {
		t.Errorf("last() = %q, want %q", got, "last words")
	}
}

func TestTailLast(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", ""},
		{"\n\n  \n", ""},
		{"one\ntwo\n\n", "two"},
		{"only\r\n", "only"},
	} {
		var tl tail
		tl.Write([]byte(tc.in))
		if got := tl.last(); got != tc.want {
			t.Errorf("last(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// a missing player must fail the run before any resolution work, so Detect
// reports rather than returning a bin that cannot exec
func TestDetectErrorsWithoutPlayers(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if p, err := Detect(""); err == nil {
		t.Errorf("empty PATH still detected %+v", p)
	}
}

func TestDetectHonoursPreference(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script players")
	}
	dir := t.TempDir()
	for _, name := range []string{"mpv", "iina"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)

	p, err := Detect(IINA)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != IINA {
		t.Errorf("Detect(IINA) picked %s", p.Kind)
	}

	// a stale preference falls back to the platform default
	p, err = Detect("vlc")
	if err != nil {
		t.Fatal(err)
	}
	want := MPV
	if runtime.GOOS == "darwin" {
		want = IINA
	}
	if p.Kind != want {
		t.Errorf("Detect(vlc) picked %s, want %s", p.Kind, want)
	}
}

// fakePlayer writes a script that prints to stderr and exits with the given code
func fakePlayer(t *testing.T, code int) Player {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script player")
	}
	bin := filepath.Join(t.TempDir(), "player")
	script := fmt.Sprintf("#!/bin/sh\necho noise >&2\necho 'the real failure'\nexit %d\n", code)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return Player{Kind: MPV, Bin: bin}
}

func TestPlayWrapsStderr(t *testing.T) {
	p := fakePlayer(t, 3)
	err := p.Play(context.Background(), miruro.Stream{URL: "http://localhost/x"}, nil, nil, "t")
	if !errors.As(err, new(*exec.ExitError)) {
		t.Fatalf("err = %v, want an ExitError", err)
	}
	if !strings.Contains(err.Error(), "the real failure") {
		t.Errorf("err = %q, want the last stderr line attached", err)
	}
}

func TestPlayCleanExit(t *testing.T) {
	p := fakePlayer(t, 0)
	if err := p.Play(context.Background(), miruro.Stream{URL: "http://localhost/x"}, nil, nil, "t"); err != nil {
		t.Errorf("clean exit returned %v", err)
	}
}

// the proxy injects the referer upstream, so the only difference between the
// two players is iina's prefix and its own two flags
func TestArgsPerPlayer(t *testing.T) {
	s := miruro.Stream{URL: "http://127.0.0.1:1/tok/pay.m3u8"}
	subs := []miruro.Subtitle{{File: "http://127.0.0.1:1/tok/en.vtt"}}

	for _, tc := range []struct {
		kind Kind
		want []string
	}{
		{MPV, []string{
			"--force-media-title=Show E1",
			"--sub-file=http://127.0.0.1:1/tok/en.vtt",
			s.URL,
		}},
		{IINA, []string{
			"--no-stdin", "--keep-running",
			"--mpv-force-media-title=Show E1",
			"--mpv-sub-file=http://127.0.0.1:1/tok/en.vtt",
			s.URL,
		}},
	} {
		got, cleanup := Player{Kind: tc.kind}.args(s, subs, nil, "Show E1")
		if cleanup != nil {
			t.Errorf("%s: no skips named a chapters file", tc.kind)
			cleanup()
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s args =\n %q, want\n %q", tc.kind, got, tc.want)
		}
	}
}

// a skip range adds the chapters flag under the same prefix, and the cleanup
// removes the file it names
func TestArgsChaptersFile(t *testing.T) {
	skips := []miruro.SkipRange{{Kind: miruro.Intro, Start: 80, End: 170}}
	args, cleanup := Player{Kind: MPV}.args(miruro.Stream{URL: "u"}, nil, skips, "Show")
	if cleanup == nil {
		t.Fatal("a skip range named no chapters file")
	}
	name, ok := strings.CutPrefix(args[len(args)-2], "--chapters-file=")
	if !ok {
		t.Fatalf("args = %q, want a chapters flag before the url", args)
	}
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if want := ";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=80000\nEND=170000\ntitle=Intro\n"; !strings.HasPrefix(string(body), want) {
		t.Errorf("chapters =\n%q, want it to open with\n%q", body, want)
	}
	cleanup()
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Errorf("cleanup left the chapters file behind: %v", err)
	}
}
