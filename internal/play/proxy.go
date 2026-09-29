package play

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/upstream"
)

const (
	tsPacket = 188
	syncRun  = 8
	// scanHead bounds how far into a body the decoy scan looks
	// generous enough to clear a real image prefix while keeping the worst-case
	// scan of a sync-free body cheap
	scanHead = 256 * 1024
	// real playlists top out around single-digit MB and real segments around
	// tens of MB, so only a hostile or broken upstream hits these caps
	maxPlaylistBody = 16 << 20
	maxSegmentBody  = 256 << 20
	// bufferedTimeout bounds one whole buffered fetch, generous enough for the
	// largest real segment on a slow link
	bufferedTimeout = 5 * time.Minute
)

var (
	errToken = errors.New("bad token")
	// errTarget marks an upstream url the relay cannot address
	errTarget = errors.New("bad target")
)

// kind selects how the proxy treats an upstream body
type kind string

const (
	playlist kind = "playlist"
	segment  kind = "segment" // TS that may carry a decoy prefix
	cipher   kind = "cipher"  // encrypted TS that must reach the player unchanged
	media    kind = "media"   // a whole video body such as an mp4
	opaque   kind = "opaque"  // byte relay that forwards a range
)

// relayed reports whether a body passes through untouched, which is what makes
// it safe to forward a Range and wrong to bound with a buffered deadline
func (k kind) relayed() bool { return k == media || k == opaque }

// picture reports whether a body is part of the video the player was handed
// an aes key and a subtitle sidecar ride the opaque path too, so counting them
// would make Served say the stream played when only its sidecar loaded
func (k kind) picture() bool { return k == segment || k == cipher || k == media }

// suffix keeps a real extension on the path because ffmpeg's hls demuxer rejects
// segments whose extension it does not recognize
// base64url has no '.', so stripping the suffix back off is unambiguous
func (k kind) suffix() string {
	switch k {
	case playlist:
		return ".m3u8"
	case segment, cipher:
		return ".ts"
	}
	return ""
}

type target struct {
	URL     string `json:"u"`
	Referer string `json:"r"`
	Kind    kind   `json:"k"`
	// Height restricts a master playlist to the variants of one picture height
	Height int `json:"h,omitempty"`
	// Tally names the stream a body counts against, zero for one nothing counts
	// every child a playlist names carries its parent's
	Tally uint64 `json:"t,omitempty"`
	// Audio marks a body of an audio rendition a master names apart from its
	// video, which every child of that rendition's playlist inherits
	Audio bool `json:"a,omitempty"`
	// Lang is the language a master's audio renditions default to, the one
	// the provider means, empty to leave the master's own default
	Lang string `json:"l,omitempty"`
}

// Proxy relays provider streams over localhost, so a player sees plain HTTP/1.1
// while the upstream fetch keeps HTTP/2, the referer, and redirect handling
type Proxy struct {
	srv   *http.Server
	hc    *http.Client
	token string
	base  string
	// tallies holds every Tally this proxy handed out, by id
	tallies sync.Map
	ids     atomic.Uint64
	// timeout bounds one buffered fetch, zero disables the bound
	timeout time.Duration
	done    chan struct{}
	once    sync.Once
}

// Tally counts what one stream relayed and was refused
// a player that exits with an error without a body served never started, which
// is what tells a dead stream from one the user quit, and ffmpeg's hls demuxer
// skips a segment it cannot fetch and asks for the next, so a stream whose CDN
// refuses every segment never ends and never shows a frame, and the refusals
// are the only sign of it
// each stream gets its own, so a handler still running for a stream the player
// just left counts against that stream and moves no other one's baseline
type Tally struct {
	px      *Proxy
	id      uint64
	served  atomic.Int64
	refused atomic.Int64
	// audio counts the audio rendition playlists relayed, and heard the audio
	// segments among the served bodies, what tells a demuxed stream playing
	// silent from one playing with sound
	audio atomic.Int64
	heard atomic.Int64
}

// Tally starts counting a stream
// it lives as long as the proxy, which a run holds for a handful of streams
func (p *Proxy) Tally() *Tally {
	t := &Tally{px: p, id: p.ids.Add(1)}
	p.tallies.Store(t.id, t)
	return t
}

// Served counts the media bodies relayed for the stream
func (t *Tally) Served() int { return int(t.served.Load()) }

// Refused counts the media bodies the upstream would not give up
func (t *Tally) Refused() int { return int(t.refused.Load()) }

