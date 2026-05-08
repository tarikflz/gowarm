package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	stdhttp "net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	httpclient "github.com/tarikflz/gowarm/internal/http"
	"github.com/tarikflz/gowarm/internal/retry"
)

// fakeWarmer is a CacheWarmer test double.
type fakeWarmer struct {
	calls atomic.Int64
	fail  bool
}

func (f *fakeWarmer) Warm(_ context.Context, url string, _ stdhttp.Header, _ []*stdhttp.Cookie) (*httpclient.WarmResult, error) {
	f.calls.Add(1)
	if f.fail {
		return &httpclient.WarmResult{URL: url, Status: 500}, errors.New("boom")
	}
	return &httpclient.WarmResult{URL: url, Status: 200, Bytes: 42, Duration: time.Millisecond}, nil
}

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestPool_Success(t *testing.T) {
	t.Parallel()
	w := &fakeWarmer{}
	rt := retry.NewExponentialBackoff(time.Millisecond, 1, 0)
	p := NewPool(4, w, rt, newSilentLogger())

	ctx := context.Background()
	p.Start(ctx)
	for i := 0; i < 20; i++ {
		p.Submit(ctx, Job{
			URL:  "https://example.com/x",
			Tags: map[string]string{"region": "qc", "language": "en"},
		})
	}
	p.Stop()
	if err := p.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	s := p.Stats().Snapshot()
	if s.Success != 20 || s.Failed != 0 || s.Total != 20 {
		t.Errorf("stats wrong: %+v", s)
	}
	if w.calls.Load() != 20 {
		t.Errorf("warmer called %d, want 20", w.calls.Load())
	}
}

func TestPool_Failures(t *testing.T) {
	t.Parallel()
	w := &fakeWarmer{fail: true}
	rt := &retry.ExponentialBackoff{
		BaseDelay:   time.Microsecond,
		Factor:      2.0,
		MaxAttempts: 2,
	}
	p := NewPool(2, w, rt, newSilentLogger())

	ctx := context.Background()
	p.Start(ctx)
	for i := 0; i < 3; i++ {
		p.Submit(ctx, Job{URL: "https://example.com/x"})
	}
	p.Stop()
	_ = p.Wait(ctx)
	s := p.Stats().Snapshot()
	if s.Failed != 3 {
		t.Errorf("expected 3 failures, got %d", s.Failed)
	}
	// 2 attempts × 3 jobs
	if w.calls.Load() != 6 {
		t.Errorf("warmer called %d, want 6", w.calls.Load())
	}
}

func TestPool_ContextCancelStopsAcceptingJobs(t *testing.T) {
	t.Parallel()
	w := &fakeWarmer{}
	rt := retry.NewExponentialBackoff(time.Millisecond, 1, 0)
	p := NewPool(1, w, rt, newSilentLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	p.Start(ctx)
	for i := 0; i < 5; i++ {
		p.Submit(ctx, Job{URL: "x"})
	}
	p.Stop()
	if err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.calls.Load() != 0 {
		t.Errorf("expected 0 calls when ctx cancelled, got %d", w.calls.Load())
	}
}

func TestPool_OnComplete_FiresOnSuccess(t *testing.T) {
	t.Parallel()
	w := &fakeWarmer{}
	rt := retry.NewExponentialBackoff(time.Millisecond, 1, 0)
	p := NewPool(2, w, rt, newSilentLogger())

	var (
		mu      sync.Mutex
		results []JobResult
	)
	p.OnComplete = func(r JobResult) {
		mu.Lock()
		defer mu.Unlock()
		results = append(results, r)
	}

	ctx := context.Background()
	p.Start(ctx)
	for i := 0; i < 5; i++ {
		p.Submit(ctx, Job{
			URL:  "https://example.com/x",
			Tags: map[string]string{"region": "qc"},
		})
	}
	p.Stop()
	if err := p.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	if len(results) != 5 {
		t.Fatalf("expected 5 OnComplete callbacks, got %d", len(results))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("[%d] expected nil err, got %v", i, r.Err)
		}
		if r.Status != 200 {
			t.Errorf("[%d] expected status 200, got %d", i, r.Status)
		}
		if r.Bytes != 42 {
			t.Errorf("[%d] expected 42 bytes, got %d", i, r.Bytes)
		}
		if r.URL != "https://example.com/x" {
			t.Errorf("[%d] url mismatch: %q", i, r.URL)
		}
		if r.Tags["region"] != "qc" {
			t.Errorf("[%d] tags missing: %v", i, r.Tags)
		}
	}
}

func TestPool_OnComplete_FiresOnFailure(t *testing.T) {
	t.Parallel()
	w := &fakeWarmer{fail: true}
	rt := &retry.ExponentialBackoff{
		BaseDelay:   time.Microsecond,
		Factor:      2.0,
		MaxAttempts: 1,
	}
	p := NewPool(1, w, rt, newSilentLogger())

	var (
		mu      sync.Mutex
		results []JobResult
	)
	p.OnComplete = func(r JobResult) {
		mu.Lock()
		defer mu.Unlock()
		results = append(results, r)
	}

	ctx := context.Background()
	p.Start(ctx)
	p.Submit(ctx, Job{URL: "https://example.com/x"})
	p.Stop()
	_ = p.Wait(ctx)

	if len(results) != 1 {
		t.Fatalf("expected 1 OnComplete callback, got %d", len(results))
	}
	r := results[0]
	if r.Err == nil {
		t.Error("expected non-nil err on failed job")
	}
	if r.Status != 500 {
		t.Errorf("expected status 500 (from fakeWarmer), got %d", r.Status)
	}
}
