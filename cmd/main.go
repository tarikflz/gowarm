// Command gowarm streams URLs from a sitemap (or sitemap index) and issues
// concurrent requests with cookie/header permutations to pre-populate origin
// and CDN caches.
//
// gowarm is content-agnostic: every dimension of the warming cartesian
// (region, language, currency, locale, …) is declared in YAML as an `axis`,
// and the program faithfully expands it. The cookie / header / value-transform
// behaviour is configured per-axis; the program itself has no built-in
// vocabulary about regions or languages.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	stdhttp "net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"log/slog"

	"github.com/tarikflz/gowarm/internal/config"
	httpclient "github.com/tarikflz/gowarm/internal/http"
	"github.com/tarikflz/gowarm/internal/parser"
	"github.com/tarikflz/gowarm/internal/retry"
	"github.com/tarikflz/gowarm/internal/worker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gowarm:", err)
		os.Exit(1)
	}
}

// multiFlag is a flag.Value that accepts -axis multiple times.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

func run() error {
	// ---------- flags ----------
	var (
		cfgPath       string
		sitemapURL    string
		workersFlag   int
		methodFlag    string
		timeoutFlag   time.Duration
		dryRun        bool
		force         bool
		logLevel      string
		logFile       string
		perJobFile    string
		summaryFile   string
		axisOverrides multiFlag
	)
	flag.StringVar(&cfgPath, "config", "config.yaml", "path to config YAML (optional)")
	flag.StringVar(&sitemapURL, "sitemap", "", "override sitemap URL")
	flag.IntVar(&workersFlag, "workers", 0, "override worker count")
	flag.StringVar(&methodFlag, "method", "", "override HTTP method (GET or HEAD)")
	flag.DurationVar(&timeoutFlag, "timeout", 0, "global deadline (e.g. 10m)")
	flag.BoolVar(&dryRun, "dry-run", false, "list jobs that would be issued and exit")
	flag.BoolVar(&force, "force", false, "bypass limits.max_jobs / limits.max_combinations_per_url safety checks")
	flag.StringVar(&logLevel, "log-level", "", "debug|info|warn|error")
	flag.StringVar(&logFile, "log-file", "", "tee slog text output to this file (in addition to stdout)")
	flag.StringVar(&perJobFile, "per-job-file", "", "write one JSONL record per warm job to this file")
	flag.StringVar(&summaryFile, "summary-file", "", "write a JSON summary report at the end of the run")
	flag.Var(&axisOverrides, "axis", "override an axis's values (e.g. -axis region=qc,on -axis language=en); repeatable")
	flag.Parse()

	// ---------- config ----------
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if sitemapURL != "" {
		cfg.SitemapURL = sitemapURL
	}
	if workersFlag > 0 {
		cfg.WorkerCount = workersFlag
	}
	if methodFlag != "" {
		cfg.Method = methodFlag
	}
	if logLevel != "" {
		cfg.LogLevel = logLevel
	}
	if logFile != "" {
		cfg.Report.LogFile = logFile
	}
	if perJobFile != "" {
		cfg.Report.PerJobFile = perJobFile
	}
	if summaryFile != "" {
		cfg.Report.SummaryFile = summaryFile
	}
	if err := applyAxisOverrides(cfg, axisOverrides); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	// ---------- logger (with optional file tee) ----------
	logSink, logCloser, err := openLogSink(cfg.Report.LogFile)
	if err != nil {
		return err
	}
	defer logCloser.Close()

	log := newLogger(cfg.LogLevel, logSink)
	startTime := time.Now()
	log.Info("gowarm starting",
		"sitemap", cfg.SitemapURL,
		"axes", axesSummary(cfg.Axes),
		"workers", cfg.WorkerCount,
		"method", cfg.Method,
	)

	// ---------- contexts ----------
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if timeoutFlag > 0 {
		rootCtx, cancel = context.WithTimeout(rootCtx, timeoutFlag)
		defer cancel()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-stop:
			log.Warn("shutdown signal received, draining...")
			cancel()
		case <-rootCtx.Done():
		}
	}()

	// ---------- HTTP client ----------
	defaultHdrs := stdhttp.Header{}
	for k, v := range cfg.GlobalHeaders {
		defaultHdrs.Set(k, v)
	}
	defaultCookies := buildDefaultCookies(cfg.GlobalCookies)

	cli := httpclient.New(httpclient.Options{
		Timeout:                  cfg.HTTP.Timeout,
		MaxIdleConns:             cfg.HTTP.MaxIdleConns,
		MaxIdleConnsPerHost:      cfg.HTTP.MaxIdleConnsPerHost,
		IdleConnTimeout:          cfg.HTTP.IdleConnTimeout,
		DisableCompression:       cfg.HTTP.DisableCompression,
		Method:                   cfg.Method,
		UserAgent:                cfg.UserAgent,
		DefaultHeaders:           defaultHdrs,
		DefaultCookies:           defaultCookies,
		RateLimitRPS:             cfg.HTTP.RateLimitRPS,
		Burst:                    cfg.HTTP.Burst,
		MaxConnsPerHost:          cfg.HTTP.MaxConnsPerHost,
		AdaptiveSlowdown:         cfg.HTTP.AdaptiveSlowdown,
		AdaptiveSlowdownFactor:   cfg.HTTP.AdaptiveSlowdownFactor,
		AdaptiveSlowdownDuration: cfg.HTTP.AdaptiveSlowdownDuration,
		CacheDetection:           &cfg.CacheDetection,
	})

	// ---------- collect URLs ----------
	urls, err := collectURLs(rootCtx, log, cli, cfg)
	if err != nil {
		return err
	}
	log.Info("sitemap parsed", "url_count", len(urls))

	// ---------- plan: sampling + priority + per-URL cap + run-wide cap ----------
	plan, planErr := planRun(urls, cfg, force)
	if plan != nil {
		log.Info("plan computed",
			"url_count", plan.URLCount,
			"original_url_count", plan.OriginalURLCount,
			"sampled_out", plan.SampledOut,
			"skipped_no_applicable", plan.SkippedNoApplicable,
			"skipped_combo_cap", plan.SkippedComboCap,
			"total_jobs", plan.TotalJobs,
		)
		// Always print a safety warning so operators see the volume up front.
		warnIfLargePlan(log, plan, cfg)
	}
	if planErr != nil {
		return planErr
	}

	if dryRun {
		printDryRun(os.Stdout, plan, cfg.Axes)
		return nil
	}
	if plan.URLCount == 0 {
		return errors.New("no URLs left after url_filter / sampling / cartesian filtering; nothing to warm")
	}

	// ---------- worker pool ----------
	rt := &retry.ExponentialBackoff{
		BaseDelay:   cfg.Retry.BaseDelay,
		MaxDelay:    cfg.Retry.MaxDelay,
		Jitter:      cfg.Retry.Jitter,
		Factor:      cfg.Retry.Factor,
		MaxAttempts: cfg.Retry.MaxAttempts,
	}
	pool := worker.NewPool(cfg.WorkerCount, cli, rt, log)

	// ---------- run reporter (per-job JSONL + summary failure capture) ----------
	reporter, perJobCloser, err := newRunReporter(
		cfg.Report.PerJobFile,
		cfg.Report.SummaryFile != "",
	)
	if err != nil {
		return err
	}
	defer perJobCloser.Close()
	pool.OnComplete = reporter.Record

	pool.Start(rootCtx)

	// ---------- progress reporter ----------
	progressDone := make(chan struct{})
	var progressWG sync.WaitGroup
	progressWG.Add(1)
	go reportProgress(rootCtx, log, pool, progressDone, &progressWG)

	// ---------- submit jobs ----------
	totalJobs := submitPlanJobs(rootCtx, log, pool, plan, cfg.Axes)
	skippedURLs := plan.SkippedNoApplicable + plan.SkippedComboCap
	pool.Stop()
	log.Info("all jobs queued",
		"jobs", totalJobs,
		"skipped_urls", skippedURLs,
		"skipped_no_applicable", plan.SkippedNoApplicable,
		"skipped_combo_cap", plan.SkippedComboCap,
	)

	// ---------- wait & report ----------
	waitErr := pool.Wait(rootCtx)
	close(progressDone)
	progressWG.Wait()

	snap := pool.Stats().Snapshot()
	avg := time.Duration(0)
	if snap.Total > 0 {
		avg = time.Duration(int64(snap.TotalTime) / snap.Total)
	}
	endTime := time.Now()
	log.Info("warming complete",
		"total", snap.Total,
		"success", snap.Success,
		"failed", snap.Failed,
		"cache_hits", snap.CacheHits,
		"cache_misses", snap.CacheMisses,
		"cache_bypass", snap.CacheBypass,
		"unknown_cache_state", snap.CacheUnknown,
		"bytes", snap.BytesIn,
		"avg_latency", avg.Round(time.Millisecond),
	)

	if cfg.Report.SummaryFile != "" {
		var hitRatio float64
		denom := snap.CacheHits + snap.CacheMisses + snap.CacheBypass + snap.CacheUnknown
		if denom > 0 {
			hitRatio = float64(snap.CacheHits) / float64(denom)
		}
		sum := summary{
			StartTime:           startTime.UTC().Format(time.RFC3339),
			EndTime:             endTime.UTC().Format(time.RFC3339),
			DurationMs:          endTime.Sub(startTime).Milliseconds(),
			SitemapURL:          cfg.SitemapURL,
			Axes:                axesSummary(cfg.Axes),
			Method:              cfg.Method,
			Workers:             cfg.WorkerCount,
			URLCount:            len(urls),
			SampledOut:          plan.SampledOut,
			TotalJobs:           totalJobs,
			SkippedURLs:         skippedURLs,
			SkippedNoApplicable: plan.SkippedNoApplicable,
			SkippedComboCap:     plan.SkippedComboCap,
			Success:             snap.Success,
			Failed:              snap.Failed,
			Cached:              snap.Cached,
			CacheHits:           snap.CacheHits,
			CacheMisses:         snap.CacheMisses,
			CacheBypass:         snap.CacheBypass,
			UnknownCacheState:   snap.CacheUnknown,
			HitRatio:            hitRatio,
			BytesTotal:          snap.BytesIn,
			AvgLatencyMs:        avg.Milliseconds(),
			Failures:            reporter.snapshotFailures(),
		}
		if werr := writeSummary(cfg.Report.SummaryFile, sum); werr != nil {
			// Don't fail the whole run if the summary can't be written; just
			// log it loudly so operators notice.
			log.Error("summary write failed", "err", werr, "path", cfg.Report.SummaryFile)
		} else {
			log.Info("summary written", "path", cfg.Report.SummaryFile)
		}
	}

	if waitErr != nil &&
		!errors.Is(waitErr, context.Canceled) &&
		!errors.Is(waitErr, context.DeadlineExceeded) {
		return waitErr
	}
	if cfg.StopOnError && snap.Failed > 0 {
		return fmt.Errorf("%d job(s) failed", snap.Failed)
	}
	return nil
}

