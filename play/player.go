// Package play sends a resolved stream to a video player or to disk.
package play

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"ysun.co/miruro"
)

type Kind string

const (
	MPV  Kind = "mpv"
	VLC  Kind = "vlc"
	IINA Kind = "iina"
)

type Player struct {
	Kind   Kind
	Bin    string
	Detach bool
}

// Detect resolves a player, honouring prefer when given and installed,
// otherwise choosing the platform default and falling back through mpv then vlc.
func Detect(prefer Kind) Player {
	if prefer != "" {
		if p, ok := lookup(prefer); ok {
			return p
		}
	}
	if runtime.GOOS == "darwin" {
		if p, ok := lookup(IINA); ok {
			return p
		}
	}
	for _, k := range []Kind{MPV, VLC} {
		if p, ok := lookup(k); ok {
			return p
		}
	}
	return Player{Kind: MPV, Bin: string(MPV)}
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

func (p Player) Play(ctx context.Context, s miruro.Stream, subs []miruro.Subtitle, title string) error {
	cmd := exec.CommandContext(ctx, p.Bin, p.args(s, subs, title)...)
	if p.Detach {
		return cmd.Start()
	}
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func (p Player) args(s miruro.Stream, subs []miruro.Subtitle, title string) []string {
	switch p.Kind {
	case VLC:
		args := []string{"--play-and-exit", "--meta-title", title}
		if s.Referer != "" {
			args = append(args, "--http-referrer="+s.Referer)
		}
		for _, sub := range subs {
			args = append(args, "--sub-file="+sub.File)
		}
		return append(args, s.URL)
	case IINA:
		args := []string{"--no-stdin", "--keep-running", "--mpv-force-media-title=" + title}
		if s.Referer != "" {
			args = append(args, "--mpv-referrer="+s.Referer)
		}
		for _, sub := range subs {
			args = append(args, "--mpv-sub-file="+sub.File)
		}
		return append(args, s.URL)
	default:
		args := []string{"--force-media-title=" + title}
		if s.Referer != "" {
			args = append(args, "--referrer="+s.Referer)
		}
		for _, sub := range subs {
			args = append(args, "--sub-file="+sub.File)
		}
		return append(args, s.URL)
	}
}

func (p Player) String() string { return fmt.Sprintf("%s (%s)", p.Kind, p.Bin) }
