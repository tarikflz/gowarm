// Package config loads and validates runtime configuration from a YAML file
// and the process environment. Environment variables can be referenced inside
// any string value using ${VAR} syntax and are expanded after the YAML is
// parsed.
//
// The model is generic: there are no built-in concepts of "region" or
// "language". Instead, the user declares one or more `axes`, each of which
// projects a set of values onto a cookie and/or header. The cartesian product
// of all axes generates the warming jobs.
package config

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration object.
type Config struct {
	SitemapURL     string               `yaml:"sitemap_url"`
	WorkerCount    int                  `yaml:"worker_count"`
	Method         string               `yaml:"method"`
	UserAgent      string               `yaml:"user_agent"`
	HTTP           HTTPConfig           `yaml:"http"`
	Retry          RetryConfig          `yaml:"retry"`
	URLFilter      URLFilter            `yaml:"url_filter"`
	Axes           []Axis               `yaml:"axes"`
	GlobalHeaders  map[string]string    `yaml:"global_headers"`
	GlobalCookies  []CookieConfig       `yaml:"global_cookies"`
	FollowIndex    bool                 `yaml:"follow_index"`
	StopOnError    bool                 `yaml:"stop_on_error"`
	LogLevel       string               `yaml:"log_level"`
	Report         ReportConfig         `yaml:"report"`
	CacheDetection CacheDetectionConfig `yaml:"cache_detection"`
	Limits         LimitsConfig         `yaml:"limits"`
	Sampling       SamplingConfig       `yaml:"sampling"`
	PriorityURLs   PriorityURLsConfig   `yaml:"priority_urls"`
}

// ReportConfig controls optional report / log artefacts written to disk. All
// three paths are independent and opt-in (empty path = disabled).
//
//   - LogFile     — tee the slog output (the same lines that go to stdout)
//     to this file as well.
//   - PerJobFile  — one JSON object per warm job (JSON Lines) describing the
//     URL, status, cache state, latency, and axis tags.
//   - SummaryFile — a single JSON document written at the end of the run with
//     aggregate counters and a list of failed jobs.
type ReportConfig struct {
	LogFile     string `yaml:"log_file"`
	PerJobFile  string `yaml:"per_job_file"`
	SummaryFile string `yaml:"summary_file"`
}

