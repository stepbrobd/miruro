package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
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
		{URL: "raw", Quality: ""}, // unlabeled, ignored
	}
	check := func(q, wantURL string, wantOK bool) {
		t.Helper()
		s, ok := pickQuality(streams, q)
		if ok != wantOK || (ok && s.URL != wantURL) {
			t.Errorf("pickQuality(%q) = (%q, %v), want (%q, %v)", q, s.URL, ok, wantURL, wantOK)
		}
	}
	check("", "1440", true)      // default is best
	check("best", "1440", true)  // tallest
	check("worst", "360", true)  // shortest
	check("720p", "720", true)   // exact
	check("720", "720", true)    // exact, no suffix
	check("144p", "", false)     // 144 must not match 1440
	check("240p", "", false)     // nothing at or below
	check("2160p", "1440", true) // absent height falls to the nearest lower
	check("1000p", "720", true)  // nearest lower between labels

	if _, ok := pickQuality([]Stream{{Quality: ""}, {Quality: "auto"}}, "720p"); ok {
		t.Error("pickQuality matched on streams with no usable height label")
	}
}

// failTransport fails the test on any network use
// a labeled request must be satisfied without touching the wire
type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("unexpected network call to %s", r.URL)
	return nil, errors.New("network use forbidden")
}

// top is the stream a caller plays, the head of Rank
func top(t *testing.T, hc *http.Client, r *Result, quality string) Stream {
	t.Helper()
	ranked := Rank(context.Background(), hc, r, quality)
	if len(ranked) == 0 {
		t.Fatal("Rank returned nothing playable")
	}
	return ranked[0]
}

func TestRankHead(t *testing.T) {
	ctx := context.Background()

	t.Run("labeled hls match needs no network", func(t *testing.T) {
		hc := &http.Client{Transport: failTransport{t}}
		r := &Result{Streams: []Stream{
			{URL: "u1080", Kind: HLS, Quality: "1080p"},
			{URL: "u720", Kind: HLS, Quality: "720p"},
		}}
		s := top(t, hc, r, "720p")
		if s.URL != "u720" {
			t.Errorf("selected %q, want u720", s.URL)
		}
	})

	t.Run("unlabeled hls restricts the master to the expanded height", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "#EXTM3U\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1920x1080\nindex-1080.m3u8\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1280x720\nindex-720.m3u8\n")
		}))
		defer srv.Close()

		hc := srv.Client()
		r := &Result{Streams: []Stream{{URL: srv.URL + "/master.m3u8", Kind: HLS}}}
		s := top(t, hc, r, "720p")
		// the master travels whole rather than the variant URL out of it, since a
		// bare variant loses the audio renditions the master associates
		if want := srv.URL + "/master.m3u8"; s.URL != want {
			t.Errorf("selected %q, want the master %q", s.URL, want)
		}
		if s.Height != 720 {
			t.Errorf("height = %d, want 720", s.Height)
		}
		if s.Quality != "720p" {
			t.Errorf("quality = %q, want 720p", s.Quality)
		}

		// the same master unrestricted stays reachable behind the pick as the
		// fallback when its restricted variant will not play
		ranked := Rank(context.Background(), hc, r, "720p")
		if len(ranked) != 2 || ranked[1].URL != s.URL || ranked[1].Height != 0 {
			t.Errorf("ranked = %+v, want the unrestricted master second", ranked)
		}
	})

	// worst reads the heights off the master when the streams name none, and
	// restricts the master rather than following the variant out of it
	t.Run("worst over an unlabeled master takes its shortest height", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "#EXTM3U\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=3,RESOLUTION=1920x1080\nindex-1080.m3u8\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=640x360\nindex-360.m3u8\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=2,RESOLUTION=1280x720\nindex-720.m3u8\n")
		}))
		defer srv.Close()

		r := &Result{Streams: []Stream{{URL: srv.URL + "/master.m3u8", Kind: HLS}}}
		s := top(t, srv.Client(), r, "worst")
		if s.URL != srv.URL+"/master.m3u8" || s.Height != 360 || s.Quality != "360p" {
			t.Errorf("selected %+v, want the master restricted to 360", s)
		}
	})

	t.Run("failed master expansion falls back to the first hls", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()

		hc := srv.Client()
		first := Stream{URL: srv.URL + "/master.m3u8", Kind: HLS}
		r := &Result{Streams: []Stream{first, {URL: srv.URL + "/other.m3u8", Kind: HLS}}}
		s := top(t, hc, r, "720p")
		if s.URL != first.URL {
			t.Errorf("selected %q, want the first hls %q", s.URL, first.URL)
		}
	})

	t.Run("mp4 only picks by label then falls back", func(t *testing.T) {
		hc := &http.Client{Transport: failTransport{t}}
		r := &Result{Streams: []Stream{
			{URL: "m480", Kind: MP4, Quality: "480p"},
			{URL: "m720", Kind: MP4, Quality: "720p"},
		}}
		s := top(t, hc, r, "720p")
		if s.URL != "m720" {
			t.Errorf("selected %q, want m720", s.URL)
		}
		s = top(t, hc, r, "2160p")
		if s.URL != "m720" {
			t.Errorf("selected %q, want the nearest lower m720", s.URL)
		}
		s = top(t, hc, r, "360p")
		if s.URL != "m480" {
			t.Errorf("selected %q, want the first mp4 m480", s.URL)
		}
	})

	t.Run("empty result ranks nothing", func(t *testing.T) {
		hc := &http.Client{Transport: failTransport{t}}
		if got := Rank(ctx, hc, &Result{}, "best"); len(got) != 0 {
			t.Errorf("Rank over an empty result = %v, want nothing playable", got)
		}
	})
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

	hc := srv.Client()
	variants, err := expandMaster(context.Background(), hc, Stream{URL: srv.URL + "/master.m3u8", Kind: HLS})
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

