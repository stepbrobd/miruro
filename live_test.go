//go:build live

package miruro

import (
	"context"
	"testing"
	"time"
)

func TestLivePipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	client := New()

	media, err := client.Search(ctx, "frieren")
	if err != nil {
		t.Fatal(err)
	}
	if len(media) == 0 {
		t.Fatal("no search results")
	}

	cat, err := client.Episodes(ctx, 154587)
	if err != nil {
		t.Fatal(err)
	}
	avail := cat.Available(1, Sub)
	if len(avail) == 0 {
		t.Fatal("no providers carry episode 1 sub")
	}

	for _, p := range avail {
		ep := p.Episodes(Sub)[0]
		res, err := client.Sources(ctx, ep.ID, p.Code, Sub)
		if err != nil || len(res.Streams) == 0 {
			continue
		}
		s, err := client.Select(ctx, res, "best")
		if err != nil {
			continue
		}
		t.Logf("provider=%s streams=%d softsub=%v kind=%s", p.Code, len(res.Streams), res.Softsub(), s.Kind)
		return
	}
	t.Fatal("no provider resolved a stream")
}
