package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/play"
	"ysun.co/miruro/internal/ui"
	"ysun.co/miruro/internal/upstream"
)

func (s *runState) download(ctx context.Context, eps []float64, pin Pin) error {
	// an episode already on disk is left out before anything is resolved for it,
	// so a rerun over a finished range asks the api nothing and draws no bars,
	// and one owed sidecars comes back for those alone
	owes := map[float64]owed{}
	onDisk := 0
	eps = slices.DeleteFunc(slices.Clone(eps), func(ep float64) bool {
		name := episodeName(s.title, ep)
		if !play.Saved(s.cfg.DownloadDir, name) {
			return false
		}
		onDisk++
		o, ok := owing(play.EpisodePath(s.cfg.DownloadDir, name))
		if ok {
			owes[ep] = o
		}
		return !ok
	})
	if len(eps) == 0 {
		fmt.Printf("%s already in %s\n", plural(onDisk, "episode", "episodes"), s.cfg.DownloadDir)
		return nil
	}

	px, err := play.StartProxy(ctx, s.hc)
	if err != nil {
		return err
	}
	defer px.Close()

	// the proxy listens on loopback, which the run's guarded client refuses to
	// dial, so the episode is read through a client of its own that bounds only
	// the header wait, so a slow episode is not truncated mid-body
	local := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}}

	labels := make([]string, len(eps))
	for i, ep := range eps {
		labels[i] = "E" + num(ep)
		if _, ok := owes[ep]; ok {
			labels[i] += " subtitles"
		}
	}

	// workers run concurrently, so the tallies of episodes that lost their
	// subtitles and of episodes that got owed ones back are shared state
	var bare, repaid atomic.Int64
	// worker i alone writes swapped[i], published by Downloads joining them
	swapped := make([]bool, len(eps))

	sv := saver{runState: s, px: px, media: local, pin: pin}

	errs := ui.Downloads(ctx, labels, flagParallel, func(dctx context.Context, i int, report func(done, total int64, share float64)) error {
		if o, ok := owes[eps[i]]; ok {
			asked, missed, err := sv.resubtitle(dctx, eps[i], o)
			switch {
			case err != nil && dctx.Err() != nil:
				return err
			case err != nil:
				// the video is on disk, so the sidecars still out of reach are
				// warned the way a first attempt warns them, and stay owed
				log.Warn("missing subtitles not fetched", "episode", labels[i], "err", err)
			case !asked:
			case missed > 0:
				bare.Add(1)
			default:
				repaid.Add(1)
			}
			return nil
		}
		src, want, missed, err := sv.save(dctx, eps[i], report)
		if err != nil {
			return err
		}
		if missed > 0 {
			bare.Add(1)
		}
		if want.Code != "" && (src.Category != want.Category || src.Attach != want.Attach) {
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
		return fmt.Errorf("%s of %d failed", plural(failed, "download", "downloads"), len(eps)-len(owes))
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
	var said []string
	if n := len(eps) - len(owes); n > 0 {
		said = append(said, fmt.Sprintf("saved %s to %s", plural(n, "episode", "episodes"), s.cfg.DownloadDir))
	}
	if n := int(repaid.Load()); n > 0 {
		said = append(said, "fetched missing subtitles for "+plural(n, "episode", "episodes"))
	}
	// an episode whose owed sidecars stayed out of reach is on disk all the same
	switch there := onDisk - int(repaid.Load()); {
	case there == 0:
	case len(said) == 0:
		said = append(said, fmt.Sprintf("%s already in %s", plural(there, "episode", "episodes"), s.cfg.DownloadDir))
	default:
		said = append(said, fmt.Sprintf("%d already there", there))
	}
	fmt.Println(strings.Join(said, ", "))
	return nil
}

// titleBytes bounds the title in a file name, leaving room under the 255 bytes a
// name takes on most filesystems for the episode, a sidecar's tag and a .part,
// which a long title in a multi-byte script outgrows on its own
const titleBytes = 200

// episodeName is what an episode is saved as, before the extension
func episodeName(title string, ep float64) string {
	if len(title) > titleBytes {
		cut := titleBytes
		for cut > 0 && !utf8.RuneStart(title[cut]) {
			cut--
		}
		title = strings.TrimRight(title[:cut], " .")
	}
	return fmt.Sprintf("%s - E%s", title, num(ep))
}

// saver holds what every episode of one download run shares
// media is named apart from runState.hc because the two clients differ on
// purpose: this one reads the episode from the proxy on loopback, which the
// guarded runState.hc refuses to dial, and a field called hc here would shadow
// the other silently
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
// it reports the source that served the episode, the one the pin wanted, and
// the sidecars that were lost, the way play.Download does
func (s saver) save(ctx context.Context, ep float64, report play.Progress) (source, source, int, error) {
	l, err := s.listing(ctx, ep)
	if err != nil {
		return source{}, source{}, 0, err
	}
	want, _ := s.wanted(l, ep)

	tried := map[string]bool{}
	var last error
	for {
		res, src, err := s.walk(ctx, l, ep, s.pin, tried)
		if err != nil {
			if last != nil {
				return source{}, want, 0, last
			}
			return source{}, want, 0, err
		}
		tried[src.Code] = true

		// one provider serves an episode from several hosts, so a dead default
		// stream is not a dead provider
		for _, stream := range upstream.Rank(ctx, s.hc, res, s.cfg.Quality) {
			missed, err := s.from(ctx, res, src, stream, ep, report)
			if err == nil {
				return src, want, missed, nil
			}
			if ctx.Err() != nil {
				return source{}, want, 0, err
			}
			// name the provider, since the next attempt reports its own failure
			last = fmt.Errorf("%s: %w", src.Code, err)
			log.Warn("download failed, trying the next stream", "episode", num(ep), "provider", src.Code, "err", err)
		}
	}
}

// wanted is the source the pinned pick would resolve, what an episode that fell
// elsewhere is measured against when the run reports a rendition swap
// without a pin there is no expectation to diverge from, and the zero source
// is what save reports for it
func (s saver) wanted(l *upstream.Listing, ep float64) (source, bool) {
	if s.pin.Code == "" {
		return source{}, false
	}
	avail, err := candidates(l, ep, s.category)
	if err != nil {
		return source{}, false
	}
	rows := orderPinned(offers(avail, l.Caps, s.category, s.pin), s.pin)
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
	name := episodeName(s.title, ep)
	cache := cacheDir(s.anilistID, ep, src.Category, src.Code, s.cfg.Quality)
	missed, err := play.Download(ctx, s.media, s.px.Stream(stream), s.px.Subtitles(subs, stream.Referer), s.cfg.DownloadDir, name, cache, report)

	// a video that landed without all of its sidecars, whether they failed or
	// the run was interrupted before them, is owed them from this rendition
	// a video on disk past a clean return is one that missed sidecars, and past
	// a failure it is one whose sidecars the failure cut short, since Download
	// removes a video it refuses
	video := play.EpisodePath(s.cfg.DownloadDir, name)
	switch {
	case err == nil && missed == 0:
		settle(video)
	case play.Saved(s.cfg.DownloadDir, name):
		o := owed{Video: video, Provider: src.Code, Category: src.Category, Server: server(stream)}
		if err := o.record(); err != nil {
			log.Warn("missing subtitles not recorded, a rerun will not fetch them", "episode", name, "err", err)
		}
	}
	return missed, err
}

// resubtitle fetches the sidecars an episode on disk is owed, from the provider
// and rendition its video came from, and settles its record once none is left
// missing
// it reports whether the sidecars were asked for at all and how many failed
// a provider that no longer lists the episode or any subtitle for it gives no
// reason to think a later run would do better, so the record is dropped with a
// warning rather than resolved on every rerun
func (s saver) resubtitle(ctx context.Context, ep float64, o owed) (bool, int, error) {
	l, err := s.listing(ctx, ep)
	if err != nil {
		return false, 0, err
	}
	var e *upstream.Episode
	if p, ok := l.Providers[o.Provider]; ok {
		e = find(p.Episodes(o.Category), ep)
	}
	if e == nil {
		log.Warn("provider no longer lists the episode, its missing subtitles are given up", "episode", num(ep), "provider", o.Provider)
		settle(o.Video)
		return false, 0, nil
	}
	res, err := l.Sources(ctx, e.ID, o.Provider, o.Category)
	if err != nil {
		return false, 0, fmt.Errorf("%s: %w", o.Provider, err)
	}
	if len(res.Subtitles) == 0 {
		log.Warn("provider no longer lists subtitles for the episode, its missing ones are given up", "episode", num(ep), "provider", o.Provider)
		settle(o.Video)
		return false, 0, nil
	}

	// sidecars take the referer of the stream the video came from, and any
	// stream of the provider's when that one is gone
	referer := ""
	i := slices.IndexFunc(res.Streams, func(st upstream.Stream) bool { return st.Playable() && server(st) == o.Server })
	if i < 0 {
		i = slices.IndexFunc(res.Streams, upstream.Stream.Playable)
	}
	if i >= 0 {
		referer = res.Streams[i].Referer
	}
	subs := upstream.Order(res.Subtitles, s.cfg.Lang)
	missed, err := play.Sidecars(ctx, s.media, s.px.Subtitles(subs, referer), s.cfg.DownloadDir, episodeName(s.title, ep))
	if err == nil && missed == 0 {
		settle(o.Video)
	}
	return true, missed, err
}
