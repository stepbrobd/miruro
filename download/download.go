// Package download saves an episode, natively for mp4 and through ffmpeg for hls.
package download

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"ysun.co/miruro/sources"
)

func Fetch(ctx context.Context, hc *http.Client, s sources.Stream, subs []sources.Subtitle, dir, name string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(dir, name+".mp4")

	switch s.Kind {
	case sources.MP4:
		if err := grab(ctx, hc, s.URL, s.Referer, dest); err != nil {
			return err
		}
	case sources.HLS:
		if err := ffmpeg(ctx, s, dest); err != nil {
			return err
		}
	default:
		return fmt.Errorf("cannot download %s stream", s.Kind)
	}

	for i, sub := range subs {
		side := filepath.Join(dir, fmt.Sprintf("%s.%d.%s.vtt", name, i, subLabel(sub)))
		if err := grab(ctx, hc, sub.File, s.Referer, side); err != nil {
			return err
		}
	}
	return nil
}

func grab(ctx context.Context, hc *http.Client, url, referer, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func ffmpeg(ctx context.Context, s sources.Stream, dest string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg is required to download hls streams")
	}
	args := []string{}
	if s.Referer != "" {
		args = append(args, "-referer", s.Referer)
	}
	args = append(args, "-i", s.URL, "-c", "copy", "-loglevel", "error", "-stats", "-y", dest)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func subLabel(s sources.Subtitle) string {
	if s.Label != "" {
		return s.Label
	}
	return "sub"
}
