package miruro

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"ysun.co/miruro/internal/upstream"
)

// mask obfuscates plain the way the catalog api does, gzip then a repeating xor
func mask(t *testing.T, plain []byte) []byte {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := gz.Bytes()
	for i := range raw {
		raw[i] ^= catalogKey[i%len(catalogKey)]
	}
	return raw
}

func TestUnmask(t *testing.T) {
	want := []byte(`{"data":[],"next_cursor":null,"has_more":false}`)
	got, err := unmask(mask(t, want))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

// the taxonomy switch tests 403 html before the general >= 400 branch
// a reorder would report a WAF rejection as recoverable upstream.ErrUnreachable and send
// the fallback loop back into the block, so every case asserts on the
// sentinel with errors.Is rather than on message text
func TestErrorTaxonomy(t *testing.T) {
	plain := []byte(`{"data":[],"has_more":false}`)
	cases := []struct {
		name    string
		status  int
		ctype   string
		body    []byte
		cancel  bool
		wantErr error
		wantMsg string
		want    []byte
	}{
		{
			name:    "forbidden html is a fatal block",
			status:  http.StatusForbidden,
			ctype:   "text/html",
			body:    []byte("<html>blocked</html>"),
			wantErr: upstream.ErrBlocked,
		},
		{
			name:    "server error is recoverable",
			status:  http.StatusInternalServerError,
			wantErr: upstream.ErrUnreachable,
		},
		{
			name:    "html with an ok status is recoverable",
			status:  http.StatusOK,
			ctype:   "text/html",
			body:    []byte("<html>challenge</html>"),
			wantErr: upstream.ErrUnreachable,
		},
		{
			name:    "a canceled context surfaces as such",
			status:  http.StatusOK,
			cancel:  true,
			wantErr: context.Canceled,
		},
		{
			name:   "a catalog body round-trips",
			status: http.StatusOK,
			ctype:  "application/octet-stream",
			body:   mask(t, plain),
			want:   plain,
		},
		{
			name:   "a plain json body passes through",
			status: http.StatusOK,
			ctype:  "application/json; charset=utf-8",
			body:   plain,
			want:   plain,
		},
		{
			name:    "a problem document is recoverable",
			status:  http.StatusBadRequest,
			ctype:   "application/problem+json",
			body:    []byte(`{"type":"about:blank","title":"Bad Request","status":400,"detail":"Unsupported catalog request."}`),
			wantErr: upstream.ErrUnreachable,
			wantMsg: "status 400: Bad Request: Unsupported catalog request.",
		},
		// the secure pipe went this way in 2026-09, and reading the html shell
		// as a firewall would have walked every mirror for nothing
		{
			name:    "a route the api no longer has is recoverable",
			status:  http.StatusNotFound,
			ctype:   "text/html; charset=UTF-8",
			body:    []byte("<!doctype html><html></html>"),
			wantErr: upstream.ErrUnreachable,
			wantMsg: "status 404",
		},
		{
			name:    "a mask that does not decode is recoverable",
			status:  http.StatusOK,
			ctype:   "application/octet-stream",
			body:    []byte("a body long enough to hold a gzip header and still not be one"),
			wantErr: upstream.ErrUnreachable,
			wantMsg: "gzip: invalid header",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.ctype != "" {
					w.Header().Set("Content-Type", tc.ctype)
				}
				w.WriteHeader(tc.status)
				w.Write(tc.body)
			}))
			defer srv.Close()

			ctx := context.Background()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
			got, err := c.get(ctx, "/api/v1/anime")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				if !strings.Contains(fmt.Sprint(err), tc.wantMsg) {
					t.Errorf("error = %v, want it to carry %q", err, tc.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("body = %s, want %s", got, tc.want)
			}
		})
	}
}

// zeros streams an endless zero body for over-cap tests
type zeros struct{}

func (zeros) Read(p []byte) (int, error) { return len(p), nil }

// an endless chunked body would otherwise buffer until memory runs out
func TestRefusesOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.CopyN(w, zeros{}, maxBody+1)
	}))
	defer srv.Close()

	c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
	_, err := c.get(context.Background(), "/x")
	if !errors.Is(err, upstream.ErrUnreachable) || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want a recoverable over-cap error, got %v", err)
	}
}

// a body that dies after its headers came from a host that answered, so it is
// the backend's failure and keeps the typed surface, with the cause beside it
func TestBodyThatDiesIsRecoverable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		io.WriteString(w, "{\"data\":")
	}))
	defer srv.Close()

	c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
	_, err := c.get(context.Background(), "/x")
	if !errors.Is(err, upstream.ErrUnreachable) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want %v wrapping %v", err, upstream.ErrUnreachable, io.ErrUnexpectedEOF)
	}
}

