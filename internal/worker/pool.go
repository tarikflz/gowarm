// Package worker is a fixed-size goroutine pool that consumes Job values and
// hands them off to a CacheWarmer behind a Retrier. The pool is the only
// component aware of concurrency: parser, retry, and httpclient packages are
// all single-threaded.
package worker

import (
	"context"
	"errors"
	"log/slog"
	stdhttp "net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tarikflz/gowarm/internal/config"
	httpclient "github.com/tarikflz/gowarm/internal/http"
	"github.com/tarikflz/gowarm/internal/retry"
)

// Job is a single warm-this-URL unit of work. Headers and Cookies override the
// client's defaults for this request only.
//
// Tags is a free-form map of axis name → applied value. It is purely
// informational (used for log context) — the cookies/headers must already
// reflect the same data.
type Job struct {
	URL     string
	Headers stdhttp.Header
	Cookies []*stdhttp.Cookie
	Tags    map[string]string
}

// JobResult is the outcome of a single warm job, passed to Pool.OnComplete.
//
// Err is nil on success. Context-cancellation (timeout / SIGINT) is reported
// as a non-nil Err that satisfies errors.Is(err, context.Canceled) or
// errors.Is(err, context.DeadlineExceeded); callers that want to distinguish
// "real" failures from cancellations should check those.
type JobResult struct {
	URL           string
	Status        int
	Bytes         int64
	Duration      time.Duration
	CacheState    config.CacheState
	CacheStateRaw string
	CacheHeader   string
	Tags          map[string]string
	Err           error
}

// Stats is a thread-safe counter set summarising a pool run.
//
// Cached is kept for backwards compatibility with existing reporters and
// equals CacheHits.
type Stats struct {
	Total        atomic.Int64
	Success      atomic.Int64
	Failed       atomic.Int64
	Cached       atomic.Int64
	CacheHits    atomic.Int64
	CacheMisses  atomic.Int64
	CacheBypass  atomic.Int64
	CacheUnknown atomic.Int64
	BytesIn      atomic.Int64
	TotalTime    atomic.Int64 // nanoseconds, summed across all warms
}

// Snapshot returns a non-atomic copy useful for logging.
func (s *Stats) Snapshot() StatsSnapshot {
	return StatsSnapshot{
		Total:        s.Total.Load(),
		Success:      s.Success.Load(),
		Failed:       s.Failed.Load(),
		Cached:       s.Cached.Load(),
		CacheHits:    s.CacheHits.Load(),
		CacheMisses:  s.CacheMisses.Load(),
		CacheBypass:  s.CacheBypass.Load(),
		CacheUnknown: s.CacheUnknown.Load(),
		BytesIn:      s.BytesIn.Load(),
		TotalTime:    time.Duration(s.TotalTime.Load()),
	}
}

// StatsSnapshot is a frozen view of Stats.
type StatsSnapshot struct {
	Total        int64
	Success      int64
	Failed       int64
	Cached       int64
	CacheHits    int64
	CacheMisses  int64
	CacheBypass  int64
	CacheUnknown int64
	BytesIn      int64
	TotalTime    time.Duration
}

// Pool is a fixed-worker concurrency pool.
//
// OnComplete is an optional callback invoked once per job after it finishes
// (success, real failure, or context cancellation). It runs on the worker
// goroutine, so callers that need to write to a shared sink (file, slice…)
// must serialise with their own mutex.
type Pool struct {
	size       int
	jobs       chan Job
	warmer     httpclient.CacheWarmer
	retrier    retry.Retrier
	log        *slog.Logger
	wg         sync.WaitGroup
	closeOnce  sync.Once
	stats      Stats
	OnComplete func(JobResult)
}

// NewPool creates a Pool but does not start the workers. Call Start.
func NewPool(size int, warmer httpclient.CacheWarmer, retrier retry.Retrier, log *slog.Logger) *Pool {
	if size <= 0 {
		size = 1
	}
	if log == nil {
		log = slog.Default()
	}
	return &Pool{
		size:    size,
		jobs:    make(chan Job, size*4),
		warmer:  warmer,
		retrier: retrier,
		log:     log,
	}
}

// Start spawns size worker goroutines.
func (p *Pool) Start(ctx context.Context) {
	for i := 0; i < p.size; i++ {
		p.wg.Add(1)
		go p.runWorker(ctx, i)
	}
}

