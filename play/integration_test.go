package play

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"ysun.co/miruro"
)

// tensura is a season with every provider carrying episode 1, so one title
// exercises the whole provider matrix
const tensura = 108511

// segmentSample bounds how much of an episode the integration run fetches
// the cache logic is per segment, so a handful proves the path without pulling
// a whole episode
const segmentSample = 3

// TestIntegrationProviderDownloads drives real providers end to end, covering
// master resolution, AES-128 key caching, segment fetch, resume and cleanup
// it is skipped unless MIRURO_INTEGRATION is set because it needs the network
// and the upstream catalog
func TestIntegrationProviderDownloads(t *testing.T) {
	if os.Getenv("MIRURO_INTEGRATION") == "" {
		t.Skip("set MIRURO_INTEGRATION=1 to run against live providers")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client := miruro.New()
	cat, err := client.Episodes(ctx, tensura)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}

	px, err := StartProxy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	var covered int
	for code, provider := range cat.Providers {
		t.Run(code, func(t *testing.T) {
			eps := provider.Episodes(miruro.Sub)
			if len(eps) == 0 {
				t.Skip("provider carries no sub episodes")
			}
			res, err := client.Sources(ctx, eps[0].ID, code, miruro.Sub)
			if err != nil {
				t.Skipf("provider did not resolve, an upstream condition rather than a defect: %v", err)
			}
			stream, err := client.Select(ctx, res, "")
			if err != nil {
				t.Skipf("no selectable stream: %v", err)
			}
			if stream.Kind != miruro.HLS {
				t.Skipf("kind %s does not exercise the segment cache", stream.Kind)
			}

			pl, err := resolvePlaylist(ctx, px.hc, px.Stream(stream).URL)
			if err != nil {
				t.Skipf("playlist not reachable, an upstream condition: %v", err)
			}
			t.Logf("segments=%d encrypted=%v", len(pl.segAt), pl.keyURI != "")
			pl = head(pl, segmentSample)

			dir := t.TempDir()
			cache := filepath.Join(dir, "cache")
			dest := filepath.Join(dir, "out.mp4")

			if err := os.MkdirAll(cache, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := reconcile(cache, pl); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			key, err := cacheKey(ctx, px.hc, pl, cache)
			if err != nil {
				t.Fatalf("key: %v", err)
			}
			if pl.keyURI != "" && key == "" {
				t.Fatal("an encrypted playlist cached no key")
			}
			if err := fetchSegments(ctx, px.hc, pl, cache, nil); err != nil {
				t.Fatalf("segments: %v", err)
			}

			// every sampled segment must be on disk before the remux
			for n := range pl.segAt {
				fi, err := os.Stat(filepath.Join(cache, segName(n)))
				if err != nil || fi.Size() == 0 {
					t.Fatalf("segment %d missing or empty: %v", n, err)
				}
			}

			// drop one segment and refetch, which is what a resumed run does
			gone := filepath.Join(cache, segName(0))
			before, err := os.Stat(gone)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(gone); err != nil {
				t.Fatal(err)
			}
			if err := fetchSegments(ctx, px.hc, pl, cache, nil); err != nil {
				t.Fatalf("resume: %v", err)
			}
			after, err := os.Stat(gone)
			if err != nil {
				t.Fatalf("resume did not restore the segment: %v", err)
			}
			if after.Size() != before.Size() {
				t.Errorf("resumed segment is %d bytes, was %d", after.Size(), before.Size())
			}

			local := filepath.Join(cache, "local.m3u8")
			if err := os.WriteFile(local, []byte(pl.localise(cache, key)), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := remux(ctx, local, dest, nil); err != nil {
				t.Fatalf("remux: %v", err)
			}
			if out, err := exec.Command("ffmpeg", "-v", "error", "-i", dest, "-f", "null", "-").CombinedOutput(); err != nil {
				t.Fatalf("remuxed file is not playable: %v: %s", err, out)
			}
			// following the wrong rendition of a master yields a file that plays
			// yet holds no picture, so the streams are checked rather than assumed
			if !streams(t, dest, "v") {
				t.Error("remuxed file carries no video stream")
			}
			if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
				t.Error("the .part file was left behind")
			}
			covered++
		})
	}
	if covered == 0 {
		t.Fatal("no provider exercised the cache path")
	}
}

// streams reports whether dest carries at least one stream of the given kind
func streams(t *testing.T, dest, kind string) bool {
	t.Helper()
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", kind,
		"-show_entries", "stream=codec_type", "-of", "csv=p=0", dest).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	return len(bytes.TrimSpace(out)) > 0
}

// head returns the first n segments of a playlist as a standalone playlist
func head(pl *mediaPlaylist, n int) *mediaPlaylist {
	if len(pl.segAt) <= n {
		return pl
	}
	cut := pl.segAt[n-1] + 1
	out := &mediaPlaylist{
		lines:     append(append([]string{}, pl.lines[:cut]...), "#EXT-X-ENDLIST"),
		segAt:     append([]int{}, pl.segAt[:n]...),
		durations: append([]float64{}, pl.durations[:n]...),
		keyAt:     pl.keyAt,
		keyURI:    pl.keyURI,
	}
	if out.keyAt >= cut {
		out.keyAt, out.keyURI = -1, ""
	}
	return out
}
