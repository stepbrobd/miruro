//go:build live

package play

import (
	"context"
	"testing"
	"time"

	"ysun.co/miruro"
)

func TestLiveDownloadProgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := miruro.New()
	cat, err := client.Episodes(ctx, 154587)
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range cat.Available(1, miruro.Sub) {
		res, err := client.Sources(ctx, p.Episodes(miruro.Sub)[0].ID, p.Code, miruro.Sub)
		if err != nil || len(res.Streams) == 0 {
			continue
		}
		s, err := client.Select(ctx, res, "best")
		if err != nil || s.Kind != miruro.MP4 {
			continue
		}

		var got int64
		dctx, dcancel := context.WithTimeout(ctx, 4*time.Second)
		_ = Download(dctx, client.HTTP, s, nil, t.TempDir(), "probe", func(done, total int64) { got = done })
		dcancel()
		if got == 0 {
			t.Fatalf("provider %s reported no progress", p.Code)
		}
		t.Logf("provider=%s progressed to %d bytes", p.Code, got)
		return
	}
	t.Skip("no mp4 provider available to probe")
}
