package play

import (
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ysun.co/miruro"
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

var errToken = errors.New("bad token")

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
// segments whose extension it does not recognise
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
}

// Proxy relays provider streams over localhost, so a player sees plain HTTP/1.1
// while the upstream fetch keeps HTTP/2, the referer, and redirect handling
type Proxy struct {
	srv   *http.Server
	hc    *http.Client
	token string
	base  string
	// served counts the media bodies relayed, so a caller can tell a player that
	// never got picture from one the user quit
	served atomic.Int64
	// refused counts the media bodies the upstream would not give up, so a
	// caller can tell a stream that cannot play from one that is merely slow
	refused atomic.Int64
	// timeout bounds one buffered fetch, zero disables the bound
	timeout time.Duration
	done    chan struct{}
	once    sync.Once
}

// StartProxy binds a relay on an ephemeral localhost port
// it serves until ctx is cancelled or Close is called
func StartProxy(ctx context.Context) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		ln.Close()
		return nil, err
	}

	// clone the default transport to keep HTTP/2 via ALPN, which the WAF needs
	// bound the header wait so a stalled CDN does not wedge a fetch indefinitely
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	p := &Proxy{
		hc:      &http.Client{Transport: tr},
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

// Refused counts the media bodies the proxy asked for and did not get
// ffmpeg's hls demuxer skips a segment it cannot fetch and asks for the next
// one, so a stream whose CDN refuses every segment never ends and never shows a
// frame, and this is the only sign of it a caller can see
func (p *Proxy) Refused() int { return int(p.refused.Load()) }

// Served counts the media bodies the proxy has relayed
// a player that exits with an error without raising this never started, which
// is what tells a dead stream from one the user quit
func (p *Proxy) Served() int { return int(p.served.Load()) }

func (p *Proxy) Close() error {
	p.once.Do(func() { close(p.done) })
	return p.srv.Close()
}

// URL returns the localhost address a player or ffmpeg should open for s
func (p *Proxy) URL(s miruro.Stream) string {
	if s.Kind == miruro.HLS {
		return p.encode(target{URL: s.URL, Referer: s.Referer, Kind: playlist, Height: s.Height})
	}
	return p.proxied(s.URL, s.Referer, media)
}

// Opaque returns a localhost address relaying rawURL byte for byte
func (p *Proxy) Opaque(rawURL, referer string) string {
	return p.proxied(rawURL, referer, opaque)
}

// Subtitles addresses each sidecar through the proxy under a file name a player
// can show
// subtitles carry no referer of their own, so they inherit the video stream's
func (p *Proxy) Subtitles(subs []miruro.Subtitle, referer string) []miruro.Subtitle {
	out := make([]miruro.Subtitle, len(subs))
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
func subName(s miruro.Subtitle) string {
	name := s.Label
	if name == "" {
		name = s.Lang
	}
	if name == "" {
		name = "subtitle"
	}
	return safeName(name) + subExt(s.File)
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
func (p *Proxy) Stream(s miruro.Stream) miruro.Stream {
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
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	case err != nil:
		http.Error(w, "bad target", http.StatusBadRequest)
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
		p.tally(t.Kind, false)
		http.Error(w, err.Error(), mirrored(err))
		return
	}
	defer resp.Body.Close()

	switch t.Kind {
	case playlist:
		body, ok := buffered(w, resp, maxPlaylistBody)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write(p.rewrite(body, t.Referer, resp.Request.URL, t.Height))
	case segment, cipher:
		// a segment is fetched whole and de-obfuscated
		// a cipher segment relays whole because CBC cannot decrypt from an offset
		body, ok := buffered(w, resp, maxSegmentBody)
		if !ok {
			p.tally(t.Kind, false)
			return
		}
		if t.Kind == segment {
			body = normalizeSegment(body)
		}
		w.Header().Set("Content-Type", "video/mp2t")
		n, _ := w.Write(body)
		p.tally(t.Kind, n > 0)
	default:
		// a relayed body runs for as long as the player reads it, which for an
		// mp4 is the whole episode, so it counts as its first bytes arrive
		// waiting for the copy to finish would report that nothing had played
		// while the picture was on screen
		opened := &opening{ResponseWriter: w, on: func() { p.tally(t.Kind, true) }}
		relay(opened, resp)
		if !opened.opened {
			p.tally(t.Kind, false)
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

// tally records whether a media body reached the player
// a body that carried nothing is not picture the player can start on
func (p *Proxy) tally(k kind, delivered bool) {
	switch {
	case !k.picture():
	case delivered:
		p.served.Add(1)
	default:
		p.refused.Add(1)
	}
}

// buffered reads a whole body of at most limit bytes
// an endless chunked body would otherwise buffer until memory runs out
func buffered(w http.ResponseWriter, resp *http.Response, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return nil, false
	}
	if int64(len(body)) > limit {
		http.Error(w, "upstream body too large", http.StatusBadGateway)
		return nil, false
	}
	return body, true
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
		return nil, err
	}
	// the upstream URL comes from a decoded payload and from playlist rewriting
	// refuse any non-http scheme so the relay cannot reach a local target
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", req.URL.Scheme)
	}
	req.Header.Set("User-Agent", miruro.UserAgent)
	if t.Referer != "" {
		req.Header.Set("Referer", t.Referer)
	}
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
// anything else is a transport failure, which is what a 502 means
func mirrored(err error) int {
	var s status
	if errors.As(err, &s) {
		return int(s)
	}
	return http.StatusBadGateway
}

// relay forwards a body untouched and reports the bytes it copied
func relay(w http.ResponseWriter, resp *http.Response) int64 {
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	return n
}

// normalizeSegment drops the decoy image some providers place before the
// transport stream
// requiring a run of aligned sync bytes keeps a random payload from matching
// by chance
func normalizeSegment(data []byte) []byte {
	for i := 0; i < len(data) && i < scanHead; i++ {
		if synced(data, i) {
			return data[i:]
		}
	}
	return data
}

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
