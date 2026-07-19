package miruro

import "testing"

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