// Silent reports a stream whose player took an audio rendition's playlist and
// got none of its segments, which plays the picture without sound
// a master carrying its audio inside the video variants names no rendition,
// so it is never silent by this measure
func (t *Tally) Silent() bool { return t.audio.Load() > 0 && t.heard.Load() == 0 }

// URL is the localhost address a player should open for s, counted here
func (t *Tally) URL(s upstream.Stream) string { return t.px.url(s, t.id) }

// Stream addresses s through the proxy, counted here
func (t *Tally) Stream(s upstream.Stream) upstream.Stream {
	s.URL, s.Referer = t.URL(s), ""
	return s
}

// StartProxy binds a relay on an ephemeral localhost port
// it serves until ctx is canceled or Close is called
// hc fetches every upstream body, and a run hands it upstream.Public so a
// provider cannot point the relay at this machine or its network
func StartProxy(ctx context.Context, hc *http.Client) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		ln.Close()
		return nil, err
	}

	p := &Proxy{
		hc:      hc,
		token:   hex.EncodeToString(tok),
		timeout: bufferedTimeout,
		done:    make(chan struct{}),
	}
	p.base = "http://" + ln.Addr().String() + "/" + p.token

	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handle)
	p.srv = &http.Server{Handler: mux}

	go p.srv.Serve(ln)
	go func() {
		select {
		case <-ctx.Done():
			p.srv.Close()
		case <-p.done:
		}
	}()
	return p, nil
}

func (p *Proxy) Close() error {
	p.once.Do(func() { close(p.done) })
	return p.srv.Close()
}

// URL returns the localhost address a player or ffmpeg should open for s,
// counted nowhere
func (p *Proxy) URL(s upstream.Stream) string { return p.url(s, 0) }

func (p *Proxy) url(s upstream.Stream, tally uint64) string {
	if s.Kind == upstream.HLS {
		return p.encode(target{URL: s.URL, Referer: s.Referer, Kind: playlist, Height: s.Height, Tally: tally, Lang: s.AudioLang})
	}
	return p.encode(target{URL: s.URL, Referer: s.Referer, Kind: media, Tally: tally})
}

// Opaque returns a localhost address relaying rawURL byte for byte
func (p *Proxy) Opaque(rawURL, referer string) string {
	return p.proxied(rawURL, referer, opaque)
}

// Subtitles addresses each sidecar through the proxy under a file name a player
// can show
// subtitles carry no referer of their own, so they inherit the video stream's
func (p *Proxy) Subtitles(subs []upstream.Subtitle, referer string) []upstream.Subtitle {
	out := make([]upstream.Subtitle, len(subs))
	for i, s := range subs {
		out[i] = s
		out[i].File = p.named(s.File, referer, subName(s))
	}
	return out
}

// named is an opaque relay carrying a readable trailing component
// mpv titles an external track from the last path component of its url, so
// without one every subtitle reads as the base64 payload
func (p *Proxy) named(rawURL, referer, name string) string {
	return p.Opaque(rawURL, referer) + "/" + url.PathEscape(name)
}

// subName is the file name a player shows for an external subtitle track
// the label names the track and the language tag is the fallback, and the
// extension is carried over so a player picks the right parser
// sun labels its tracks as file names, English.vtt, so a label already ending
// in the extension keeps it once rather than twice
func subName(s upstream.Subtitle) string {
	name := s.Label
	if name == "" {
		name = s.Lang
	}
	if name == "" {
		name = "subtitle"
	}
	ext := subExt(s.File)
	if strings.HasSuffix(strings.ToLower(name), ext) {
		name = name[:len(name)-len(ext)]
	}
	return safeName(name) + ext
}

// subExt is the upstream subtitle extension
// the result becomes a file name a player parses, so an extension outside the
// known subtitle formats is replaced rather than passed on
func subExt(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ".vtt"
	}
	switch ext := strings.ToLower(path.Ext(u.Path)); ext {
	case ".vtt", ".srt", ".ass", ".ssa", ".sub":
		return ext
	}
	return ".vtt"
}

// Stream addresses s through the proxy
// the referer is cleared because the proxy sends it upstream itself
func (p *Proxy) Stream(s upstream.Stream) upstream.Stream {
	s.URL = p.URL(s)
	s.Referer = ""
	return s
}

func (p *Proxy) proxied(rawURL, referer string, k kind) string {
	return p.encode(target{URL: rawURL, Referer: referer, Kind: k})
}

