package upstream

import (
	"context"
	"sync"
	"time"
)

// Pacer lets Burst requests leave at once and one per Every after that, the
// generic cell rate algorithm, and holds every request through a wait an
// upstream asked for
// a turn is counted from when it is asked for, so an idle spell banks at most
// Burst turns
// the zero Pacer spaces nothing and still sits out a hold
type Pacer struct {
	Burst int
	Every time.Duration

	mu sync.Mutex
	// due is when every turn handed out so far would have come at the steady
	// rate, and a turn comes once due runs at most Burst-1 intervals ahead
	due time.Time
	// until is when the last hold ends
	until time.Time
}

// Wait holds the caller until its turn, or until ctx ends
// a turn that comes inside a hold begun while the caller slept is given up for
// a fresh one, so a hold keeps back the requests already queued as well
func (p *Pacer) Wait(ctx context.Context) error {
	for {
		p.mu.Lock()
		now := time.Now()
		due := p.due
		if due.Before(now) {
			due = now
		}
		at := due.Add(-p.slack())
		p.due = due.Add(p.Every)
		p.mu.Unlock()

		if err := sleep(ctx, at.Sub(now)); err != nil {
			return err
		}
		p.mu.Lock()
		held := p.until.After(time.Now())
		p.mu.Unlock()
		if !held {
			return nil
		}
	}
}

// Hold keeps every request that has not left back for d, the wait a 429
// named, and lets them go one per Every afterwards
func (p *Pacer) Hold(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if until := now.Add(d); until.After(p.until) {
		p.until = until
	}
	if due := now.Add(d + p.slack()); due.After(p.due) {
		p.due = due
	}
}

// slack is how far due may run ahead of a turn that comes at once
func (p *Pacer) slack() time.Duration {
	return time.Duration(max(p.Burst-1, 0)) * p.Every
}

// sleep waits d or until ctx ends, whichever is first
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
