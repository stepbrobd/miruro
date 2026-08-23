package miruro

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func obfuscate(t *testing.T, plain []byte, version string) []byte {
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
	if version == "2" {
		for i := range raw {
			raw[i] ^= obfKey[i%len(obfKey)]
		}
	}
	return []byte(base64.RawURLEncoding.EncodeToString(raw))
}

func TestDecode(t *testing.T) {
	want := []byte(`{"mappings":{"id":21},"providers":{}}`)
	for _, version := range []string{"1", "2"} {
		got, err := decode(obfuscate(t, want, version), version)
		if err != nil {
			t.Fatalf("version %s: %v", version, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("version %s: got %s want %s", version, got, want)
		}
	}
}

// the taxonomy switch tests 403 html before the general >= 400 branch
// a reorder would report a WAF rejection as recoverable ErrUpstream and send
// the fallback loop back into the block, so every case asserts on the
// sentinel with errors.Is rather than on message text
func TestPipeErrorTaxonomy(t *testing.T) {
	plain := []byte(`{"providers":{"bonk":{}}}`)
	cases := []struct {
		name    string
		status  int
		ctype   string
		obf     string
		body    []byte
		cancel  bool
		wantErr error
		want    []byte
	}{
		{
			name:    "forbidden html is a fatal block",
			status:  http.StatusForbidden,
			ctype:   "text/html",
			body:    []byte("<html>blocked</html>"),
			wantErr: ErrBlocked,
		},
		{
			name:    "server error is recoverable",
			status:  http.StatusInternalServerError,
			wantErr: ErrUpstream,
		},
		{
			name:    "html with an ok status is recoverable",
			status:  http.StatusOK,
			ctype:   "text/html",
			body:    []byte("<html>challenge</html>"),
			wantErr: ErrUpstream,
		},
		{
			name:   "xor envelope round-trips",
			status: http.StatusOK,
			obf:    "2",
			body:   obfuscate(t, plain, "2"),
			want:   plain,
		},
		{
			name:   "plain gzip envelope round-trips",
			status: http.StatusOK,
			obf:    "1",
			body:   obfuscate(t, plain, "1"),
			want:   plain,
		},
		{
			name:    "an error object with an ok status is recoverable",
			status:  http.StatusOK,
			obf:     "1",
			body:    obfuscate(t, []byte(`{"error":"Secure pipe failed"}`), "1"),
			wantErr: ErrUpstream,
		},
		{
			name:    "a cancelled context surfaces as such",
			status:  http.StatusOK,
			cancel:  true,
			wantErr: context.Canceled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.ctype != "" {
					w.Header().Set("Content-Type", tc.ctype)
				}
				if tc.obf != "" {
					w.Header().Set("x-obfuscated", tc.obf)
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
			got, err := c.pipe(ctx, "sources", nil)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("pipe error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("pipe body = %s, want %s", got, tc.want)
			}
		})
	}
}

// zeros streams an endless zero body for over-cap tests
type zeros struct{}

func (zeros) Read(p []byte) (int, error) { return len(p), nil }

// an endless chunked pipe body would otherwise buffer until memory runs out
func TestPipeRefusesOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.CopyN(w, zeros{}, maxPipeRaw+1)
	}))
	defer srv.Close()

	c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
	_, err := c.pipe(context.Background(), "/x", nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want an over-cap error, got %v", err)
	}
}

// ResponseHeaderTimeout stops once headers arrive, so an upstream that answers
// and then goes quiet mid-body needs a whole-request bound to fail rather than
// hang the CLI with no output
func TestClientBoundsAStalledBody(t *testing.T) {
	if New().HTTP.Timeout == 0 {
		t.Fatal("the api client has no whole-request timeout, a stalled body hangs forever")
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

	// the same construction with a short bound, so the mechanism is proven
	// without waiting out the real one
	hc := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(), Timeout: 500 * time.Millisecond}
	resp, err := hc.Get(srv.URL)
	if err != nil {
		t.Fatalf("headers should arrive before the bound trips: %v", err)
	}
	defer resp.Body.Close()

	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(resp.Body); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stalled body read returned without error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the body read was never bounded")
	}
}