// Submit enqueues j unless ctx is done. It blocks if the queue is full so the
// caller's producer naturally throttles to the consumer rate.
func (p *Pool) Submit(ctx context.Context, j Job) {
	select {
	case <-ctx.Done():
		return
	case p.jobs <- j:
	}
}

// Stop closes the job queue. Workers drain remaining jobs then exit.
func (p *Pool) Stop() {
	p.closeOnce.Do(func() { close(p.jobs) })
}

// Wait blocks until every worker has exited. It returns nil unless ctx fired
// before workers finished.
func (p *Pool) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats exposes the live counter set.
func (p *Pool) Stats() *Stats { return &p.stats }

func (p *Pool) runWorker(ctx context.Context, id int) {
	defer p.wg.Done()
	wlog := p.log.With("worker_id", id)
	for j := range p.jobs {
		if ctx.Err() != nil {
			return
		}
		p.process(ctx, wlog, j)
	}
}

func (p *Pool) process(ctx context.Context, log *slog.Logger, j Job) {
	p.stats.Total.Add(1)
	var lastResult *httpclient.WarmResult
	err := p.retrier.Execute(ctx, func(ctx context.Context) error {
		res, err := p.warmer.Warm(ctx, j.URL, j.Headers, j.Cookies)
		if res != nil {
			lastResult = res
		}
		return err
	})

	if lastResult != nil {
		p.stats.BytesIn.Add(lastResult.Bytes)
		p.stats.TotalTime.Add(int64(lastResult.Duration))
		switch lastResult.CacheState {
		case config.CacheStateHit:
			p.stats.CacheHits.Add(1)
			p.stats.Cached.Add(1) // legacy alias
		case config.CacheStateMiss:
			p.stats.CacheMisses.Add(1)
		case config.CacheStateBypass:
			p.stats.CacheBypass.Add(1)
		default:
			p.stats.CacheUnknown.Add(1)
		}
	}

	switch {
	case err == nil:
		p.stats.Success.Add(1)
		log.Debug("warmed", warmAttrs(j, lastResult)...)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// Don't count cancellations as failures; they are caused by the
		// caller (timeout / SIGINT), not by the target.
		log.Debug("cancelled", "url", j.URL, "err", err)
	default:
		p.stats.Failed.Add(1)
		attrs := warmAttrs(j, lastResult)
		attrs = append(attrs, "err", err)
		log.Warn("warm failed", attrs...)
	}

	if p.OnComplete != nil {
		p.OnComplete(JobResult{
			URL:           j.URL,
			Status:        statusOf(lastResult),
			Bytes:         bytesOf(lastResult),
			Duration:      durationOf(lastResult),
			CacheState:    cacheOf(lastResult),
			CacheStateRaw: cacheRawOf(lastResult),
			CacheHeader:   cacheHeaderOf(lastResult),
			Tags:          j.Tags,
			Err:           err,
		})
	}
}

// warmAttrs builds a flat slog attribute slice including every axis tag so
// the log line is grep-friendly: `region=QC language=en` etc.
func warmAttrs(j Job, r *httpclient.WarmResult) []any {
	attrs := []any{
		"url", j.URL,
		"status", statusOf(r),
		"cache", string(cacheOf(r)),
		"cache_raw", cacheRawOf(r),
		"duration", durationOf(r),
		"bytes", bytesOf(r),
	}
	for k, v := range j.Tags {
		attrs = append(attrs, k, v)
	}
	return attrs
}

func statusOf(r *httpclient.WarmResult) int {
	if r == nil {
		return 0
	}
	return r.Status
}

func cacheOf(r *httpclient.WarmResult) config.CacheState {
	if r == nil {
		return ""
	}
	return r.CacheState
}

func cacheRawOf(r *httpclient.WarmResult) string {
	if r == nil {
		return ""
	}
	return r.CacheStateRaw
}

func cacheHeaderOf(r *httpclient.WarmResult) string {
	if r == nil {
		return ""
	}
	return r.CacheHeader
}

func durationOf(r *httpclient.WarmResult) time.Duration {
	if r == nil {
		return 0
	}
	return r.Duration
}

func bytesOf(r *httpclient.WarmResult) int64 {
	if r == nil {
		return 0
	}
	return r.Bytes
}
