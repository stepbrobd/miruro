package miruro

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseHeight(t *testing.T) {
	cases := map[string]int{
		"1080p": 1080, "720": 720, " 480p ": 480,
		"": 0, "best": 0, "worst": 0, "p": 0, "-5": 0,
	}
	for in, want := range cases {
		if got := parseHeight(in); got != want {
			t.Errorf("parseHeight(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestPickQuality(t *testing.T) {
	streams := []Stream{
		{URL: "360", Quality: "360p"},
		{URL: "1080", Quality: "1080p"},
		{URL: "720", Quality: "720p"},
		{URL: "1440", Quality: "1440p"},
		{URL: "raw", Quality: ""}, // unlabelled, ignored
	}
	check := func(q, wantURL string, wantOK bool) {
		t.Helper()
		s, ok := pickQuality(streams, q)
		if ok != wantOK || (ok && s.URL != wantURL) {
			t.Errorf("pickQuality(%q) = (%q, %v), want (%q, %v)", q, s.URL, ok, wantURL, wantOK)
		}
	}
	check("", "1440", true)     // default is best
	check("best", "1440", true) // tallest
	check("worst", "360", true) // shortest
	check("720p", "720", true)  // exact
	check("720", "720", true)   // exact, no suffix
	check("144p", "", false)    // 144 must not match 1440
	check("2160p", "", false)   // absent height

	if _, ok := pickQuality([]Stream{{Quality: ""}, {Quality: "auto"}}, "720p"); ok {
		t.Error("pickQuality matched on streams with no usable height label")
	}
}

// a token-signed master can redirect to another host or path, so relative
// variants must resolve against the URL the master was served from
func TestExpandMasterRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/cdn/v2/master.m3u8", http.StatusFound)
	})
	mux.HandleFunc("/cdn/v2/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1920x1080\nindex-1080.m3u8\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	variants, err := c.expandMaster(context.Background(), Stream{URL: srv.URL + "/master.m3u8", Kind: HLS})
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 1 {
		t.Fatalf("got %d variants, want 1", len(variants))
	}
	if want := srv.URL + "/cdn/v2/index-1080.m3u8"; variants[0].URL != want {
		t.Errorf("variant URL = %q, want %q", variants[0].URL, want)
	}
	if variants[0].Quality != "1080p" {
		t.Errorf("variant quality = %q, want %q", variants[0].Quality, "1080p")
	}
}
