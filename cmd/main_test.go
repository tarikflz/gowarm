package main

import (
	"sort"
	"strings"
	"testing"

	"github.com/tarikflz/gowarm/internal/config"
)

// ---------- forEachCombo ----------

func TestForEachCombo_Cartesian(t *testing.T) {
	t.Parallel()
	in := [][]config.AxisValue{
		{{Value: "a"}, {Value: "b"}},
		{{Value: "1"}, {Value: "2"}, {Value: "3"}},
	}
	var got []string
	forEachCombo(in, func(combo []config.AxisValue) bool {
		got = append(got, combo[0].Value+combo[1].Value)
		return true
	})
	sort.Strings(got)
	want := []string{"a1", "a2", "a3", "b1", "b2", "b3"}
	if !equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestForEachCombo_AbortsEarly(t *testing.T) {
	t.Parallel()
	in := [][]config.AxisValue{
		{{Value: "a"}, {Value: "b"}, {Value: "c"}},
		{{Value: "1"}, {Value: "2"}, {Value: "3"}},
	}
	count := 0
	forEachCombo(in, func(combo []config.AxisValue) bool {
		count++
		return count < 2 // abort after 2 invocations
	})
	if count != 2 {
		t.Errorf("expected 2 invocations, got %d", count)
	}
}

func TestForEachCombo_SingleAxis(t *testing.T) {
	t.Parallel()
	in := [][]config.AxisValue{{{Value: "a"}, {Value: "b"}}}
	var got []string
	forEachCombo(in, func(combo []config.AxisValue) bool {
		got = append(got, combo[0].Value)
		return true
	})
	if !equal(got, []string{"a", "b"}) {
		t.Errorf("got %v", got)
	}
}

// ---------- buildApplicable ----------

func TestBuildApplicable_AllAxesApply(t *testing.T) {
	t.Parallel()
	axes := []config.Axis{
		{Name: "region", Values: []config.AxisValue{{Value: "qc"}, {Value: "on"}}},
		{Name: "lang", Values: []config.AxisValue{{Value: "en"}, {Value: "fr"}}},
	}
	got, ok := buildApplicable("https://x.test/page", axes)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(got) != 2 || len(got[0]) != 2 || len(got[1]) != 2 {
		t.Errorf("expected 2x2 applicable, got %v", got)
	}
}

func TestBuildApplicable_OneAxisHasNoMatch(t *testing.T) {
	t.Parallel()
	v := config.AxisValue{Value: "fr", WhenURLContains: []string{"/fr/"}}
	if err := v.Compile(); err != nil {
		t.Fatal(err)
	}
	axes := []config.Axis{
		{Name: "lang", Values: []config.AxisValue{v}},
	}
	_, ok := buildApplicable("https://x.test/en/page", axes)
	if ok {
		t.Error("expected URL to be skipped (no applicable language)")
	}
}

// ---------- buildJobInputs ----------

func TestBuildJobInputs_CookieAndHeader(t *testing.T) {
	t.Parallel()
	axes := []config.Axis{
		{Name: "region", CookieName: "region", Transform: "upper"},
		{Name: "lang", HeaderName: "Accept-Language"},
		{Name: "currency", CookieName: "currency", HeaderName: "X-Currency"},
	}
	combo := []config.AxisValue{
		{Value: "qc"},
		{Value: "en"},
		{Value: "usd"},
	}
	cookies, headers, tags := buildJobInputs(axes, combo)

	cookieMap := make(map[string]string)
	for _, c := range cookies {
		cookieMap[c.Name] = c.Value
	}
	if cookieMap["region"] != "QC" {
		t.Errorf("region cookie not uppercased: %q", cookieMap["region"])
	}
	if cookieMap["currency"] != "usd" {
		t.Errorf("currency cookie wrong: %q", cookieMap["currency"])
	}
	if _, ok := cookieMap["lang"]; ok {
		t.Errorf("lang should not be a cookie (header_name only axis)")
	}
	if headers.Get("Accept-Language") != "en" {
		t.Errorf("lang header missing: %v", headers)
	}
	if headers.Get("X-Currency") != "usd" {
		t.Errorf("currency header missing: %v", headers)
	}
	if tags["region"] != "QC" || tags["lang"] != "en" || tags["currency"] != "usd" {
		t.Errorf("tags wrong: %v", tags)
	}
}

// ---------- applyAxisOverrides ----------

func TestApplyAxisOverrides_PreservesFilters(t *testing.T) {
	t.Parallel()
	original := config.AxisValue{
		Value:           "en",
		WhenURLContains: []string{"/en/"},
	}
	if err := original.Compile(); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Axes: []config.Axis{{
			Name:       "lang",
			CookieName: "lang",
			Values:     []config.AxisValue{original, {Value: "fr"}},
		}},
	}
	if err := applyAxisOverrides(cfg, multiFlag{"lang=en"}); err != nil {
		t.Fatal(err)
	}
	got := cfg.Axes[0].Values
	if len(got) != 1 || got[0].Value != "en" {
		t.Fatalf("expected single 'en' value, got %v", got)
	}
	if len(got[0].WhenURLContains) != 1 || got[0].WhenURLContains[0] != "/en/" {
		t.Errorf("filters not preserved: %v", got[0])
	}
}

func TestApplyAxisOverrides_NewValueGetsNoFilter(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Axes: []config.Axis{{
			Name:       "region",
			CookieName: "region",
			Values:     []config.AxisValue{{Value: "qc"}},
		}},
	}
	if err := applyAxisOverrides(cfg, multiFlag{"region=qc,bc"}); err != nil {
		t.Fatal(err)
	}
	got := cfg.Axes[0].Values
	if len(got) != 2 {
		t.Fatalf("expected 2 values, got %v", got)
	}
	if got[1].Value != "bc" || len(got[1].WhenURLContains) != 0 {
		t.Errorf("new value should have no filter: %v", got[1])
	}
}

func TestApplyAxisOverrides_RejectsUnknownAxis(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Axes: []config.Axis{{Name: "region", CookieName: "region", Values: []config.AxisValue{{Value: "qc"}}}},
	}
	err := applyAxisOverrides(cfg, multiFlag{"language=en"})
	if err == nil || !strings.Contains(err.Error(), "no axis named") {
		t.Errorf("expected 'no axis named' error, got %v", err)
	}
}

func TestApplyAxisOverrides_RejectsBadSyntax(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Axes: []config.Axis{{Name: "r", CookieName: "r", Values: []config.AxisValue{{Value: "qc"}}}},
	}
	for _, in := range []string{"", "no-equals", "=val", "name="} {
		if err := applyAxisOverrides(cfg, multiFlag{in}); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

// ---------- helpers ----------

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
