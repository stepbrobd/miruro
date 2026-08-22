package play

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"time"
)

// attempts bounds how many times one fetch is tried
// providers answer 5xx and drop connections mid-episode, and a single dead read
// used to abort a whole episode
const attempts = 3

// pause is the wait before retry n, counting from one
// it is a variable so a test can drop the wait rather than sleep through it
var pause = func(n int) time.Duration { return time.Duration(1<<(n-1)) * time.Second }

// errTooLarge marks a body over its cap
// a cap guards against a hostile or broken upstream rather than a hiccup, so
// refetching one only pays for the same oversized body again
var errTooLarge = errors.New("body exceeds its limit")

// status is a non-200 answer, kept typed so retry can tell a transient 502 from
// a permanent 404
type status int

func (s status) Error() string { return fmt.Sprintf("status %d", int(s)) }

// retry runs op until it succeeds, until its failure stops being transient, or
// until the attempts run out
// op is repeated whole, so one that writes to disk must truncate on entry
func retry(ctx context.Context, op func() error) error {
	var err error
	for n := range attempts {
		// a cancelled run must not start another attempt
		// the select below cannot promise that on its own, since a fired timer
		// and a done context are both ready and the choice between them is random
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if n > 0 {
			select {
			case <-time.After(pause(n)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err = op(); err == nil || !transient(err) {
			return err
		}
	}
	return err
}

// transient reports whether another attempt could plausibly succeed
// a retryable status, a dropped connection, and a body that failed its
// plausibility check are all worth another go
// a cancelled context, a filesystem error, and a permanent status are not
func transient(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, errTooLarge):
		return false
	}
	var s status
	if errors.As(err, &s) {
		return retryable(int(s))
	}
	// a file that cannot be written will not write on the next attempt either
	var pe *fs.PathError
	return !errors.As(err, &pe)
}

// retryable reports whether a status is worth another attempt
// 5xx, 408, 425, and 429 are transient by definition, and every other status, a
// 403 or a 404 included, says the URL itself is wrong
func retryable(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return code >= 500
}
