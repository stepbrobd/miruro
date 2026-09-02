package allanime

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"ysun.co/miruro"
)

// TestIntegrationFrieren drives the live site end to end, from the title to a
// stream the proxy could relay
// it runs only under MIRURO_INTEGRATION=1, and what it proves is that the
// handshake still matches the deployed bundle, which nothing hermetic can
func TestIntegrationFrieren(t *testing.T) {
	if os.Getenv("MIRURO_INTEGRATION") != "1" {
		t.Skip("set MIRURO_INTEGRATION=1 to hit the live site")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	b := New()
	cat, err := b.Episodes(ctx, miruro.Media{ID: 154587, Romaji: "Sousou no Frieren", English: "Frieren: Beyond Journey's End"})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cat.Providers[Code]
	if !ok || len(p.Sub) == 0 {
		t.Fatalf("providers = %v, want %s with sub episodes", cat.Providers, Code)
	}
	ep := p.Sub[0]
	res, err := b.Sources(ctx, ep.ID, Code, miruro.Sub)
	if err != nil {
		t.Fatal(err)
	}
	ranked := miruro.Rank(ctx, b.HTTP, res, "best")
	if len(ranked) == 0 {
		t.Fatalf("nothing playable among %+v", res.Streams)
	}
	head := ranked[0]
	t.Logf("episode %v from %s: %s %s %s", ep.Number, head.Server, head.Kind, head.Quality, head.URL)

	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, head.URL, nil)
	req.Header.Set("User-Agent", miruro.UserAgent)
	req.Header.Set("Referer", head.Referer)
	resp, err := b.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength < 1<<20 {
		t.Errorf("stream answered %d with %d bytes", resp.StatusCode, resp.ContentLength)
	}
}