func (p *Proxy) encode(t target) string {
	b, _ := json.Marshal(t)
	return p.base + "/" + base64.RawURLEncoding.EncodeToString(b) + t.Kind.suffix()
}

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	t, err := p.decode(r.URL.Path)
	switch {
	case errors.Is(err, errToken):
		refuse(w, t, http.StatusForbidden, err)
		return
	case err != nil:
		refuse(w, t, http.StatusBadRequest, fmt.Errorf("%w: %v", errTarget, err))
		return
	}

	// buffered kinds read the body whole, so a stalled upstream must trip a
	// deadline rather than wedge the player or a download worker
	// an opaque relay may stream a long video legitimately and stays unbounded
	ctx := r.Context()
	if !t.Kind.relayed() && p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	resp, err := p.fetch(ctx, r, t)
	if err != nil {
		p.count(t, false)
		refuse(w, t, mirrored(err), err)
		return
	}
	defer resp.Body.Close()

	// a buffered body is announced with its length, so a reader that checks
	// what it got against what was announced can do so through the proxy
	switch t.Kind {
	case playlist:
		body, err := buffered(resp, maxPlaylistBody)
		if err != nil {
			refuse(w, t, http.StatusBadGateway, err)
			return
		}
		out, err := p.rewrite(body, t, resp.Request.URL)
		if err != nil {
			refuse(w, t, http.StatusBadGateway, err)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		if n, _ := w.Write(out); n > 0 && t.Audio {
			if c := p.tally(t); c != nil {
				c.audio.Add(1)
			}
		}
	case segment, cipher:
		// a segment is fetched whole and de-obfuscated
		// a cipher segment relays whole because CBC cannot decrypt from an offset
		body, err := buffered(resp, maxSegmentBody)
		if err != nil {
			p.count(t, false)
			refuse(w, t, http.StatusBadGateway, err)
			return
		}
		if t.Kind == segment {
			body = normalizeSegment(body)
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		n, _ := w.Write(body)
		p.count(t, n > 0)
	default:
		// a relayed body runs for as long as the player reads it, which for an
		// mp4 is the whole episode, so it counts as its first bytes arrive
		// waiting for the copy to finish would report that nothing had played
		// while the picture was on screen
		opened := &opening{ResponseWriter: w, on: func() { p.count(t, true) }}
		err := relay(opened, resp)
		if !opened.opened {
			p.count(t, false)
		}
		// the headers are gone, so the connection is dropped the way the
		// upstream dropped it rather than answered
		if err != nil {
			log.Debug("relay cut short", "kind", t.Kind, "host", host(t.URL), "err", err)
			panic(http.ErrAbortHandler)
		}
	}
}

// opening calls on as the first bytes of a body reach the player
// only the handler goroutine touches opened, before and after the copy it
// guards, so it needs no lock
type opening struct {
	http.ResponseWriter
	on     func()
	opened bool
}

func (o *opening) Write(b []byte) (int, error) {
	n, err := o.ResponseWriter.Write(b)
	if n > 0 && !o.opened {
		o.opened = true
		o.on()
	}
	return n, err
}

// count records against its stream whether a media body reached the player
// a body that carried nothing is not picture the player can start on, and a
// payload naming a tally this proxy never handed out counts nowhere
func (p *Proxy) count(t target, delivered bool) {
	if !t.Kind.picture() {
		return
	}
	c := p.tally(t)
	switch {
	case c == nil:
	case delivered:
		c.served.Add(1)
		if t.Audio {
			c.heard.Add(1)
		}
	default:
		c.refused.Add(1)
	}
}

// tally is the Tally a target counts against, nil for none this proxy issued
func (p *Proxy) tally(t target) *Tally {
	if t.Tally == 0 {
		return nil
	}
	v, ok := p.tallies.Load(t.Tally)
	if !ok {
		return nil
	}
	return v.(*Tally)
}

// refuse answers a request the relay could not serve, and says why under
// --verbose, since the body it answers with reaches mpv, IINA or ffmpeg, which
// show nobody the cause
// hop's hostless subtitle urls read as a bare upstream 502 on 2026-09-23 while
// this body said http: no Host in request URL
func refuse(w http.ResponseWriter, t target, code int, err error) {
	log.Debug("relay refused", "kind", t.Kind, "status", code, "host", host(t.URL), "err", err)
	http.Error(w, err.Error(), code)
}

// host names the upstream a target reaches, empty for one naming none
func host(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host
	}
	return ""
}

// buffered reads a whole body of at most limit bytes
// an endless chunked body would otherwise buffer until memory runs out
func buffered(resp *http.Response, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("upstream body too large")
	}
	return body, nil
}

// Upstream is the address a proxied url relays, so a caller can say which host
// answered behind the proxy
func (p *Proxy) Upstream(proxied string) (string, error) {
	u, err := url.Parse(proxied)
	if err != nil {
		return "", err
	}
	t, err := p.decode(u.Path)
	if err != nil {
		return "", err
	}
	return t.URL, nil
}

