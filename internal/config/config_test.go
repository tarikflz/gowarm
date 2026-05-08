package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const minimalYAML = `
sitemap_url: "https://example.com/sm.xml"
axes:
  - name: region
    cookie_name: region
    transform: upper
    values: [on, mb, qc, ab]
`

func TestLoad_Minimal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, []byte(minimalYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.WorkerCount != 50 {
		t.Errorf("worker_count default not applied: %d", c.WorkerCount)
	}
	if len(c.Axes) != 1 || c.Axes[0].Name != "region" {
		t.Errorf("axes: %+v", c.Axes)
	}
	if len(c.Axes[0].Values) != 4 {
		t.Errorf("axis values: %+v", c.Axes[0].Values)
	}
}

func TestAxisValue_ShorthandAndFullForm(t *testing.T) {
	t.Parallel()
	yaml := `
sitemap_url: "https://x.test/s.xml"
axes:
  - name: language
    cookie_name: lang
    values:
      - en
      - value: fr
        when_url_contains: ["/fr/"]
        when_url_matches: ["/fr$"]
`
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	vs := c.Axes[0].Values
	if len(vs) != 2 {
		t.Fatalf("expected 2 values, got %d", len(vs))
	}
	if vs[0].Value != "en" {
		t.Errorf("shorthand: %q", vs[0].Value)
	}
	if vs[1].Value != "fr" || len(vs[1].WhenURLContains) != 1 {
		t.Errorf("full form: %+v", vs[1])
	}
}

func TestAxisValue_Applies(t *testing.T) {
	t.Parallel()
	v := AxisValue{
		Value:           "fr",
		WhenURLContains: []string{"/fr/"},
		WhenURLMatches:  []string{"/fr$"},
	}
	if err := v.Compile(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		url string
		ok  bool
	}{
		{"https://x.test/fr", true},
		{"https://x.test/fr/about", true},
		{"https://x.test/en/about", false},
		{"https://x.test/about/fr-test", false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			if got := v.Applies(tc.url); got != tc.ok {
				t.Errorf("Applies(%q) = %v, want %v", tc.url, got, tc.ok)
			}
		})
	}

	// no filter → applies always
	universal := AxisValue{Value: "en"}
	if !universal.Applies("https://x.test/anything") {
		t.Error("unfiltered value must apply universally")
	}
}

func TestAxis_TransformValue(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		transform, in, out string
	}{
		"upper": {"upper", "qc", "QC"},
		"lower": {"lower", "QC", "qc"},
		"none":  {"none", "MixED", "MixED"},
		"empty": {"", "MixED", "MixED"},
		"weird": {"  Upper  ", "qc", "QC"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := Axis{Transform: tc.transform}
			if got := a.TransformValue(tc.in); got != tc.out {
				t.Errorf("TransformValue(%q) = %q, want %q", tc.in, got, tc.out)
			}
		})
	}
}

func TestURLFilter_Allow(t *testing.T) {
	t.Parallel()
	f := URLFilter{
		IncludeSubstrings: []string{"/en/", "/fr/"},
		ExcludeSubstrings: []string{"/admin/"},
		IncludeRegex:      []string{`/(en|fr)$`},
		ExcludeRegex:      []string{`\.json$`},
	}
	if err := f.Compile(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		url string
		ok  bool
	}{
		{"https://x.test/en/", true},
		{"https://x.test/fr/about", true},
		{"https://x.test/en", true},        // by regex
		{"https://x.test/admin/en", false}, // exclude wins
		{"https://x.test/page.json", false},
		{"https://x.test/page.html", false}, // doesn't match any include
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			if got := f.Allow(tc.url); got != tc.ok {
				t.Errorf("Allow(%q) = %v, want %v", tc.url, got, tc.ok)
			}
		})
	}
}

func TestURLFilter_NoIncludesAllowsAll(t *testing.T) {
	t.Parallel()
	f := URLFilter{ExcludeSubstrings: []string{"/admin/"}}
	if err := f.Compile(); err != nil {
		t.Fatal(err)
	}
	if !f.Allow("https://x.test/anything") {
		t.Error("with no includes, every non-excluded URL must pass")
	}
	if f.Allow("https://x.test/admin/x") {
		t.Error("excluded URL must not pass")
	}
}

func TestLoad_ExpandsEnv(t *testing.T) {
	t.Setenv("GOWARM_TEST_TOKEN", "secret-123")
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	contents := `
sitemap_url: "https://x.test/${GOWARM_TEST_TOKEN}/s.xml"
global_headers:
  Authorization: "Bearer ${GOWARM_TEST_TOKEN}"
axes:
  - name: region
    cookie_name: region
    values: [qc]
`
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.SitemapURL != "https://x.test/secret-123/s.xml" {
		t.Errorf("sitemap_url: %q", c.SitemapURL)
	}
	if c.GlobalHeaders["Authorization"] != "Bearer secret-123" {
		t.Errorf("auth: %q", c.GlobalHeaders["Authorization"])
	}
}

func TestValidate_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	base := func() *Config {
		c := Default()
		c.SitemapURL = "https://x.test/s.xml"
		c.Axes = []Axis{
			{Name: "region", CookieName: "region", Values: []AxisValue{{Value: "qc"}}},
		}
		return c
	}

	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{"baseline", func(c *Config) {}, false},
		{"empty url", func(c *Config) { c.SitemapURL = "" }, true},
		{"zero workers", func(c *Config) { c.WorkerCount = 0 }, true},
		{"bad method", func(c *Config) { c.Method = "PATCH" }, true},
		{"zero timeout", func(c *Config) { c.HTTP.Timeout = 0 }, true},
		{"no axes", func(c *Config) { c.Axes = nil }, true},
		{"axis without name", func(c *Config) { c.Axes[0].Name = "" }, true},
		{"axis without target", func(c *Config) { c.Axes[0].CookieName = ""; c.Axes[0].HeaderName = "" }, true},
		{"axis with bad transform", func(c *Config) { c.Axes[0].Transform = "weird" }, true},
		{"axis without values", func(c *Config) { c.Axes[0].Values = nil }, true},
		{"axis value empty", func(c *Config) { c.Axes[0].Values = []AxisValue{{Value: ""}} }, true},
		{"axis value bad regex", func(c *Config) {
			c.Axes[0].Values = []AxisValue{{Value: "qc", WhenURLMatches: []string{"["}}}
		}, true},
		{"url filter bad regex", func(c *Config) { c.URLFilter.IncludeRegex = []string{"["} }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c)
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestValidate_NormalisesMethod(t *testing.T) {
	t.Parallel()
	c := Default()
	c.SitemapURL = "https://x.test/s.xml"
	c.Method = "get"
	c.Axes = []Axis{
		{Name: "r", CookieName: "r", Values: []AxisValue{{Value: "1"}}},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Method != "GET" {
		t.Errorf("method not uppercased: %q", c.Method)
	}
}

func TestDefault_HTTPSetting(t *testing.T) {
	t.Parallel()
	c := Default()
	if c.HTTP.Timeout != 20*time.Second {
		t.Errorf("default timeout: %s", c.HTTP.Timeout)
	}
	if c.WorkerCount != 50 {
		t.Errorf("default workers: %d", c.WorkerCount)
	}
	// Default has no axes intentionally.
	if len(c.Axes) != 0 {
		t.Errorf("default should not preconfigure axes; got %v", c.Axes)
	}
}
