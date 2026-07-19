//go:build live

package anilist

import (
	"context"
	"testing"
	"time"
)

func TestLiveSearch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	got, err := Search(ctx, nil, "frieren")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no results")
	}
	t.Logf("%d results, first %q id=%d", len(got), got[0].Title(), got[0].ID)
}
