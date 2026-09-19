package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/charmbracelet/log"

	"ysun.co/miruro"
	"ysun.co/miruro/ui"
)

// resolve resolves an episode and returns the source that served it and the pin
// to carry forward
// with a pinned provider it resolves with fallback and carries the pin unchanged
// with no pin it asks once, for a provider and its subtitle rendition together,
// and the pick is the pin whether or not that provider ends up serving
func (s *runState) resolve(ctx context.Context, ep float64, pin Pin) (*miruro.Result, source, Pin, error) {
	if pin.Code != "" {
		res, src, err := s.autoResolve(ctx, ep, pin, nil)
		return res, src, pin, err
	}

	avail, err := candidates(s.cat, ep, s.category, s.caps)
	if err != nil {
		return nil, source{}, pin, err
	}

	rows := offers(avail, s.caps, s.category, pin)
	width := widest(rows)
	pick, err := ui.Select("Select provider", rows, func(o offer) string { return o.label(width) })
	if err != nil {
		return nil, source{}, pin, err
	}
	res, src, err := s.autoResolve(ctx, ep, pick.Pin, nil)
	return res, src, pick.Pin, err
}

// autoResolve tries the pinned pick first then the rest, never prompting
// skip names providers a caller has already used, so an episode being retried
// moves on instead of resolving the same dead source again
// a backend that refuses this client takes its other providers out of the walk
// with it for the rest of the run, since each would cost a request against the
// same refusal, and the refusal is what is reported when nothing else served
func (s *runState) autoResolve(ctx context.Context, ep float64, pin Pin, skip map[string]bool) (*miruro.Result, source, error) {
	avail, err := candidates(s.cat, ep, s.category, s.caps)
	if err != nil {
		return nil, source{}, err
	}

	var last, blocked error
	for _, o := range orderPinned(offers(avail, s.caps, s.category, pin), pin) {
		p := s.cat.Providers[o.Code]
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
		res, err := s.cat.Sources(ctx, e.ID, o.Code, src.Category)
		if err != nil {
			if ctx.Err() != nil {
				return nil, source{}, ctx.Err()
			}
			if errors.Is(err, miruro.ErrBlocked) {
				if s.refused.add(p.Backend, err) {
					log.Warn("backend refused the run, skipping its providers", "backend", p.Backend.Name(), "err", err)
				}
				blocked = err
				continue
			}
			// a provider that fails to resolve is reported only when none served,
			// so the pinned one says so as it happens and the rest under --verbose
			if o.Code == pin.Code {
				log.Warn("pinned provider did not resolve, trying the next", "provider", o.Code, "err", err)
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
	case last == nil:
		last = fmt.Errorf("no source resolved for episode %s", num(ep))
	}
	return nil, source{}, last
}
