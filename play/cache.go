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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/log"
)

// errNoCache marks a playlist this package cannot take apart safely, so the
// caller hands the original URL to ffmpeg in one shot and gives up resuming
var errNoCache = errors.New("playlist not cacheable")

// segWorkers bounds concurrent segment fetches for one episode
// episodes already download in parallel, so this multiplies with --parallel and
// stays deliberately small
const segWorkers = 4

// aesKeyLen is the one key size AES-128 media streams use, per RFC 8216
const aesKeyLen = 16

// maxTextBody caps a playlist or key body
// real playlists top out around single-digit MB, so only a hostile or broken
// upstream hits this
const maxTextBody = 16 << 20

// maxSegments caps how many cache files one playlist can mint
// a hostile playlist with millions of entries would mint millions of cache
// files while ffmpeg can still stream it without caching
// ten thousand four-second segments is over eleven hours, no real episode
// comes close
const maxSegments = 10000

var (
	bandwidthAttr = regexp.MustCompile(`BANDWIDTH=(\d+)`)
	durationAttr  = regexp.MustCompile(`^#EXTINF:\s*([0-9.]+)`)
)

// playlist is a media playlist split into the lines to reproduce and the
// segments to fetch
// line indices are kept so the local copy preserves every tag verbatim, which
// is what makes EXT-X-KEY and EXT-X-MEDIA-SEQUENCE still describe the segments
type mediaPlaylist struct {
	lines     []string
	segAt     []int
	durations []float64
	keyAt     int
	keyURI    string
	// encrypted means an EXT-X-KEY with a method other than NONE applies to
	// the segments, even when the key is inline or not fetchable
	encrypted bool
}

// manifest records what the cache directory holds
// segment URLs are deliberately not part of the identity because a signed CDN
// URL rotates between runs while the content does not
type manifest struct {
	Count     int       `json:"count"`
	Durations []float64 `json:"durations"`
	Updated   time.Time `json:"updated"`
}

