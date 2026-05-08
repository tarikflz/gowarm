package main

import (
	"strings"
	"testing"

	"github.com/tarikflz/gowarm/internal/config"
)

// helper: build a config with a single 2-value axis applying to every URL.
func twoValueAxisConfig() *config.Config {
	return &config.Config{
		Axes: []config.Axis{
			{
				Name:       "region",
				CookieName: "region",
				Values: []config.AxisValue{
					{Value: "qc"},
					{Value: "on"},
				},
			},
		},
	}
}

func TestPlanRun_HappyPath(t *testing.T) {
	t.Parallel()
	cfg := twoValueAxisConfig()
	urls := []string{
		"https://example.com/a",
		"https://example.com/b",
		"https://example.com/c",
	}
	plan, err := planRun(urls, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.URLCount != 3 {
		t.Errorf("URLCount: got %d want 3", plan.URLCount)
	}
	if plan.TotalJobs != 6 { // 3 URLs × 2 axis values
		t.Errorf("TotalJobs: got %d want 6", plan.TotalJobs)
	}
	if plan.SkippedNoApplicable != 0 || plan.SkippedComboCap != 0 || plan.SampledOut != 0 {
		t.Errorf("expected no skips: %+v", plan)
	}
}

func TestPlanRun_MaxCombinationsPerURL(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Axes: []config.Axis{
			{Name: "a", CookieName: "a", Values: []config.AxisValue{{Value: "1"}, {Value: "2"}, {Value: "3"}}},
			{Name: "b", CookieName: "b", Values: []config.AxisValue{{Value: "x"}, {Value: "y"}, {Value: "z"}}},
		},
		Limits: config.LimitsConfig{MaxCombinationsPerURL: 4}, // 3×3 = 9 > 4 → skip
	}
	plan, err := planRun([]string{"https://example.com/a"}, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.URLCount != 0 || plan.SkippedComboCap != 1 {
		t.Errorf("expected 0 URLs and 1 cap-skip: %+v", plan)
	}

	// With -force, the URL must be retained.
	plan, err = planRun([]string{"https://example.com/a"}, cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.URLCount != 1 || plan.TotalJobs != 9 || plan.SkippedComboCap != 0 {
		t.Errorf("force should keep the URL: %+v", plan)
	}
}

func TestPlanRun_MaxJobsExceeded(t *testing.T) {
	t.Parallel()
	cfg := twoValueAxisConfig()
	cfg.Limits.MaxJobs = 3 // 4 URLs × 2 axis values = 8 > 3
	urls := []string{
		"https://example.com/a",
		"https://example.com/b",
		"https://example.com/c",
		"https://example.com/d",
	}
	_, err := planRun(urls, cfg, false)
	if err == nil {
		t.Fatal("expected MaxJobs error")
	}
	if !strings.Contains(err.Error(), "max_jobs") {
		t.Errorf("error should mention max_jobs: %v", err)
	}

	// With force, no error.
	plan, err := planRun(urls, cfg, true)
	if err != nil {
		t.Fatalf("force should bypass MaxJobs: %v", err)
	}
	if plan.TotalJobs != 8 {
		t.Errorf("forced run should plan 8 jobs, got %d", plan.TotalJobs)
	}
}

func TestPlanRun_SamplingFirstN(t *testing.T) {
	t.Parallel()
	cfg := twoValueAxisConfig()
	cfg.Sampling = config.SamplingConfig{FirstN: 2}
	urls := []string{
		"https://example.com/a",
		"https://example.com/b",
		"https://example.com/c",
		"https://example.com/d",
	}
	plan, err := planRun(urls, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.URLCount != 2 || plan.SampledOut != 2 {
		t.Errorf("FirstN=2 expected 2 URLs and 2 sampled out: %+v", plan)
	}
	if plan.URLs[0] != urls[0] || plan.URLs[1] != urls[1] {
		t.Errorf("FirstN should keep the first 2 URLs in order: %v", plan.URLs)
	}
}

func TestPlanRun_SamplingPercentDeterministicStride(t *testing.T) {
	t.Parallel()
	cfg := twoValueAxisConfig()
	cfg.Sampling = config.SamplingConfig{Percent: 50} // keep ~half deterministically
	urls := make([]string, 10)
	for i := range urls {
		urls[i] = "https://example.com/" + string(rune('a'+i))
	}

	plan1, _ := planRun(urls, cfg, false)
	plan2, _ := planRun(urls, cfg, false)

	if plan1.URLCount != plan2.URLCount {
		t.Errorf("stride sampling not deterministic: %d vs %d", plan1.URLCount, plan2.URLCount)
	}
	for i := range plan1.URLs {
		if plan1.URLs[i] != plan2.URLs[i] {
			t.Errorf("stride sampling not deterministic at %d: %q vs %q", i, plan1.URLs[i], plan2.URLs[i])
		}
	}
	if plan1.URLCount < 4 || plan1.URLCount > 6 {
		t.Errorf("50%% of 10 should give ~5, got %d", plan1.URLCount)
	}
}

func TestPlanRun_SamplingPercentRandomReproducible(t *testing.T) {
	t.Parallel()
	cfg := twoValueAxisConfig()
	cfg.Sampling = config.SamplingConfig{Percent: 30, Random: true, Seed: 42}
	urls := make([]string, 20)
	for i := range urls {
		urls[i] = "https://example.com/" + string(rune('a'+i))
	}

	plan1, _ := planRun(urls, cfg, false)
	plan2, _ := planRun(urls, cfg, false)

	if plan1.URLCount != plan2.URLCount {
		t.Errorf("random sampling with seed not reproducible: %d vs %d", plan1.URLCount, plan2.URLCount)
	}
	for i := range plan1.URLs {
		if plan1.URLs[i] != plan2.URLs[i] {
			t.Errorf("random sampling with seed not reproducible at %d: %q vs %q",
				i, plan1.URLs[i], plan2.URLs[i])
		}
	}
}

func TestPlanRun_PriorityURLsFirst(t *testing.T) {
	t.Parallel()
	cfg := twoValueAxisConfig()
	cfg.PriorityURLs = config.PriorityURLsConfig{
		IncludeSubstrings: []string{"/hot/"},
		IncludeFirst:      true,
	}
	if err := cfg.PriorityURLs.Compile(); err != nil {
		t.Fatal(err)
	}
	urls := []string{
		"https://example.com/cold/a",
		"https://example.com/hot/x",
		"https://example.com/cold/b",
		"https://example.com/hot/y",
	}
	plan, err := planRun(urls, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.URLs[0] != "https://example.com/hot/x" || plan.URLs[1] != "https://example.com/hot/y" {
		t.Errorf("priority URLs not at front: %v", plan.URLs)
	}
}

func TestPlanRun_NoApplicableValuesSkipsURL(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Axes: []config.Axis{
			{
				Name:       "lang",
				CookieName: "lang",
				Values: []config.AxisValue{
					{Value: "en", WhenURLContains: []string{"/en/"}},
				},
			},
		},
	}
	for i := range cfg.Axes[0].Values {
		_ = cfg.Axes[0].Values[i].Compile()
	}
	plan, err := planRun([]string{
		"https://example.com/en/page",
		"https://example.com/fr/page", // no applicable lang
	}, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.URLCount != 1 || plan.SkippedNoApplicable != 1 {
		t.Errorf("expected 1 URL and 1 skip, got %+v", plan)
	}
}

func TestPlanRun_DryRunFriendlyTotals(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Axes: []config.Axis{
			{Name: "a", CookieName: "a", Values: []config.AxisValue{{Value: "1"}, {Value: "2"}}},
			{Name: "b", CookieName: "b", Values: []config.AxisValue{{Value: "x"}, {Value: "y"}, {Value: "z"}}},
		},
	}
	urls := []string{"u1", "u2", "u3"}
	plan, err := planRun(urls, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	// 3 URLs × 2 × 3 = 18
	if plan.TotalJobs != 18 {
		t.Errorf("total jobs: got %d want 18", plan.TotalJobs)
	}
}

func TestComboCount_Overflow(t *testing.T) {
	t.Parallel()
	// 4 axes of 65536 values each → 65536^4 = 2^64 > MaxInt(63).
	// 65536 AxisValues × 4 slices is ~20MB on 64-bit, which is OK for a test.
	const n = 1 << 16
	axis := make([]config.AxisValue, n)
	got := comboCount([][]config.AxisValue{axis, axis, axis, axis})
	if got != int(^uint(0)>>1) {
		t.Errorf("expected MaxInt on overflow, got %d", got)
	}
}

func TestComboCount_NoOverflow(t *testing.T) {
	t.Parallel()
	a := make([]config.AxisValue, 4)
	b := make([]config.AxisValue, 5)
	c := make([]config.AxisValue, 3)
	if got := comboCount([][]config.AxisValue{a, b, c}); got != 60 {
		t.Errorf("comboCount: got %d want 60", got)
	}
}
