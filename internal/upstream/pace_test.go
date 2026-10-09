package upstream

import (
	"context"
	"errors"
	"testing"
	"time"
)

// requests are spaced from when each asks, so a caller that waited elsewhere
// does not bank its turns into a burst, and a canceled caller stops waiting
func TestPacerSpacesRequests(t *testing.T) {
	p := &Pacer{Burst: 1, Every: 20 * time.Millisecond}
	ctx := context.Background()
	start := time.Now()
	for range 5 {
		if err := p.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took < 80*time.Millisecond {
		t.Errorf("five turns took %v, want at least four gaps", took)
	}

	time.Sleep(100 * time.Millisecond)
	start = time.Now()
	for range 3 {
		p.Wait(ctx)
	}
	if took := time.Since(start); took < 40*time.Millisecond {
		t.Errorf("three turns after an idle spell took %v, want at least two gaps", took)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	p.Every = time.Hour
	p.Wait(ctx)
	if err := p.Wait(canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled wait returned %v", err)
	}
}

// a burst leaves at once and every request past it one interval after the last
func TestPacerBursts(t *testing.T) {
	ctx := context.Background()
	every := 100 * time.Millisecond
	p := &Pacer{Burst: 3, Every: every}

	start := time.Now()
	for range 3 {
		if err := p.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took >= every {
		t.Errorf("the burst took %v, want it to leave at once", took)
	}
	for range 2 {
		if err := p.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took < 2*every {
		t.Errorf("five turns took %v, want the two past the burst an interval apart", took)
	}
}

// a hold keeps a later request back for the wait it names, and the ones after
// it leave an interval apart rather than as a fresh burst into the same limit
func TestPacerHolds(t *testing.T) {
	ctx := context.Background()
	every, d := 50*time.Millisecond, 100*time.Millisecond
	p := &Pacer{Burst: 4, Every: every}

	start := time.Now()
	p.Hold(d)
	if err := p.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < d {
		t.Errorf("the first turn came after %v, want the hold of %v", took, d)
	}
	if err := p.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < d+every {
		t.Errorf("the second turn came after %v, want an interval past the hold", took)
	}
}

// a request already queued when the hold begins is held too, since its turn
// would otherwise send it into the limit the hold is waiting out
func TestPacerHoldsTheQueue(t *testing.T) {
	ctx := context.Background()
	d := 300 * time.Millisecond
	p := &Pacer{Burst: 1, Every: 50 * time.Millisecond}
	if err := p.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	left := make(chan time.Duration)
	go func() {
		p.Wait(ctx)
		left <- time.Since(start)
	}()
	time.Sleep(10 * time.Millisecond)
	p.Hold(d)
	if took := <-left; took < d {
		t.Errorf("the queued turn came after %v, inside the hold of %v", took, d)
	}
}

// the zero pacer holds nothing back until a hold asks it to, and a turn still
// far off ends with the context waiting for it
func TestPacerZeroAndCanceled(t *testing.T) {
	var zero Pacer
	start := time.Now()
	for range 100 {
		if err := zero.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the zero pacer took %v over 100 turns, want none held", took)
	}
	zero.Hold(50 * time.Millisecond)
	start = time.Now()
	zero.Wait(context.Background())
	if took := time.Since(start); took < 50*time.Millisecond {
		t.Errorf("the zero pacer came after %v, want it to sit out the hold", took)
	}

	p := &Pacer{Burst: 1, Every: time.Hour}
	p.Wait(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the wait to end with its context", err)
	}
}
