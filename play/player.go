// Package play sends a resolved stream to a video player or to disk.
package play

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"

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

func (p Player) Play(ctx context.Context, s miruro.Stream, subs []miruro.Subtitle, skips []miruro.SkipRange, title string) error {
	args, cleanup := p.args(s, subs, skips, title)
	if cleanup != nil {
		defer cleanup()
	}
	cmd := exec.CommandContext(ctx, p.Bin, args...)
	if p.Detach {
		return cmd.Start()
	}
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func (p Player) args(s miruro.Stream, subs []miruro.Subtitle, skips []miruro.SkipRange, title string) ([]string, func()) {
	switch p.Kind {
	case VLC:
		args := []string{"--play-and-exit", "--meta-title", title}
		if s.Referer != "" {
			args = append(args, "--http-referrer="+s.Referer)
		}
		for _, sub := range subs {
			args = append(args, "--sub-file="+sub.File)
		}
		return append(args, s.URL), nil
	case IINA:
		args := []string{"--no-stdin", "--keep-running", "--mpv-force-media-title=" + title}
		if s.Referer != "" {
			args = append(args, "--mpv-referrer="+s.Referer)
		}
		for _, sub := range subs {
			args = append(args, "--mpv-sub-file="+sub.File)
		}
		if f, cleanup := chaptersFile(skips); f != "" {
			return append(args, "--mpv-chapters-file="+f, s.URL), cleanup
		}
		return append(args, s.URL), nil
	default:
		args := []string{"--force-media-title=" + title}
		if s.Referer != "" {
			args = append(args, "--referrer="+s.Referer)
		}
		for _, sub := range subs {
			args = append(args, "--sub-file="+sub.File)
		}
		if f, cleanup := chaptersFile(skips); f != "" {
			return append(args, "--chapters-file="+f, s.URL), cleanup
		}
		return append(args, s.URL), nil
	}
}

// chaptersFile writes an ffmetadata chapters file marking intro and outro so the
// player can jump past them, and returns a cleanup that removes it.
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
		return "", nil
	}
	fmt.Fprintln(f, ";FFMETADATA1")
	for i, m := range marks {
		end := m.at + 1
		if i+1 < len(marks) {
			end = marks[i+1].at
		}
		fmt.Fprintf(f, "[CHAPTER]\nTIMEBASE=1/1000\nSTART=%d\nEND=%d\ntitle=%s\n",
			int64(m.at*1000), int64(end*1000), m.title)
	}
	name := f.Name()
	f.Close()
	return name, func() { os.Remove(name) }
}

func (p Player) String() string { return fmt.Sprintf("%s (%s)", p.Kind, p.Bin) }
