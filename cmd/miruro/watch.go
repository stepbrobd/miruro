package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/play"
	"ysun.co/miruro/internal/ui"
	"ysun.co/miruro/internal/upstream"
)

func (s *runState) watch(ctx context.Context, st *store, numbers, queue []float64, pin Pin, player play.Player) error {
	px, err := play.StartProxy(ctx, s.hc)
	if err != nil {
		return err
	}
	defer px.Close()

	details := s.cat.Details(s.category)
	ep := queue[0]
	queue = queue[1:]

	// skipped holds the streams change stream moved past for the episode on
	// screen, which any other step lets go of
	var skipped map[feed]bool
	for {
		res, src, carry, err := s.resolve(ctx, ep, pin)
		if err != nil {
			return err
		}
		// carry the user's intent across episodes
		// a transient fallback serves another provider but must not overwrite the pin
		pin = carry

		var skips []upstream.SkipRange
		if flagSkip {
			skips = episodeSkips(s.cat, ep)
		}

		mediaTitle := fmt.Sprintf("%s Episode %s", s.title, num(ep))
		if d := details[ep]; d.Title != "" {
			mediaTitle += " - " + d.Title
		}

		var shown feed
		stage := playback{
			runState: s, px: px, pin: pin, ep: ep, kind: player.Kind, watch: patience(),
			skip: skipped, shown: &shown,
			launch: func(pctx context.Context, stream upstream.Stream, tl *play.Tally, subs []upstream.Subtitle) error {
				return player.Play(pctx, tl.Stream(stream), px.Subtitles(subs, stream.Referer), skips, mediaTitle)
			},
		}

		e := entry{AnilistID: s.anilistID, Title: s.title, Provider: pin.String(), Category: s.category, Episode: ep}
		action, err := playAndControl(ctx, controlMenu,
			fmt.Sprintf("Episode %s of %s", num(ep), s.title),
			controls(numbers, ep, hosts(res, src.Code, skipped)),
			len(queue) > 0,
			func(pctx context.Context) error { return stage.run(pctx, res, src) },
			func() error { return st.save(e) },
		)
		if err != nil {
			return err
		}

		if action == "" {
			// the menu only dismisses itself mid-batch, so the queue is not empty
			ep = queue[0]
			queue = queue[1:]
			continue
		}

		next, quit := apply(action, numbers, ep)
		if quit {
			return nil
		}
		skipped = moveOn(skipped, next, shown)
		if next.reprovide {
			pin = Pin{}
		}
		if next.reselect {
			ep, err = ui.Select("Select episode", numbers, episodeLabel(details))
			if err != nil {
				return err
			}
		} else {
			ep = next.ep
		}
		queue = ahead(queue, ep)
	}
}

// menuFunc raises the action menu over a playback, with ui.Control's contract:
// a dismissal is "" and ended, and a pick is the action and whether playback
// was already over
type menuFunc func(ctx context.Context, title string, actions []string, wait func() bool) (string, bool, error)

// controlMenu is the menu a run raises, the terminal's own
func controlMenu(ctx context.Context, title string, actions []string, wait func() bool) (string, bool, error) {
	return ui.Control(ctx, title, actions, wait)
}

// playAndControl runs one playback with the action menu raised over it and
// joins both before returning the picked action, "" on a dismissal
// the menu is up while the player runs, so a pick races playback ending
// an early pick interrupts the player, a clean end mid-batch dismisses the
// menu to auto-advance
func playAndControl(ctx context.Context, raise menuFunc, title string, actions []string, batch bool, run func(context.Context) error, save func() error) (string, error) {
	pctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	var perr, werr error
	go func() {
		perr = run(pctx)
		// save the moment playback ends cleanly rather than when the menu
		// closes, so killing an idle menu cannot lose the watched entry
		if perr == nil {
			werr = save()
		}
		close(done)
	}()

	wait := func() bool {
		<-done
		return outcome(perr, batch)
	}
	action, ended, err := raise(ctx, title, actions, wait)
	stop()
	<-done
	if err != nil {
		return "", err
	}

	switch {
	case action == "" && perr != nil:
		// outcome dismisses a failed playback only when the player never ran,
		// which no menu action can fix
		return "", perr
	case perr != nil && ended:
		// keep the failure in scrollback after the menu clears
		log.Warn("player exited", "err", perr)
	case perr != nil:
		// an early pick interrupts the player and still counts as watched
		werr = save()
	}
	if werr != nil {
		log.Warn("history not saved", "err", werr)
	}
	return action, nil
}

// outcome reports whether the menu dismisses itself when playback stops
// a clean end mid-batch advances, a failure keeps the menu up so a broken
// provider does not burn the range, and a player that never ran is fatal
func outcome(err error, batch bool) bool {
	switch {
	case err == nil:
		return batch
	case errors.As(err, new(*exec.ExitError)):
		return false
	default:
		return true
	}
}

type step struct {
	ep        float64
	reprovide bool
	reselect  bool
	// restream replays the episode on the provider's next stream
	restream bool
}

// moveOn is what change stream leaves skipped for the next pass of an episode:
// the stream on screen joins the ones already moved past, one that never
// reached the screen adds nothing, and any other step lets go of them all
func moveOn(skipped map[feed]bool, next step, shown feed) map[feed]bool {
	switch {
	case !next.restream:
		return nil
	case shown == (feed{}):
		return skipped
	}
	if skipped == nil {
		skipped = map[feed]bool{}
	}
	skipped[shown] = true
	return skipped
}

// hosts counts the servers a provider's result can still play from past the
// ones skipped, one however many qualities a server lists, since moving past a
// stream moves past its server
func hosts(res *upstream.Result, provider string, skipped map[feed]bool) int {
	left := map[string]bool{}
	for _, s := range res.Streams {
		if s.Playable() && !skipped[feed{provider, server(s)}] {
			left[server(s)] = true
		}
	}
	return len(left)
}

// controls is the action menu for one episode
// change stream is offered while the provider has a server besides the one
// about to play, since a stream that dies partway keeps its player and the
// provider's other hosts are otherwise out of reach
func controls(numbers []float64, ep float64, servers int) []string {
	_, hasNext := neighbor(numbers, ep, +1)
	_, hasPrev := neighbor(numbers, ep, -1)

	var actions []string
	if hasNext {
		actions = append(actions, "next")
	}
	actions = append(actions, "replay")
	if hasPrev {
		actions = append(actions, "previous")
	}
	actions = append(actions, "select")
	if servers > 1 {
		actions = append(actions, "change stream")
	}
	return append(actions, "change provider", "quit")
}

// apply maps a menu action to the next step, quit included
func apply(action string, numbers []float64, ep float64) (step, bool) {
	switch action {
	case "next":
		n, _ := neighbor(numbers, ep, +1)
		return step{ep: n}, false
	case "previous":
		p, _ := neighbor(numbers, ep, -1)
		return step{ep: p}, false
	case "replay":
		return step{ep: ep}, false
	case "select":
		return step{reselect: true}, false
	case "change stream":
		return step{ep: ep, restream: true}, false
	case "change provider":
		return step{ep: ep, reprovide: true}, false
	default:
		return step{}, true
	}
}
