package allanime

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ysun.co/miruro"
)

// site is a fake AllAnime, the page, the bundle, the bootstrap, and the api,
// running the same handshake the live one does against the fixture build
type site struct {
	t     *testing.T
	srv   *httptest.Server
	build *build
	partB []byte
	key   []byte
	// stale makes the next episode answers refuse the attestation
	stale atomic.Int32
	// block makes the api answer a cloudflare challenge, and captcha makes
	// it ask for a turnstile solve
	block      atomic.Bool
	captcha    atomic.Bool
	bootstraps atomic.Int32
	shows      []map[string]any
	episode    map[string]any
}

func newSite(t *testing.T) *site {
	t.Helper()
	bd, err := parseBuild(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	s := &site{t: t, build: bd, partB: make([]byte, keyLen)}
	if _, err := rand.Read(s.partB); err != nil {
		t.Fatal(err)
	}
	if s.key, err = bd.sessionKey(s.partB); err != nil {
		t.Fatal(err)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	s.episode = map[string]any{
		"episodeString": "1",
		"sourceUrls": []map[string]any{
			{"sourceUrl": "https://ok.ru/videoembed/1", "sourceName": "Ok", "priority": 3.5, "type": "iframe"},
			{"sourceUrl": "--" + encode("/apivtwo/clock?id=abc"), "sourceName": "Luf-Mp4", "priority": 7.5, "type": "iframe"},
			{"sourceUrl": s.srv.URL + "/media/1?auth=x", "sourceName": "Yt-mp4", "priority": 7.9, "type": "player", "fallBack": "mp4", "fileExtenstion": "mp4"},
			{"sourceUrl": "javascript:alert(1)", "sourceName": "Evil", "priority": 9, "type": "player", "fileExtenstion": "mp4"},
		},
		"episodeInfo": map[string]any{
			"vidInforssub": map[string]any{"vidResolution": 1080},
			"vidInforsdub": map[string]any{"vidResolution": 720},
		},
	}
	return s
}

func (s *site) backend() *Backend {
	return &Backend{HTTP: s.srv.Client(), Site: s.srv.URL, API: s.srv.URL}
}

func (s *site) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		fmt.Fprintf(w, `<html><script>import("%s/_app/immutable/entry/app.Xyz.js")</script></html>`, s.srv.URL)
	case "/_app/immutable/entry/app.Xyz.js":
		io.WriteString(w, `import("../chunks/other.js");import("../chunks/main.js");`)
	case "/_app/immutable/chunks/other.js":
		io.WriteString(w, `export const nothing = 1;`)
	case "/_app/immutable/chunks/main.js":
		io.WriteString(w, fixture(s.t))
	case "/client-crypto/v1/bootstrap":
		s.bootstrap(w, r)
	case "/api":
		s.api(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *site) bootstrap(w http.ResponseWriter, r *http.Request) {
	s.bootstraps.Add(1)
	q := r.URL.Query()
	if q.Get("buildId") != s.build.ID || q.Get("k") != lane || r.Header.Get("x-build-id") != s.build.ID {
		http.Error(w, "unknown build", http.StatusForbidden)
		return
	}
	host, _ := url.Parse(s.srv.URL)
	now := time.Now().UnixMilli()
	epoch := now / s.build.epoch.Milliseconds()
	for _, e := range []int64{epoch, epoch - 1} {
		if r.Header.Get("x-aa-boot") != s.build.boot(host.Hostname(), group, lane, e) {
			continue
		}
		json.NewEncoder(w).Encode(map[string]any{
			"epoch": e, "epochMs": s.build.epoch.Milliseconds(), "graceMs": s.build.grace.Milliseconds(),
			"switchAt": now + int64(time.Hour/time.Millisecond), "partB": base64.StdEncoding.EncodeToString(s.partB), "k": lane,
		})
		return
	}
	http.Error(w, "bad signature", http.StatusForbidden)
}

func (s *site) api(w http.ResponseWriter, r *http.Request) {
	if s.block.Load() {
		w.Header().Set("cf-mitigated", "challenge")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "<html>Just a moment...</html>")
		return
	}
	var body struct {
		Query      string          `json:"query"`
		Variables  json.RawMessage `json:"variables"`
		Extensions struct {
			Persisted struct {
				Hash string `json:"sha256Hash"`
			} `json:"persistedQuery"`
			K     string `json:"k"`
			AAReq string `json:"aaReq"`
		} `json:"extensions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fail := func(code string) {
		json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{{"message": code, "extensions": map[string]any{"code": code}}},
			"data":   map[string]any{"episode": nil},
		})
	}
	switch {
	case strings.Contains(body.Query, "shows("):
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"shows": map[string]any{"edges": s.shows}}})
	case strings.Contains(body.Query, "episode("):
		if r.Header.Get("x-build-id") != s.build.ID {
			fail("AA_CRYPTO_MISSING_BUILD")
			return
		}
		sum := sha256.Sum256([]byte(body.Query))
		if body.Extensions.Persisted.Hash != hex.EncodeToString(sum[:]) || body.Extensions.K != lane {
			fail("AA_CRYPTO_QUERY_MISMATCH")
			return
		}
		plain, err := open(s.key, body.Extensions.AAReq)
		if err != nil {
			fail("AA_CRYPTO_MISSING")
			return
		}
		var tok struct {
			QH string `json:"qh"`
			K  string `json:"k"`
			TS int64  `json:"ts"`
		}
		if json.Unmarshal(plain, &tok) != nil || tok.QH != body.Extensions.Persisted.Hash || tok.K != lane {
			fail("AA_CRYPTO_QUERY_MISMATCH")
			return
		}
		if age := time.Since(time.UnixMilli(tok.TS)); age < 0 || age > window {
			fail("AA_CRYPTO_EXPIRED")
			return
		}
		if s.stale.Load() > 0 {
			s.stale.Add(-1)
			fail("AA_CRYPTO_STALE")
			return
		}
		if s.captcha.Load() {
			json.NewEncoder(w).Encode(map[string]any{
				"errors": []map[string]any{{"message": "NEED_CAPTCHA", "extensions": map[string]any{"code": "INTERNAL_SERVER_ERROR"}}},
				"data":   map[string]any{"episode": nil},
			})
			return
		}
		var vars struct {
			Translation string `json:"translationType"`
		}
		json.Unmarshal(body.Variables, &vars)
		if vars.Translation != "sub" && vars.Translation != "dub" {
			http.Error(w, "bad translation", http.StatusBadRequest)
			return
		}
		payload, _ := json.Marshal(map[string]any{"episode": s.episode})
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"_m": "b7", "tobeparsed": seal(s.t, s.key, payload)}})
	default:
		http.Error(w, "unknown query", http.StatusBadRequest)
	}
}

// seal is the site's side of open
func seal(t *testing.T, key, plain []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, nonceLen)
	rand.Read(nonce)
	out := append([]byte{blobVersion}, nonce...)
	return base64.StdEncoding.EncodeToString(gcm.Seal(out, nonce, plain, nil))
}

// encode is the inverse of decode, for a fixture source path
func encode(s string) string {
	back := map[byte]string{}
	for pair, c := range substitution {
		back[c] = pair
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteString(back[s[i]])
	}
	return b.String()
}

func TestEpisodesMatchesTheAniListID(t *testing.T) {
	s := newSite(t)
	s.shows = []map[string]any{
		{"_id": "sequel", "name": "Sousou no Frieren 2nd Season", "aniListId": "182255",
			"availableEpisodesDetail": map[string]any{"sub": []string{"2", "1"}, "dub": []string{}}},
		{"_id": "ReHMC7TQnch3C6z8j", "name": "Sousou no Frieren", "aniListId": "154587",
			"availableEpisodesDetail": map[string]any{"sub": []string{"3", "12.5", "1", "x"}, "dub": []string{"1"}}},
	}
	cat, err := s.backend().Episodes(context.Background(), miruro.Media{ID: 154587, Romaji: "Sousou no Frieren"})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cat.Providers[Code]
	if !ok || p.Backend == nil {
		t.Fatalf("providers = %v, want %s pointing back at the backend", cat.Providers, Code)
	}
	var got []string
	for _, e := range p.Sub {
		got = append(got, fmt.Sprintf("%s=%v", e.ID, e.Number))
	}
	if want := "ReHMC7TQnch3C6z8j/1=1,ReHMC7TQnch3C6z8j/3=3,ReHMC7TQnch3C6z8j/12.5=12.5"; strings.Join(got, ",") != want {
		t.Errorf("sub episodes = %v, want %s", got, want)
	}
	if len(p.Dub) != 1 || p.Dub[0].ID != "ReHMC7TQnch3C6z8j/1" {
		t.Errorf("dub episodes = %v", p.Dub)
	}
}

func TestCapabilitiesDeclareHardsub(t *testing.T) {
	caps, err := New().Capabilities(context.Background())
	if err != nil || !caps[Code].Hard || caps[Code].Soft || caps[Code].Embed {
		t.Errorf("capabilities = %v, %v, want hardsub only", caps, err)
	}
}

func TestEpisodesUnknownTitleIsEmpty(t *testing.T) {
	s := newSite(t)
	s.shows = []map[string]any{{"_id": "other", "name": "Other", "aniListId": "1"}}
	cat, err := s.backend().Episodes(context.Background(), miruro.Media{ID: 2, English: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Providers) != 0 {
		t.Errorf("providers = %v, want none", cat.Providers)
	}
}

func TestSourcesResolveThroughTheHandshake(t *testing.T) {
	s := newSite(t)
	b := s.backend()
	res, err := b.Sources(context.Background(), "ReHMC7TQnch3C6z8j/1", Code, miruro.Sub)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Playable() {
		t.Fatalf("result not playable: %+v", res.Streams)
	}
	var kinds []string
	for _, st := range res.Streams {
		kinds = append(kinds, st.Server+":"+string(st.Kind))
	}
	// the highest priority playable source leads and the javascript url is gone
	if want := "Yt-mp4:mp4,Luf-Mp4:embed,Ok:embed"; strings.Join(kinds, ",") != want {
		t.Errorf("streams = %v, want %s", kinds, want)
	}
	head := res.Streams[0]
	if !head.Default || head.Quality != "1080p" || head.Referer != s.srv.URL || !strings.HasPrefix(head.URL, s.srv.URL+"/media/1") {
		t.Errorf("head = %+v", head)
	}
	if res.Streams[1].URL != clockHost+"/apivtwo/clock?id=abc" {
		t.Errorf("encoded source = %q", res.Streams[1].URL)
	}

	// the build and the session are kept, so a second episode costs no bootstrap
	if _, err := b.Sources(context.Background(), "ReHMC7TQnch3C6z8j/2", Code, miruro.Dub); err != nil {
		t.Fatal(err)
	}
	if n := s.bootstraps.Load(); n != 1 {
		t.Errorf("bootstraps = %d, want 1", n)
	}
}

func TestSourcesRefreshOnceWhenTheAttestationIsStale(t *testing.T) {
	s := newSite(t)
	b := s.backend()
	s.stale.Store(1)
	if _, err := b.Sources(context.Background(), "show/1", Code, miruro.Sub); err != nil {
		t.Fatal(err)
	}
	if n := s.bootstraps.Load(); n != 2 {
		t.Errorf("bootstraps = %d, want a refresh after the refusal", n)
	}
	// a refusal that survives the refresh is reported rather than retried again
	s.stale.Store(2)
	if _, err := b.Sources(context.Background(), "show/1", Code, miruro.Sub); !errors.Is(err, errCrypto) {
		t.Errorf("err = %v, want %v", err, errCrypto)
	}
}

func TestSourcesRefuseWhatTheyCannotServe(t *testing.T) {
	s := newSite(t)
	b := s.backend()
	if _, err := b.Sources(context.Background(), "show/1", Code, miruro.Ssub); !errors.Is(err, miruro.ErrUpstream) {
		t.Errorf("ssub err = %v, want %v", err, miruro.ErrUpstream)
	}
	if _, err := b.Sources(context.Background(), "show/1", "ally", miruro.Sub); err == nil {
		t.Error("another backend's provider resolved")
	}
	if _, err := b.Sources(context.Background(), "1", Code, miruro.Sub); err == nil {
		t.Error("an episode id naming no show resolved")
	}
	s.block.Store(true)
	if _, err := b.Sources(context.Background(), "show/1", Code, miruro.Sub); !errors.Is(err, miruro.ErrBlocked) {
		t.Errorf("challenged err = %v, want %v", err, miruro.ErrBlocked)
	}
	s.block.Store(false)
	s.captcha.Store(true)
	if _, err := b.Sources(context.Background(), "show/1", Code, miruro.Sub); !errors.Is(err, miruro.ErrBlocked) {
		t.Errorf("captcha err = %v, want %v", err, miruro.ErrBlocked)
	}
}

func TestFetchBuildRefusesAnUnreadableSite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html>nothing</html>")
	}))
	defer srv.Close()
	b := &Backend{HTTP: srv.Client(), Site: srv.URL, API: srv.URL}
	if _, err := b.Sources(context.Background(), "show/1", Code, miruro.Sub); !errors.Is(err, errBundle) {
		t.Errorf("err = %v, want %v", err, errBundle)
	}
}
