package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenBucket_RateLimits(t *testing.T) {
	t.Parallel()
	// 10 RPS, burst=2 → 5 sequential tokens should take ~3 refill periods
	// (2 burst + 3 refills @ 100ms = ~300ms).
	tb := newTokenBucket(10, 2)
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := tb.Wait(ctx); err != nil {
			t.Fatalf("wait[%d]: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 250*time.Millisecond {
		t.Errorf("expected at least 250ms for 5 tokens at 10rps burst=2, got %s", elapsed)
	}
	if elapsed > 600*time.Millisecond {
		t.Errorf("expected at most 600ms for 5 tokens at 10rps burst=2, got %s", elapsed)
	}
}

func TestTokenBucket_BurstAllowsImmediate(t *testing.T) {
	t.Parallel()
	tb := newTokenBucket(1, 5) // 1 RPS but burst of 5 → 5 immediate tokens
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := tb.Wait(ctx); err != nil {
			t.Fatalf("wait[%d]: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("burst should grant 5 tokens immediately, got %s", elapsed)
	}
}

func TestTokenBucket_CtxCancel(t *testing.T) {
	t.Parallel()
	tb := newTokenBucket(0.5, 1) // very slow
	// drain the burst
	if err := tb.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := tb.Wait(ctx)
	if err == nil {
		t.Fatal("expected ctx cancel error")
	}
}

func TestTokenBucket_SetMultiplier(t *testing.T) {
	t.Parallel()
	tb := newTokenBucket(100, 1) // 100 RPS
	ctx := context.Background()

	// Drain burst then halve the rate.
	_ = tb.Wait(ctx)
	tb.SetMultiplier(0.1) // effective 10 RPS

	start := time.Now()
	if err := tb.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	// At 10 RPS we expect ~100ms before the next token. The cap inside Wait
	// is 250ms per iteration so worst case is 250ms; tolerate a wide range.
	if elapsed < 50*time.Millisecond {
		t.Errorf("multiplier did not slow the bucket: got %s", elapsed)
	}
}

func TestThrottle_PerHostConcurrency(t *testing.T) {
	t.Parallel()
	// Server that sleeps so we can observe concurrent in-flight count.
	var (
		inflight atomic.Int32
		maxObs   atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cur := inflight.Add(1)
		for {
			old := maxObs.Load()
			if cur <= old || maxObs.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inflight.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cli := New(Options{
		Timeout:         2 * time.Second,
		Method:          http.MethodGet,
		MaxConnsPerHost: 2, // cap to 2 concurrent
	})

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = cli.Warm(context.Background(), srv.URL, nil, nil)
		}()
	}
	wg.Wait()

	if got := maxObs.Load(); got > 2 {
		t.Errorf("max in-flight per host = %d, want <= 2", got)
	}
}

func TestThrottle_AdaptiveSlowdownOn429(t *testing.T) {
	t.Parallel()
	// First two responses return 429 to trigger the slowdown; subsequent
	// requests should pace more slowly.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cli := New(Options{
		Timeout:                  2 * time.Second,
		Method:                   http.MethodGet,
		RateLimitRPS:             100, // base = 100 rps
		Burst:                    1,
		AdaptiveSlowdown:         true,
		AdaptiveSlowdownFactor:   0.05, // 5% of base = 5 rps
		AdaptiveSlowdownDuration: 200 * time.Millisecond,
	})

	// First call: 429 → triggers slowdown.
	_, err := cli.Warm(context.Background(), srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected error on 429")
	}

	// Now measure the time for the next two requests; they should be paced
	// at 5 rps so the second should arrive ~200ms after the first.
	start := time.Now()
	_, _ = cli.Warm(context.Background(), srv.URL, nil, nil)
	_, _ = cli.Warm(context.Background(), srv.URL, nil, nil)
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("adaptive slowdown didn't pace requests: 2 reqs in %s", elapsed)
	}
}

func TestThrottle_DisabledByDefault(t *testing.T) {
	t.Parallel()
	if newThrottle(Options{}) != nil {
		t.Error("throttle should be nil when no rate / host / adaptive options are set")
	}
}

func TestThrottle_NilSafe(t *testing.T) {
	t.Parallel()
	var t0 *throttle // nil
	rel, err := t0.acquire(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	rel()           // must not panic
	t0.observe(429) // must not panic
}
