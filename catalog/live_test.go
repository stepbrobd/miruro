//go:build live

package catalog

import (
	"context"
	"testing"
	"time"

	"ysun.co/miruro/pipe"
)

func TestLiveFetch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := Fetch(ctx, pipe.New(), 154587)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Providers) == 0 {
		t.Fatal("no providers")
	}
	avail := c.Available(1, Sub)
	if len(avail) == 0 {
		t.Fatal("no providers carry episode 1 sub")
	}
	t.Logf("title=%q providers=%d ep1sub=%d aniskip=%d", c.Mappings.Title, len(c.Providers), len(avail), len(c.Aniskip))
}
