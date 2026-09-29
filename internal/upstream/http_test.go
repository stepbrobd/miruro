package upstream

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// a CDN keyed on the browser pair refuses a request carrying only the referer,
// which is hop's 403 on every segment, so the origin has to travel with it
func TestSetReferer(t *testing.T) {
	for _, tc := range []struct {
		referer, origin string
	}{
		{"https://krussdomi.com/", "https://krussdomi.com"},
		{"https://cdn.example.com:8443/player/x", "https://cdn.example.com:8443"},
		{"http://plain.example/", "http://plain.example"},
		{"not a url", ""},
		{"/relative", ""},
	} {
		h := http.Header{}
		SetReferer(h, tc.referer)
		if got := h.Get("Referer"); got != tc.referer {
			t.Errorf("referer %q, want %q", got, tc.referer)
		}
		if got := h.Get("Origin"); got != tc.origin {
			t.Errorf("origin for %q = %q, want %q", tc.referer, got, tc.origin)
		}
	}

	h := http.Header{}
	SetReferer(h, "")
	if len(h) != 0 {
		t.Errorf("a provider naming no referer set %v", h)
	}
}

// a provider names every url a stream fetch follows, so one naming this
// machine or its LAN must not make the program fetch it
func TestDialable(t *testing.T) {
	proxy := netip.MustParseAddrPort("127.0.0.1:7890")
	for _, tc := range []struct {
		address string
		ok      bool
	}{
		{"93.184.215.14:443", true},
		{"[2606:2800:21f:cb07:6820:80da:af6b:8b2c]:443", true},
		{"127.0.0.1:80", false},
		{"[::1]:80", false},
		{"10.0.0.1:80", false},
		{"172.16.5.4:80", false},
		{"192.168.1.1:80", false},
		{"169.254.169.254:80", false},
		{"[fe80::1]:80", false},
		{"[fd00::1]:80", false},
		{"100.100.100.100:80", false},
		{"0.0.0.0:80", false},
		{"[::]:80", false},
		{"224.0.0.1:80", false},
		// an ipv4 address mapped into ipv6 is still the ipv4 address
		{"[::ffff:192.168.1.1]:80", false},
		{"[::ffff:93.184.215.14]:443", true},
		// the environment's proxy is the one private address a dial may reach,
		// and only on its own port
		{"127.0.0.1:7890", true},
		{"127.0.0.1:7891", false},
	} {
		err := dialable(tc.address, map[netip.AddrPort]bool{proxy: true})
		if (err == nil) != tc.ok {
			t.Errorf("dialable(%s) = %v, want ok %v", tc.address, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrPrivate) {
			t.Errorf("dialable(%s) = %v, want %v", tc.address, err, ErrPrivate)
		}
	}
}

// the refusal has to hold for a real fetch, where the address is only known
// once the name resolves and the dial starts
func TestPublicRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the guarded client reached a loopback server")
	}))
	defer srv.Close()

	_, err := Public().Get(srv.URL)
	if !errors.Is(err, ErrPrivate) {
		t.Fatalf("err = %v, want %v", err, ErrPrivate)
	}
	// localhost resolves to loopback, so naming it does not get around the check
	_, err = Public().Get(strings.Replace(srv.URL, "127.0.0.1", "localhost", 1))
	if !errors.Is(err, ErrPrivate) {
		t.Fatalf("err = %v, want %v through a name", err, ErrPrivate)
	}
}

// a child of a playlist keeps the host its parent was served from unless it
// names its own, and a line's surrounding space is no part of the reference
func TestResolve(t *testing.T) {
	for _, tc := range []struct{ base, ref, want string }{
		{"https://cdn.example/a/master.m3u8", "index-f1.m3u8", "https://cdn.example/a/index-f1.m3u8"},
		{"https://cdn.example/a/master.m3u8?token=x", "seg-1.ts", "https://cdn.example/a/seg-1.ts"},
		{"https://cdn.example/a/master.m3u8", "/b/seg-1.ts", "https://cdn.example/b/seg-1.ts"},
		{"https://cdn.example/a/master.m3u8", "https://other.example/seg-1.ts", "https://other.example/seg-1.ts"},
		{"https://cdn.example/a/master.m3u8", "  seg-1.ts\r", "https://cdn.example/a/seg-1.ts"},
	} {
		got, err := Resolve(tc.base, tc.ref)
		if err != nil || got != tc.want {
			t.Errorf("Resolve(%q, %q) = %q, %v, want %q", tc.base, tc.ref, got, err, tc.want)
		}
	}
	if _, err := Resolve("https://cdn.example/a/", "%zz"); err == nil {
		t.Error("a reference that does not parse resolved")
	}
}
