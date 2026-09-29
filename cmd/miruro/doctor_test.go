package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"

	"ysun.co/miruro/internal/play"
	"ysun.co/miruro/internal/upstream"
)

// every provider row is walked through the proxy to its first segments, and a
// row names the step that failed and the host behind it, which is what named
// the failure both times the throwaway this grew from ran
func TestDoctorWalksEveryProvider(t *testing.T) {
	seg := bytes.Repeat(append([]byte{0x47}, make([]byte, 187)...), 8)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/live/master.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=9,BANDWIDTH=1\nlow.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=5\nhigh.m3u8\n")
		case r.URL.Path == "/live/low.m3u8", r.URL.Path == "/gone/master.m3u8", r.URL.Path == "/lost/v.m3u8":
			http.NotFound(w, r)
		case r.URL.Path == "/lost/master.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n")
		case strings.HasSuffix(r.URL.Path, ".m3u8"):
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:4,\ns0.ts\n#EXTINF:4,\ns1.ts\n#EXTINF:4,\ns2.ts\n#EXTINF:4,\ns3.ts\n#EXT-X-ENDLIST\n")
		case strings.HasPrefix(r.URL.Path, "/dead/"):
			w.WriteHeader(http.StatusForbidden)
		case r.URL.Path == "/clip.mp4":
			// a ranged answer is what tells the doctor asked for the head only
			http.ServeContent(w, r, "clip.mp4", time.Time{}, strings.NewReader(episodeBody+strings.Repeat("x", 2*probeRange)))
		default:
			w.Write(seg)
		}
	}))
	defer cdn.Close()

	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"live": {1}, "dead": {1}, "clip": {1}, "gone": {1}, "lost": {1}}, replies: map[string]reply{
		"live": streams(upstream.Stream{URL: cdn.URL + "/live/master.m3u8", Kind: upstream.HLS, Server: "Vid"}),
		"dead": streams(upstream.Stream{URL: cdn.URL + "/dead/media.m3u8", Kind: upstream.HLS, Server: "HD-1"}),
		"clip": streams(upstream.Stream{URL: cdn.URL + "/clip.mp4", Kind: upstream.MP4, Server: "Mp4"}),
		"gone": streams(upstream.Stream{URL: cdn.URL + "/gone/master.m3u8", Kind: upstream.HLS, Server: "G"}),
		"lost": streams(upstream.Stream{URL: cdn.URL + "/lost/master.m3u8", Kind: upstream.HLS, Server: "L"}),
	}}
	st := resolver(upstream.Sub, b)
	st.cfg = config{Quality: "best"}
	st.cat.Sub = []upstream.Episode{{Number: 1}}

	var out bytes.Buffer
	d := &doctor{out: &out}
	d.providers(context.Background(), st)
	got := out.String()
	host := strings.TrimPrefix(cdn.URL, "http://")

	rows := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(got), "\n") {
		name, _, _ := strings.Cut(line, "  ")
		rows[name] = line
	}
	for name, want := range map[string][]string{
		"episode 1": {" ok ", "clip dead gone live lost serve it"},
		"live sub":  {" ok ", "Vid hls", "master 200 from " + host, "media 200", "segments 200 200 200 from " + host, "served 3 refused 0"},
		"dead sub":  {" fail ", "segments 403 403 403", "served 0 refused 3", "nothing relayed"},
		"clip sub":  {" ok ", "Mp4 mp4", "body 206 from " + host, "served 1 refused 0"},
		// a step that failed ends the walk rather than reading an error page on
		"gone sub": {" fail ", "master 404 from " + host + ", served 0 refused 0, nothing relayed"},
		"lost sub": {" fail ", "master 200 from " + host + ", media 404, served 0 refused 0, nothing relayed"},
	} {
		for _, w := range want {
			if !strings.Contains(rows[name], w) {
				t.Errorf("%s row lacks %q:\n%s", name, w, got)
			}
		}
	}
	if d.failed != 3 {
		t.Errorf("failed = %d, want the dead, gone and lost providers", d.failed)
	}
}

// a segment that never answers is named by its index and its host, and the
// proxied address the request carried stays out of the row
func TestWalkStreamNamesASegmentThatNeverAnswers(t *testing.T) {
	stall := make(chan struct{})
	defer close(stall)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:4,\ns0.ts\n#EXT-X-ENDLIST\n")
			return
		}
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer cdn.Close()
	px, err := play.StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	got, err := walkStream(ctx, px, upstream.Stream{URL: cdn.URL + "/media.m3u8", Kind: upstream.HLS})
	if err == nil || !strings.Contains(err.Error(), "segment 0: ") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("err = %v, want the segment named without the proxied address", err)
	}
	if want := "segments none from " + strings.TrimPrefix(cdn.URL, "http://"); !strings.Contains(got, want) {
		t.Errorf("walk = %q, want %q", got, want)
	}
}

