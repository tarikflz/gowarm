package config

import (
	"net/http"
	"testing"
)

func TestDefaultCacheRules_CFCacheStatus(t *testing.T) {
	t.Parallel()
	d := &CacheDetectionConfig{} // empty → use defaults
	if err := d.Compile(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		header string
		value  string
		want   CacheState
	}{
		{"cf hit", "CF-Cache-Status", "HIT", CacheStateHit},
		{"cf miss", "CF-Cache-Status", "MISS", CacheStateMiss},
		{"cf expired = miss", "CF-Cache-Status", "EXPIRED", CacheStateMiss},
		{"cf bypass", "CF-Cache-Status", "BYPASS", CacheStateBypass},
		{"cf dynamic = bypass", "CF-Cache-Status", "DYNAMIC", CacheStateBypass},
		{"x-cache hit", "X-Cache", "HIT from edge-pop", CacheStateHit},
		{"x-cache miss", "X-Cache", "MISS", CacheStateMiss},
		{"drupal hit", "X-Drupal-Dynamic-Cache", "HIT", CacheStateHit},
		{"drupal miss", "X-Drupal-Dynamic-Cache", "MISS", CacheStateMiss},
		{"age presence = hit", "Age", "42", CacheStateHit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			h.Set(tc.header, tc.value)
			state, _, raw := d.Classify(func(name string) string { return h.Get(name) })
			if state != tc.want {
				t.Errorf("classify %s=%q: got state %q want %q", tc.header, tc.value, state, tc.want)
			}
			if raw != tc.value {
				t.Errorf("classify %s=%q: raw=%q want %q", tc.header, tc.value, raw, tc.value)
			}
		})
	}
}

func TestDefaultCacheRules_NoMatchUnknown(t *testing.T) {
	t.Parallel()
	d := &CacheDetectionConfig{}
	if err := d.Compile(); err != nil {
		t.Fatal(err)
	}
	state, _, raw := d.Classify(func(string) string { return "" })
	if state != CacheStateUnknown {
		t.Errorf("empty headers: state=%q want %q", state, CacheStateUnknown)
	}
	if raw != "" {
		t.Errorf("empty headers: raw=%q want empty", raw)
	}
}

func TestCustomCacheRules_RegexAndEquals(t *testing.T) {
	t.Parallel()
	d := &CacheDetectionConfig{
		Rules: []CacheRule{
			{Header: "X-Custom-Cache", MatchRegex: `^HIT-\d+$`, State: CacheStateHit},
			{Header: "X-Custom-Cache", MatchEquals: "MISS", State: CacheStateMiss},
			{Header: "X-Other", MatchContains: "byp", State: CacheStateBypass},
		},
	}
	if err := d.Compile(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		header string
		value  string
		want   CacheState
	}{
		{"regex match", "X-Custom-Cache", "HIT-123", CacheStateHit},
		{"regex no match falls through to equals", "X-Custom-Cache", "MISS", CacheStateMiss},
		{"contains match", "X-Other", "request_bypassed", CacheStateBypass},
		{"unmatched header", "X-Other", "fresh", CacheStateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			h.Set(tc.header, tc.value)
			got, _, _ := d.Classify(func(name string) string { return h.Get(name) })
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestCacheRule_PresenceOnly(t *testing.T) {
	t.Parallel()
	r := CacheRule{Header: "Age", State: CacheStateHit}
	if err := r.Compile(); err != nil {
		t.Fatal(err)
	}
	if !r.Match("anything") {
		t.Error("presence-only rule should match any non-empty value")
	}
	if r.Match("") {
		t.Error("presence-only rule must not match empty value")
	}
}

func TestCacheRule_BadRegexErrors(t *testing.T) {
	t.Parallel()
	d := &CacheDetectionConfig{
		Rules: []CacheRule{{Header: "X-A", MatchRegex: "[", State: CacheStateHit}},
	}
	if err := d.Compile(); err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestCacheRule_MissingHeaderErrors(t *testing.T) {
	t.Parallel()
	d := &CacheDetectionConfig{
		Rules: []CacheRule{{Header: "", State: CacheStateHit}},
	}
	if err := d.Compile(); err == nil {
		t.Error("expected error for empty header")
	}
}

func TestCacheRule_BadStateErrors(t *testing.T) {
	t.Parallel()
	d := &CacheDetectionConfig{
		Rules: []CacheRule{{Header: "X-A", State: "weird"}},
	}
	if err := d.Compile(); err == nil {
		t.Error("expected error for invalid state")
	}
}

func TestCacheDetection_BackwardsCompatLegacyExample(t *testing.T) {
	t.Parallel()
	// Verify the README example (cf-cache-status: HIT) keeps working with the
	// default rule set unchanged.
	d := &CacheDetectionConfig{}
	if err := d.Compile(); err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set("cf-cache-status", "HIT")
	state, header, raw := d.Classify(func(name string) string { return h.Get(name) })
	if state != CacheStateHit {
		t.Errorf("expected hit, got %q", state)
	}
	if raw != "HIT" {
		t.Errorf("expected raw=HIT, got %q", raw)
	}
	if header == "" {
		t.Errorf("expected non-empty header attribution")
	}
}
