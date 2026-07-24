package miruro

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// zeros streams an endless zero body for over-cap tests
type zeros struct{}

func (zeros) Read(p []byte) (int, error) { return len(p), nil }

// an endless chunked pipe body would otherwise buffer until memory runs out
func TestPipeRefusesOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.CopyN(w, zeros{}, maxPipeRaw+1)
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
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
