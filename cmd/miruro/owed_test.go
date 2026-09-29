package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrg/xdg"

	"ysun.co/miruro/internal/upstream"
)

// vtt is a subtitle body a sidecar check reads as WebVTT
const vtt = "WEBVTT\n\n00:00.000 --> 00:01.000\nhi\n"

// a record is found by the video it names whichever way a caller spells the
// path, one naming no provider is nothing owed, and settling a record, or a
// video owing nothing, says nothing
func TestOwedRecords(t *testing.T) {
	stateRoot(t)
	dir := t.TempDir()
	video := filepath.Join(dir, "Show - E1.mp4")
	if err := (owed{Video: video, Provider: "bonk", Category: upstream.Sub, Server: "HD-1"}).record(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if o, ok := owing("Show - E1.mp4"); !ok || o.Provider != "bonk" {
		t.Errorf("owing by a relative path = %+v, %v, want the record written by the absolute one", o, ok)
	}

	said := captureLog(t)
	settle(video)
	settle(video)
	if _, ok := owing(video); ok {
		t.Error("a settled record is still owed")
	}
	if strings.Contains(said.String(), "not cleared") {
		t.Errorf("settling warned:\n%s", said)
	}

	if err := os.WriteFile(owedPath(video), []byte(`{"video":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := owing(video); ok {
		t.Error("a record naming no provider is owed")
	}
}

// owedCDN serves a video and one sidecar per name, each sidecar only to the
// referer named for it, and 404s a sidecar named with no referer
func owedCDN(t *testing.T, referers map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		switch ref, ok := referers[name]; {
		case name == "live.mp4":
			io.WriteString(w, episodeBody)
		case ok && ref != "" && r.Header.Get("Referer") == ref:
			io.WriteString(w, vtt)
		case ok && ref != "":
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// owedResult is a bonk result of two servers with referers of their own and the
// sidecars named, in the order the servers are given
func owedResult(cdn string, servers []string, subs ...string) reply {
	return func(upstream.Category) (*upstream.Result, error) {
		res := &upstream.Result{}
		for _, s := range servers {
			res.Streams = append(res.Streams, upstream.Stream{
				URL: cdn + "/live.mp4", Kind: upstream.MP4, Server: s,
				Referer: "https://" + strings.ToLower(s) + ".example/",
			})
		}
		for _, name := range subs {
			res.Subtitles = append(res.Subtitles, upstream.Subtitle{File: cdn + "/" + name, Label: name, Lang: strings.TrimSuffix(name, ".vtt")})
		}
		return res, nil
	}
}

// the sidecars take the referer of the server the video came from wherever the
// provider lists it now, and any playable server's when it lists that one no
// more
func TestResubtitleTakesTheRecordedServersReferer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		servers []string
		server  string
		ref     string
	}{
		{"listed second", []string{"HD-1", "HD-2"}, "HD-2", "https://hd-2.example/"},
		{"listed first", []string{"HD-2", "HD-1"}, "HD-2", "https://hd-2.example/"},
		{"no longer listed", []string{"HD-1", "HD-2"}, "HD-9", "https://hd-1.example/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cdn := owedCDN(t, map[string]string{"en.vtt": tc.ref})
			b := &stub{t: t, name: "miruro", sub: map[string][]float64{"bonk": {1}}, replies: map[string]reply{
				"bonk": owedResult(cdn.URL, tc.servers, "en.vtt"),
			}}
			sv, dir := newSaver(t, b)
			video := filepath.Join(dir, "Show - E1.mp4")
			o := owed{Video: video, Provider: "bonk", Category: upstream.Sub, Server: tc.server}
			if err := o.record(); err != nil {
				t.Fatal(err)
			}
			asked, missed, err := sv.resubtitle(context.Background(), 1, o)
			if !asked || missed != 0 || err != nil {
				t.Fatalf("resubtitle = %v, %d, %v, want the sidecar fetched", asked, missed, err)
			}
			if _, ok := owing(video); ok {
				t.Error("a record whose sidecars all arrived is still owed")
			}
		})
	}
}

// a record stays while any of its sidecars is still missing
func TestResubtitleKeepsTheRecordWhileATrackIsMissing(t *testing.T) {
	cdn := owedCDN(t, map[string]string{"en.vtt": "https://hd-1.example/"})
	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"bonk": {1}}, replies: map[string]reply{
		"bonk": owedResult(cdn.URL, []string{"HD-1"}, "en.vtt", "pt.vtt"),
	}}
	sv, dir := newSaver(t, b)
	o := owed{Video: filepath.Join(dir, "Show - E1.mp4"), Provider: "bonk", Category: upstream.Sub, Server: "HD-1"}
	if err := o.record(); err != nil {
		t.Fatal(err)
	}
	if asked, missed, err := sv.resubtitle(context.Background(), 1, o); !asked || missed != 1 || err != nil {
		t.Fatalf("resubtitle = %v, %d, %v, want one track still missing", asked, missed, err)
	}
	if _, ok := owing(o.Video); !ok {
		t.Error("a record with a track still missing was settled")
	}
}

// onDisk puts episode 1 on disk owing its sidecars to bonk's sub rendition
func onDisk(t *testing.T, dir string) owed {
	t.Helper()
	video := filepath.Join(dir, "Show - E1.mp4")
	if err := os.WriteFile(video, []byte(episodeBody), 0o644); err != nil {
		t.Fatal(err)
	}
	o := owed{Video: video, Provider: "bonk", Category: upstream.Sub, Server: "HD-1"}
	if err := o.record(); err != nil {
		t.Fatal(err)
	}
	return o
}

// owed sidecars the provider cannot serve this run are warned about and stay
// owed, and the episode is on disk all the same, so the run does not fail
func TestDownloadKeepsOwedSubtitlesThroughAFailure(t *testing.T) {
	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"bonk": {1}}, replies: map[string]reply{"bonk": down}}
	sv, dir := newSaver(t, b)
	o := onDisk(t, dir)
	said := captureLog(t)
	out := printed(t, func() error { return sv.download(context.Background(), []float64{1}, Pin{}) })
	if want := fmt.Sprintf("1 episode already in %s\n", dir); out != want {
		t.Errorf("printed %q, want %q", out, want)
	}
	if !strings.Contains(said.String(), "missing subtitles not fetched") {
		t.Errorf("the failure was not warned about:\n%s", said)
	}
	if _, ok := owing(o.Video); !ok {
		t.Error("a failed fetch settled the record")
	}
}

// a run interrupted while fetching owed sidecars stops as interrupted rather
// than warning it away as a failure
func TestDownloadStopsOnAnInterruptedOwedFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	defer cdn.Close()
	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"bonk": {1}}, replies: map[string]reply{
		"bonk": owedResult(cdn.URL, []string{"HD-1"}, "en.vtt"),
	}}
	sv, dir := newSaver(t, b)
	o := onDisk(t, dir)
	if err := sv.download(ctx, []float64{1}, Pin{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the interruption", err)
	}
	if _, ok := owing(o.Video); !ok {
		t.Error("an interrupted fetch settled the record")
	}
}

// the failure count is of the downloads a run made, which an owed episode on
// disk is not one of
func TestDownloadCountsOnlyItsDownloadsWhenOneFails(t *testing.T) {
	cdn := owedCDN(t, map[string]string{"en.vtt": "https://hd-1.example/"})
	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"bonk": {1, 2}}, replies: map[string]reply{
		"bonk": func(upstream.Category) (*upstream.Result, error) {
			return &upstream.Result{
				Streams:   []upstream.Stream{{URL: cdn.URL + "/gone.mp4", Kind: upstream.MP4, Server: "HD-1", Referer: "https://hd-1.example/"}},
				Subtitles: []upstream.Subtitle{{File: cdn.URL + "/en.vtt", Label: "English", Lang: "en"}},
			}, nil
		},
	}}
	sv, dir := newSaver(t, b)
	onDisk(t, dir)
	err := sv.download(context.Background(), []float64{1, 2}, Pin{})
	if err == nil || err.Error() != "1 download of 1 failed" {
		t.Errorf("err = %v, want the one download counted alone", err)
	}
}

// a video that failed to download is owed nothing, whatever sidecars its source
// lists, since there is no episode on disk to owe them to
func TestSaveOwesNothingForAVideoThatFailed(t *testing.T) {
	cdn := owedCDN(t, map[string]string{"en.vtt": "https://hd-1.example/"})
	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"bonk": {1}}, replies: map[string]reply{
		"bonk": func(upstream.Category) (*upstream.Result, error) {
			return &upstream.Result{
				Streams:   []upstream.Stream{{URL: cdn.URL + "/gone.mp4", Kind: upstream.MP4, Server: "HD-1", Referer: "https://hd-1.example/"}},
				Subtitles: []upstream.Subtitle{{File: cdn.URL + "/en.vtt", Label: "English", Lang: "en"}},
			}, nil
		},
	}}
	sv, dir := newSaver(t, b)
	if _, _, _, err := sv.save(context.Background(), 1, nil); err == nil {
		t.Fatal("a video that 404s saved")
	}
	if _, ok := owing(filepath.Join(dir, "Show - E1.mp4")); ok {
		t.Error("a failed video is owed sidecars")
	}
}

// a record that cannot be written is warned about, since without it a rerun
// never fetches the missing sidecars
func TestSaveWarnsWhenItCannotRecordWhatIsOwed(t *testing.T) {
	cdn := owedCDN(t, map[string]string{"en.vtt": ""})
	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"bonk": {1}}, replies: map[string]reply{
		"bonk": owedResult(cdn.URL, []string{"HD-1"}, "en.vtt"),
	}}
	sv, _ := newSaver(t, b)
	// a file where the state directory would go leaves nowhere to write
	if err := os.WriteFile(filepath.Join(xdg.StateHome, "miruro"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	said := captureLog(t)
	if _, _, missed, err := sv.save(context.Background(), 1, nil); err != nil || missed != 1 {
		t.Fatalf("save = %d missed, %v, want the video saved and its sidecar missed", missed, err)
	}
	if !strings.Contains(said.String(), "missing subtitles not recorded") {
		t.Errorf("the lost record was not warned about:\n%s", said)
	}
}