// ---------------------------------------------------------------------------
// URL collection + filtering.
// ---------------------------------------------------------------------------

// collectURLs fetches the sitemap and (optionally) follows nested
// <sitemapindex> entries. Results are filtered by the configured URLFilter.
func collectURLs(ctx context.Context, log *slog.Logger, cli *httpclient.Client, cfg *config.Config) ([]string, error) {
	sp := parser.NewStreamingXMLParser()

	visited := make(map[string]struct{})
	queue := []string{cfg.SitemapURL}
	var allURLs []string

	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if _, seen := visited[next]; seen {
			continue
		}
		visited[next] = struct{}{}

		urls, indexes, err := fetchAndParse(ctx, cli, sp, next)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", next, err)
		}
		log.Debug("sitemap fragment parsed", "src", next, "urls", len(urls), "indexes", len(indexes))

		for _, u := range urls {
			if cfg.URLFilter.Allow(u) {
				allURLs = append(allURLs, u)
			}
		}
		if cfg.FollowIndex {
			queue = append(queue, indexes...)
		}
	}

	return dedup(allURLs), nil
}

// fetchAndParse downloads a single sitemap URL, classifies it as urlset or
// sitemapindex, and returns parsed URL lists for each.
func fetchAndParse(ctx context.Context, cli *httpclient.Client, sp *parser.StreamingXMLParser, sitemapURL string) (urls []string, indexes []string, err error) {
	resp, err := cli.Get(ctx, sitemapURL, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != stdhttp.StatusOK {
		return nil, nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	// Peek the first 256 bytes to classify the root element, then re-feed
	// those bytes plus the rest of the body into the streaming parser.
	head := make([]byte, 256)
	n, _ := resp.Body.Read(head)
	head = head[:n]
	isIndex := parser.IsIndex(head)

	combined := &prefixReader{head: head, tail: resp.Body}

	ch, perr := sp.Parse(ctx, combined)
	if perr != nil {
		return nil, nil, perr
	}

	for u := range ch {
		if isIndex {
			indexes = append(indexes, u)
		} else {
			urls = append(urls, u)
		}
	}
	return urls, indexes, nil
}

type prefixReader struct {
	head []byte
	tail interface {
		Read(p []byte) (int, error)
	}
}

func (p *prefixReader) Read(buf []byte) (int, error) {
	if len(p.head) > 0 {
		n := copy(buf, p.head)
		p.head = p.head[n:]
		return n, nil
	}
	return p.tail.Read(buf)
}

// ---------------------------------------------------------------------------
// Job submission: cartesian product of URLs × axes.
// ---------------------------------------------------------------------------

// submitPlanJobs streams the precomputed runPlan into the pool. The plan
// already contains per-URL applicable axis values (post-sampling, post-cap)
// so this function is just the iteration + combo expansion.
func submitPlanJobs(ctx context.Context, log *slog.Logger, pool *worker.Pool, plan *runPlan, axes []config.Axis) int {
	jobs := 0
	for i, u := range plan.URLs {
		if ctx.Err() != nil {
			return jobs
		}
		log.Debug("submit url", "url", u, "combos", plan.Combos[i])
		forEachCombo(plan.Applicable[i], func(combo []config.AxisValue) bool {
			if ctx.Err() != nil {
				return false
			}
			cookies, headers, tags := buildJobInputs(axes, combo)
			pool.Submit(ctx, worker.Job{
				URL:     u,
				Headers: headers,
				Cookies: cookies,
				Tags:    tags,
			})
			jobs++
			return true
		})
	}
	return jobs
}

// warnIfLargePlan logs a high-visibility warning whenever the planned job
// count is large in absolute terms or relative to the configured limits.
// The thresholds are intentionally simple: any plan that would issue >= 10k
// requests, or that uses >= 80% of MaxJobs, deserves a warning so operators
// notice before they hit send.
func warnIfLargePlan(log *slog.Logger, plan *runPlan, cfg *config.Config) {
	const absoluteThreshold = 10_000
	const relativeThreshold = 0.8

	if plan.TotalJobs >= absoluteThreshold {
		log.Warn("large warming run planned (>10k jobs); double-check axes/url_filter/sampling",
			"total_jobs", plan.TotalJobs)
	}
	if cfg.Limits.MaxJobs > 0 {
		usage := float64(plan.TotalJobs) / float64(cfg.Limits.MaxJobs)
		if usage >= relativeThreshold && plan.TotalJobs <= cfg.Limits.MaxJobs {
			log.Warn("plan close to limits.max_jobs",
				"total_jobs", plan.TotalJobs,
				"max_jobs", cfg.Limits.MaxJobs,
				"usage_pct", fmt.Sprintf("%.1f", usage*100),
			)
		}
	}
}

// buildApplicable computes the per-axis list of values applicable to u.
// Returns false if any axis has zero applicable values (in which case the
// URL must be skipped).
func buildApplicable(u string, axes []config.Axis) ([][]config.AxisValue, bool) {
	out := make([][]config.AxisValue, len(axes))
	for i := range axes {
		out[i] = axes[i].ApplicableValues(u)
		if len(out[i]) == 0 {
			return nil, false
		}
	}
	return out, true
}

// forEachCombo invokes fn with every cartesian combination from applicable.
// fn returning false aborts the iteration early (used for ctx cancellation).
func forEachCombo(applicable [][]config.AxisValue, fn func([]config.AxisValue) bool) {
	combo := make([]config.AxisValue, len(applicable))
	var rec func(i int) bool
	rec = func(i int) bool {
		if i == len(applicable) {
			return fn(combo)
		}
		for _, v := range applicable[i] {
			combo[i] = v
			if !rec(i + 1) {
				return false
			}
		}
		return true
	}
	rec(0)
}

// buildJobInputs converts an axis combination into the cookies, headers, and
// tag map that get attached to a Job.
func buildJobInputs(axes []config.Axis, combo []config.AxisValue) (cookies []*stdhttp.Cookie, headers stdhttp.Header, tags map[string]string) {
	cookies = make([]*stdhttp.Cookie, 0, len(axes))
	headers = stdhttp.Header{}
	tags = make(map[string]string, len(axes))
	for i, a := range axes {
		v := a.TransformValue(combo[i].Value)
		tags[a.Name] = v
		if a.CookieName != "" {
			cookies = append(cookies, &stdhttp.Cookie{
				Name:  a.CookieName,
				Value: v,
				Path:  "/",
			})
		}
		if a.HeaderName != "" {
			headers.Set(a.HeaderName, v)
		}
	}
	return
}

// ---------------------------------------------------------------------------
// CLI overrides + dry-run + helpers.
// ---------------------------------------------------------------------------

// applyAxisOverrides parses -axis name=v1,v2 entries and replaces the matching
// axis's Values list. Unknown axis names are an error so typos surface fast.
//
// When an overridden value's name matches an existing config value, the
// existing per-value URL filters (when_url_contains / when_url_matches) are
// preserved. New value names that aren't in the config get no filters and
// apply universally. This makes `-axis region=qc` behave like the user
// expects: drop the other regions but keep the region's transform/filters.
func applyAxisOverrides(cfg *config.Config, overrides multiFlag) error {
	for _, raw := range overrides {
		name, vals, ok := strings.Cut(raw, "=")
		if !ok || name == "" || vals == "" {
			return fmt.Errorf("invalid -axis %q (expected name=v1,v2)", raw)
		}
		idx := -1
		for i := range cfg.Axes {
			if cfg.Axes[i].Name == name {
				idx = i
				break
			}
		}
		if idx == -1 {
			return fmt.Errorf("-axis %q: no axis named %q in config", raw, name)
		}

		existing := make(map[string]config.AxisValue, len(cfg.Axes[idx].Values))
		for _, v := range cfg.Axes[idx].Values {
			existing[v.Value] = v
		}

		parts := splitCSV(vals)
		newVals := make([]config.AxisValue, 0, len(parts))
		for _, p := range parts {
			if v, ok := existing[p]; ok {
				newVals = append(newVals, v)
			} else {
				newVals = append(newVals, config.AxisValue{Value: p})
			}
		}
		cfg.Axes[idx].Values = newVals
	}
	return nil
}

func reportProgress(ctx context.Context, log *slog.Logger, pool *worker.Pool, done <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			s := pool.Stats().Snapshot()
			log.Info("progress",
				"completed", s.Success+s.Failed,
				"success", s.Success,
				"failed", s.Failed,
				"cached", s.Cached,
			)
		}
	}
}

