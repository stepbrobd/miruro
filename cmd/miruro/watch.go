package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/charmbracelet/log"

	"ysun.co/miruro"
	"ysun.co/miruro/play"
	"ysun.co/miruro/ui"
)

func (s *runState) watch(ctx context.Context, st *store, numbers, queue []float64, pin Pin, player play.Player) error {
	px, err := play.StartProxy(ctx)
	if err != nil {
		return err
	}
	defer px.Close()

	details := s.cat.Details(s.category)
	ep := queue[0]
	queue = queue[1:]

	for {
		res, src, carry, err := s.resolve(ctx, ep, pin)
		if err != nil {
			return err
		}
		// carry the user's intent across episodes
		// a transient fallback serves another provider but must not overwrite the pin
		pin = carry

		var skips []miruro.SkipRange
		if flagSkip {
			skips = episodeSkips(s.cat, ep)
		}

		mediaTitle := fmt.Sprintf("%s Episode %s", s.title, num(ep))
		if d := details[ep]; d.Title != "" {
			mediaTitle += " - " + d.Title
		}

		stage := playback{
			runState: s, px: px, pin: pin, ep: ep, kind: player.Kind,
			launch: func(pctx context.Context, stream miruro.Stream, subs []miruro.Subtitle) error {
				return player.Play(pctx, px.Stream(stream), px.Subtitles(subs, stream.Referer), skips, mediaTitle)
			},
		}

		e := entry{AnilistID: s.anilistID, Title: s.title, Provider: pin.String(), Category: s.category, Episode: ep}
		action, err := playAndControl(ctx,
			fmt.Sprintf("Episode %s of %s", num(ep), s.title),
			controls(numbers, ep),
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

// playAndControl runs one playback with the action menu raised over it and
// joins both before returning the picked action, "" on a dismissal
// the menu is up while the player runs, so a pick races playback ending
// an early pick interrupts the player, a clean end mid-batch dismisses the
// menu to auto-advance
func playAndControl(ctx context.Context, title string, actions []string, batch bool, run func(context.Context) error, save func() error) (string, error) {
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
	action, ended, err := ui.Control(ctx, title, actions, wait)
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
}

func controls(numbers []float64, ep float64) []string {
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
	return append(actions, "select", "change provider", "quit")
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
	case "change provider":
		return step{ep: ep, reprovide: true}, false
	default:
		return step{}, true
	}
}