// the search resource answers 200 with an error object when it fails, and 200
// with manga when the type filter does not hold, so neither may reach the picker
func TestSearch(t *testing.T) {
	ctx := context.Background()

	t.Run("keeps anime and drops everything else", func(t *testing.T) {
		var got map[string]string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = envelopeQuery(t, r)
			io.WriteString(w, `[
				{"id":1,"type":"ANIME","title":{"romaji":"Shingeki","english":"Attack on Titan"},"episodes":25,"format":"TV"},
				{"id":2,"type":"MANGA","title":{"romaji":"Berserk"},"format":"MANGA"},
				{"id":3,"type":"ANIME","title":{"romaji":"Only Romaji"},"format":"OVA"}]`)
		}))
		defer srv.Close()

		media, err := (&Client{Bases: []string{srv.URL}, HTTP: srv.Client()}).Search(ctx, "titan")
		if err != nil {
			t.Fatal(err)
		}
		if len(media) != 2 {
			t.Fatalf("got %d results, want the 2 anime", len(media))
		}
		if media[0].Title() != "Attack on Titan" || media[1].Title() != "Only Romaji" {
			t.Errorf("titles = %q and %q", media[0].Title(), media[1].Title())
		}
		if got["q"] != "titan" || got["type"] != "ANIME" {
			t.Errorf("query = %v, want the term and the anime filter", got)
		}
	})

	t.Run("an error object is reported rather than a json type error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"error":"Secure pipe failed"}`)
		}))
		defer srv.Close()

		_, err := (&Client{Bases: []string{srv.URL}, HTTP: srv.Client()}).Search(ctx, "titan")
		if !errors.Is(err, ErrUpstream) || !strings.Contains(err.Error(), "Secure pipe failed") {
			t.Errorf("err = %v, want the upstream reason", err)
		}
	})
}

// envelopeQuery decodes the query the client packed into the pipe envelope
func envelopeQuery(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("e"))
	if err != nil {
		t.Fatalf("undecodable envelope: %v", err)
	}
	var env struct {
		Query map[string]string `json:"query"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope is not json: %v", err)
	}
	return env.Query
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
func TestPipeRotation(t *testing.T) {
	ctx := context.Background()

	t.Run("a blocked mirror hands over and stays handed over", func(t *testing.T) {
		bad := mirror(t, blocks)
		good := mirror(t, serves(`{"ok":true}`))
		c := &Client{Bases: []string{bad.URL, good.URL}, HTTP: good.Client()}

		for range 3 {
			if _, err := c.pipe(ctx, "config", nil); err != nil {
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
		if _, err := c.pipe(ctx, "config", nil); err != nil {
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
		if _, err := c.pipe(ctx, "sources", nil); !errors.Is(err, ErrUpstream) {
			t.Fatalf("err = %v, want ErrUpstream", err)
		}
		if got := other.hits.Load(); got != 0 {
			t.Errorf("the second mirror served %d requests, want none for a backend status", got)
		}
	})

	t.Run("blocked everywhere is fatal", func(t *testing.T) {
		a, b := mirror(t, blocks), mirror(t, blocks)
		c := &Client{Bases: []string{a.URL, b.URL}, HTTP: a.Client()}
		if _, err := c.pipe(ctx, "config", nil); !errors.Is(err, ErrBlocked) {
			t.Fatalf("err = %v, want ErrBlocked", err)
		}
	})

	// reporting this as recoverable would send the fallback loop back into the
	// block for every remaining provider
	t.Run("a block outranks a later unreachable mirror", func(t *testing.T) {
		blocked := mirror(t, blocks)
		dead := httptest.NewServer(http.HandlerFunc(blocks))
		dead.Close()

		c := &Client{Bases: []string{blocked.URL, dead.URL}, HTTP: blocked.Client()}
		if _, err := c.pipe(ctx, "config", nil); !errors.Is(err, ErrBlocked) {
			t.Fatalf("err = %v, want ErrBlocked", err)
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
			if _, err := c.pipe(ctx, "sources", nil); !errors.Is(err, ErrUpstream) {
				t.Fatalf("err = %v, want ErrUpstream", err)
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
		if _, err := c.pipe(ctx, "sources", nil); !errors.Is(err, ErrUpstream) {
			t.Fatalf("err = %v, want ErrUpstream", err)
		}
		if got := other.hits.Load(); got != 0 {
			t.Errorf("the second mirror served %d requests, want none for a backend that went quiet", got)
		}
	})

	t.Run("no mirror configured is recoverable", func(t *testing.T) {
		if _, err := (&Client{HTTP: http.DefaultClient}).pipe(ctx, "config", nil); !errors.Is(err, ErrUpstream) {
			t.Fatalf("err = %v, want ErrUpstream", err)
		}
	})
}

// a rule comparing Origin against Host would reject a header set pinned to one
// domain the moment the walk moved off it
func TestPipeOriginFollowsTheMirror(t *testing.T) {
	var origin, referer string
	srv := mirror(t, func(w http.ResponseWriter, r *http.Request) {
		origin, referer = r.Header.Get("Origin"), r.Header.Get("Referer")
		io.WriteString(w, `{"ok":true}`)
	})

	c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
	if _, err := c.pipe(context.Background(), "config", nil); err != nil {
		t.Fatal(err)
	}
	if origin != srv.URL || referer != srv.URL+"/" {
		t.Errorf("origin = %q referer = %q, want them on %q", origin, referer, srv.URL)
	}
}