func newLogger(level string, sink io.Writer) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	if sink == nil {
		sink = os.Stdout
	}
	h := slog.NewTextHandler(sink, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

// openLogSink builds the io.Writer that the slog handler will use. When path
// is set, output is tee'd to both stdout and the file; otherwise it is just
// stdout. The returned io.Closer is always non-nil so callers can defer it
// without a nil check.
func openLogSink(path string) (io.Writer, io.Closer, error) {
	if path == "" {
		return os.Stdout, noopCloser{}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file %s: %w", path, err)
	}
	return io.MultiWriter(os.Stdout, f), f, nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func dedup(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func buildDefaultCookies(in []config.CookieConfig) []*stdhttp.Cookie {
	out := make([]*stdhttp.Cookie, 0, len(in))
	for _, c := range in {
		if c.Name == "" {
			continue
		}
		ck := &stdhttp.Cookie{
			Name:   c.Name,
			Value:  c.Value,
			Domain: c.Domain,
			Path:   c.Path,
		}
		if ck.Path == "" {
			ck.Path = "/"
		}
		out = append(out, ck)
	}
	return out
}

// axesSummary formats axes for the startup log line: "region(4),language(2)".
func axesSummary(axes []config.Axis) string {
	parts := make([]string, len(axes))
	for i, a := range axes {
		parts[i] = fmt.Sprintf("%s(%d)", a.Name, len(a.Values))
	}
	return strings.Join(parts, ",")
}

// printDryRun lists every job the plan would submit as TSV. Output is
// deterministic across runs (sorted axis names per line) so it is friendly
// to diffing and grep. The trailing comment line summarises the plan
// (URL count, total combos, skip counters) so operators can sanity-check
// the volume before launching the real run.
func printDryRun(out *os.File, plan *runPlan, axes []config.Axis) {
	for i, u := range plan.URLs {
		forEachCombo(plan.Applicable[i], func(combo []config.AxisValue) bool {
			tagPairs := make([]string, len(axes))
			for j, a := range axes {
				tagPairs[j] = fmt.Sprintf("%s=%s", a.Name, a.TransformValue(combo[j].Value))
			}
			sort.Strings(tagPairs)
			fmt.Fprintf(out, "%s\t%s\n", u, strings.Join(tagPairs, "\t"))
			return true
		})
	}
	fmt.Fprintf(out,
		"# urls=%d (sampled_out=%d, no_applicable=%d, combo_cap=%d), total_jobs=%d\n",
		plan.URLCount,
		plan.SampledOut,
		plan.SkippedNoApplicable,
		plan.SkippedComboCap,
		plan.TotalJobs,
	)
}
