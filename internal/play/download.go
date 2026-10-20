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
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/upstream"
)

// Progress reports bytes written so far and the total when known, and share,
// the part of the episode's running time written, for an hls download whose
// byte total nobody announces, zero when that is not known either
// total is 0 for hls where the final size is not announced ahead of time
type Progress func(done, total int64, share float64)

// Download writes the video and one sidecar per subtitle track
// an episode already on disk is skipped whole, since the rendition of a file on
// disk is unknown here and a soft sidecar over a hardsub doubles the
// subtitles, so a caller owing one its sidecars fetches them with Sidecars
// cache names a directory for hls segments, so an interrupted episode resumes
// from what it already fetched, and an empty cache disables that
// it reports how many sidecars failed so the caller can summarize the run, and
// a failure is warned rather than returned because the video is the deliverable
// and an episode already on disk must not be discarded over a missing sidecar
func Download(ctx context.Context, hc *http.Client, s upstream.Stream, subs []upstream.Subtitle, dir, name, cache string, prog Progress) (int, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	dest := EpisodePath(dir, name)
	// dest only ever appears via a .part rename, so it is always complete
	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		if prog != nil {
			prog(fi.Size(), fi.Size(), 0)
		}
		return 0, nil
	}

	switch s.Kind {
	case upstream.MP4:
		if err := grab(ctx, hc, s.URL, dest, 0, prog, mp4Head); err != nil {
			return 0, err
		}
	case upstream.HLS:
		if err := hls(ctx, hc, s.URL, dest, cache, prog); err != nil {
			return 0, err
		}
	default:
		return 0, fmt.Errorf("cannot download %s stream", s.Kind)
	}
	// a finished episode must carry audio whatever container it arrived in
	if err := audible(ctx, dest); err != nil {
		os.Remove(dest)
		return 0, err
	}
	return Sidecars(ctx, hc, subs, dir, name)
}

// sidecarTimeout bounds the fetch of one sidecar, its retries included, far
// past what a subtitle of a few MB takes on a slow link
// it is a variable so a test can shorten it rather than wait it out
var sidecarTimeout = 5 * time.Minute

// Sidecars writes one sidecar per subtitle track next to the named episode
// under dir, replacing any already there, and reports how many failed
// a failure is warned rather than returned, since the video is the deliverable,
// and only a canceled run is an error
func Sidecars(ctx context.Context, hc *http.Client, subs []upstream.Subtitle, dir, name string) (int, error) {
	name = safeName(name)
	var missed int
	seen := map[string]int{}
	for _, sub := range subs {
		side := filepath.Join(dir, name+sidecar(sub, seen))
		// a sidecar rides the proxy's opaque relay, so it is held to that cap
		sctx, cancel := context.WithTimeout(ctx, sidecarTimeout)
		err := grab(sctx, hc, sub.File, side, maxOpaqueBody, nil, nil)
		cancel()
		if err == nil {
			if err := asVTT(side); err != nil {
				log.Warn("subtitle kept under its upstream name", "episode", name, "label", subLabel(sub), "err", err)
			}
			continue
		}
		// a canceled run is not a missing subtitle, so report it as cancellation
		if ctx.Err() != nil {
			return missed, ctx.Err()
		}
		missed++
		log.Warn("subtitle not saved", "episode", name, "label", subLabel(sub), "err", err)
	}
	return missed, nil
}

// Saved reports whether the named episode is already on disk under dir, the
// test Download skips an episode by
func Saved(dir, name string) bool {
	fi, err := os.Stat(EpisodePath(dir, name))
	return err == nil && fi.Size() > 0
}

// EpisodePath is the file the named episode lands in under dir
func EpisodePath(dir, name string) string {
	return filepath.Join(dir, safeName(name)+".mp4")
}

// grab streams url to dest atomically
// it writes a .part file and renames on success, so an interrupted or failed
// fetch never leaves a truncated file that looks complete
// a transient failure is retried, since a provider that 502s once usually
// answers the next attempt, and a body that fails its plausibility check is
// one, since a host answering a page where a video should be is what a
// dropped connection looks like too
// limit caps the body, zero for none, and a body past it is not retried
// the proxy injects the referer upstream, so none is set here
func grab(ctx context.Context, hc *http.Client, url, dest string, limit int64, prog Progress, plausible func(head []byte) error) error {
	part := dest + ".part"
	err := retry(ctx, func() error { return fetchFile(ctx, hc, url, part, limit, prog, plausible) })
	if err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, dest)
}

// mp4Head refuses a body that does not open with the file type box every mp4
// leads with, which is what a host answering an error page with a 200 looks
// like
func mp4Head(head []byte) error {
	if len(head) < 8 || string(head[4:8]) != "ftyp" {
		return errors.New("body is not an mp4")
	}
	return nil
}

