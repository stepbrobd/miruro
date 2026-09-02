package play

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ysun.co/miruro"
	"ysun.co/miruro/backend/mirurotv"
)

// titles are the anime the integration run pulls its provider list from
// provider codes are never written down here, they come from whatever the live
// catalog carries, but no single title carries all of them, so the run covers
// the union across a couple of fixed titles
// tensura was the original and is the only one of the two carrying ANIMEDUNYA
// shingeki is what reaches pewe
var titles = []int{
	108511, // Tensei shitara Slime Datta Ken 2nd Season
	16498,  // Shingeki no Kyojin
}

// providerMatrix is every provider the live catalogs carry, each mapped to the
// first title that has it
func providerMatrix(ctx context.Context, t *testing.T, client *mirurotv.Client) map[string]miruro.Provider {
	t.Helper()
	out := map[string]miruro.Provider{}
	for _, id := range titles {
		cat, err := client.Episodes(ctx, miruro.Media{ID: id})
		if err != nil {
			t.Fatalf("catalog %d: %v", id, err)
		}
		for code, p := range cat.Providers {
			if _, seen := out[code]; !seen {
				out[code] = p
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no provider in any catalog")
	}
	t.Logf("provider matrix: %d providers across %d titles", len(out), len(titles))
	return out
}

// rendition is the sub category a provider carries, from its declared
// capabilities
// asking for the one it does not carry answers 444, which would skip half the
// matrix on a condition the run itself created
// soft wins when a provider declares both, matching what the cli resolves to
// with no pin, so the run covers the rendition the pick defaults to
func rendition(caps miruro.Capabilities, code string) miruro.Category {
	if c, ok := caps[code]; ok && c.Soft {
		return miruro.Ssub
	}
	return miruro.Sub
}

// capabilities fetches the provider table, empty when the resource is down so
// the run falls back to asking every provider for sub
func capabilities(ctx context.Context, t *testing.T, client *mirurotv.Client) miruro.Capabilities {
	t.Helper()
	caps, err := client.Capabilities(ctx)
	if err != nil {
		t.Logf("capability table unavailable, every provider will be asked for sub: %v", err)
	}
	return caps
}

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

	client := mirurotv.New()

	px, err := StartProxy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	var covered int
	caps := capabilities(ctx, t, client)
	for code, provider := range providerMatrix(ctx, t, client) {
		t.Run(code, func(t *testing.T) {
			cat := rendition(caps, code)
			eps := provider.Episodes(cat)
			if len(eps) == 0 {
				t.Skip("provider carries no sub episodes")
			}
			res, err := client.Sources(ctx, eps[0].ID, code, cat)
			if err != nil {
				t.Skipf("provider did not resolve %s, an upstream condition rather than a defect: %v", cat, err)
			}
			ranked := miruro.Rank(ctx, client.HTTP, res, "")
			if len(ranked) == 0 {
				t.Skip("no selectable stream")
			}
			stream := ranked[0]
			if stream.Kind != miruro.HLS {
				t.Skipf("kind %s does not exercise the segment cache", stream.Kind)
			}

			pl, err := resolvePlaylist(ctx, px.hc, px.Stream(stream).URL)
			if err != nil {
				t.Skipf("playlist not reachable, an upstream condition: %v", err)
			}
			t.Logf("segments=%d encrypted=%v", len(pl.segAt), pl.encrypted)
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
				// a CDN that refuses a segment is the same upstream condition the
				// checks above skip on, and bonk's ad-CDN does it routinely
				var code status
				if errors.As(err, &code) {
					t.Skipf("segments not reachable, an upstream condition: %v", err)
				}
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
				var code status
				if errors.As(err, &code) {
					t.Skipf("resume not reachable, an upstream condition: %v", err)
				}
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
			if err := remux(ctx, local, dest); err != nil {
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
		encrypted: pl.encrypted,
	}
	if out.keyAt >= cut {
		// staying marked encrypted only relaxes the TS check
		out.keyAt, out.keyURI = -1, ""
	}
	return out
}

// TestIntegrationSubtitleTracks checks that every sidecar a live provider ships
// reaches a player under a readable name and with a body a player can parse
// it is skipped unless MIRURO_INTEGRATION is set because it needs the network
func TestIntegrationSubtitleTracks(t *testing.T) {
	if os.Getenv("MIRURO_INTEGRATION") == "" {
		t.Skip("set MIRURO_INTEGRATION=1 to run against live providers")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := mirurotv.New()

	px, err := StartProxy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	caps := capabilities(ctx, t, client)
	var covered int
	for code, provider := range providerMatrix(ctx, t, client) {
		t.Run(code, func(t *testing.T) {
			cat := rendition(caps, code)
			eps := provider.Episodes(cat)
			if len(eps) == 0 {
				t.Skip("provider carries no sub episodes")
			}
			res, err := client.Sources(ctx, eps[0].ID, code, cat)
			if err != nil {
				t.Skipf("provider did not resolve %s, an upstream condition rather than a defect: %v", cat, err)
			}
			if len(res.Subtitles) == 0 {
				t.Skip("provider ships no subtitles")
			}
			ranked := miruro.Rank(ctx, client.HTTP, res, "")
			if len(ranked) == 0 {
				t.Skip("no selectable stream")
			}
			stream := ranked[0]

			for _, s := range px.Subtitles(miruro.Order(res.Subtitles, "en"), stream.Referer) {
				u, err := url.Parse(s.File)
				if err != nil {
					t.Fatalf("subtitle url: %v", err)
				}
				name := path.Base(u.Path)
				t.Logf("track %q lang=%q default=%v shows as %q", s.Label, s.Lang, s.Default, name)
				if len(name) > 64 || !strings.Contains(name, ".") {
					t.Errorf("track shows as %q, which is the payload rather than a name", name)
				}
				resp, err := http.Get(s.File)
				if err != nil {
					t.Fatalf("sidecar fetch: %v", err)
				}
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Logf("sidecar answered %d, an upstream gap rather than a proxy defect", resp.StatusCode)
					continue
				}
				if !bytes.HasPrefix(body, []byte("WEBVTT")) && !bytes.Contains(body, []byte("-->")) {
					t.Errorf("sidecar body is not a subtitle: %.40q", body)
				}
				covered++
			}
		})
	}
	if covered == 0 {
		t.Skip("no provider served a sidecar")
	}
}
