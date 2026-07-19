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
	"strings"
	"sync"

	"ysun.co/miruro"
)

const (
	tsPacket = 188
	syncRun  = 8
	// scanHead bounds how far into a body the decoy scan looks
	// generous enough to clear a real image prefix while keeping the worst-case
	// scan of a sync-free body cheap
	scanHead = 256 * 1024
)

var errToken = errors.New("bad token")

// kind selects how the proxy treats an upstream body
type kind string

const (
	playlist kind = "playlist"
	segment  kind = "segment" // TS that may carry a decoy prefix
	cipher   kind = "cipher"  // encrypted TS that must reach the player unchanged
	opaque   kind = "opaque"  // byte relay that forwards a range
)

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
}

// Proxy relays provider streams over localhost, so a player sees plain HTTP/1.1
// while the upstream fetch keeps HTTP/2, the referer, and redirect handling
type Proxy struct {
	srv   *http.Server
	hc    *http.Client
	token string
	base  string
	done  chan struct{}
	once  sync.Once
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

	p := &Proxy{
		hc:    &http.Client{},
		token: hex.EncodeToString(tok),
		done:  make(chan struct{}),
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

// URL returns the localhost address a player or ffmpeg should open for s
func (p *Proxy) URL(s miruro.Stream) string {
	k := opaque
	if s.Kind == miruro.HLS {
		k = playlist
	}
	return p.proxied(s.URL, s.Referer, k)
}

// Opaque returns a localhost address relaying rawURL byte for byte
func (p *Proxy) Opaque(rawURL, referer string) string {
	return p.proxied(rawURL, referer, opaque)
}

// Stream addresses s through the proxy
// the referer is cleared because the proxy sends it upstream itself
func (p *Proxy) Stream(s miruro.Stream) miruro.Stream {
	s.URL = p.URL(s)
	s.Referer = ""
	return s
}

func (p *Proxy) proxied(rawURL, referer string, k kind) string {
	b, _ := json.Marshal(target{URL: rawURL, Referer: referer, Kind: k})
	return p.base + "/" + base64.RawURLEncoding.EncodeToString(b) + k.suffix()
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

	resp, err := p.fetch(r, t)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	switch {
	case t.Kind == playlist:
		body, ok := buffered(w, resp)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write(p.rewrite(body, t.Referer, resp.Request.URL))
	case t.Kind == segment && resp.StatusCode == http.StatusOK:
		body, ok := buffered(w, resp)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(normalizeSegment(body))
	default:
		// cipher, a partial segment answering a byterange, and opaque all pass
		// through untouched with their range headers intact
		relay(w, resp)
	}
}

func buffered(w http.ResponseWriter, resp *http.Response) ([]byte, bool) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return nil, false
	}
	return body, true
}

func (p *Proxy) decode(path string) (target, error) {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
	if len(parts) != 2 || parts[0] != p.token {
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

func (p *Proxy) fetch(r *http.Request, t target) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, t.URL, nil)
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
	// forward a byterange for any media body but never a playlist
	// a playlist is read whole and rewritten, so a range would corrupt parsing
	if rng := r.Header.Get("Range"); rng != "" && t.Kind != playlist {
		req.Header.Set("Range", rng)
	}

	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	return resp, nil
}

func relay(w http.ResponseWriter, resp *http.Response) {
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
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
