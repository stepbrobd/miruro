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
