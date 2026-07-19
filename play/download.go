package play

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/log"

	"ysun.co/miruro"
)

// Progress reports bytes written so far and the total when known
// total is 0 for hls where the final size is not announced ahead of time
type Progress func(done, total int64)

// Download writes the video and one sidecar per subtitle track
// cache names a directory for hls segments, so an interrupted episode resumes
// from what it already fetched, and an empty cache disables that
// it reports how many sidecars failed so the caller can summarise the run, and
// a failure is warned rather than returned because the video is the deliverable
// and an episode already on disk must not be discarded over a missing sidecar
func Download(ctx context.Context, hc *http.Client, s miruro.Stream, subs []miruro.Subtitle, dir, name, cache string, prog Progress) (int, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	name = safeName(name)
	dest := filepath.Join(dir, name+".mp4")

	switch s.Kind {
	case miruro.MP4:
		if err := grab(ctx, hc, s.URL, dest, prog); err != nil {
			return 0, err
		}
	case miruro.HLS:
		if err := hls(ctx, hc, s.URL, dest, cache, prog); err != nil {
			return 0, err
		}
	default:
		return 0, fmt.Errorf("cannot download %s stream", s.Kind)
	}

	var missed int
	for i, sub := range subs {
		side := filepath.Join(dir, fmt.Sprintf("%s.%d.%s.vtt", name, i, safeName(subLabel(sub))))
		err := grab(ctx, hc, sub.File, side, nil)
		if err == nil {
			continue
		}
		// a cancelled run is not a missing subtitle, so report it as cancellation
		if ctx.Err() != nil {
			return missed, ctx.Err()
		}
		missed++
		log.Warn("subtitle not saved", "episode", name, "label", subLabel(sub), "err", err)
	}
	return missed, nil
}

// grab streams url to dest atomically
// it writes a .part file and renames on success, so an interrupted or failed
// fetch never leaves a truncated file that looks complete
// the proxy injects the referer upstream, so none is set here
func grab(ctx context.Context, hc *http.Client, url, dest string, prog Progress) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}

	part := dest + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}

	var src io.Reader = resp.Body
	if prog != nil {
		src = &reader{r: resp.Body, total: resp.ContentLength, prog: prog}
	}
	_, err = io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, dest)
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

// hls prefers the resumable segment cache and falls back to handing the
// playlist straight to ffmpeg
// a playlist this package cannot take apart is still downloadable, it just
// starts over when interrupted
func hls(ctx context.Context, hc *http.Client, srcURL, dest, cache string, prog Progress) error {
	if cache != "" {
		err := cachedHLS(ctx, hc, srcURL, dest, cache, prog)
		if !errors.Is(err, errNoCache) {
			return err
		}
		log.Debug("playlist is not cacheable, downloading without resume", "dest", dest)
	}
	return runFFmpeg(ctx, dest, prog, "-i", srcURL)
}

// runFFmpeg remuxes the given input to dest atomically
// its stderr is captured rather than inherited because the downloads TUI owns
// the terminal, and its error output would scribble over the progress bars
// any failure is surfaced through the returned error, which the TUI shows on
// the task row
func runFFmpeg(ctx context.Context, dest string, prog Progress, input ...string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg is required to download hls streams")
	}
	// name the muxer explicitly because ffmpeg infers the output format from the
	// file extension, and the .part suffix hides the real one
	part := dest + ".part"
	args := append(append([]string{}, input...),
		"-c", "copy", "-y", "-loglevel", "error",
		"-progress", "pipe:1", "-nostats", "-f", "mp4", part)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if size, ok := strings.CutPrefix(sc.Text(), "total_size="); ok && prog != nil {
			if n, err := strconv.ParseInt(size, 10, 64); err == nil {
				prog(n, 0)
			}
		}
	}
	// a scan error only costs progress updates, so the exit status decides
	if err := cmd.Wait(); err != nil {
		os.Remove(part)
		if msg := strings.TrimSpace(errBuf.String()); msg != "" {
			return fmt.Errorf("ffmpeg: %w: %s", err, msg)
		}
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return os.Rename(part, dest)
}

func subLabel(s miruro.Subtitle) string {
	if s.Label != "" {
		return s.Label
	}
	return "sub"
}

// safeName reduces an API-supplied title or subtitle label to a single path
// component
// path separators and characters illegal on common filesystems become '-', so a
// hostile "../../x" cannot escape the download dir and a title with a slash
// cannot fail os.Create
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return '-'
		}
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '-'
		}
		return r
	}, s)
	s = strings.Trim(s, " .")
	if s == "" {
		return "untitled"
	}
	return s
}
