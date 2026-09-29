package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/play"
	"ysun.co/miruro/internal/upstream"
)

// playback is one episode's attempt, the ambient state the stream and provider
// walk needs
type playback struct {
	*runState
	px   *play.Proxy
	pin  Pin
	ep   float64
	kind play.Kind
	// watch decides when a stream that shows nothing is stopped
	watch watchdog
	// launch runs the player on one stream, addressed through the proxy under
	// tl, with the subtitles chosen for the provider that served it, a field so
	// a test needs no player binary
	launch func(ctx context.Context, s upstream.Stream, tl *play.Tally, subs []upstream.Subtitle) error
}

// run plays the episode, walking the streams of a provider and then the
// providers, and reports the last failure when none of them produced picture
// a provider that relayed no media body at all is dead for this episode however
// many streams it listed, and the download path has always moved off one
func (p playback) run(pctx context.Context, res *upstream.Result, src source) error {
	tried := map[string]bool{}
	var last error
	for {
		subs := upstream.Order(res.Subtitles, p.cfg.Lang)
		if !src.Attach {
			subs = nil
		}

		if ranked := upstream.Rank(pctx, p.hc, res, p.cfg.Quality); len(ranked) > 0 {
			log.Info("playing", "title", p.title, "ep", num(p.ep), "provider", src.Code,
				"server", server(ranked[0]), "rendition", src.Category,
				"player", p.kind, "subs", len(subs))

			var played bool
			played, last = p.watch.playStreams(pctx, p.px, ranked, func(ctx context.Context, s upstream.Stream, tl *play.Tally) error {
				return p.launch(ctx, s, tl, subs)
			})
			if last == nil || pctx.Err() != nil || played {
				return last
			}
		} else {
			last = fmt.Errorf("%s: %w", src.Code, upstream.ErrNoStream)
		}

		tried[src.Code] = true
		next, nsrc, err := p.autoResolve(pctx, p.ep, p.pin, tried)
		switch {
		case errors.Is(err, upstream.ErrBlocked):
			// the session is over, and saying the player exited would hide that
			return err
		case err != nil:
			// both halves are named because the playback failure alone reads as
			// the user having quit, and says nothing about why the walk stopped
			log.Warn("no provider left to try", "provider", src.Code, "err", last, "resolve", err)
			// report what failed to play rather than what failed to resolve after
			return last
		}
		log.Warn("nothing played, trying the next provider", "provider", src.Code, "next", nsrc.Code, "err", last)
		res, src = next, nsrc
	}
}

// playStreams hands each stream in turn to play until one of them plays, and
// reports whether any relayed picture
// a provider serving an episode from several hosts is not dead when the first
// of them is, and the action menu stays raised throughout because this runs
// inside the playback goroutine
// each stream is counted under a tally of its own, so a request the player
// left running for one cannot count toward the next
func (w watchdog) playStreams(ctx context.Context, px *play.Proxy, ranked []upstream.Stream, play func(context.Context, upstream.Stream, *play.Tally) error) (bool, error) {
	var err error
	played := false
	for i, s := range ranked {
		tl := px.Tally()
		pctx, stop := context.WithCancel(ctx)
		settled := w.abandonStalled(pctx, tl, server(s), stop)
		err = play(pctx, s, tl)
		stop()
		abandoned := <-settled
		played = played || tl.Served() > 0
		if !deadStream(err, tl.Served()) || ctx.Err() != nil {
			return played, err
		}
		// the watcher already said why it stopped this one
		if !abandoned && i+1 < len(ranked) {
			log.Warn("stream did not play, trying the next", "server", server(s), "err", err)
		}
	}
	return played, err
}

// server names a stream for the log, since a provider does not always name its
// own host
// the url's host is what tells one unnamed stream of a provider from another,
// where a placeholder reads as the provider serving a thing called "stream"
func server(s upstream.Stream) string {
	if s.Server != "" {
		return s.Server
	}
	if u, err := url.Parse(s.URL); err == nil && u.Host != "" {
		return u.Host
	}
	return "unnamed"
}

// watchdog decides when a running player whose stream shows nothing is stopped
// each playback carries its own, so a test shortening the bounds cannot rewrite
// them under a watcher another test left running
// the zero watchdog stops every stream at once, so a playback always carries
// patience or a test's own
type watchdog struct {
	// grace is how long a stream has to relay its first media body
	grace time.Duration
	// budget is how many media bodies a stream may be refused before it has
	// relayed one
	budget int
	// check is how often a running player is asked whether its stream is
	// getting anything
	check time.Duration
}

// patience is the watchdog a run plays with
// pewe and bee reached their first body in 1.0s and 2.3s through the proxy on
// 2026-08-23, so the grace is an order of magnitude of headroom rather than a
// tuned value
// bee played after two refusals, while bonk and hop reached 25 and 538 in forty
// seconds without ever relaying one, so the budget separates them with room to
// spare
// a second between checks is far below the wait it replaces and far above the
// cost of reading two counters
func patience() watchdog {
	return watchdog{grace: 20 * time.Second, budget: 8, check: time.Second}
}

// abandonStalled stops a player whose stream has shown nothing
// ffmpeg's hls demuxer skips a segment it cannot fetch and asks for the next,
// so a stream whose CDN refuses every one runs forever without a frame and the
// player never exits for playStreams to move on
// once any media body is relayed the user is watching, and a later gap is theirs
// to deal with rather than grounds for restarting the episode elsewhere
// the returned channel yields whether the player was stopped and closes when the
// watcher is done, and a caller must receive from it before it returns, since
// nothing else joins the goroutine that reports through note
func (w watchdog) abandonStalled(ctx context.Context, tl *play.Tally, name string, stop context.CancelFunc) <-chan bool {
	settled := make(chan bool, 1)
	go func() {
		abandoned := false
		defer func() { settled <- abandoned; close(settled) }()
		grace := time.NewTimer(w.grace)
		defer grace.Stop()
		tick := time.NewTicker(w.check)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-grace.C:
				if tl.Served() == 0 {
					log.Warn("stream showed nothing in time, abandoning it", "server", name, "after", w.grace)
					abandoned = true
					stop()
				}
				return
			case <-tick.C:
				if tl.Served() > 0 {
					return
				}
				if n := tl.Refused(); n >= w.budget {
					log.Warn("stream refused before it played, abandoning it", "server", name, "refused", n)
					abandoned = true
					stop()
					return
				}
			}
		}
	}()
	return settled
}

// deadStream reports whether a finished playback is worth retrying on another
// stream
// a player that exits with an error before the proxy relayed a single media
// body of its stream never started, which is what tells a dead stream from one
// the user quit a few seconds in
func deadStream(err error, served int) bool {
	return err != nil && served == 0
}
