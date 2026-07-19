package play

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"ysun.co/miruro"
)

// Progress reports bytes written so far and the total when known, total is 0
// for hls where the final size is not announced ahead of time.
type Progress func(done, total int64)

func Download(ctx context.Context, hc *http.Client, s miruro.Stream, subs []miruro.Subtitle, dir, name string, prog Progress) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(dir, name+".mp4")

	switch s.Kind {
	case miruro.MP4:
		if err := grab(ctx, hc, s.URL, s.Referer, dest, prog); err != nil {
			return err
		}
	case miruro.HLS:
		if err := ffmpeg(ctx, s, dest, prog); err != nil {
			return err
		}
	default:
		return fmt.Errorf("cannot download %s stream", s.Kind)
	}

	for i, sub := range subs {
		side := filepath.Join(dir, fmt.Sprintf("%s.%d.%s.vtt", name, i, subLabel(sub)))
		if err := grab(ctx, hc, sub.File, s.Referer, side, nil); err != nil {
			return err
		}
	}
	return nil
}

func grab(ctx context.Context, hc *http.Client, url, referer, dest string, prog Progress) error {
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

	var src io.Reader = resp.Body
	if prog != nil {
		src = &reader{r: resp.Body, total: resp.ContentLength, prog: prog}
	}
	_, err = io.Copy(f, src)
	return err
}

type reader struct {
	r     io.Reader
	done  int64
	total int64
	prog  Progress
}

func (r *reader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.done += int64(n)
	r.prog(r.done, r.total)
	return n, err
}

func ffmpeg(ctx context.Context, s miruro.Stream, dest string, prog Progress) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg is required to download hls streams")
	}
	args := []string{}
	if s.Referer != "" {
		args = append(args, "-referer", s.Referer)
	}
	args = append(args, "-i", s.URL, "-c", "copy", "-y", "-loglevel", "error", "-progress", "pipe:1", "-nostats", dest)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = os.Stderr

	if prog == nil {
		cmd.Stdout = os.Stderr
		return cmd.Run()
	}

	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if size, ok := strings.CutPrefix(sc.Text(), "total_size="); ok {
			if n, err := strconv.ParseInt(size, 10, 64); err == nil {
				prog(n, 0)
			}
		}
	}
	return cmd.Wait()
}

func subLabel(s miruro.Subtitle) string {
	if s.Label != "" {
		return s.Label
	}
	return "sub"
}
