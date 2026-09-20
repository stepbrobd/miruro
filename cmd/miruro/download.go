package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/play"
	"ysun.co/miruro/internal/ui"
	"ysun.co/miruro/internal/upstream"
)

func (s *runState) download(ctx context.Context, eps []float64, pin Pin) error {
	px, err := play.StartProxy(ctx)
	if err != nil {
		return err
	}
	defer px.Close()

	// bound only the header wait so a slow episode is not truncated mid-body
	hc := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}}

	labels := make([]string, len(eps))
	for i, ep := range eps {
		labels[i] = "E" + num(ep)
	}

	// workers run concurrently, so the tally of episodes that lost their
	// subtitles is shared state
	var bare atomic.Int64
	// worker i alone writes swapped[i], published by Downloads joining them
	swapped := make([]bool, len(eps))

	sv := saver{runState: s, px: px, media: hc, pin: pin}

	errs := ui.Downloads(ctx, labels, flagParallel, func(dctx context.Context, i int, report func(done, total int64)) error {
		src, missed, err := sv.save(dctx, eps[i], report)
		if err != nil {
			return err
		}
		if missed > 0 {
			bare.Add(1)
		}
		if want, ok := sv.wanted(eps[i]); ok && (src.Category != want.Category || src.Attach != want.Attach) {
			// the run reports every swapped episode once it ends, so the worker
			// only records it
			swapped[i] = true
		}
		return nil
	})

	var failed, canceled int
	for i, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, ui.ErrCanceled):
			canceled++
		default:
			failed++
			// the TUI shows each failure on its task row, but a piped or scripted
			// run draws no rows and would otherwise report only a count
			log.Error("download failed", "episode", labels[i], "err", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%s of %d failed", plural(failed, "download", "downloads"), len(eps))
	}
	if canceled > 0 {
		// map an interrupt onto the same silent 130 exit every other abort takes
		return context.Canceled
	}
	// warn rather than log so a soft-subbed run that lost its sidecars is visible
	// without --verbose
	if n := bare.Load(); n > 0 {
		log.Warn("saved without subtitles", "episodes", n)
	}
	// name the episodes so a re-fetch knows which files to delete first, since a
	// rerun skips whatever is already on disk
	var off []string
	for i, s := range swapped {
		if s {
			off = append(off, labels[i])
		}
	}
	if len(off) > 0 {
		log.Warn("episodes saved with a different rendition than pinned", "episodes", strings.Join(off, " "))
	}
	// the default level is warn, so a result logged as info never reached the
	// user and a long run ended without saying where anything landed
	fmt.Printf("saved %s to %s\n", plural(len(eps), "episode", "episodes"), s.cfg.DownloadDir)
	return nil
}

// saver holds what every episode of one download run shares
// media is named apart from runState.hc because the two clients differ on
// purpose: this one drops the whole-request timeout so a long episode is not
// cut mid-body, and a field called hc here would shadow the other silently
type saver struct {
	*runState
	px    *play.Proxy
	media *http.Client
	pin   Pin
}

// save writes one episode, dropping to the next provider when a download fails
// after its own retries
// a provider that resolves and then dies mid-episode is common enough that
// failing the episode over it would waste the rest of the run
// it reports the source that served the episode and the sidecars that were
// lost, the way play.Download does
func (s saver) save(ctx context.Context, ep float64, report play.Progress) (source, int, error) {
	tried := map[string]bool{}
	var last error
	for {
		res, src, err := s.autoResolve(ctx, ep, s.pin, tried)
		if err != nil {
			if last != nil {
				return source{}, 0, last
			}
			return source{}, 0, err
		}
		tried[src.Code] = true

		// one provider serves an episode from several hosts, so a dead default
		// stream is not a dead provider
		for _, stream := range upstream.Rank(ctx, s.media, res, s.cfg.Quality) {
			missed, err := s.from(ctx, res, src, stream, ep, report)
			if err == nil {
				return src, missed, nil
			}
			if ctx.Err() != nil {
				return source{}, 0, err
			}
			// name the provider, since the next attempt reports its own failure
			last = fmt.Errorf("%s: %w", src.Code, err)
			log.Warn("download failed, trying the next stream", "episode", num(ep), "provider", src.Code, "err", err)
		}
	}
}

// wanted is the source the pinned pick would resolve, what an episode that fell
// elsewhere is measured against when the run reports a rendition swap
// without a pin there is no expectation to diverge from
func (s saver) wanted(ep float64) (source, bool) {
	if s.pin.Code == "" {
		return source{}, false
	}
	avail, err := candidates(s.cat, ep, s.category, s.caps)
	if err != nil {
		return source{}, false
	}
	rows := orderPinned(offers(avail, s.caps, s.category, s.pin), s.pin)
	return rows[0].source(s.category), true
}

// from downloads one episode from one stream of the source that served it
// the cache is keyed by the rendition asked for, since sub and ssub are
// different cuts of the episode and must not share a segment directory
func (s saver) from(ctx context.Context, res *upstream.Result, src source, stream upstream.Stream, ep float64, report play.Progress) (int, error) {
	subs := upstream.Order(res.Subtitles, s.cfg.Lang)
	if !src.Attach {
		subs = nil
	}
	name := fmt.Sprintf("%s - E%s", s.title, num(ep))
	cache := cacheDir(s.anilistID, ep, src.Category, src.Code, s.cfg.Quality)
	return play.Download(ctx, s.media, s.px.Stream(stream), s.px.Subtitles(subs, stream.Referer), s.cfg.DownloadDir, name, cache, report)
}
