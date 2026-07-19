//go:build live

package pipe

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestLiveEpisodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	body, err := New().Get(ctx, "episodes", map[string]string{"anilistId": "154587"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"mappings"`)) || !bytes.Contains(body, []byte(`"providers"`)) {
		t.Fatalf("unexpected body head %s", body[:min(160, len(body))])
	}
	t.Logf("decoded %d bytes", len(body))
}