// cachedHLS downloads each segment into dir and then remuxes from disk
// a segment is renamed into place only once it is whole, so an interrupted run
// resumes with just the segments it still lacks
func cachedHLS(ctx context.Context, hc *http.Client, srcURL, dest, dir string, prog Progress) error {
	pl, err := resolvePlaylist(ctx, hc, srcURL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := reconcile(dir, pl); err != nil {
		return err
	}

	key, err := cacheKey(ctx, hc, pl, dir)
	if err != nil {
		return err
	}
	if err := fetchSegments(ctx, hc, pl, dir, prog); err != nil {
		return err
	}

	local := filepath.Join(dir, "local.m3u8")
	if err := os.WriteFile(local, []byte(pl.localise(dir, key)), 0o644); err != nil {
		return err
	}
	// the remux reports its own output size, which would send the bar backwards
	// after the fetch already counted every segment
	if err := remux(ctx, local, dest, nil); err != nil {
		// a remux that failed with the context still live means the cached
		// bytes themselves are bad, and replaying them can only fail again
		// a cancelled remux keeps the cache so the interrupted run resumes
		if ctx.Err() == nil {
			if werr := wipe(dir); werr != nil {
				log.Warn("bad cache not removed", "dir", dir, "err", werr)
			}
		}
		return err
	}
	// the episode is on disk, so the segments are dead weight
	// failing to drop them wastes space but does not undo a finished download
	if err := os.RemoveAll(dir); err != nil {
		log.Warn("cached segments not removed", "dir", dir, "err", err)
	}
	return nil
}

// resolvePlaylist fetches srcURL and follows a master playlist one level down
// to the highest bandwidth variant
func resolvePlaylist(ctx context.Context, hc *http.Client, srcURL string) (*mediaPlaylist, error) {
	body, err := fetchText(ctx, hc, srcURL)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(body, []byte("#EXT-X-STREAM-INF")) {
		// a rendition group carries audio or subtitles in their own playlists, and
		// following only the video variant would silently drop them
		if bytes.Contains(body, []byte("#EXT-X-MEDIA")) {
			return nil, errNoCache
		}
		variant, err := bestVariant(body, srcURL)
		if err != nil {
			return nil, err
		}
		if body, err = fetchText(ctx, hc, variant); err != nil {
			return nil, err
		}
	}
	// a byterange playlist addresses slices of one resource, and an init segment
	// is a separate resource every segment depends on
	// neither is reproducible by caching whole segment files alone
	for _, tag := range []string{"#EXT-X-BYTERANGE", "#EXT-X-MAP"} {
		if bytes.Contains(body, []byte(tag)) {
			return nil, errNoCache
		}
	}
	// a playlist without an endlist is still growing, and a snapshot of it
	// would remux into a finished-looking partial episode
	// ffmpeg follows a live playlist as it grows, so the fallback stays whole
	if !bytes.Contains(body, []byte("#EXT-X-ENDLIST")) {
		return nil, errNoCache
	}
	return parsePlaylist(body, srcURL)
}

func fetchText(ctx context.Context, hc *http.Client, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("playlist %s: status %d", rawURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTextBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxTextBody {
		return nil, fmt.Errorf("playlist %s: body exceeds %d bytes", rawURL, maxTextBody)
	}
	return body, nil
}

// bestVariant picks the highest bandwidth rendition of a master playlist
func bestVariant(body []byte, base string) (string, error) {
	var (
		best     string
		bestRate int64 = -1
		rate     int64 = -1
	)
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
			rate = -1
			if m := bandwidthAttr.FindStringSubmatch(line); m != nil {
				rate, _ = strconv.ParseInt(m[1], 10, 64)
			}
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		default:
			if rate > bestRate {
				bestRate, best = rate, line
			}
			rate = -1
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if best == "" {
		return "", errNoCache
	}
	return absolute(best, base)
}

func absolute(ref, base string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}

func parsePlaylist(body []byte, base string) (*mediaPlaylist, error) {
	pl := &mediaPlaylist{keyAt: -1}
	var pending float64 = -1

	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "#EXT-X-KEY"):
			if strings.Contains(trimmed, "METHOD=NONE") {
				break
			}
			// any other method leaves the segments ciphertext, even when the
			// key line carries no fetchable URI, so mark that before any of
			// the URI checks below can bail out
			pl.encrypted = true
			m := uriAttr.FindStringSubmatch(trimmed)
			if m == nil {
				break
			}
			// only one key is rewritten to its cached copy, so a playlist that
			// rotates keys would leave the earlier lines pointing upstream
			if pl.keyAt >= 0 {
				return nil, errNoCache
			}
			// a data URI carries the key inline and needs no fetch, so it is left
			// exactly as it stands
			if !strings.HasPrefix(m[1], "http://") && !strings.HasPrefix(m[1], "https://") {
				break
			}
			pl.keyAt = len(pl.lines)
			var err error
			if pl.keyURI, err = absolute(m[1], base); err != nil {
				return nil, err
			}
		case strings.HasPrefix(trimmed, "#EXTINF"):
			if m := durationAttr.FindStringSubmatch(trimmed); m != nil {
				pending, _ = strconv.ParseFloat(m[1], 64)
			}
		case strings.HasPrefix(trimmed, "#"):
		default:
			abs, err := absolute(trimmed, base)
			if err != nil {
				return nil, err
			}
			pl.segAt = append(pl.segAt, len(pl.lines))
			if len(pl.segAt) > maxSegments {
				return nil, errNoCache
			}
			pl.durations = append(pl.durations, pending)
			pending = -1
			line = abs
		}
		pl.lines = append(pl.lines, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(pl.segAt) == 0 {
		return nil, errNoCache
	}
	return pl, nil
}

// segName is the cached file for segment i
// the index is the identity because the playlist order is what the remux
// replays, and a signed URL is not stable across runs
func segName(i int) string { return fmt.Sprintf("%05d.ts", i) }

// localise renders the playlist against the cache directory
// every tag is reproduced untouched so EXT-X-MEDIA-SEQUENCE still lines up with
// the segments, which is what the AES-128 IV derivation depends on
func (p *mediaPlaylist) localise(dir, key string) string {
	lines := make([]string, len(p.lines))
	copy(lines, p.lines)
	for n, at := range p.segAt {
		lines[at] = filepath.Join(dir, segName(n))
	}
	if key != "" && p.keyAt >= 0 {
		lines[p.keyAt] = uriAttr.ReplaceAllString(lines[p.keyAt], `URI="`+key+`"`)
	}
	return strings.Join(lines, "\n") + "\n"
}

// reconcile drops a cache directory describing different content
// the caller keys the directory by title, episode, provider and quality, so a
// mismatch here means the provider re-encoded rather than that the URL rotated
func reconcile(dir string, pl *mediaPlaylist) error {
	want := manifest{Count: len(pl.segAt), Durations: pl.durations, Updated: time.Now()}
	path := filepath.Join(dir, "manifest.json")

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var have manifest
		if json.Unmarshal(data, &have) == nil && have.matches(want) {
			break
		}
		// the cached segments describe something else, so none of them can be
		// trusted for this remux
		if err := wipe(dir); err != nil {
			return err
		}
	case os.IsNotExist(err):
	default:
		return err
	}

	out, err := json.Marshal(want)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func (m manifest) matches(other manifest) bool {
	if m.Count != other.Count || len(m.Durations) != len(other.Durations) {
		return false
	}
	for i, d := range m.Durations {
		// an unknown duration leaves only the segment count to tell two playlists
		// apart, which is too weak to risk splicing one episode into another
		if d < 0 || other.Durations[i] < 0 {
			return false
		}
		// durations are printed with limited precision upstream, so compare with
		// a tolerance rather than for equality
		if diff := d - other.Durations[i]; diff > 0.001 || diff < -0.001 {
			return false
		}
	}
	return true
}

// wipe removes cached segments while leaving the directory in place
func wipe(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// cacheKey stores the decryption key next to the segments and returns its path
// caching it matters because the segments are ciphertext, and a key URL that
// expires before the resume would otherwise leave them undecryptable
func cacheKey(ctx context.Context, hc *http.Client, pl *mediaPlaylist, dir string) (string, error) {
	if pl.keyURI == "" {
		return "", nil
	}
	path := filepath.Join(dir, "key.bin")
	// only a key of exactly the AES-128 size is trusted on resume, anything
	// else is a cached error body and gets refetched
	if fi, err := os.Stat(path); err == nil && fi.Size() == aesKeyLen {
		return path, nil
	}
	body, err := fetchText(ctx, hc, pl.keyURI)
	if err != nil {
		return "", err
	}
	if len(body) != aesKeyLen {
		return "", fmt.Errorf("key %s is %d bytes, want %d", pl.keyURI, len(body), aesKeyLen)
	}
	part := path + ".part"
	if err := os.WriteFile(part, body, 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(part, path)
}

func fetchSegments(ctx context.Context, hc *http.Client, pl *mediaPlaylist, dir string, prog Progress) error {
	var (
		mu    sync.Mutex
		done  int64
		errs  []error
		sem   = make(chan struct{}, segWorkers)
		wg    sync.WaitGroup
		fatal = make(chan struct{})
	)

	// already cached bytes count toward progress so a resumed run does not
	// restart its bar from zero
	for n := range pl.segAt {
		if fi, err := os.Stat(filepath.Join(dir, segName(n))); err == nil {
			done += fi.Size()
		}
	}
	if prog != nil {
		prog(done, 0)
	}

	for n, at := range pl.segAt {
		select {
		case <-fatal:
		case <-ctx.Done():
		default:
			sem <- struct{}{}
			wg.Add(1)
			go func(n int, src string) {
				defer wg.Done()
				defer func() { <-sem }()

				written, err := fetchSegment(ctx, hc, src, filepath.Join(dir, segName(n)), !pl.encrypted)
				mu.Lock()
				if err != nil {
					errs = append(errs, fmt.Errorf("segment %d: %w", n, err))
					// one dead segment makes the remux incomplete, so stop early
					// rather than fetch the rest
					select {
					case <-fatal:
					default:
						close(fatal)
					}
					mu.Unlock()
					return
				}
				done += written
				d := done
				mu.Unlock()
				// the progress callback can block on UI delivery, so it must
				// never run under mu
				if prog != nil {
					prog(d, 0)
				}
			}(n, pl.lines[at])
			continue
		}
		break
	}
	wg.Wait()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// fetchSegment writes one segment atomically and reports the bytes it added
// an already cached segment is left alone, which is what makes a rerun resume
// a body is only renamed into place once it is whole and plausibly media, since
// a cached error page would otherwise remux into a silently truncated episode
func fetchSegment(ctx context.Context, hc *http.Client, src, dest string, plain bool) (int64, error) {
	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return 0, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}

	part := dest + ".part"
	f, err := os.Create(part)
	if err != nil {
		return 0, err
	}
	var head bytes.Buffer
	n, err := io.Copy(f, io.TeeReader(resp.Body, limitWriter(&head, tsPacket*syncRun)))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = plausibleSegment(head.Bytes(), n, resp.ContentLength, plain)
	}
	if err != nil {
		os.Remove(part)
		return 0, err
	}
	return n, os.Rename(part, dest)
}

// plausibleSegment rejects a body that cannot be a whole segment
// ciphertext cannot be inspected, so an encrypted stream is only checked for
// truncation against the announced length
func plausibleSegment(head []byte, n, announced int64, plain bool) error {
	if announced >= 0 && n != announced {
		return fmt.Errorf("short segment, got %d of %d bytes", n, announced)
	}
	if n == 0 {
		return errors.New("empty segment")
	}
	if plain && !looksTS(head) {
		return errors.New("segment is not a transport stream")
	}
	return nil
}

// looksTS reports whether data carries aligned transport stream sync bytes
// the proxy strips any decoy prefix, so a real segment starts on a sync byte
func looksTS(data []byte) bool {
	if len(data) == 0 || data[0] != 0x47 {
		return false
	}
	for i := tsPacket; i < len(data) && i < tsPacket*syncRun; i += tsPacket {
		if data[i] != 0x47 {
			return false
		}
	}
	return true
}

// limitWriter keeps only the first n bytes, so a segment can be inspected
// without holding it in memory
func limitWriter(w io.Writer, n int) io.Writer { return &capped{w: w, left: n} }

type capped struct {
	w    io.Writer
	left int
}

// Write always reports the full length because io.TeeReader treats a short
// write as an error, and this writer drops the tail on purpose
func (c *capped) Write(p []byte) (int, error) {
	full := len(p)
	if c.left <= 0 {
		return full, nil
	}
	if len(p) > c.left {
		p = p[:c.left]
	}
	n, err := c.w.Write(p)
	c.left -= n
	return full, err
}

// remux concatenates the cached segments into dest
// the protocol whitelist is named because ffmpeg refuses file and crypto access
// from a playlist unless asked, and both are exactly what a local cache needs
func remux(ctx context.Context, local, dest string, prog Progress) error {
	return runFFmpeg(ctx, dest, prog,
		"-protocol_whitelist", "file,crypto,data",
		"-allowed_extensions", "ALL",
		"-i", local)
}
