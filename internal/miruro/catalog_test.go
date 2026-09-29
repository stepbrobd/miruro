package miruro

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ysun.co/miruro/internal/upstream"
)

// api answers each path from bodies the way the catalog does, masked under
// /api/v1 and plain json for the provider table, and 404s the rest with the
// site's html shell
// asked records every request, path and query, in order
func api(t *testing.T, bodies map[string]string) (*counter, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := mirror(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RequestURI())
		mu.Unlock()
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<!doctype html>")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(mask(t, []byte(body)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	})
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

func client(srv *counter) *Client {
	return &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
}

// frieren is the lookup answer for AniList 154587, two tracks counted
const frieren = `{"data":[{"id":"o2Eqmv4w0JYgQJWQFi9CDDdH-PgffNpm","external_ids":{"anilist":["154587"]},
	"title":{"romaji":"Sousou no Frieren","english":"Frieren: Beyond Journey's End"},"format":"TV","status":"FINISHED",
	"episode_count":3,"episode_counts":{"raw":3,"sub":3,"dub":2}}],"next_cursor":null,"has_more":false}`

func TestEpisodes(t *testing.T) {
	ctx := context.Background()

	t.Run("lists a title by its AniList id", func(t *testing.T) {
		srv, asked := api(t, map[string]string{
			"/api/v1/anime": frieren,
			"/api/v1/anime/o2Eqmv4w0JYgQJWQFi9CDDdH-PgffNpm/episodes": `{"data":[
				{"episode_number":1,"title":"The Journey's End","canon_type":"manga_canon","duration_seconds":1500,
				 "skip_times":[{"kind":"mixed_op","start_seconds":58,"end_seconds":148},{"kind":"op","start_seconds":3,"end_seconds":93},{"kind":"ed","start_seconds":1417,"end_seconds":1507}]},
				{"episode_number":2,"title":"","canon_type":"filler","duration_seconds":1500,
				 "skip_times":[{"kind":"ed","start_seconds":0.8,"end_seconds":90},{"kind":"recap","start_seconds":0,"end_seconds":60}]},
				{"episode_number":3,"title":"Killing Magic","canon_type":null,"duration_seconds":1500,"skip_times":[]},
				{"episode_number":4,"title":"Not Out Yet","canon_type":null,"duration_seconds":null,"skip_times":[]}],
				"next_cursor":null,"has_more":false}`,
		})

		cat, err := client(srv).Episodes(ctx, upstream.Media{ID: 154587})
		if err != nil {
			t.Fatal(err)
		}
		if cat.Title != "Frieren: Beyond Journey's End" {
			t.Errorf("title = %q", cat.Title)
		}
		// the fourth runs past the raw count, so the site does not list it
		sub := []upstream.Episode{
			{Number: 1, Title: "The Journey's End"},
			{Number: 2, Filler: true},
			{Number: 3, Title: "Killing Magic"},
		}
		if !reflect.DeepEqual(cat.Sub, sub) {
			t.Errorf("sub = %+v, want %+v", cat.Sub, sub)
		}
		if !reflect.DeepEqual(cat.Dub, sub[:2]) {
			t.Errorf("dub = %+v, want the two the dub count covers", cat.Dub)
		}
		// a mixed intro, a recap, and an outro in the opening seconds all drop
		skips := []upstream.SkipRange{
			{Episode: 1, Kind: upstream.Intro, Start: 3, End: 93},
			{Episode: 1, Kind: upstream.Outro, Start: 1417, End: 1507},
		}
		if !reflect.DeepEqual(cat.Aniskip, skips) {
			t.Errorf("aniskip = %+v, want %+v", cat.Aniskip, skips)
		}
		if got := cat.Refs["miruro"]; got != "o2Eqmv4w0JYgQJWQFi9CDDdH-PgffNpm" {
			t.Errorf("ref = %q, want the catalog's own key", got)
		}
		want := []string{
			"/api/v1/anime?anilist_id_in=154587&limit=100",
			"/api/v1/anime/o2Eqmv4w0JYgQJWQFi9CDDdH-PgffNpm/episodes?kind=regular&limit=10000",
		}
		if got := asked(); !slices.Equal(got, want) {
			t.Errorf("asked %v, want %v", got, want)
		}
	})

	t.Run("a film is listed under its own kind", func(t *testing.T) {
		srv, asked := api(t, map[string]string{
			"/api/v1/anime": `{"data":[{"id":"f","external_ids":{"anilist":["199"]},"title":{"romaji":"Sen to Chihiro no Kamikakushi"},
				"format":"MOVIE","status":"FINISHED","episode_count":1,"episode_counts":{"raw":1,"sub":1,"dub":1}}]}`,
			"/api/v1/anime/f/episodes": `{"data":[{"episode_number":1,"title":"","canon_type":null,"skip_times":[]}],"has_more":false}`,
		})
		cat, err := client(srv).Episodes(ctx, upstream.Media{ID: 199})
		if err != nil {
			t.Fatal(err)
		}
		if len(cat.Sub) != 1 || len(cat.Dub) != 1 {
			t.Errorf("catalog = %+v, want the one episode in both tracks", cat)
		}
		if got := asked()[1]; got != "/api/v1/anime/f/episodes?kind=film&limit=10000" {
			t.Errorf("asked %q, want the film kind", got)
		}
	})

	t.Run("a title the catalog does not carry is an empty catalog", func(t *testing.T) {
		srv, _ := api(t, map[string]string{"/api/v1/anime": `{"data":[],"next_cursor":null,"has_more":false}`})
		cat, err := client(srv).Episodes(ctx, upstream.Media{ID: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(cat.Sub) != 0 || len(cat.Dub) != 0 || len(cat.Refs) != 0 {
			t.Errorf("catalog = %+v, want nothing", cat)
		}
	})

	t.Run("two titles carrying one id cannot be told apart", func(t *testing.T) {
		srv, _ := api(t, map[string]string{"/api/v1/anime": `{"data":[
			{"id":"a","external_ids":{"anilist":["21"]}},{"id":"b","external_ids":{"anilist":["21","22"]}}]}`})
		if _, err := client(srv).Episodes(ctx, upstream.Media{ID: 21}); err == nil || !strings.Contains(err.Error(), "2 titles") {
			t.Errorf("err = %v, want the ambiguity named", err)
		}
	})

	// the site never pages this list, so the next page's request shape is not
	// one the catalog is known to accept
	t.Run("a list running past one page is an error", func(t *testing.T) {
		srv, _ := api(t, map[string]string{
			"/api/v1/anime": frieren,
			"/api/v1/anime/o2Eqmv4w0JYgQJWQFi9CDDdH-PgffNpm/episodes": `{"data":[],"next_cursor":"x","has_more":true}`,
		})
		if _, err := client(srv).Episodes(ctx, upstream.Media{ID: 154587}); err == nil {
			t.Error("a truncated list was read as whole")
		}
	})
}

// the site plays sub whether or not the catalog counted it, and offers a dub
// only once it did, which the HeartCatch Precure listing on 2026-09-29 needs:
// no sub count, and ssub and sub providers on every episode
func TestCatalogCounts(t *testing.T) {
	eps := []episode{{Number: 1}, {Number: 2}, {Number: 3}}
	three := 3
	var tt title
	tt.EpisodeCounts.Raw = &three

	cat := tt.catalog("miruro", eps, time.Now())
	if len(cat.Sub) != 3 {
		t.Errorf("sub = %v, want every listed episode for an uncounted sub", cat.Numbers(upstream.Sub))
	}
	if len(cat.Dub) != 0 {
		t.Errorf("dub = %v, want none for an uncounted dub", cat.Numbers(upstream.Dub))
	}

	// a fractional episode is something no play request can address
	cat = tt.catalog("miruro", []episode{{Number: 1}, {Number: 1.5}, {Number: 0}}, time.Now())
	if got := cat.Numbers(upstream.Sub); !slices.Equal(got, []float64{1}) {
		t.Errorf("sub = %v, want only the whole episode", got)
	}
}

// the list runs ahead of what has aired, and a title whose raw episodes nobody
// counted falls back to the air dates the way the site's own list does
func TestListed(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	eps := []episode{
		{Number: 1, AiredOn: "2026-09-01"},
		{Number: 2, AiredOn: "2026-09-29"},
		{Number: 3, AiredOn: "2026-10-06"},
		{Number: 4},
	}
	numbers := func(t title) []float64 {
		var out []float64
		for _, e := range t.listed(eps, now) {
			out = append(out, e.Number)
		}
		return out
	}
	one, two := 1, 2

	var airing title
	if got := numbers(airing); !slices.Equal(got, []float64{1, 2, 4}) {
		t.Errorf("uncounted = %v, want what has aired plus the undated one", got)
	}
	airing.Status = "NOT_YET_RELEASED"
	if got := numbers(airing); !slices.Equal(got, []float64{1, 2}) {
		t.Errorf("unreleased = %v, want only what has aired", got)
	}

	var counted title
	counted.EpisodeCounts.Dub = &two
	counted.Status = "NOT_YET_RELEASED"
	if got := numbers(counted); !slices.Equal(got, []float64{1, 2}) {
		t.Errorf("counted = %v, want what the dub count covers", got)
	}

	var capped title
	capped.EpisodeCount, capped.EpisodeCounts.Raw = &two, &one
	if got := numbers(capped); !slices.Equal(got, []float64{1}) {
		t.Errorf("capped = %v, want the lower of the total and the raw count", got)
	}
}
