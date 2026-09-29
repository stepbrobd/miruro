package main

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/ui"
	"ysun.co/miruro/internal/upstream"
)

// resolve resolves an episode and returns the source that served it and the pin
// to carry forward
// with a pinned provider it resolves without prompting and carries the pin
// unchanged
// with no pin it asks once, for a provider and its subtitle rendition together,
// and the pick is the pin whether or not that provider ends up serving
func (s *runState) resolve(ctx context.Context, ep float64, pin Pin) (*upstream.Result, source, Pin, error) {
	if pin.Code != "" {
		res, src, err := s.autoResolve(ctx, ep, pin, nil)
		return res, src, pin, err
	}

	l, err := s.listing(ctx, ep)
	if err != nil {
		return nil, source{}, pin, err
	}
	avail, err := candidates(l, ep, s.category)
	if err != nil {
		return nil, source{}, pin, err
	}

	rows := offers(avail, l.Caps, s.category, pin)
	width := widest(rows)
	pick, err := ui.Select("Select provider", rows, func(o offer) string { return o.label(width) })
	if err != nil {
		return nil, source{}, pin, err
	}
	res, src, err := s.walk(ctx, l, ep, pick.Pin, nil)
	return res, src, pick.Pin, err
}

// listing asks the backends that listed the title which providers serve one
// episode
// a backend that refused this client is not asked again, and its refusal is
// what comes back when no other backend lists a provider
// one that cannot answer costs the episode its providers and nothing else
func (s *runState) listing(ctx context.Context, ep float64) (*upstream.Listing, error) {
	var ask upstream.Backends
	var blocked error
	for _, b := range s.backends {
		if err := s.refused.get(b); err != nil {
			blocked = err
			continue
		}
		ask = append(ask, b)
	}

	l, failed := ask.Listing(ctx, s.cat, ep)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var errs []error
	for _, f := range failed {
		i := slices.IndexFunc(ask, func(b upstream.Backend) bool { return b.Name() == f.Backend })
		if errors.Is(f.Err, upstream.ErrBlocked) && i >= 0 {
			if s.refused.add(ask[i], f.Err) {
				log.Warn("backend refused the run, skipping its providers", "backend", f.Backend, "err", f.Err)
			}
			blocked = f.Err
			continue
		}
		errs = append(errs, f)
	}
	if len(l.Providers) > 0 {
		for _, err := range errs {
			log.Warn("backend did not list the episode", "episode", num(ep), "err", err)
		}
		return l, nil
	}
	if blocked != nil {
		return nil, blocked
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return l, nil
}

// autoResolve lists the episode and walks its providers, never prompting
func (s *runState) autoResolve(ctx context.Context, ep float64, pin Pin, skip map[string]bool) (*upstream.Result, source, error) {
	l, err := s.listing(ctx, ep)
	if err != nil {
		return nil, source{}, err
	}
	return s.walk(ctx, l, ep, pin, skip)
}

// walk tries the pinned pick first then the rest of what l lists
// skip names providers a caller has already used, so an episode being retried
// moves on instead of resolving the same dead source again
// a backend that refuses this client takes its other providers out of the walk
// with it for the rest of the run, since each would cost a request against the
// same refusal, and the refusal is what is reported when nothing else served
// a provider named in the config or on the command line holds the run to
// itself, since trading a stated choice for another provider without being
// asked is what --fallback exists to allow
func (s *runState) walk(ctx context.Context, l *upstream.Listing, ep float64, pin Pin, skip map[string]bool) (*upstream.Result, source, error) {
	avail, err := candidates(l, ep, s.category)
	if err != nil {
		return nil, source{}, err
	}

	// the first walk of an episode says why the pin was passed over, and a walk
	// retrying the episode has already said it
	if _, listed := l.Providers[pin.Code]; pin.Code != "" && !listed && s.fallback && len(skip) == 0 {
		log.Warn("pinned provider does not serve the episode, using the preference order", "provider", pin.Code, "episode", num(ep))
	}

	rows := orderPinned(offers(avail, l.Caps, s.category, pin), pin)
	if !s.fallback && pin.Code != "" {
		// both renditions of the pinned provider stay, since either is still it
		rows = slices.DeleteFunc(rows, func(o offer) bool { return o.Code != pin.Code })
	}

	var last, blocked error
	for _, o := range rows {
		p := l.Providers[o.Code]
		if skip[o.Code] {
			continue
		}
		if err := s.refused.get(p.Backend); err != nil {
			blocked = err
			continue
		}
		src := o.source(s.category)
		e := find(p.Episodes(src.Category), ep)
		if e == nil {
			continue
		}
		res, err := l.Sources(ctx, e.ID, o.Code, src.Category)
		if err != nil {
			if ctx.Err() != nil {
				return nil, source{}, ctx.Err()
			}
			if errors.Is(err, upstream.ErrBlocked) {
				if s.refused.add(p.Backend, err) {
					log.Warn("backend refused the run, skipping its providers", "backend", p.Backend.Name(), "err", err)
				}
				blocked = err
				continue
			}
			// a provider that fails to resolve is reported only when none served,
			// so the pinned one says so as it happens and the rest under --verbose
			if o.Code == pin.Code {
				// with the walk held to this provider there is no next to try
				what := "trying the next"
				if !s.fallback {
					what = "and the walk is held to it"
				}
				log.Warn("pinned provider did not resolve, "+what, "provider", o.Code, "err", err)
			} else {
				log.Debug("provider did not resolve, trying the next", "provider", o.Code, "err", err)
			}
			// name the provider so a report points at the one that failed
			last = fmt.Errorf("%s: %w", o.Code, err)
			continue
		}
		// an embed-only result is not playable, so skip it inside the loop rather
		// than fail later at Select outside it
		if !res.Playable() {
			last = fmt.Errorf("%s has no playable stream", o.Code)
			continue
		}
		return res, src, nil
	}
	switch {
	case blocked != nil:
		return nil, source{}, blocked
	case !s.fallback && pin.Code != "":
		// a caller retrying an episode has already put the pin in skip, so a walk
		// that reached nothing says the pin is spent rather than that it carries
		// nothing, which is false when it resolved and then failed to play
		if last == nil {
			if skip[pin.Code] {
				last = fmt.Errorf("%s is the only provider this run may use", pin.Code)
			} else if _, ok := l.Providers[pin.Code]; !ok {
				last = fmt.Errorf("%s does not serve episode %s", pin.Code, num(ep))
			} else {
				last = fmt.Errorf("%s carries no stream for episode %s", pin.Code, num(ep))
			}
		}
		return nil, source{}, fmt.Errorf("%w (pass --fallback to try the rest)", last)
	case last == nil:
		last = fmt.Errorf("no provider resolved a stream for episode %s", num(ep))
	}
	return nil, source{}, last
}
