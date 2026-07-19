//go:build live

package sources

import (
	"context"
	"net/http"
	"testing"
	"time"

	"ysun.co/miruro/catalog"
	"ysun.co/miruro/pipe"
)

func TestLiveResolve(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	c := pipe.New()
	cat, err := catalog.Fetch(ctx, c, 154587)
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range cat.Available(1, catalog.Sub) {
		ep := p.Episodes(catalog.Sub)[0]
		res, err := Resolve(ctx, c, ep.ID, p.Code, catalog.Sub)
		if err != nil {
			t.Logf("provider %s unavailable: %v", p.Code, err)
			continue
		}
		if len(res.Streams) == 0 {
			t.Logf("provider %s no streams", p.Code)
			continue
		}
		s, err := Select(ctx, http.DefaultClient, res, "best")
		if err != nil {
			t.Logf("provider %s select: %v", p.Code, err)
			continue
		}
		t.Logf("provider %s streams=%d softsub=%v chosen kind=%s referer=%s", p.Code, len(res.Streams), res.Softsub(), s.Kind, s.Referer)
		return
	}
	t.Fatal("no provider resolved a stream")
}
