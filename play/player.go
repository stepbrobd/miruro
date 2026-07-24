// Package play sends a resolved stream to a video player or to disk.
package play

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/log"

	"ysun.co/miruro"
)

type Kind string

const (
	MPV  Kind = "mpv"
	IINA Kind = "iina"
)

type Player struct {
	Kind Kind
	Bin  string
}

// Detect resolves a player, honouring prefer when it names a supported player
// that is installed, otherwise preferring IINA on macOS and falling back to mpv
// a stale prefer such as vlc is ignored rather than launched with mpv's flags
// it errors when nothing supported is installed, so a run fails before any
// resolution work rather than at first playback
func Detect(prefer Kind) (Player, error) {
	if prefer == MPV || prefer == IINA {
		if p, ok := lookup(prefer); ok {
			return p, nil
		}
	}
	if runtime.GOOS == "darwin" {
		if p, ok := lookup(IINA); ok {
			return p, nil
		}
	}
	if p, ok := lookup(MPV); ok {
		return p, nil
	}
	if runtime.GOOS == "darwin" {
		return Player{}, errors.New("no player found, install mpv or iina")
	}
	return Player{}, errors.New("no player found, install mpv")
}

func lookup(k Kind) (Player, bool) {
	for _, name := range binaries(k) {
		if bin, err := exec.LookPath(name); err == nil {
			return Player{Kind: k, Bin: bin}, true
		}
	}
	return Player{}, false
}

func binaries(k Kind) []string {
	if runtime.GOOS == "windows" {
		return []string{string(k) + ".exe", string(k)}
	}
	return []string{string(k)}
}

func (p Player) Play(ctx context.Context, s miruro.Stream, subs []miruro.Subtitle, skips []miruro.SkipRange, title string) error {
	args, cleanup := p.args(s, subs, skips, title)
	if cleanup != nil {
		defer cleanup()
	}
	// the terminal belongs to the control menu while the player runs, so the
	// player gets no stdin and its diagnostics feed the error instead of the tty
	// mpv logs to stdout, so both streams land in the same buffer
	var buf tail
	cmd := exec.CommandContext(ctx, p.Bin, args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	// interrupt rather than kill so mpv flushes state before exiting
	// WaitDelay still forces a kill if it ignores the signal
	// windows cannot deliver an interrupt, and the failed signal would otherwise
	// stall the whole WaitDelay before the kill lands
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return cmd.Process.Kill()
		}
		return cmd.Process.Signal(os.Interrupt)
	}
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err != nil {
		if line := buf.last(); line != "" {
			return fmt.Errorf("%w: %s", err, line)
		}
	}
	return err
}

const tailMax = 2 << 10

// tail keeps the end of the player's stderr for error reporting
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if n := len(t.b); n > tailMax {
		t.b = t.b[n-tailMax:]
	}
	return len(p), nil
}

// last is the final non-empty line, where the player states its failure
func (t *tail) last() string {
	for _, line := range slices.Backward(strings.Split(string(t.b), "\n")) {
		if l := strings.TrimSpace(line); l != "" {
			return l
		}
	}
	return ""
}

// args carries no referer flag because the proxy injects the referer upstream
// so every URL a player sees is localhost
func (p Player) args(s miruro.Stream, subs []miruro.Subtitle, skips []miruro.SkipRange, title string) ([]string, func()) {
	if p.Kind == IINA {
		args := []string{"--no-stdin", "--keep-running", "--mpv-force-media-title=" + title}
		for _, sub := range subs {
			args = append(args, "--mpv-sub-file="+sub.File)
		}
		if f, cleanup := chaptersFile(skips); f != "" {
			return append(args, "--mpv-chapters-file="+f, s.URL), cleanup
		}
		return append(args, s.URL), nil
	}
	args := []string{"--force-media-title=" + title}
	for _, sub := range subs {
		args = append(args, "--sub-file="+sub.File)
	}
	if f, cleanup := chaptersFile(skips); f != "" {
		return append(args, "--chapters-file="+f, s.URL), cleanup
	}
	return append(args, s.URL), nil
}

// chaptersFile writes an ffmetadata chapters file marking intro and outro so the
// player can jump past them, and returns a cleanup that removes it
// a write failure warns and disables skip rather than passing a broken file to
// the player
func chaptersFile(skips []miruro.SkipRange) (string, func()) {
	if len(skips) == 0 {
		return "", nil
	}

	type mark struct {
		at    float64
		title string
	}
	var marks []mark
	for _, s := range skips {
		start := "Intro"
		if s.Kind == miruro.Outro {
			start = "Outro"
		}
		marks = append(marks, mark{s.Start, start}, mark{s.End, "Episode"})
	}
	sort.Slice(marks, func(i, j int) bool { return marks[i].at < marks[j].at })

	f, err := os.CreateTemp("", "miruro-*.ffmeta")
	if err != nil {
		log.Warn("skip disabled, cannot create chapters file", "err", err)
		return "", nil
	}
	name := f.Name()

	var werr error
	write := func(format string, a ...any) {
		if werr == nil {
			_, werr = fmt.Fprintf(f, format, a...)
		}
	}
	write(";FFMETADATA1\n")
	for i, m := range marks {
		end := m.at + 1
		if i+1 < len(marks) {
			end = marks[i+1].at
		}
		write("[CHAPTER]\nTIMEBASE=1/1000\nSTART=%d\nEND=%d\ntitle=%s\n",
			int64(m.at*1000), int64(end*1000), m.title)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		log.Warn("skip disabled, cannot write chapters file", "err", werr)
		os.Remove(name)
		return "", nil
	}
	return name, func() { os.Remove(name) }
}
