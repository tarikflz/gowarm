// Package retry implements an exponential-backoff retrier with jitter that is
// fully cancellable via context. Operations may signal that an error is fatal
// (e.g. a 4xx response) by returning a *Permanent error; the retrier will then
// stop immediately.
package retry

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"time"
)

// Retrier is the contract used by the worker pool.
type Retrier interface {
	Execute(ctx context.Context, op func(ctx context.Context) error) error
}

// Permanent wraps an error to signal the retrier that no further attempts
// should be made. Useful for HTTP 4xx responses, validation failures, etc.
type Permanent struct {
	Err error
}

func (p *Permanent) Error() string {
	if p.Err == nil {
		return "permanent error"
	}
	return p.Err.Error()
}

func (p *Permanent) Unwrap() error { return p.Err }

// PermanentError builds a Permanent wrapping err.
func PermanentError(err error) error {
	if err == nil {
		return nil
	}
	return &Permanent{Err: err}
}

// IsPermanent reports whether err (or any error in its chain) is *Permanent.
func IsPermanent(err error) bool {
	var p *Permanent
	return errors.As(err, &p)
}

// ExponentialBackoff implements Retrier with jittered exponential backoff.
type ExponentialBackoff struct {
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      time.Duration
	Factor      float64
	MaxAttempts int
}

// NewExponentialBackoff returns a configured retrier.
func NewExponentialBackoff(base time.Duration, maxAttempts int, jitter time.Duration) *ExponentialBackoff {
	return &ExponentialBackoff{
		BaseDelay:   base,
		MaxDelay:    30 * time.Second,
		Jitter:      jitter,
		Factor:      2.0,
		MaxAttempts: maxAttempts,
	}
}

// Execute runs op with exponential backoff. It returns the last error if all
// attempts fail, ctx.Err() on cancellation, or nil on success.
func (e *ExponentialBackoff) Execute(ctx context.Context, op func(ctx context.Context) error) error {
	if e.MaxAttempts < 1 {
		e.MaxAttempts = 1
	}
	if e.Factor <= 0 {
		e.Factor = 2.0
	}

	var lastErr error
	for attempt := 0; attempt < e.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if IsPermanent(err) {
			return err
		}
		if attempt == e.MaxAttempts-1 {
			break
		}
		delay := e.computeDelay(attempt)
		if !sleepCtx(ctx, delay) {
			return ctx.Err()
		}
	}
	return lastErr
}

func (e *ExponentialBackoff) computeDelay(attempt int) time.Duration {
	exp := math.Pow(e.Factor, float64(attempt))
	delay := time.Duration(float64(e.BaseDelay) * exp)
	if e.MaxDelay > 0 && delay > e.MaxDelay {
		delay = e.MaxDelay
	}
	if e.Jitter > 0 {
		j := rand.Int64N(int64(e.Jitter))
		delay += time.Duration(j)
	}
	return delay
}

// sleepCtx waits for d or ctx cancellation, whichever first. Returns false if
// ctx was cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