// ResponseHeaderTimeout stops once headers arrive, so an upstream that answers
// and then goes quiet mid-body needs a whole-request bound to fail rather than
// hang the CLI with no output
// the bound is the request's own deadline, so running out of it is the
// backend's failure and not the run being stopped
func TestClientBoundsAStalledBody(t *testing.T) {
	if New().Timeout == 0 {
		t.Fatal("the api client has no whole-request bound, a stalled body hangs forever")
	}

	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-stall
	}))
	// the handler blocks until stall closes, and Close waits on the handler, so
	// this defer has to run first
	defer srv.Close()
	defer close(stall)

	c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client(), Timeout: 300 * time.Millisecond}
	done := make(chan error, 1)
	go func() { _, err := c.get(context.Background(), "/api/config"); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, upstream.ErrUnreachable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the backend's failure carrying the deadline", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the body read was never bounded")
	}
}

// the catalog refuses any page size but the site's own, and a record that names
// no AniList id or several cannot key the history, so neither may reach the
// picker
func TestSearch(t *testing.T) {
	ctx := context.Background()

	t.Run("asks the way the site does and keys every hit by its AniList id", func(t *testing.T) {
		var asked *http.Request
		srv := mirror(t, func(w http.ResponseWriter, r *http.Request) {
			asked = r
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(mask(t, []byte(`{"data":[
				{"id":"a","external_ids":{"anilist":["16498"],"mal":["16498"]},"title":{"romaji":"Shingeki no Kyojin","english":"Attack on Titan"},"format":"TV","episode_count":25,"episode_counts":{"raw":25}},
				{"id":"b","external_ids":{"mal":["1"]},"title":{"romaji":"Unkeyed"},"format":"TV"},
				{"id":"c","external_ids":{"anilist":["1","2"]},"title":{"romaji":"Ambiguous"},"format":"TV"},
				{"id":"d","external_ids":{"anilist":["x"]},"title":{"romaji":"Garbled"},"format":"TV"},
				{"id":"e","external_ids":{"anilist":["21"]},"title":{"romaji":"ONE PIECE"},"format":"TV","episode_count":null,"episode_counts":{"raw":1180}}],
				"next_cursor":"n","has_more":true}`)))
		})

		media, next, err := (&Client{Bases: []string{srv.URL}, HTTP: srv.Client()}).Search(ctx, "attack on titan", "")
		if err != nil {
			t.Fatal(err)
		}
		if next != "n" {
			t.Errorf("next = %q, want the cursor the page named", next)
		}
		want := []upstream.Media{
			{ID: 16498, Romaji: "Shingeki no Kyojin", English: "Attack on Titan", Episodes: 25, Format: "TV"},
			{ID: 21, Romaji: "ONE PIECE", Episodes: 1180, Format: "TV"},
		}
		if !reflect.DeepEqual(media, want) {
			t.Errorf("media = %+v, want %+v", media, want)
		}
		if asked.URL.Path != "/api/v1/anime" {
			t.Errorf("path = %q, want the anime resource", asked.URL.Path)
		}
		if q := asked.URL.Query(); q.Get("q") != "attack on titan" || q.Get("limit") != "15" || q.Get("sort") != "-popularity" || len(q) != 3 {
			t.Errorf("query = %v, want the term, the site's page size and its default sort and nothing else", q)
		}
	})

	// the next page is asked for with the cursor the last one named, which is
	// the shape the site's own search sends, and the last page names none
	t.Run("pages with the cursor", func(t *testing.T) {
		var asked *http.Request
		srv := mirror(t, func(w http.ResponseWriter, r *http.Request) {
			asked = r
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(mask(t, []byte(`{"data":[{"id":"z","external_ids":{"anilist":["5"]},"title":{"romaji":"Last"},"format":"TV"}],
				"next_cursor":"stale","has_more":false}`)))
		})

		media, next, err := (&Client{Bases: []string{srv.URL}, HTTP: srv.Client()}).Search(ctx, "titan", "c1_abc")
		if err != nil {
			t.Fatal(err)
		}
		if q := asked.URL.Query(); q.Get("cursor") != "c1_abc" || q.Get("q") != "titan" || q.Get("limit") != "15" || q.Get("sort") != "-popularity" || len(q) != 4 {
			t.Errorf("query = %v, want the term, the page size, the sort and the cursor", q)
		}
		if len(media) != 1 || next != "" {
			t.Errorf("media = %v, next = %q, want the last page and no cursor past it", media, next)
		}
	})

	t.Run("a refused search names the reason", func(t *testing.T) {
		srv := mirror(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"type":"about:blank","title":"Bad Request","status":400,"detail":"Unsupported catalog request."}`)
		})

		_, _, err := (&Client{Bases: []string{srv.URL}, HTTP: srv.Client()}).Search(ctx, "titan", "")
		if !errors.Is(err, upstream.ErrUnreachable) || !strings.Contains(err.Error(), "Unsupported catalog request") {
			t.Errorf("err = %v, want the upstream reason", err)
		}
	})
}

// counter is a mirror that records how often it was asked
type counter struct {
	*httptest.Server
	hits atomic.Int64
}

// mirror serves handler and counts every request, so a test can prove which
// mirror answered and which was never reached
func mirror(t *testing.T, handler http.HandlerFunc) *counter {
	t.Helper()
	c := &counter{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(c.Close)
	return c
}

func blocks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusForbidden)
	io.WriteString(w, "<html>blocked</html>")
}

func serves(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }
}

// every mirror fronts one backend, so the walk exists for the failures a
// different host can answer
func TestMirrorRotation(t *testing.T) {
	ctx := context.Background()

	t.Run("a blocked mirror hands over and stays handed over", func(t *testing.T) {
		bad := mirror(t, blocks)
		good := mirror(t, serves(`{"ok":true}`))
		c := &Client{Bases: []string{bad.URL, good.URL}, HTTP: good.Client()}

		for range 3 {
			if _, err := c.get(ctx, "/api/config"); err != nil {
				t.Fatal(err)
			}
		}
		// the first call walks past the block, the rest start where it landed
		if got := bad.hits.Load(); got != 1 {
			t.Errorf("blocked mirror served %d requests, want the one that found the block", got)
		}
		if got := good.hits.Load(); got != 3 {
			t.Errorf("working mirror served %d requests, want all 3", got)
		}
	})

	t.Run("an unreachable mirror hands over", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(blocks))
		dead.Close()
		good := mirror(t, serves(`{"ok":true}`))

		c := &Client{Bases: []string{dead.URL, good.URL}, HTTP: good.Client()}
		if _, err := c.get(ctx, "/api/config"); err != nil {
			t.Fatal(err)
		}
		if got := good.hits.Load(); got != 1 {
			t.Errorf("working mirror served %d requests, want 1", got)
		}
	})

	t.Run("an upstream status does not multiply the requests", func(t *testing.T) {
		down := mirror(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		other := mirror(t, serves(`{"ok":true}`))

		c := &Client{Bases: []string{down.URL, other.URL}, HTTP: other.Client()}
		if _, err := c.get(ctx, "/api/v1/anime"); !errors.Is(err, upstream.ErrUnreachable) {
			t.Fatalf("err = %v, want upstream.ErrUnreachable", err)
		}
		if got := other.hits.Load(); got != 0 {
			t.Errorf("the second mirror served %d requests, want none for a backend status", got)
		}
	})

	t.Run("blocked everywhere is fatal", func(t *testing.T) {
		a, b := mirror(t, blocks), mirror(t, blocks)
		c := &Client{Bases: []string{a.URL, b.URL}, HTTP: a.Client()}
		if _, err := c.get(ctx, "/api/config"); !errors.Is(err, upstream.ErrBlocked) {
			t.Fatalf("err = %v, want upstream.ErrBlocked", err)
		}
	})

	// reporting this as recoverable would send the fallback loop back into the
	// block for every remaining provider
	t.Run("a block outranks a later unreachable mirror", func(t *testing.T) {
		blocked := mirror(t, blocks)
		dead := httptest.NewServer(http.HandlerFunc(blocks))
		dead.Close()

		c := &Client{Bases: []string{blocked.URL, dead.URL}, HTTP: blocked.Client()}
		if _, err := c.get(ctx, "/api/config"); !errors.Is(err, upstream.ErrBlocked) {
			t.Fatalf("err = %v, want upstream.ErrBlocked", err)
		}
	})

	// the walk exists for hosts that cannot answer, so a host that reached the
	// backend is the one to start from even when the backend said no
	t.Run("a mirror that reached the backend becomes the preferred one", func(t *testing.T) {
		bad := mirror(t, blocks)
		down := mirror(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		c := &Client{Bases: []string{bad.URL, down.URL}, HTTP: down.Client()}

		for range 3 {
			if _, err := c.get(ctx, "/api/v1/anime"); !errors.Is(err, upstream.ErrUnreachable) {
				t.Fatalf("err = %v, want upstream.ErrUnreachable", err)
			}
		}
		if got := bad.hits.Load(); got != 1 {
			t.Errorf("blocked mirror served %d requests, want only the one that found the block", got)
		}
		if got := down.hits.Load(); got != 3 {
			t.Errorf("reachable mirror served %d requests, want all 3", got)
		}
	})

	// a backend that accepts the connection and then goes quiet reproduces on
	// every mirror, so walking them multiplies one header timeout by four
	t.Run("a host that answered nothing is still the host that answered", func(t *testing.T) {
		stall := make(chan struct{})
		quiet := mirror(t, func(w http.ResponseWriter, r *http.Request) { <-stall })
		defer close(stall)
		other := mirror(t, serves(`{"ok":true}`))

		hc := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 200 * time.Millisecond}}
		c := &Client{Bases: []string{quiet.URL, other.URL}, HTTP: hc}
		if _, err := c.get(ctx, "/api/v1/anime"); !errors.Is(err, upstream.ErrUnreachable) {
			t.Fatalf("err = %v, want upstream.ErrUnreachable", err)
		}
		if got := other.hits.Load(); got != 0 {
			t.Errorf("the second mirror served %d requests, want none for a backend that went quiet", got)
		}
	})

	t.Run("no mirror configured is recoverable", func(t *testing.T) {
		if _, err := (&Client{HTTP: http.DefaultClient}).get(ctx, "/api/config"); !errors.Is(err, upstream.ErrUnreachable) {
			t.Fatalf("err = %v, want upstream.ErrUnreachable", err)
		}
	})
}

// a rule comparing Origin against Host would reject a header set pinned to one
// domain the moment the walk moved off it
func TestOriginFollowsTheMirror(t *testing.T) {
	var origin, referer string
	srv := mirror(t, func(w http.ResponseWriter, r *http.Request) {
		origin, referer = r.Header.Get("Origin"), r.Header.Get("Referer")
		io.WriteString(w, `{"ok":true}`)
	})

	c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
	if _, err := c.get(context.Background(), "/api/config"); err != nil {
		t.Fatal(err)
	}
	if origin != srv.URL || referer != srv.URL+"/" {
		t.Errorf("origin = %q referer = %q, want them on %q", origin, referer, srv.URL)
	}
}

// a picker label or a failed download row carries the styling huh put in it,
// so the views keep escapes, and an OSC 52 clipboard write or an OSC 0 title
// change inside catalog text would reach the terminal whole
// the sequences here are C0, a C1 CSI and a DEL, and only those characters go
func TestEscapesInCatalogTextStayOffTheTerminal(t *testing.T) {
	ctx := context.Background()
	plain := func(t *testing.T, what, s string) {
		t.Helper()
		if i := strings.IndexFunc(s, unicode.IsControl); i >= 0 {
			t.Errorf("%s = %q, carries a control character at %d", what, s, i)
		}
	}

	t.Run("a search hit", func(t *testing.T) {
		srv, _ := api(t, map[string]string{"/api/v1/anime": `{"data":[{"id":"f","external_ids":{"anilist":["154587"]},
			"title":{"romaji":"Sousou no Frieren\u001b]52;c;cm0gLXJmIH4K\u0007","english":"葬送のフリーレン\u009b2J\u007f"},
			"format":"TV\u001b]0;pwned\u0007"}],"has_more":false}`})
		media, _, err := client(srv).Search(ctx, "frieren", "")
		if err != nil {
			t.Fatal(err)
		}
		want := []upstream.Media{{ID: 154587, Romaji: "Sousou no Frieren]52;c;cm0gLXJmIH4K", English: "葬送のフリーレン2J", Format: "TV]0;pwned"}}
		if !reflect.DeepEqual(media, want) {
			t.Errorf("media = %#v, want %#v", media, want)
		}
	})

	t.Run("an episode title and the title it lists under", func(t *testing.T) {
		srv, _ := api(t, map[string]string{
			"/api/v1/anime": `{"data":[{"id":"f","external_ids":{"anilist":["154587"]},
				"title":{"romaji":"Sousou no Frieren","english":"Frieren\u001b]0;pwned\u0007"},"episode_counts":{"sub":1}}],"has_more":false}`,
			"/api/v1/anime/f/episodes": `{"data":[{"episode_number":1,"kind":"regular","title":"The Journey's End\u001b]52;c;cm0gLXJmIH4K\u0007\r\n"}],"has_more":false}`,
		})
		cat, err := client(srv).Episodes(ctx, upstream.Media{ID: 154587})
		if err != nil {
			t.Fatal(err)
		}
		if cat.Title != "Frieren]0;pwned" {
			t.Errorf("title = %q, want the escapes dropped", cat.Title)
		}
		if len(cat.Sub) != 1 || cat.Sub[0].Title != "The Journey's End]52;c;cm0gLXJmIH4K" {
			t.Errorf("sub = %#v, want the episode title without its escapes", cat.Sub)
		}
	})

	t.Run("an error naming a catalog key", func(t *testing.T) {
		srv, _ := api(t, map[string]string{
			"/api/v1/anime": `{"data":[{"id":"f\u001b]0;pwned\u0007","external_ids":{"anilist":["154587"]}}],"has_more":false}`,
			"/api/v1/anime/f\x1b]0;pwned\x07/episodes": `{"data":[],"has_more":true}`,
		})
		_, err := client(srv).Episodes(ctx, upstream.Media{ID: 154587})
		if err == nil || !strings.Contains(err.Error(), "more than") {
			t.Fatalf("err = %v, want the truncated list refused", err)
		}
		plain(t, "err", err.Error())
	})

	// the problem document is what a failed download row and the closing error
	// line both show
	t.Run("a problem document", func(t *testing.T) {
		srv := mirror(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"title":"Bad Request\u001b]52;c;cm0gLXJmIH4K\u0007","status":400,"detail":"Unsupported\u009b2J catalog request.\n"}`)
		})
		_, _, err := client(srv).Search(ctx, "titan", "")
		if !errors.Is(err, upstream.ErrUnreachable) {
			t.Fatalf("err = %v, want %v", err, upstream.ErrUnreachable)
		}
		if want := "status 400: Bad Request]52;c;cm0gLXJmIH4K: Unsupported2J catalog request."; !strings.HasSuffix(err.Error(), want) {
			t.Errorf("err = %q, want it to end in %q", err, want)
		}
	})

	t.Run("a provider, its servers and its subtitles", func(t *testing.T) {
		srv, _ := api(t, map[string]string{
			"/api/config": `{"streaming":{"kickassanime":{"label":"h\u007fop","visible":true},"new\u001bcomer":{"label":"","visible":true}}}`,
			"/api/v1/anime/f/episodes/1/play": `{"tracks":[{"track":"ssub","providers":[{"provider":"kickassanime","subtitles":[
				{"language":"en","label":"English\u001b]52;c;eA==\u0007","file":"https://subst.example/en.vtt","format":"vtt"},
				{"language":"\u001b]0;pwned\u0007","label":"","file":"https://subst.example/x.vtt","format":"vtt"}],
			 "servers":[{"server":"Vid\u001b]0;pwned\u0007","headers":{"Referer":"https://krussdomi.com/"},"streams":[
				{"url":"https://hls.example/master.m3u8","format":"hls","quality":"1080p\u009b"}]}]}]}]}`,
		})
		c := client(srv)
		codes, err := c.providerCodes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// the api's own ids stay as sent, since they are what a request names
		if want := map[string]string{"kickassanime": "hop", "new\x1bcomer": "newcomer"}; !reflect.DeepEqual(codes, want) {
			t.Errorf("codes = %+q, want %+q", codes, want)
		}
		l, err := c.Listing(ctx, "f", 1)
		if err != nil {
			t.Fatal(err)
		}
		hop, ok := l.Providers["hop"]
		if !ok || len(hop.Sub) != 1 {
			t.Fatalf("providers = %+v, want hop under its label without the DEL", l.Providers)
		}
		res, err := c.Sources(ctx, hop.Sub[0].ID, "hop", upstream.Ssub)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Streams) != 1 || res.Streams[0].Server != "Vid]0;pwned" || res.Streams[0].Quality != "1080p" {
			t.Errorf("streams = %#v, want the server and quality labels without their escapes", res.Streams)
		}
		if len(res.Subtitles) != 2 || res.Subtitles[0].Label != "English]52;c;eA==" || res.Subtitles[0].Lang != "en" {
			t.Errorf("subtitles = %#v, want the label without its escapes and its language read", res.Subtitles)
		}
		for _, s := range res.Subtitles {
			plain(t, "subtitle label", s.Label)
			plain(t, "subtitle language", s.Lang)
		}
	})

	t.Run("an error naming providers", func(t *testing.T) {
		srv, _ := api(t, map[string]string{"/api/config": `{"streaming":{
			"a\u001b]0;pwned\u0007":{"label":"bee","visible":true},"b\u001b]0;pwned\u0007":{"label":"bee","visible":true}}}`})
		_, err := client(srv).providerCodes(ctx)
		if err == nil || !strings.Contains(err.Error(), "bee") {
			t.Fatalf("err = %v, want the shared code named", err)
		}
		plain(t, "err", err.Error())
	})
}