// HTTPConfig tunes the underlying transport.
//
// Rate limiting + concurrency:
//
//   - RateLimitRPS / Burst gate the global outbound request rate via a
//     token-bucket. 0 disables rate limiting entirely (the default).
//   - MaxConnsPerHost caps the number of in-flight requests per host. It is
//     a *concurrency* limit, not a connection-pool tuning knob (that's
//     MaxIdleConnsPerHost). 0 disables the limit.
//   - AdaptiveSlowdown enables transient throttling whenever the origin
//     returns 429 or 503: the rate limiter's effective limit is halved
//     for AdaptiveSlowdownDuration before recovering.
type HTTPConfig struct {
	Timeout                  time.Duration `yaml:"timeout"`
	MaxIdleConns             int           `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost      int           `yaml:"max_idle_conns_per_host"`
	IdleConnTimeout          time.Duration `yaml:"idle_conn_timeout"`
	DisableCompression       bool          `yaml:"disable_compression"`
	RateLimitRPS             float64       `yaml:"rate_limit_rps"`
	Burst                    int           `yaml:"burst"`
	MaxConnsPerHost          int           `yaml:"max_conns_per_host"`
	AdaptiveSlowdown         bool          `yaml:"adaptive_slowdown"`
	AdaptiveSlowdownFactor   float64       `yaml:"adaptive_slowdown_factor"`
	AdaptiveSlowdownDuration time.Duration `yaml:"adaptive_slowdown_duration"`
}

// RetryConfig controls the exponential backoff retry policy.
type RetryConfig struct {
	MaxAttempts int           `yaml:"max_attempts"`
	BaseDelay   time.Duration `yaml:"base_delay"`
	MaxDelay    time.Duration `yaml:"max_delay"`
	Jitter      time.Duration `yaml:"jitter"`
	Factor      float64       `yaml:"factor"`
}

// CookieConfig describes a global cookie applied to every warm request unless
// a per-job cookie of the same name overrides it.
type CookieConfig struct {
	Name   string `yaml:"name"`
	Value  string `yaml:"value"`
	Domain string `yaml:"domain"`
	Path   string `yaml:"path"`
}

// ---------------------------------------------------------------------------
// Axes: the generic cookie/header dimension model.
// ---------------------------------------------------------------------------

// Axis is one dimension of the warming cartesian product. Each value of the
// axis is projected onto a cookie (CookieName) and/or a header (HeaderName).
// At least one of CookieName or HeaderName must be set.
//
// Transform optionally normalises each value before sending:
//   - "upper":  strings.ToUpper
//   - "lower":  strings.ToLower
//   - "" / "none": no change
type Axis struct {
	Name       string      `yaml:"name"`
	CookieName string      `yaml:"cookie_name"`
	HeaderName string      `yaml:"header_name"`
	Transform  string      `yaml:"transform"`
	Values     []AxisValue `yaml:"values"`
}

// AxisValue is one value of an Axis. It can be written in two YAML forms:
//
//	values: [on, mb, qc, ab]                          # shorthand: just strings
//	values:                                           # full form
//	  - value: en
//	    when_url_contains: ["/en/"]
//	    when_url_matches: ["/en$"]
//
// when_url_contains and when_url_matches let a single value bind to a subset
// of URLs. If both lists are empty, the value applies to every URL.
type AxisValue struct {
	Value           string   `yaml:"value"`
	WhenURLContains []string `yaml:"when_url_contains"`
	WhenURLMatches  []string `yaml:"when_url_matches"`

	// Compiled regexes. Not serialised.
	matchesRE []*regexp.Regexp `yaml:"-"`
}

// UnmarshalYAML supports the shorthand form `values: [on, mb, qc, ab]` by
// accepting either a scalar (treated as Value) or a mapping (full struct).
func (a *AxisValue) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		a.Value = node.Value
		return nil
	}
	type rawAxisValue AxisValue
	var raw rawAxisValue
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*a = AxisValue(raw)
	return nil
}

// Compile pre-compiles WhenURLMatches into regexes. Idempotent.
func (v *AxisValue) Compile() error {
	v.matchesRE = nil
	for _, s := range v.WhenURLMatches {
		re, err := regexp.Compile(s)
		if err != nil {
			return fmt.Errorf("when_url_matches %q: %w", s, err)
		}
		v.matchesRE = append(v.matchesRE, re)
	}
	return nil
}

// Applies reports whether this value should be used for the given URL. A
// value with no filters applies universally.
func (v *AxisValue) Applies(u string) bool {
	if len(v.WhenURLContains) == 0 && len(v.matchesRE) == 0 {
		return true
	}
	for _, s := range v.WhenURLContains {
		if strings.Contains(u, s) {
			return true
		}
	}
	for _, re := range v.matchesRE {
		if re.MatchString(u) {
			return true
		}
	}
	return false
}

// ApplicableValues returns the subset of an axis's values that apply to u.
func (a *Axis) ApplicableValues(u string) []AxisValue {
	out := make([]AxisValue, 0, len(a.Values))
	for _, v := range a.Values {
		if v.Applies(u) {
			out = append(out, v)
		}
	}
	return out
}

// TransformValue applies the axis's Transform to raw.
func (a *Axis) TransformValue(raw string) string {
	switch strings.ToLower(strings.TrimSpace(a.Transform)) {
	case "upper":
		return strings.ToUpper(raw)
	case "lower":
		return strings.ToLower(raw)
	default:
		return raw
	}
}

// ---------------------------------------------------------------------------
// CacheDetection: provider-agnostic cache verification.
// ---------------------------------------------------------------------------

// CacheState is the normalised, provider-agnostic outcome of inspecting an
// HTTP response's cache-state headers.
type CacheState string

// The four normalised cache states gowarm reports. RawDetail (on the
// per-job record) keeps the original header for debugging.
const (
	CacheStateHit     CacheState = "hit"
	CacheStateMiss    CacheState = "miss"
	CacheStateBypass  CacheState = "bypass"
	CacheStateUnknown CacheState = "unknown"
)

// CacheDetectionConfig drives how a response is classified into one of the
// four normalised CacheState values. Rules are evaluated in order and the
// first match wins. If no rule matches, the response is reported as
// CacheStateUnknown with the raw header value preserved (when available).
//
// When Rules is empty, gowarm falls back to a built-in default that covers
// CF-Cache-Status, X-Cache, X-Drupal-Cache, X-Drupal-Dynamic-Cache, and Age,
// so the existing cf:HIT example keeps working out of the box.
type CacheDetectionConfig struct {
	Rules []CacheRule `yaml:"rules"`
}

// CacheRule maps an HTTP response header to a normalised CacheState.
//
// Match semantics (any of the three options applies — they are OR-ed):
//
//   - MatchEquals      — exact value, case-insensitive
//   - MatchContains    — substring, case-insensitive
//   - MatchRegex       — regex, applied to the raw header value
//
// If none of the three is set, the rule matches whenever Header is present
// (any non-empty value) — useful for the `Age` header pattern.
type CacheRule struct {
	Header        string     `yaml:"header"`
	MatchEquals   string     `yaml:"match_equals"`
	MatchContains string     `yaml:"match_contains"`
	MatchRegex    string     `yaml:"match_regex"`
	State         CacheState `yaml:"state"`

	matchRE *regexp.Regexp `yaml:"-"`
}

// Compile pre-compiles MatchRegex. Idempotent.
func (r *CacheRule) Compile() error {
	r.matchRE = nil
	if r.MatchRegex == "" {
		return nil
	}
	re, err := regexp.Compile(r.MatchRegex)
	if err != nil {
		return fmt.Errorf("match_regex %q: %w", r.MatchRegex, err)
	}
	r.matchRE = re
	return nil
}

// Match reports whether headerVal triggers the rule.
func (r *CacheRule) Match(headerVal string) bool {
	if headerVal == "" {
		return false
	}
	if r.MatchEquals == "" && r.MatchContains == "" && r.matchRE == nil {
		return true // header presence is enough (e.g. `Age`)
	}
	if r.MatchEquals != "" && strings.EqualFold(headerVal, r.MatchEquals) {
		return true
	}
	if r.MatchContains != "" && strings.Contains(strings.ToLower(headerVal), strings.ToLower(r.MatchContains)) {
		return true
	}
	if r.matchRE != nil && r.matchRE.MatchString(headerVal) {
		return true
	}
	return false
}

// Compile pre-compiles every rule's regex. Must be called once before
// Classify is used.
func (d *CacheDetectionConfig) Compile() error {
	for i := range d.Rules {
		if strings.TrimSpace(d.Rules[i].Header) == "" {
			return fmt.Errorf("cache_detection.rules[%d].header is required", i)
		}
		if d.Rules[i].State == "" {
			return fmt.Errorf("cache_detection.rules[%d].state is required", i)
		}
		switch d.Rules[i].State {
		case CacheStateHit, CacheStateMiss, CacheStateBypass, CacheStateUnknown:
		default:
			return fmt.Errorf("cache_detection.rules[%d].state %q: must be hit, miss, bypass, or unknown",
				i, d.Rules[i].State)
		}
		if err := d.Rules[i].Compile(); err != nil {
			return fmt.Errorf("cache_detection.rules[%d]: %w", i, err)
		}
	}
	return nil
}

// DefaultCacheRules covers the common CDN/origin cache headers so existing
// configurations keep working without declaring `cache_detection.rules`.
//
// The order is intentional: more specific (CF-Cache-Status) before more
// generic (X-Cache, Age) so that a multi-layer response is classified by its
// closest cache layer.
func DefaultCacheRules() []CacheRule {
	return []CacheRule{
		// Cloudflare
		{Header: "CF-Cache-Status", MatchEquals: "HIT", State: CacheStateHit},
		{Header: "CF-Cache-Status", MatchEquals: "MISS", State: CacheStateMiss},
		{Header: "CF-Cache-Status", MatchEquals: "EXPIRED", State: CacheStateMiss},
		{Header: "CF-Cache-Status", MatchEquals: "REVALIDATED", State: CacheStateMiss},
		{Header: "CF-Cache-Status", MatchEquals: "BYPASS", State: CacheStateBypass},
		{Header: "CF-Cache-Status", MatchEquals: "DYNAMIC", State: CacheStateBypass},
		{Header: "CF-Cache-Status", MatchContains: "HIT", State: CacheStateHit},
		// Drupal
		{Header: "X-Drupal-Dynamic-Cache", MatchEquals: "HIT", State: CacheStateHit},
		{Header: "X-Drupal-Dynamic-Cache", MatchEquals: "MISS", State: CacheStateMiss},
		{Header: "X-Drupal-Dynamic-Cache", MatchEquals: "UNCACHEABLE", State: CacheStateBypass},
		{Header: "X-Drupal-Cache", MatchEquals: "HIT", State: CacheStateHit},
		{Header: "X-Drupal-Cache", MatchEquals: "MISS", State: CacheStateMiss},
		// Generic / Varnish / Fastly / nginx-cache
		{Header: "X-Cache", MatchContains: "HIT", State: CacheStateHit},
		{Header: "X-Cache", MatchContains: "MISS", State: CacheStateMiss},
		{Header: "X-Cache", MatchContains: "BYPASS", State: CacheStateBypass},
		{Header: "X-Cache-Status", MatchContains: "HIT", State: CacheStateHit},
		{Header: "X-Cache-Status", MatchContains: "MISS", State: CacheStateMiss},
		// Age presence is a weak signal of a cached response.
		{Header: "Age", State: CacheStateHit},
	}
}

// Classify maps a response's headers to a normalised CacheState plus the
// raw header value that drove the decision. It returns CacheStateUnknown and
// an empty raw value when no rule matches.
func (d *CacheDetectionConfig) Classify(get func(string) string) (CacheState, string, string) {
	rules := d.Rules
	if len(rules) == 0 {
		rules = DefaultCacheRules()
	}
	for i := range rules {
		v := get(rules[i].Header)
		if v == "" {
			continue
		}
		if rules[i].Match(v) {
			return rules[i].State, rules[i].Header, v
		}
	}
	return CacheStateUnknown, "", ""
}

// ---------------------------------------------------------------------------
// Limits: cartesian-explosion guards.
// ---------------------------------------------------------------------------

// LimitsConfig protects against accidentally generating a million-job run.
// Both limits are advisory by default — when exceeded, gowarm aborts with a
// descriptive error before any traffic is sent. Pass `-force` on the CLI to
// bypass them.
//
//   - MaxJobs               — total cap across the entire run.
//   - MaxCombinationsPerURL — per-URL cap. URLs whose applicable cartesian
//     exceeds this are skipped (and counted in skipped_urls) unless -force
//     is set.
type LimitsConfig struct {
	MaxJobs               int `yaml:"max_jobs"`
	MaxCombinationsPerURL int `yaml:"max_combinations_per_url"`
}

// SamplingConfig optionally trims the sitemap URL list before the cartesian
// product is generated. The two modes are mutually exclusive — Percent wins
// when both are non-zero.
//
//   - Percent  ∈ (0, 100]  — keep this percentage of URLs (deterministic
//     stride; Seed only changes which URLs are picked when StridedSampling
//     is false). 100 disables sampling.
//   - FirstN   > 0          — keep the first N URLs (after URL filter).
//   - Seed     int          — seed for the (deterministic) sampling RNG.
//   - Random   bool         — when true, use random sampling instead of a
//     stride. With Random=false, sampling picks every Nth URL so re-runs
//     warm the same set.
type SamplingConfig struct {
	Percent float64 `yaml:"percent"`
	FirstN  int     `yaml:"first_n"`
	Seed    int64   `yaml:"seed"`
	Random  bool    `yaml:"random"`
}

// PriorityURLsConfig promotes URLs matching one of its filters to the front
// of the queue so hot paths warm up first. Matching is OR-ed across all four
// lists.
type PriorityURLsConfig struct {
	IncludeSubstrings []string `yaml:"include_substrings"`
	IncludeRegex      []string `yaml:"include_regex"`
	IncludeFirst      bool     `yaml:"include_first"`

	includeRE []*regexp.Regexp `yaml:"-"`
}

// Compile pre-compiles regex patterns.
func (p *PriorityURLsConfig) Compile() error {
	p.includeRE = nil
	for _, s := range p.IncludeRegex {
		re, err := regexp.Compile(s)
		if err != nil {
			return fmt.Errorf("priority_urls.include_regex %q: %w", s, err)
		}
		p.includeRE = append(p.includeRE, re)
	}
	return nil
}

// IsPriority reports whether u should warm before any non-priority URL.
func (p *PriorityURLsConfig) IsPriority(u string) bool {
	if !p.IncludeFirst {
		return false
	}
	for _, s := range p.IncludeSubstrings {
		if strings.Contains(u, s) {
			return true
		}
	}
	for _, re := range p.includeRE {
		if re.MatchString(u) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// URLFilter: optional sitemap URL gating.
// ---------------------------------------------------------------------------

// URLFilter selects which sitemap URLs are actually warmed. All four lists
// are optional and cumulative:
//   - excludes win over includes;
//   - if no includes are configured, every non-excluded URL passes;
//   - otherwise a URL must match at least one include.
type URLFilter struct {
	IncludeSubstrings []string `yaml:"include_substrings"`
	ExcludeSubstrings []string `yaml:"exclude_substrings"`
	IncludeRegex      []string `yaml:"include_regex"`
	ExcludeRegex      []string `yaml:"exclude_regex"`

	includeRE []*regexp.Regexp `yaml:"-"`
	excludeRE []*regexp.Regexp `yaml:"-"`
}

// Compile pre-compiles regex patterns. Must be called once before Allow.
func (f *URLFilter) Compile() error {
	f.includeRE = nil
	f.excludeRE = nil
	for _, s := range f.IncludeRegex {
		re, err := regexp.Compile(s)
		if err != nil {
			return fmt.Errorf("url_filter.include_regex %q: %w", s, err)
		}
		f.includeRE = append(f.includeRE, re)
	}
	for _, s := range f.ExcludeRegex {
		re, err := regexp.Compile(s)
		if err != nil {
			return fmt.Errorf("url_filter.exclude_regex %q: %w", s, err)
		}
		f.excludeRE = append(f.excludeRE, re)
	}
	return nil
}

// Allow reports whether u passes the filter.
func (f *URLFilter) Allow(u string) bool {
	for _, s := range f.ExcludeSubstrings {
		if strings.Contains(u, s) {
			return false
		}
	}
	for _, re := range f.excludeRE {
		if re.MatchString(u) {
			return false
		}
	}
	if len(f.IncludeSubstrings) == 0 && len(f.includeRE) == 0 {
		return true
	}
	for _, s := range f.IncludeSubstrings {
		if strings.Contains(u, s) {
			return true
		}
	}
	for _, re := range f.includeRE {
		if re.MatchString(u) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Default + Load + Validate.
// ---------------------------------------------------------------------------

// Default returns a Config preloaded with safe baseline values. The default
// has NO axes — callers must declare at least one before Validate succeeds.
// This keeps gowarm agnostic of any specific website.
func Default() *Config {
	return &Config{
		WorkerCount: 50,
		Method:      "GET",
		UserAgent:   "gowarm/1.0 (+sitemap-cache-warmer)",
		HTTP: HTTPConfig{
			Timeout:             20 * time.Second,
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
		Retry: RetryConfig{
			MaxAttempts: 3,
			BaseDelay:   200 * time.Millisecond,
			MaxDelay:    5 * time.Second,
			Jitter:      200 * time.Millisecond,
			Factor:      2.0,
		},
		FollowIndex: true,
		StopOnError: false,
		LogLevel:    "info",
	}
}

// Load reads a YAML file (if it exists) into a Config seeded from Default(),
// expands ${VAR} references against the environment, validates, and returns.
// An empty path skips the file step (useful when overriding everything via
// CLI flags).
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		if err == nil {
			if err := yaml.Unmarshal(raw, cfg); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
		}
	}

	expandEnvInStruct(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate ensures the Config is internally consistent. It also normalises
// (trims, lowercases) some fields and pre-compiles regex matchers.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.SitemapURL) == "" {
		return fmt.Errorf("sitemap_url is required")
	}
	if c.WorkerCount <= 0 {
		return fmt.Errorf("worker_count must be > 0 (got %d)", c.WorkerCount)
	}
	if c.HTTP.Timeout <= 0 {
		return fmt.Errorf("http.timeout must be > 0")
	}
	method := strings.ToUpper(strings.TrimSpace(c.Method))
	if method != "GET" && method != "HEAD" {
		return fmt.Errorf("method must be GET or HEAD (got %q)", c.Method)
	}
	c.Method = method

	if len(c.Axes) == 0 {
		return fmt.Errorf("at least one axis must be configured")
	}
	for i := range c.Axes {
		a := &c.Axes[i]
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("axes[%d].name is required", i)
		}
		if a.CookieName == "" && a.HeaderName == "" {
			return fmt.Errorf("axes[%d] (%s): cookie_name or header_name (or both) must be set", i, a.Name)
		}
		switch strings.ToLower(strings.TrimSpace(a.Transform)) {
		case "", "none", "upper", "lower":
		default:
			return fmt.Errorf("axes[%d] (%s).transform: must be upper, lower, or none (got %q)", i, a.Name, a.Transform)
		}
		if len(a.Values) == 0 {
			return fmt.Errorf("axes[%d] (%s): at least one value must be declared", i, a.Name)
		}
		for j := range a.Values {
			if strings.TrimSpace(a.Values[j].Value) == "" {
				return fmt.Errorf("axes[%d] (%s).values[%d].value is empty", i, a.Name, j)
			}
			if err := a.Values[j].Compile(); err != nil {
				return fmt.Errorf("axes[%d] (%s).values[%d]: %w", i, a.Name, j, err)
			}
		}
	}

	if err := c.URLFilter.Compile(); err != nil {
		return err
	}

	if err := c.CacheDetection.Compile(); err != nil {
		return err
	}

	if err := c.PriorityURLs.Compile(); err != nil {
		return err
	}

	if c.Retry.MaxAttempts < 1 {
		c.Retry.MaxAttempts = 1
	}
	if c.Retry.Factor <= 0 {
		c.Retry.Factor = 2.0
	}

	// HTTP rate limiting + adaptive slowdown defaults.
	if c.HTTP.RateLimitRPS < 0 {
		return fmt.Errorf("http.rate_limit_rps must be >= 0 (got %v)", c.HTTP.RateLimitRPS)
	}
	if c.HTTP.Burst < 0 {
		return fmt.Errorf("http.burst must be >= 0 (got %d)", c.HTTP.Burst)
	}
	if c.HTTP.MaxConnsPerHost < 0 {
		return fmt.Errorf("http.max_conns_per_host must be >= 0 (got %d)", c.HTTP.MaxConnsPerHost)
	}
	if c.HTTP.RateLimitRPS > 0 && c.HTTP.Burst == 0 {
		// Reasonable default: allow a 1-second burst.
		c.HTTP.Burst = int(c.HTTP.RateLimitRPS)
		if c.HTTP.Burst < 1 {
			c.HTTP.Burst = 1
		}
	}
	if c.HTTP.AdaptiveSlowdown {
		if c.HTTP.AdaptiveSlowdownFactor <= 0 || c.HTTP.AdaptiveSlowdownFactor >= 1 {
			c.HTTP.AdaptiveSlowdownFactor = 0.5
		}
		if c.HTTP.AdaptiveSlowdownDuration <= 0 {
			c.HTTP.AdaptiveSlowdownDuration = 30 * time.Second
		}
	}

	// Limits sanity.
	if c.Limits.MaxJobs < 0 {
		return fmt.Errorf("limits.max_jobs must be >= 0 (got %d)", c.Limits.MaxJobs)
	}
	if c.Limits.MaxCombinationsPerURL < 0 {
		return fmt.Errorf("limits.max_combinations_per_url must be >= 0 (got %d)", c.Limits.MaxCombinationsPerURL)
	}

	// Sampling sanity.
	if c.Sampling.Percent < 0 || c.Sampling.Percent > 100 {
		return fmt.Errorf("sampling.percent must be in [0, 100] (got %v)", c.Sampling.Percent)
	}
	if c.Sampling.FirstN < 0 {
		return fmt.Errorf("sampling.first_n must be >= 0 (got %d)", c.Sampling.FirstN)
	}

	return nil
}

// ---------------------------------------------------------------------------
// ${VAR} expansion (reflection-based, recursive).
// ---------------------------------------------------------------------------

// expandEnvInStruct walks the struct via reflection and applies os.ExpandEnv
// to every settable string and []string field. Maps with string values are
// expanded too.
func expandEnvInStruct(v any) {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	expandEnvInValue(rv)
}

func expandEnvInValue(rv reflect.Value) {
	switch rv.Kind() {
	case reflect.Struct:
		for i := 0; i < rv.NumField(); i++ {
			f := rv.Field(i)
			if !f.CanSet() {
				continue
			}
			expandEnvInValue(f)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			expandEnvInValue(rv.Index(i))
		}
	case reflect.Map:
		iter := rv.MapRange()
		for iter.Next() {
			k := iter.Key()
			val := iter.Value()
			if val.Kind() == reflect.String {
				rv.SetMapIndex(k, reflect.ValueOf(os.ExpandEnv(val.String())))
			}
		}
	case reflect.String:
		rv.SetString(os.ExpandEnv(rv.String()))
	case reflect.Ptr, reflect.Interface:
		if !rv.IsNil() {
			expandEnvInValue(rv.Elem())
		}
	}
}
