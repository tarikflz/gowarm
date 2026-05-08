package main

import (
	"fmt"
	"math/rand/v2"
	"sort"

	"github.com/tarikflz/gowarm/internal/config"
)

// runPlan is the post-collection, pre-submission view of the warming run. It
// is shared by both the dry-run output and the real submitter so that the
// two paths agree about which URLs and combinations will be exercised.
//
// Applicable is indexed by URL position; each entry is the per-axis list of
// applicable values for that URL (i.e. what forEachCombo expects).
type runPlan struct {
	URLs                []string               // in execution order
	Applicable          [][][]config.AxisValue // per-URL × per-axis applicable values
	Combos              []int                  // per-URL combination count
	TotalJobs           int                    // sum of Combos
	URLCount            int                    // len(URLs) (post-sampling, post-filter)
	SkippedNoApplicable int                    // URLs dropped because at least one axis had no applicable value
	SkippedComboCap     int                    // URLs dropped because their combo count exceeded MaxCombinationsPerURL
	SampledOut          int                    // URLs dropped by the sampling step
	OriginalURLCount    int                    // URL count before sampling
}

// planRun applies sampling, priority ordering, per-URL applicability, the
// per-URL combinations cap, and the run-wide MaxJobs guard. It returns an
// error when MaxJobs is exceeded and force is false; the caller is expected
// to surface that as a non-zero exit.
func planRun(urls []string, cfg *config.Config, force bool) (*runPlan, error) {
	plan := &runPlan{
		OriginalURLCount: len(urls),
	}

	// 1. Priority ordering — stable sort that keeps non-priority order
	// intact. Done BEFORE sampling so priority URLs are guaranteed to be in
	// the run regardless of how many URLs are kept.
	prioritised := applyPriority(urls, cfg.PriorityURLs)

	// 2. Sampling — Percent wins when both Percent and FirstN are set.
	sampled := applySampling(prioritised, cfg.Sampling)
	plan.SampledOut = len(prioritised) - len(sampled)

	// 3. Per-URL applicability + combo count + per-URL cap.
	plan.URLs = make([]string, 0, len(sampled))
	plan.Applicable = make([][][]config.AxisValue, 0, len(sampled))
	plan.Combos = make([]int, 0, len(sampled))

	for _, u := range sampled {
		applicable, ok := buildApplicable(u, cfg.Axes)
		if !ok {
			plan.SkippedNoApplicable++
			continue
		}
		count := comboCount(applicable)
		if cfg.Limits.MaxCombinationsPerURL > 0 && count > cfg.Limits.MaxCombinationsPerURL && !force {
			plan.SkippedComboCap++
			continue
		}
		plan.URLs = append(plan.URLs, u)
		plan.Applicable = append(plan.Applicable, applicable)
		plan.Combos = append(plan.Combos, count)
		plan.TotalJobs += count
	}
	plan.URLCount = len(plan.URLs)

	// 4. Run-wide cap.
	if cfg.Limits.MaxJobs > 0 && plan.TotalJobs > cfg.Limits.MaxJobs && !force {
		return plan, fmt.Errorf(
			"limits.max_jobs exceeded: planned=%d limit=%d (run with -force to bypass, or tighten axes/url_filter/sampling)",
			plan.TotalJobs, cfg.Limits.MaxJobs)
	}
	return plan, nil
}

// comboCount multiplies the per-axis applicable counts with overflow
// protection. If the product would overflow, it is capped at math.MaxInt so
// callers can still compare it to MaxJobs.
func comboCount(applicable [][]config.AxisValue) int {
	count := 1
	for _, vs := range applicable {
		n := len(vs)
		if n == 0 {
			return 0
		}
		// Detect overflow.
		const maxInt = int(^uint(0) >> 1)
		if count > maxInt/n {
			return maxInt
		}
		count *= n
	}
	return count
}

// applySampling reduces urls according to cfg. With Percent it keeps a
// deterministic stride (every Nth URL) so re-runs warm the same set.
// With Random=true the sampling is RNG-driven.
func applySampling(urls []string, cfg config.SamplingConfig) []string {
	if cfg.Percent > 0 && cfg.Percent < 100 {
		return samplePercent(urls, cfg.Percent, cfg.Seed, cfg.Random)
	}
	if cfg.FirstN > 0 && cfg.FirstN < len(urls) {
		return urls[:cfg.FirstN]
	}
	return urls
}

// samplePercent keeps roughly p% of urls. With random=false it uses a
// deterministic stride so re-runs are reproducible without a seed; with
// random=true it picks via a seeded PRNG.
func samplePercent(urls []string, p float64, seed int64, random bool) []string {
	keep := int(float64(len(urls)) * p / 100.0)
	if keep <= 0 {
		keep = 1
	}
	if keep >= len(urls) {
		return urls
	}
	out := make([]string, 0, keep)
	if !random {
		// Deterministic stride: pick every Nth URL.
		stride := float64(len(urls)) / float64(keep)
		for i := 0; i < keep; i++ {
			idx := int(float64(i) * stride)
			if idx >= len(urls) {
				idx = len(urls) - 1
			}
			out = append(out, urls[idx])
		}
		return out
	}
	if seed == 0 {
		seed = 1
	}
	const golden = uint64(0x9E3779B97F4A7C15)
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)^golden))
	picked := make(map[int]struct{}, keep)
	for len(out) < keep {
		idx := rng.IntN(len(urls))
		if _, ok := picked[idx]; ok {
			continue
		}
		picked[idx] = struct{}{}
		out = append(out, urls[idx])
	}
	// Stable order: the original URL order is more grep-friendly than the
	// pick order, so re-sort by the original index.
	pos := make(map[string]int, len(urls))
	for i, u := range urls {
		if _, ok := pos[u]; !ok {
			pos[u] = i
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return pos[out[i]] < pos[out[j]] })
	return out
}

// applyPriority returns urls reordered so that priority URLs (per cfg) come
// first, preserving the original relative order within each bucket.
func applyPriority(urls []string, cfg config.PriorityURLsConfig) []string {
	if !cfg.IncludeFirst {
		return urls
	}
	if len(cfg.IncludeSubstrings) == 0 && len(cfg.IncludeRegex) == 0 {
		return urls
	}
	out := make([]string, 0, len(urls))
	priority := make([]string, 0)
	rest := make([]string, 0)
	for _, u := range urls {
		if cfg.IsPriority(u) {
			priority = append(priority, u)
		} else {
			rest = append(rest, u)
		}
	}
	out = append(out, priority...)
	out = append(out, rest...)
	return out
}
