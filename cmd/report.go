package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/tarikflz/gowarm/internal/config"
	"github.com/tarikflz/gowarm/internal/worker"
)

// runReporter is the optional output sink wired into the worker pool. It
// receives every JobResult on the worker goroutine, so its public methods are
// internally serialised; callers do not need to add a lock of their own.
//
// All three artefacts are independently opt-in:
//
//   - perJobEnc   set → write one JSON object per job to a JSONL file.
//   - captureFail true → keep the list of real (non-ctx) failures so they can
//     be emitted in the summary at the end.
//   - jobLogFile  set → tee slog text output (configured by main.go) to the
//     file as well as stdout.
type runReporter struct {
	perJobEnc *json.Encoder
	perJobMu  sync.Mutex

	captureFail bool
	failures    []failedJob
	failuresMu  sync.Mutex
}

// perJobRecord is the per-line shape of the JSONL artefact.
//
// CacheState is the normalised hit/miss/bypass/unknown classification.
// CacheStateRaw + CacheHeader retain the original header value/name so
// operators can still see provider-specific nuance (e.g. "EXPIRED").
type perJobRecord struct {
	Time          string            `json:"time"`
	URL           string            `json:"url"`
	Status        int               `json:"status,omitempty"`
	Bytes         int64             `json:"bytes,omitempty"`
	DurationMs    int64             `json:"duration_ms,omitempty"`
	CacheState    string            `json:"cache_state,omitempty"`
	CacheStateRaw string            `json:"cache_state_raw,omitempty"`
	CacheHeader   string            `json:"cache_header,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
	Err           string            `json:"err,omitempty"`
	Cancelled     bool              `json:"cancelled,omitempty"`
}

// failedJob is one entry in the summary's failures array.
type failedJob struct {
	URL    string            `json:"url"`
	Status int               `json:"status,omitempty"`
	Tags   map[string]string `json:"tags,omitempty"`
	Err    string            `json:"err"`
}

// summary is the run-end JSON document.
//
// CacheHits/Misses/Bypass/Unknown are the normalised counters that drive
// HitRatio (= CacheHits / (CacheHits + CacheMisses + CacheBypass + CacheUnknown)).
// Cached is kept for backwards compatibility and equals CacheHits.
//
// SkippedNoApplicable + SkippedComboCap together explain SkippedURLs.
// SampledOut is the number of URLs the sampling step dropped before any
// cartesian work happened.
type summary struct {
	StartTime           string      `json:"start_time"`
	EndTime             string      `json:"end_time"`
	DurationMs          int64       `json:"duration_ms"`
	SitemapURL          string      `json:"sitemap_url"`
	Axes                string      `json:"axes"`
	Method              string      `json:"method"`
	Workers             int         `json:"workers"`
	URLCount            int         `json:"url_count"`
	SampledOut          int         `json:"sampled_out"`
	TotalJobs           int         `json:"total_jobs"`
	SkippedURLs         int         `json:"skipped_urls"`
	SkippedNoApplicable int         `json:"skipped_no_applicable"`
	SkippedComboCap     int         `json:"skipped_combo_cap"`
	Success             int64       `json:"success"`
	Failed              int64       `json:"failed"`
	Cached              int64       `json:"cached"`
	CacheHits           int64       `json:"cache_hits"`
	CacheMisses         int64       `json:"cache_misses"`
	CacheBypass         int64       `json:"cache_bypass"`
	UnknownCacheState   int64       `json:"unknown_cache_state"`
	HitRatio            float64     `json:"hit_ratio"`
	BytesTotal          int64       `json:"bytes_total"`
	AvgLatencyMs        int64       `json:"avg_latency_ms"`
	Failures            []failedJob `json:"failures"`
}

// newRunReporter wires up an optional per-job JSONL writer and a summary
// failure collector. Pass empty paths to disable either side. The returned
// closer flushes / closes the per-job file (if any); call it from a defer.
func newRunReporter(perJobPath string, captureFail bool) (*runReporter, io.Closer, error) {
	r := &runReporter{captureFail: captureFail}
	if perJobPath == "" {
		return r, noopCloser{}, nil
	}
	f, err := os.Create(perJobPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open per-job file %s: %w", perJobPath, err)
	}
	r.perJobEnc = json.NewEncoder(f)
	return r, f, nil
}

// Record is the Pool.OnComplete callback. It writes a JSONL line and / or
// captures the failure depending on how the reporter was configured.
func (r *runReporter) Record(res worker.JobResult) {
	cancelled := errors.Is(res.Err, context.Canceled) ||
		errors.Is(res.Err, context.DeadlineExceeded)

	if r.perJobEnc != nil {
		state := string(res.CacheState)
		if state == "" {
			state = string(config.CacheStateUnknown)
		}
		rec := perJobRecord{
			Time:          time.Now().UTC().Format(time.RFC3339),
			URL:           res.URL,
			Status:        res.Status,
			Bytes:         res.Bytes,
			DurationMs:    res.Duration.Milliseconds(),
			CacheState:    state,
			CacheStateRaw: res.CacheStateRaw,
			CacheHeader:   res.CacheHeader,
			Tags:          res.Tags,
			Cancelled:     cancelled,
		}
		if res.Err != nil {
			rec.Err = res.Err.Error()
		}
		r.perJobMu.Lock()
		_ = r.perJobEnc.Encode(rec)
		r.perJobMu.Unlock()
	}

	if r.captureFail && res.Err != nil && !cancelled {
		r.failuresMu.Lock()
		r.failures = append(r.failures, failedJob{
			URL:    res.URL,
			Status: res.Status,
			Tags:   res.Tags,
			Err:    res.Err.Error(),
		})
		r.failuresMu.Unlock()
	}
}

// snapshotFailures returns a copy of the captured failures, safe to encode
// after the pool has stopped (no further writers exist at that point, but the
// copy keeps the API symmetric).
func (r *runReporter) snapshotFailures() []failedJob {
	r.failuresMu.Lock()
	defer r.failuresMu.Unlock()
	out := make([]failedJob, len(r.failures))
	copy(out, r.failures)
	return out
}

// writeSummary emits the run summary as pretty-printed JSON. Failures is
// always present (possibly empty) so consumers can rely on the field shape.
func writeSummary(path string, s summary) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("open summary file %s: %w", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if s.Failures == nil {
		s.Failures = []failedJob{}
	}
	if err := enc.Encode(s); err != nil {
		return fmt.Errorf("encode summary: %w", err)
	}
	return nil
}

// noopCloser is returned when no per-job file was opened; it simplifies the
// caller's deferred cleanup logic.
type noopCloser struct{}

func (noopCloser) Close() error { return nil }
