package httpclient

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// throttle bundles three independent concerns gowarm needs in front of every
// outbound HTTP request:
//
//  1. A global token-bucket rate limiter (RPS + burst).
//  2. A per-host semaphore that caps the number of in-flight requests
//     against a single hostname.
//  3. An optional adaptive slowdown that temporarily lowers the rate
//     limiter's effective limit whenever the origin returns 429 or 503.
//
// All three are independently optional. A throttle with a nil limiter, no
// host slots, and adaptive slowdown disabled is a no-op.
type throttle struct {
	limiter *tokenBucket // global RPS bucket; nil = no limit

	hostMu     sync.Mutex
	hostSlots  map[string]chan struct{}
	maxPerHost int

	adaptiveEnabled  bool
	adaptiveFactor   float64
	adaptiveDuration time.Duration
	throttleUntil    atomic.Int64 // unix nano of when the slowdown ends; 0 = idle
}

// newThrottle builds a throttle from the public Options. It returns nil if
// every feature is disabled, so the caller can short-circuit the slow path.
func newThrottle(opts Options) *throttle {
	t := &throttle{
		hostSlots:        make(map[string]chan struct{}),
		maxPerHost:       opts.MaxConnsPerHost,
		adaptiveEnabled:  opts.AdaptiveSlowdown,
		adaptiveFactor:   opts.AdaptiveSlowdownFactor,
		adaptiveDuration: opts.AdaptiveSlowdownDuration,
	}
	if opts.RateLimitRPS > 0 {
		burst := opts.Burst
		if burst <= 0 {
			burst = 1
		}
		t.limiter = newTokenBucket(opts.RateLimitRPS, burst)
	}
	if t.limiter == nil && t.maxPerHost == 0 && !t.adaptiveEnabled {
		return nil
	}
	return t
}

// acquire blocks until the request may proceed. It honours ctx cancellation.
// The returned release func MUST be called once the request is done so the
// host's semaphore slot can be returned (release is a no-op when no host
// limit is configured).
func (t *throttle) acquire(ctx context.Context, host string) (release func(), err error) {
	if t == nil {
		return func() {}, nil
	}

	if t.limiter != nil {
		if err := t.limiter.Wait(ctx); err != nil {
			return nil, err
		}
	}
	if t.maxPerHost > 0 && host != "" {
		ch := t.slotsFor(host)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ch <- struct{}{}:
			return func() { <-ch }, nil
		}
	}
	return func() {}, nil
}

// slotsFor lazily creates (and memoises) the buffered channel that backs the
// per-host semaphore.
func (t *throttle) slotsFor(host string) chan struct{} {
	t.hostMu.Lock()
	defer t.hostMu.Unlock()
	ch, ok := t.hostSlots[host]
	if !ok {
		ch = make(chan struct{}, t.maxPerHost)
		t.hostSlots[host] = ch
	}
	return ch
}

// observe is called after every HTTP response. When adaptive slowdown is
// enabled and status is 429 or 503, the limiter's rate is temporarily
// reduced. Subsequent observations within the slowdown window keep extending
// the throttle. Once the window expires, the original limit is restored.
//
// observe is safe to call concurrently from many worker goroutines.
func (t *throttle) observe(status int) {
	if t == nil || !t.adaptiveEnabled || t.limiter == nil {
		return
	}
	if status != 429 && status != 503 {
		return
	}
	until := time.Now().Add(t.adaptiveDuration).UnixNano()
	prev := t.throttleUntil.Swap(until)
	if prev == 0 {
		// First time entering the slowdown window: lower the limiter and
		// schedule the recovery.
		t.limiter.SetMultiplier(t.adaptiveFactor)
		go t.recoverLoop()
	}
}

// recoverLoop blocks until the latest throttleUntil deadline passes, then
// restores the limiter's base limit. Concurrent observe calls update
// throttleUntil so this loop stays alive until the origin recovers.
func (t *throttle) recoverLoop() {
	for {
		until := t.throttleUntil.Load()
		if until == 0 {
			return
		}
		now := time.Now().UnixNano()
		if now >= until {
			if t.throttleUntil.CompareAndSwap(until, 0) {
				t.limiter.SetMultiplier(1.0)
				return
			}
			continue
		}
		time.Sleep(time.Duration(until - now))
	}
}

// ---------------------------------------------------------------------------
// tokenBucket: a simple, ctx-aware rate limiter.
// ---------------------------------------------------------------------------

// tokenBucket is a thread-safe token-bucket rate limiter. Tokens accumulate
// at `effectiveRate` per second up to `capacity`. Wait blocks until one
// token is available or ctx is cancelled.
//
// SetMultiplier scales the effective rate by `m` relative to baseRate. A
// multiplier of 0.5 halves the rate; 1.0 restores the original.
type tokenBucket struct {
	mu sync.Mutex

	baseRate   float64   // configured rate (tokens/sec)
	multiplier float64   // current scale (0 < m <= 1 typically)
	capacity   int       // max tokens
	tokens     float64   // current tokens
	lastFilled time.Time // last refill time
}

func newTokenBucket(rps float64, burst int) *tokenBucket {
	if burst < 1 {
		burst = 1
	}
	return &tokenBucket{
		baseRate:   rps,
		multiplier: 1.0,
		capacity:   burst,
		tokens:     float64(burst),
		lastFilled: time.Now(),
	}
}

// SetMultiplier scales the active fill rate. The new rate becomes
// baseRate * m. Non-positive values are clamped to a tiny floor so the
// limiter doesn't deadlock.
func (tb *tokenBucket) SetMultiplier(m float64) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if m <= 0 {
		m = 0.01
	}
	tb.multiplier = m
}

// Wait blocks until a token is available (one token per call) or ctx is done.
// On context cancellation it returns ctx.Err() without consuming a token.
func (tb *tokenBucket) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		tb.mu.Lock()
		tb.refillLocked()
		if tb.tokens >= 1 {
			tb.tokens -= 1
			tb.mu.Unlock()
			return nil
		}
		rate := tb.baseRate * tb.multiplier
		if rate <= 0 {
			rate = 0.01
		}
		need := 1 - tb.tokens
		waitFor := time.Duration(need / rate * float64(time.Second))
		tb.mu.Unlock()
		// Cap the per-iteration sleep so a SetMultiplier-induced rate change
		// is observed within a bounded delay even by long-waiting callers.
		if waitFor > 250*time.Millisecond {
			waitFor = 250 * time.Millisecond
		}
		t := time.NewTimer(waitFor)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// refillLocked tops up the bucket. Caller must hold tb.mu.
func (tb *tokenBucket) refillLocked() {
	now := time.Now()
	elapsed := now.Sub(tb.lastFilled).Seconds()
	if elapsed <= 0 {
		return
	}
	rate := tb.baseRate * tb.multiplier
	tb.tokens += elapsed * rate
	if tb.tokens > float64(tb.capacity) {
		tb.tokens = float64(tb.capacity)
	}
	tb.lastFilled = now
}
