package play

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
// an episode already on disk is skipped whole, so sidecar subtitles are only
// fetched together with a fresh video download
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
	// dest only ever appears via a .part rename, so it is always complete
	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		if prog != nil {
			prog(fi.Size(), fi.Size())
		}
		return 0, nil
	}

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
	seen := map[string]int{}
	for _, sub := range subs {
		side := filepath.Join(dir, name+sidecar(sub, seen))
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
// a transient failure is retried, since a provider that 502s once usually
// answers the next attempt
// the proxy injects the referer upstream, so none is set here
func grab(ctx context.Context, hc *http.Client, url, dest string, prog Progress) error {
	part := dest + ".part"
	err := retry(ctx, func() error { return fetchFile(ctx, hc, url, part, prog) })
	if err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, dest)
}

// fetchFile writes one whole body to part, truncating whatever a failed earlier
// attempt left behind
func fetchFile(ctx context.Context, hc *http.Client, url, part string, prog Progress) error {
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
		return fmt.Errorf("download %s: %w", url, status(resp.StatusCode))
	}

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

// hls prefers the resumable segment cache and falls back to handing the
// playlist straight to ffmpeg
// a playlist this package cannot take apart is still downloadable, it just
// starts over when interrupted
// the finished file must carry audio, because ffmpeg keeps going when a demuxed
// master's audio rendition refuses to serve and exits zero on a silent episode
func hls(ctx context.Context, hc *http.Client, srcURL, dest, cache string, prog Progress) error {
	// fail before fetching so a missing binary cannot wipe a good cache
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("ffmpeg is required to download hls streams")
	}
	err := errNoCache
	if cache != "" {
		err = cachedHLS(ctx, hc, srcURL, dest, cache, prog)
	}
	if errors.Is(err, errNoCache) {
		if cache != "" {
			log.Debug("playlist is not cacheable, downloading without resume", "dest", dest)
		}
		err = runFFmpeg(ctx, dest, prog, "-i", srcURL)
	}
	if err != nil {
		return err
	}
	if err := audible(ctx, dest); err != nil {
		os.Remove(dest)
		return err
	}
	return nil
}

// audible refuses an episode whose audio is missing or runs far short of the
// picture, so a caller retries another stream instead of keeping a silent file
// providers serve audio as a separate hls rendition, and one that dies while
// the video keeps serving remuxes into exactly such a file with no error
// a missing ffprobe or an unreadable report skips the check rather than failing
// a download the old behaviour would have kept
func audible(ctx context.Context, dest string) error {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		log.Debug("ffprobe not installed, audio not verified", "dest", dest)
		return nil
	}
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error",
		"-show_entries", "stream=codec_type,duration", "-of", "json", dest).Output()
	if err != nil {
		log.Debug("ffprobe failed, audio not verified", "dest", dest, "err", err)
		return nil
	}
	var report struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Duration  string `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		log.Debug("ffprobe report unreadable, audio not verified", "dest", dest, "err", err)
		return nil
	}

	var video, audio float64
	heard := false
	for _, s := range report.Streams {
		d, _ := strconv.ParseFloat(s.Duration, 64)
		switch s.CodecType {
		case "video":
			video = max(video, d)
		case "audio":
			heard = true
			audio = max(audio, d)
		}
	}
	if !heard {
		return errors.New("episode has no audio")
	}
	// renditions drift by fractions of a second, a dead one falls minutes short
	if gap := video - audio; audio > 0 && gap > 5 && gap > video/10 {
		return fmt.Errorf("audio ends %.0fs before the video", gap)
	}
	return nil
}

// runFFmpeg remuxes the given input to dest atomically
// its stderr is captured rather than inherited because the downloads TUI owns
// the terminal, and its error output would scribble over the progress bars
// any failure is surfaced through the returned error, which the TUI shows on
// the task row
func runFFmpeg(ctx context.Context, dest string, prog Progress, input ...string) error {
	// ffmpeg reads a leading dash as an option, and the default download
	// directory is relative, so a title starting with one has to be named
	// absolutely to stay an output
	dest, err := filepath.Abs(dest)
	if err != nil {
		return err
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

// sidecar is the tail a subtitle takes next to the video, ".en.vtt" for an
// english track, which is the shape a player auto-loads
// seen counts the tags already used, because two tracks can name one language
// and the second must not overwrite the first
func sidecar(s miruro.Subtitle, seen map[string]int) string {
	tag := s.Lang
	if tag == "" {
		tag = s.Label
	}
	if tag == "" {
		tag = "sub"
	}
	tag = safeName(tag)
	n := seen[tag]
	seen[tag]++
	if n > 0 {
		tag = fmt.Sprintf("%s.%d", tag, n)
	}
	return "." + tag + subExt(s.File)
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