// decode reads the target out of a request path
// anything past the payload is the readable name a player shows and carries no
// meaning here
func (p *Proxy) decode(reqPath string) (target, error) {
	parts := strings.Split(strings.TrimPrefix(reqPath, "/"), "/")
	if len(parts) < 2 || parts[0] != p.token {
		return target{}, errToken
	}
	payload := parts[1]
	if i := strings.LastIndexByte(payload, '.'); i >= 0 {
		payload = payload[:i]
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return target{}, err
	}
	var t target
	err = json.Unmarshal(raw, &t)
	return t, err
}

func (p *Proxy) fetch(ctx context.Context, r *http.Request, t target) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errTarget, err)
	}
	// the upstream URL comes from a decoded payload and from playlist rewriting
	// refuse any non-http scheme so the relay cannot reach a local target
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, fmt.Errorf("%w: unsupported scheme %q", errTarget, req.URL.Scheme)
	}
	if req.URL.Host == "" {
		return nil, fmt.Errorf("%w: no host in %q", errTarget, t.URL)
	}
	req.Header.Set("User-Agent", upstream.UserAgent)
	upstream.SetReferer(req.Header, t.Referer)
	// forward a range only for a relayed body such as an mp4 or a .vtt
	// a segment must arrive whole so the decoy strip and any decryption line up
	if rng := r.Header.Get("Range"); rng != "" && t.Kind.relayed() {
		req.Header.Set("Range", rng)
	}

	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, status(resp.StatusCode)
	}
	return resp, nil
}

// mirrored is the status the proxy answers with for a failed fetch
// an upstream status is passed through rather than flattened, because a caller
// that retries has to tell a dead url from a hiccup, and every failure looking
// like a 502 makes a 404 look worth retrying
// a target the relay cannot address is refused like a payload it cannot decode,
// since no request left for an upstream and none ever could, and a target off
// the public internet is forbidden outright, so neither is retried
// anything else is a transport failure, which is what a 502 means
func mirrored(err error) int {
	var s status
	switch {
	case errors.As(err, &s):
		return int(s)
	case errors.Is(err, errTarget):
		return http.StatusBadRequest
	case errors.Is(err, upstream.ErrPrivate):
		return http.StatusForbidden
	}
	return http.StatusBadGateway
}

// relay forwards a body untouched, and reports the upstream cutting it short
// a body the upstream cuts short must not end cleanly for the player, since a
// clean end hands it a truncated episode as a whole one and a download would
// rename it into place, so the caller drops the connection on that report
// a write failure is the player going away, which is its choice and no error
func relay(w http.ResponseWriter, resp *http.Response) error {
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return nil
			}
		}
		switch {
		case err == io.EOF:
			return nil
		case err != nil:
			return err
		}
	}
}

// pngMagic opens every decoy seen, a 1x1 image in front of the stream
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// normalizeSegment drops the decoy image some providers place before the
// transport stream
// requiring a run of aligned sync bytes keeps a random payload from matching
// by chance
// a final segment can hold fewer packets than that run, and one keeping its
// decoy fails the cache path's check and with it the whole download, so a body
// opening with the decoy's image is cut where it becomes whole packets to its
// end instead
// the image is what makes the shorter test safe, since without it a body that
// merely ends on a sync byte would be cut down to its last packet
func normalizeSegment(data []byte) []byte {
	for i := 0; i < len(data) && i < scanHead; i++ {
		if synced(data, i) {
			return data[i:]
		}
	}
	if bytes.HasPrefix(data, pngMagic) {
		for i := len(pngMagic); i < len(data) && i < scanHead; i++ {
			if (len(data)-i)%tsPacket == 0 && packets(data, i) {
				return data[i:]
			}
		}
	}
	return data
}

// packets reports whether data from at to its end is whole packets that each
// open on a sync byte
func packets(data []byte, at int) bool {
	for i := at; i < len(data); i += tsPacket {
		if data[i] != 0x47 {
			return false
		}
	}
	return true
}

// synced reports whether data carries a run of aligned sync bytes from at
// it is the stricter twin of the cache path's looksTS, since it is looking for
// a sync point inside a body that may open with a decoy rather than judging a
// segment it already has whole
func synced(data []byte, at int) bool {
	runs := 0
	for i := at; i < len(data) && runs < syncRun; i += tsPacket {
		if data[i] != 0x47 {
			return false
		}
		runs++
	}
	return runs == syncRun
}