// the setup checks name each api origin that answered and each that did not,
// read the config file by its own keys, and fail the run for any failure
func TestDoctorChecksTheSetup(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"streaming":{}}`)
	}))
	defer up.Close()
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()

	dir := t.TempDir()
	t.Cleanup(xdg.Reload)
	t.Setenv("XDG_CONFIG_HOME", dir)
	xdg.Reload()
	ctx := context.Background()
	cfg := config{Mirrors: []string{up.URL, down.URL}}
	rows := func(out string) map[string]string {
		m := map[string]string{}
		for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			name, _, _ := strings.Cut(line, " ")
			m[name] += line + "\n"
		}
		return m
	}

	var out bytes.Buffer
	err := (&doctor{out: &out}).run(ctx, cfg, nil)
	got := rows(out.String())
	if !strings.Contains(got["config"], "ok   no file at") {
		t.Errorf("a missing config file is not a default setup:\n%s", out.String())
	}
	upHost, downHost := strings.TrimPrefix(up.URL, "http://"), strings.TrimPrefix(down.URL, "http://")
	if !strings.Contains(got["api"], "ok   "+upHost) || !strings.Contains(got["api"], "fail "+downHost) {
		t.Errorf("the api rows do not tell the live origin from the dead one:\n%s", out.String())
	}
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Errorf("err = %v, want the dead origin failing the run", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "miruro"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "miruro", "config.toml"), []byte("quality = \"best\"\nqualty = \"720p\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	(&doctor{out: &out}).run(ctx, config{Mirrors: []string{up.URL}}, nil)
	if got := rows(out.String()); !strings.Contains(got["config"], `fail unknown key "qualty"`) {
		t.Errorf("a typo in the config passed:\n%s", out.String())
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := (&doctor{out: io.Discard}).run(canceled, config{Mirrors: []string{up.URL}}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the interruption", err)
	}

	// with a player and the download tools on the path, a good config and a
	// live origin, nothing fails and nothing past the setup is checked
	if runtime.GOOS == "windows" {
		return
	}
	bin := t.TempDir()
	for _, name := range []string{"mpv", "ffmpeg", "ffprobe"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	if err := os.WriteFile(filepath.Join(dir, "miruro", "config.toml"), []byte("quality = \"best\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	d := &doctor{out: &out}
	err = d.run(ctx, config{Player: "mpv", Mirrors: []string{up.URL}}, nil)
	got = rows(out.String())
	if runtime.GOOS == "darwin" && strings.Contains(got["player"], "iina") {
		t.Skip("an installed IINA wins detection over the mpv on the path")
	}
	if err != nil || d.failed != 0 {
		t.Errorf("a clean setup failed %d checks: %v\n%s", d.failed, err, out.String())
	}
	if want := "ok   " + filepath.Join(dir, "miruro", "config.toml"); !strings.Contains(got["config"], want) {
		t.Errorf("a good config is not named as ok:\n%s", out.String())
	}
	if !strings.Contains(got["player"], "ok   mpv at "+filepath.Join(bin, "mpv")) {
		t.Errorf("the player on the path is not named:\n%s", out.String())
	}
	if got["title"] != "" || got["catalog"] != "" {
		t.Errorf("a run without a query checked a title:\n%s", out.String())
	}
}

// what downloads use and playback does without is a warning when missing and
// not a failure, and a present one names where it was found
func TestDoctorLooksForTheDownloadTools(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no shell script executables")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var out bytes.Buffer
	d := &doctor{out: &out}
	d.tools()
	got := out.String()
	if !strings.Contains(got, "ffmpeg         ok   "+filepath.Join(bin, "ffmpeg")) {
		t.Errorf("the ffmpeg on the path is not named:\n%s", got)
	}
	if !strings.Contains(got, "ffprobe        warn not found") {
		t.Errorf("a missing ffprobe is not a warning:\n%s", got)
	}
	if d.failed != 0 {
		t.Errorf("failed = %d, want a missing download tool to fail nothing", d.failed)
	}
}

// the variant a player opens first is the highest advertised bandwidth, read
// whole so an AVERAGE-BANDWIDTH listed first is not taken for it
func TestBestURI(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"#EXTM3U\n#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=9,BANDWIDTH=1\nlow.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=5\nhigh.m3u8\n", "high.m3u8"},
		{"#EXTM3U\n#EXT-X-STREAM-INF:RESOLUTION=1x1\nonly.m3u8\n", "only.m3u8"},
		{"#EXTM3U\n#EXTINF:4,\ns0.ts\n#EXT-X-ENDLIST\n", ""},
		// a tag between a variant and its uri is no uri, and a tie keeps the first
		{"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=5\n#EXT-X-PROGRAM-DATE-TIME:x\nfirst.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=5\nsecond.m3u8\n", "first.m3u8"},
	} {
		if got := bestURI([]byte(tc.body)); got != tc.want {
			t.Errorf("bestURI = %q, want %q for\n%s", got, tc.want, tc.body)
		}
	}
}