// fetchFile writes one whole body to part, truncating whatever a failed earlier
// attempt left behind
// plausible, when given, judges the head of the body once it is on disk
// limit caps the body, zero for none, and one byte past it is read so a body
// exactly at the cap is told from one over it
func fetchFile(ctx context.Context, hc *http.Client, url, part string, limit int64, prog Progress, plausible func(head []byte) error) error {
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
		// the url is the proxy's own loopback address carrying an encoded target,
		// which names nothing a reader can act on and, cut to the terminal from
		// the right, hides the status that does
		return fmt.Errorf("download refused: %w", refused(resp))
	}
	if limit > 0 && resp.ContentLength > limit {
		return fmt.Errorf("download %w of %d bytes", errTooLarge, limit)
	}

	f, err := os.Create(part)
	if err != nil {
		return err
	}

	var src io.Reader = resp.Body
	if limit > 0 {
		src = io.LimitReader(src, limit+1)
	}
	if prog != nil {
		src = &reader{r: src, total: resp.ContentLength, prog: prog}
	}
	var head bytes.Buffer
	n, err := io.Copy(f, io.TeeReader(src, limitWriter(&head, 8)))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && limit > 0 && n > limit {
		err = fmt.Errorf("download %w of %d bytes", errTooLarge, limit)
	}
	if err == nil && plausible != nil {
		err = plausible(head.Bytes())
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
	r.prog(r.done, r.total, 0)
	return n, err
}

// hls prefers the resumable segment cache and falls back to handing the
// playlist straight to ffmpeg
// a playlist this package cannot take apart is still downloadable, it just
// starts over when interrupted
func hls(ctx context.Context, hc *http.Client, srcURL, dest, cache string, prog Progress) error {
	// fail before fetching so a missing binary cannot wipe a good cache
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("ffmpeg is required to download hls streams")
	}
	// cachedHLS refuses an empty cache root itself, so nothing is guarded here
	err := cachedHLS(ctx, hc, srcURL, dest, cache, prog)
	if errors.Is(err, errNoCache) {
		if cache != "" {
			log.Debug("playlist is not cacheable, downloading without resume", "dest", dest)
		}
		err = runFFmpeg(ctx, dest, prog, "-i", srcURL)
	}
	return err
}

