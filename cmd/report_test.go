package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tarikflz/gowarm/internal/config"
	"github.com/tarikflz/gowarm/internal/worker"
)

// TestRunReporter_PerJobJSONL_Success verifies that successful jobs produce a
// JSONL line with the expected fields and no err / cancelled flag.
func TestRunReporter_PerJobJSONL_Success(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.jsonl")

	r, closer, err := newRunReporter(path, false)
	if err != nil {
		t.Fatal(err)
	}

	r.Record(worker.JobResult{
		URL:           "https://example.com/a",
		Status:        200,
		Bytes:         1234,
		Duration:      150 * time.Millisecond,
		CacheState:    config.CacheStateHit,
		CacheStateRaw: "HIT",
		CacheHeader:   "CF-Cache-Status",
		Tags:          map[string]string{"region": "qc", "language": "en"},
	})
	if err := closer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatalf("expected at least one JSONL record, got none")
	}
	var rec perJobRecord
	if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.URL != "https://example.com/a" || rec.Status != 200 || rec.Bytes != 1234 {
		t.Errorf("record fields wrong: %+v", rec)
	}
	if rec.DurationMs != 150 {
		t.Errorf("duration_ms: got %d, want 150", rec.DurationMs)
	}
	if rec.CacheState != string(config.CacheStateHit) {
		t.Errorf("cache_state: got %q want %q", rec.CacheState, config.CacheStateHit)
	}
	if rec.CacheStateRaw != "HIT" {
		t.Errorf("cache_state_raw: got %q want HIT", rec.CacheStateRaw)
	}
	if rec.CacheHeader != "CF-Cache-Status" {
		t.Errorf("cache_header: got %q want CF-Cache-Status", rec.CacheHeader)
	}
	if rec.Tags["region"] != "qc" || rec.Tags["language"] != "en" {
		t.Errorf("tags missing: %v", rec.Tags)
	}
	if rec.Err != "" || rec.Cancelled {
		t.Errorf("expected no err / cancelled flag on success: %+v", rec)
	}
}

// TestRunReporter_CapturesFailures verifies that real failures are captured
// in the failures slice while context cancellations are skipped.
func TestRunReporter_CapturesFailures(t *testing.T) {
	t.Parallel()
	r, closer, err := newRunReporter("", true)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	r.Record(worker.JobResult{URL: "https://example.com/ok"})
	r.Record(worker.JobResult{
		URL:    "https://example.com/boom",
		Status: 500,
		Tags:   map[string]string{"region": "ab"},
		Err:    errors.New("boom"),
	})
	r.Record(worker.JobResult{
		URL: "https://example.com/cancelled",
		Err: context.Canceled,
	})
	r.Record(worker.JobResult{
		URL: "https://example.com/timeout",
		Err: context.DeadlineExceeded,
	})

	got := r.snapshotFailures()
	if len(got) != 1 {
		t.Fatalf("expected 1 captured failure, got %d (%+v)", len(got), got)
	}
	if got[0].URL != "https://example.com/boom" || got[0].Err != "boom" {
		t.Errorf("captured failure wrong: %+v", got[0])
	}
	if got[0].Tags["region"] != "ab" || got[0].Status != 500 {
		t.Errorf("failure tags/status wrong: %+v", got[0])
	}
}

// TestRunReporter_DisabledNoOps verifies that an empty path + captureFail=false
// reporter accepts records without crashing and writes nothing to disk.
func TestRunReporter_DisabledNoOps(t *testing.T) {
	t.Parallel()
	r, closer, err := newRunReporter("", false)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	r.Record(worker.JobResult{URL: "x", Err: errors.New("any")})

	if got := r.snapshotFailures(); len(got) != 0 {
		t.Errorf("expected no failures captured, got %d", len(got))
	}
}

// TestWriteSummary_HappyPath verifies that the summary file contains the
// expected fields and that Failures is always present (possibly empty).
func TestWriteSummary_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "summary.json")

	in := summary{
		StartTime:    "2026-05-07T00:00:00Z",
		EndTime:      "2026-05-07T00:01:00Z",
		DurationMs:   60000,
		SitemapURL:   "https://example.com/sitemap.xml",
		Axes:         "region(2),language(2)",
		Method:       "GET",
		Workers:      10,
		URLCount:     50,
		TotalJobs:    200,
		SkippedURLs:  0,
		Success:      198,
		Failed:       2,
		Cached:       150,
		BytesTotal:   1024 * 1024,
		AvgLatencyMs: 120,
		Failures: []failedJob{
			{URL: "https://example.com/x", Err: "boom", Status: 500},
		},
	}
	if err := writeSummary(path, in); err != nil {
		t.Fatalf("writeSummary: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got summary
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if got.SitemapURL != in.SitemapURL || got.TotalJobs != 200 || got.Failed != 2 {
		t.Errorf("summary roundtrip mismatch: got %+v", got)
	}
	if len(got.Failures) != 1 || got.Failures[0].URL != "https://example.com/x" {
		t.Errorf("failures roundtrip mismatch: %+v", got.Failures)
	}
}

// TestWriteSummary_CacheCountersAndHitRatio verifies that the new normalised
// cache counters (hits / misses / bypass / unknown) and the derived hit_ratio
// roundtrip through JSON.
func TestWriteSummary_CacheCountersAndHitRatio(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "summary.json")

	in := summary{
		SitemapURL:        "x",
		CacheHits:         60,
		CacheMisses:       30,
		CacheBypass:       5,
		UnknownCacheState: 5,
		HitRatio:          0.6,
	}
	if err := writeSummary(path, in); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got summary
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.CacheHits != 60 || got.CacheMisses != 30 || got.CacheBypass != 5 || got.UnknownCacheState != 5 {
		t.Errorf("counters mismatch: %+v", got)
	}
	if got.HitRatio < 0.59 || got.HitRatio > 0.61 {
		t.Errorf("hit_ratio mismatch: %v", got.HitRatio)
	}
}

// TestRunReporter_DefaultsCacheStateToUnknown verifies the per-job record
// always carries a non-empty cache_state (defaulting to "unknown") so
// consumers don't have to special-case its absence.
func TestRunReporter_DefaultsCacheStateToUnknown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.jsonl")

	r, closer, err := newRunReporter(path, false)
	if err != nil {
		t.Fatal(err)
	}
	r.Record(worker.JobResult{URL: "https://example.com/x", Status: 200})
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec perJobRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.CacheState != string(config.CacheStateUnknown) {
		t.Errorf("cache_state default: got %q want %q", rec.CacheState, config.CacheStateUnknown)
	}
}

// TestWriteSummary_NoFailures verifies that the failures field is an empty
// array (not null) when there are no failures, so consumers can rely on it.
func TestWriteSummary_NoFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "summary.json")

	if err := writeSummary(path, summary{SitemapURL: "x"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Use a generic map to assert the JSON shape (failures must be []).
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	failures, ok := generic["failures"].([]any)
	if !ok {
		t.Fatalf("failures should decode as []any, got %T (%v)", generic["failures"], generic["failures"])
	}
	if len(failures) != 0 {
		t.Errorf("expected empty failures array, got %v", failures)
	}
}