// the api mirrors the html5 track kinds, so a thumbnails sprite index arrives on
// the same list as dialogue and must never reach a player as subtitles
// every other body a host answers is capped, and a master is the one read
// that had none
func TestExpandMasterRefusesAnOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n")
		filler := []byte(strings.Repeat("#", 1<<20-1) + "\n")
		for range maxMaster/len(filler) + 2 {
			w.Write(filler)
		}
	}))
	defer srv.Close()
	_, err := expandMaster(context.Background(), srv.Client(), Stream{URL: srv.URL + "/master.m3u8", Kind: HLS})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want the body refused", err)
	}
}

// the client bounds only the wait for headers, so a host that sends them and
// then stalls mid-master would hold the walk on this one stream for good
func TestRankBoundsAStalledMaster(t *testing.T) {
	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1280x720\n")
		w.(http.Flusher).Flush()
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	// the handler blocks until stall closes, and Close waits on the handler, so
	// this defer has to run first
	defer srv.Close()
	defer close(stall)

	defer func(d time.Duration) { masterTimeout = d }(masterTimeout)
	masterTimeout = 100 * time.Millisecond

	master := Stream{URL: srv.URL + "/master.m3u8", Kind: HLS}
	done := make(chan []Stream, 1)
	go func() { done <- Rank(context.Background(), srv.Client(), &Result{Streams: []Stream{master}}, "720p") }()
	select {
	case ranked := <-done:
		// a master that ran out of time is a failed expansion, which leaves the
		// master leading unrestricted
		if len(ranked) != 1 || ranked[0].URL != master.URL || ranked[0].Height != 0 {
			t.Errorf("ranked = %+v, want the master unrestricted", ranked)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Rank never returned from a master whose body stalled")
	}
}

// one provider serves an episode from several hosts, and the one it lists first
// can be the dead one, so the rest have to stay reachable behind it
func TestRank(t *testing.T) {
	ctx := context.Background()
	hc := &http.Client{Transport: failTransport{t}}

	r := &Result{Streams: []Stream{
		{URL: "hd1", Kind: HLS, Quality: "1080p"},
		{URL: "embed", Kind: Embed},
		{URL: "hd2", Kind: HLS, Quality: "720p"},
		{URL: "direct", Kind: MP4, Quality: "480p"},
	}}

	urls := func(quality string) []string {
		t.Helper()
		var out []string
		for _, s := range Rank(ctx, hc, r, quality) {
			out = append(out, s.URL)
		}
		return out
	}

	if got, want := urls("best"), []string{"hd1", "hd2", "direct"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Rank(best) = %v, want %v", got, want)
	}
	// the quality pick leads and is not repeated further down
	if got, want := urls("720p"), []string{"hd2", "hd1", "direct"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Rank(720p) = %v, want %v", got, want)
	}
	if got := Rank(ctx, hc, &Result{Streams: []Stream{{URL: "embed", Kind: Embed}}}, "best"); len(got) != 0 {
		t.Errorf("Rank over an embed-only result = %v, want nothing playable", got)
	}

	// the order the api lists streams in is not a promise, the flag is
	flagged := &Result{Streams: []Stream{
		{URL: "second", Kind: HLS, Server: "HD-2"},
		{URL: "first", Kind: HLS, Server: "HD-1", Default: true},
	}}
	if got := Rank(ctx, hc, flagged, "best"); len(got) != 2 || got[0].URL != "first" || got[1].URL != "second" {
		t.Errorf("Rank ignored the provider's default flag: %+v", got)
	}
}

// cdn answers the master it holds for a url and 404 for any other, the way a
// labeled stream that is no master fails its expansion
type cdn map[string]string

func (c cdn) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := c[r.URL.String()]
	status := http.StatusOK
	if !ok {
		status = http.StatusNotFound
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

// masterOf is a master playlist with one variant per height, in the order given
func masterOf(heights ...int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for _, h := range heights {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d\nindex-%d.m3u8\n", h, h*16/9, h, h)
	}
	return b.String()
}

// the head is what a caller plays and the rest is the order it retries in, so
// each case asserts the whole list, a restricted master written url@height
func TestRankQuality(t *testing.T) {
	const master = "https://cdn.test/master.m3u8"
	hls := func(url, quality string) Stream { return Stream{URL: url, Kind: HLS, Quality: quality} }
	mp4 := func(url, quality string) Stream { return Stream{URL: url, Kind: MP4, Quality: quality} }

	cases := []struct {
		name    string
		streams []Stream
		// hosts serves the masters a case expands, and nil forbids the network
		hosts   cdn
		quality string
		want    []string
	}{
		{
			name:    "best takes the tallest labeled hls when listed lowest first",
			streams: []Stream{hls("u360", "360p"), hls("u720", "720p"), hls("u1080", "1080p")},
			quality: "best",
			want:    []string{"u1080", "u360", "u720"},
		},
		{
			name:    "the empty quality is best",
			streams: []Stream{hls("u360", "360p"), hls("u720", "720p"), hls("u1080", "1080p")},
			quality: "",
			want:    []string{"u1080", "u360", "u720"},
		},
		{
			name:    "best passes over an unlabeled master for the tallest label",
			streams: []Stream{hls(master, ""), hls("u480", "480p"), hls("u720", "720p")},
			quality: "best",
			want:    []string{"u720", master, "u480"},
		},
		{
			name:    "best with no hls label hands over the first hls unexpanded",
			streams: []Stream{hls(master, ""), hls("u2", "")},
			quality: "best",
			want:    []string{master, "u2"},
		},
		{
			name:    "best keeps hls ahead of a taller mp4",
			streams: []Stream{mp4("m1080", "1080p"), hls("u360", "360p"), hls("u720", "720p")},
			quality: "best",
			want:    []string{"u720", "u360", "m1080"},
		},
		{
			name:    "best between equal heights keeps the provider order",
			streams: []Stream{hls("u720", "720p"), hls("a1080", "1080p"), hls("b1080", "1080p")},
			quality: "best",
			want:    []string{"a1080", "u720", "b1080"},
		},
		{
			name:    "worst between equal heights keeps the provider order",
			streams: []Stream{hls("u720", "720p"), hls("a360", "360p"), hls("b360", "360p")},
			quality: "worst",
			want:    []string{"a360", "u720", "b360"},
		},
		{
			name:    "an exact height between equal labels keeps the provider order",
			streams: []Stream{hls("u1080", "1080p"), hls("a720", "720p"), hls("b720", "720p")},
			quality: "720p",
			want:    []string{"a720", "u1080", "b720"},
		},
		{
			name:    "a lower height between equal labels keeps the provider order",
			streams: []Stream{hls("u1080", "1080p"), hls("a480", "480p"), hls("b480", "480p")},
			hosts:   cdn{},
			quality: "720p",
			want:    []string{"a480", "u1080", "b480"},
		},
		{
			name:    "a missing height falls to the nearest lower label",
			streams: []Stream{hls("u360", "360p"), hls("u720", "720p")},
			hosts:   cdn{},
			quality: "1080p",
			want:    []string{"u720", "u360"},
		},
		{
			name:    "a missing height restricts an unlabeled master to its nearest lower variant",
			streams: []Stream{hls(master, "")},
			hosts:   cdn{master: masterOf(480, 720)},
			quality: "1080p",
			want:    []string{master + "@720", master},
		},
		{
			name:    "an exact height in the master beats a lower label",
			streams: []Stream{hls(master, ""), hls("u480", "480p")},
			hosts:   cdn{master: masterOf(480, 720)},
			quality: "720p",
			want:    []string{master + "@720", master, "u480"},
		},
		{
			name:    "a taller variant beats a shorter label",
			streams: []Stream{hls(master, ""), hls("u360", "360p")},
			hosts:   cdn{master: masterOf(480, 720)},
			quality: "1080p",
			want:    []string{master + "@720", master, "u360"},
		},
		{
			name:    "a taller label beats a shorter variant",
			streams: []Stream{hls(master, ""), hls("u720", "720p")},
			hosts:   cdn{master: masterOf(360, 480)},
			quality: "1080p",
			want:    []string{"u720", master},
		},
		{
			name:    "a label wins a tie with a variant",
			streams: []Stream{hls(master, ""), hls("u720", "720p")},
			hosts:   cdn{master: masterOf(480, 720)},
			quality: "1080p",
			want:    []string{"u720", master},
		},
		{
			name:    "nothing lower keeps the first hls unrestricted",
			streams: []Stream{hls("u1080", "1080p"), hls("u720", "720p")},
			hosts:   cdn{},
			quality: "480p",
			want:    []string{"u1080", "u720"},
		},
		{
			name:    "nothing lower in the master keeps it unrestricted",
			streams: []Stream{hls(master, "")},
			hosts:   cdn{master: masterOf(720, 1080)},
			quality: "480p",
			want:    []string{master},
		},
		{
			name:    "a lower hls label stays ahead of an exact mp4",
			streams: []Stream{mp4("m1080", "1080p"), hls("u480", "480p")},
			hosts:   cdn{},
			quality: "1080p",
			want:    []string{"u480", "m1080"},
		},
		{
			name:    "a missing height falls to the nearest lower mp4",
			streams: []Stream{mp4("m480", "480p"), mp4("m720", "720p")},
			quality: "1080p",
			want:    []string{"m720", "m480"},
		},
		{
			name:    "nothing lower among mp4 keeps the first",
			streams: []Stream{mp4("m720", "720p"), mp4("m480", "480p")},
			quality: "360p",
			want:    []string{"m720", "m480"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hc := &http.Client{Transport: failTransport{t}}
			if tc.hosts != nil {
				hc.Transport = tc.hosts
			}
			var got []string
			for _, s := range Rank(context.Background(), hc, &Result{Streams: tc.streams}, tc.quality) {
				if s.Height > 0 {
					got = append(got, fmt.Sprintf("%s@%d", s.URL, s.Height))
				} else {
					got = append(got, s.URL)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Rank(%q) = %v, want %v", tc.quality, got, tc.want)
			}
		})
	}
}

// the api marks some streams inactive, and offering one costs a playback
// attempt that cannot work
func TestDeadStreamsAreSkipped(t *testing.T) {
	hc := &http.Client{Transport: failTransport{t}}
	res := &Result{Streams: []Stream{
		{URL: "dead", Kind: HLS, Dead: true, Default: true},
		{URL: "live", Kind: HLS},
	}}
	got := Rank(context.Background(), hc, res, "best")
	if len(got) != 1 || got[0].URL != "live" {
		t.Errorf("Rank = %v, want the dead stream dropped", got)
	}
	if !res.Playable() {
		t.Error("a result with one live stream must stay playable")
	}

	// Playable has to agree with Rank, or the caller accepts a provider and then
	// fails on ErrNoStream outside the fallback loop
	only := &Result{Streams: []Stream{{URL: "dead", Kind: HLS, Dead: true}}}
	if only.Playable() {
		t.Error("a result whose only stream is dead must not report playable")
	}
	if got := Rank(context.Background(), hc, only, "best"); len(got) != 0 {
		t.Errorf("Rank = %v, want nothing", got)
	}
}