// audible refuses an episode whose audio is missing or runs far short of the
// picture, so a caller retries another stream instead of keeping a silent file
// providers serve audio as a separate hls rendition, and one that dies while
// the video keeps serving remuxes into exactly such a file with no error, and
// ffmpeg keeps going when a demuxed master's audio rendition refuses to serve
// and exits zero on the result
// a missing ffprobe or an unreadable report skips the check rather than failing
// a download the old behavior would have kept
func audible(ctx context.Context, dest string) error {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		log.Debug("ffprobe not installed, audio not verified", "dest", dest)
		return nil
	}
	// a relative name starting with a dash reads as an option, and a probe that
	// fails skips the check, so it is named absolutely as runFFmpeg names it
	abs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error",
		"-show_entries", "stream=codec_type,duration", "-of", "json", abs).Output()
	if err != nil {
		log.Debug("ffprobe failed, audio not verified", "dest", dest, "err", err)
		return nil
	}
	var report struct {
		Streams []probed `json:"streams"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		log.Debug("ffprobe report unreadable, audio not verified", "dest", dest, "err", err)
		return nil
	}
	return silence(report.Streams)
}

// probed is one stream of ffprobe's report
type probed struct {
	CodecType string `json:"codec_type"`
	// Duration is seconds, or N/A for a container that times its streams
	// nowhere ffprobe reads per stream
	Duration string `json:"duration"`
}

// silence says why the streams ffprobe reported make no audible episode, nil
// when they do
// an audio stream reported with no duration is heard and not measured, since
// measuring it as zero would refuse every such file as falling the whole
// episode short
func silence(streams []probed) error {
	var video, audio float64
	heard := false
	for _, s := range streams {
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
	// the log runs at warning level, tagged, since a segment the hls demuxer
	// skips is only a warning, see ffLog
	part := dest + ".part"
	args := append(append([]string{}, input...),
		"-c", "copy", "-y", "-loglevel", "level+warning",
		"-progress", "pipe:1", "-nostats", "-f", "mp4", part)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	var stderr ffLog
	cmd.Stderr = &stderr

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
				prog(n, 0, 0)
			}
		}
	}
	// a scan error only costs progress updates, so the exit status decides
	err = cmd.Wait()
	stderr.flush()
	if err != nil {
		os.Remove(part)
		if msg := strings.TrimSpace(stderr.errs.String()); msg != "" {
			return fmt.Errorf("ffmpeg: %w: %s", err, msg)
		}
		return fmt.Errorf("ffmpeg: %w", err)
	}
	if stderr.skipped != "" {
		os.Remove(part)
		return fmt.Errorf("ffmpeg: %s", stderr.skipped)
	}
	return os.Rename(part, dest)
}

// ffLog reads ffmpeg's log, each line tagged with its level, for what a run is
// judged by
// errs holds the errors, as -loglevel error alone would have printed them,
// which a failed run reports
// skipped is the first segment the hls demuxer could not open, which it logs as
// a warning before skipping the segment and exiting zero on an episode missing
// it, empty when none was
// exec copies the log in one goroutine and Wait joins it before either field
// is read, so the fields need no lock
type ffLog struct {
	line    []byte
	level   string
	errs    bytes.Buffer
	skipped string
}

// maxLogLine and maxLogErrs bound what the log keeps, since every packet of a
// broken stream can earn its own line
// a line is judged by its tag and the start of its message, so the tail past
// maxLogLine costs nothing but the end of a long error
const (
	maxLogLine = 4 << 10
	maxLogErrs = 64 << 10
)

// Write always reports the whole of p, since a short write would end the copy
// and leave ffmpeg blocked on a full pipe
func (l *ffLog) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			l.keep(p)
			break
		}
		l.keep(p[:i])
		l.end()
		p = p[i+1:]
	}
	return n, nil
}

// keep adds b to the line being read, dropping what runs past maxLogLine
func (l *ffLog) keep(b []byte) {
	if room := maxLogLine - len(l.line); room < len(b) {
		b = b[:max(room, 0)]
	}
	l.line = append(l.line, b...)
}

// flush judges a last line the log left unterminated
func (l *ffLog) flush() {
	if len(l.line) > 0 {
		l.end()
	}
}

// logLevels are the tags a line logged at warning level or above carries
var logLevels = []string{"[warning] ", "[error] ", "[fatal] ", "[panic] "}

// end judges the line just read, keeping it as -loglevel error would have
// printed it, without its tag, unless it is a warning
// the tag follows the context the line names, so the first one on the line is
// its level and a later one is the message quoting it
// a line carrying no tag continues the message before it, whose level it takes
func (l *ffLog) end() {
	line := string(l.line)
	l.line = l.line[:0]
	at, tag := -1, ""
	for _, t := range logLevels {
		if i := strings.Index(line, t); i >= 0 && (at < 0 || i < at) {
			at, tag = i, t
		}
	}
	if at >= 0 {
		msg := line[at+len(tag):]
		l.level, line = tag, line[:at]+msg
		if l.skipped == "" && strings.HasPrefix(msg, "Failed to open segment") {
			l.skipped = msg
		}
	}
	if l.level == "[warning] " || l.errs.Len() >= maxLogErrs {
		return
	}
	l.errs.WriteString(line)
	l.errs.WriteByte('\n')
}

func subLabel(s upstream.Subtitle) string {
	if s.Label != "" {
		return s.Label
	}
	return "sub"
}

// asVTT names a sidecar holding WebVTT .vtt whatever the upstream called it
// hop's older encodes list .srt files that carry WebVTT, under a format the
// api declares as srt too, and a player trusting the extension reads them as
// SubRip and shows nothing
// each track's tag is its own, so the new name cannot be another track's
func asVTT(path string) error {
	ext := filepath.Ext(path)
	if ext == ".vtt" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	f.Close()
	if !bytes.HasPrefix(bytes.TrimPrefix(head[:n], []byte("\xef\xbb\xbf")), []byte("WEBVTT")) {
		return nil
	}
	return os.Rename(path, strings.TrimSuffix(path, ext)+".vtt")
}

// sidecar is the tail a subtitle takes next to the video, ".en.vtt" for an
// english track, which is the shape a player auto-loads
// seen counts the tags already used, because two tracks can name one language
// and the second must not overwrite the first
func sidecar(s upstream.Subtitle, seen map[string]int) string {
	tag := s.Lang
	if tag == "" {
		tag = s.Label
	}
	if tag == "" {
		tag = "sub"
	}
	tag = clip(safeName(tag), tagBytes)
	n := seen[tag]
	seen[tag]++
	if n > 0 {
		tag = fmt.Sprintf("%s.%d", tag, n)
	}
	return "." + tag + subExt(s.File)
}

// tagBytes bounds the tag a sidecar takes from a label when the track names no
// language, since a label is free text and the file name has 255 bytes on most
// filesystems for the episode's name as well
const tagBytes = 32

// clip cuts s to at most n bytes without splitting a character
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " .")
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
