package upstream

import (
	"net/http"
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
