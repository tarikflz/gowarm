// Package httpclient is gowarm's wrapper around net/http. It builds
// requests with merged headers + cookies, classifies HTTP status codes for the
// retry layer, and exposes a small, testable surface.
//
// The package is named httpclient (not http) so callers can still import the
// stdlib "net/http" alongside it without aliasing.
package httpclient

import (
	"context"
	"fmt"
	"io"
	stdhttp "net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tarikflz/gowarm/internal/config"
	"github.com/tarikflz/gowarm/internal/retry"
)

// CacheWarmer is the contract used by the worker pool.
type CacheWarmer interface {
	Warm(ctx context.Context, target string, headers stdhttp.Header, cookies []*stdhttp.Cookie) (*WarmResult, error)
}

// WarmResult is structured information about a single warmed URL.
//
// CacheState is gowarm's normalised classification (hit / miss / bypass /
// unknown). CacheStateRaw is the original header value that drove the
// classification ("HIT", "EXPIRED" …) — kept for debugging and reporting so
// operators can still see provider-specific nuance.
type WarmResult struct {
	URL           string
	Status        int
	Bytes         int64
	Duration      time.Duration
	CacheState    config.CacheState
	CacheStateRaw string
	CacheHeader   string
}

// Client wraps stdhttp.Client with merging logic.
type Client struct {
	httpClient    *stdhttp.Client
	method        string
	userAgent     string
	defaultHdrs   stdhttp.Header
	defaultCookie []*stdhttp.Cookie
	throttle      *throttle
	cacheCfg      *config.CacheDetectionConfig
}

// Options configures a new Client.
type Options struct {
	Timeout             time.Duration
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	DisableCompression  bool
	Method              string
	UserAgent           string
	DefaultHeaders      stdhttp.Header
	DefaultCookies      []*stdhttp.Cookie

	// Rate-limiting / per-host concurrency.
	RateLimitRPS             float64
	Burst                    int
	MaxConnsPerHost          int
	AdaptiveSlowdown         bool
	AdaptiveSlowdownFactor   float64
	AdaptiveSlowdownDuration time.Duration

	// CacheDetection drives WarmResult.CacheState. When nil, a default rule
	// set covering CF-Cache-Status, X-Cache, X-Drupal-*, and Age is used.
	CacheDetection *config.CacheDetectionConfig
}

// New constructs a Client with a tuned transport.
func New(opts Options) *Client {
	tr := &stdhttp.Transport{
		MaxIdleConns:        opts.MaxIdleConns,
		MaxIdleConnsPerHost: opts.MaxIdleConnsPerHost,
		IdleConnTimeout:     opts.IdleConnTimeout,
		DisableCompression:  opts.DisableCompression,
		ForceAttemptHTTP2:   true,
	}
	if opts.MaxIdleConns == 0 {
		tr.MaxIdleConns = 100
	}
	if opts.MaxIdleConnsPerHost == 0 {
		tr.MaxIdleConnsPerHost = 100
	}
	if opts.IdleConnTimeout == 0 {
		tr.IdleConnTimeout = 90 * time.Second
	}
	method := strings.ToUpper(opts.Method)
	if method == "" {
		method = stdhttp.MethodGet
	}
	return &Client{
		httpClient: &stdhttp.Client{
			Transport: tr,
			Timeout:   opts.Timeout,
		},
		method:        method,
		userAgent:     opts.UserAgent,
		defaultHdrs:   opts.DefaultHeaders.Clone(),
		defaultCookie: opts.DefaultCookies,
		throttle:      newThrottle(opts),
		cacheCfg:      opts.CacheDetection,
	}
}

// Get is a convenience wrapper used during sitemap fetch in main. It returns
// the response body verbatim; the caller must close it.
func (c *Client) Get(ctx context.Context, target string, headers stdhttp.Header, cookies []*stdhttp.Cookie) (*stdhttp.Response, error) {
	req, err := c.buildRequest(ctx, stdhttp.MethodGet, target, headers, cookies)
	if err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

// Warm performs the configured method (GET or HEAD) against target with merged
// headers/cookies and consumes the body so the connection can be reused.
//
// Throttling: the global rate limiter and per-host semaphore are consulted
// before the request is dispatched. If the context fires while waiting for a
// token or a slot, Warm returns the context error directly (no retry).
//
// Adaptive slowdown: 429 / 503 responses notify the throttle so subsequent
// requests are paced more conservatively for AdaptiveSlowdownDuration.
func (c *Client) Warm(ctx context.Context, target string, headers stdhttp.Header, cookies []*stdhttp.Cookie) (*WarmResult, error) {
	req, err := c.buildRequest(ctx, c.method, target, headers, cookies)
	if err != nil {
		return nil, retry.PermanentError(err)
	}

	if c.throttle != nil {
		release, terr := c.throttle.acquire(ctx, req.URL.Host)
		if terr != nil {
			return nil, terr
		}
		defer release()
	}

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err // transient: retry
	}
	defer resp.Body.Close()

	// Drain the body so the connection can be reused. We don't need to keep it.
	n, _ := io.Copy(io.Discard, resp.Body)

	state, header, raw := c.classify(resp.Header)
	res := &WarmResult{
		URL:           target,
		Status:        resp.StatusCode,
		Bytes:         n,
		Duration:      time.Since(start),
		CacheState:    state,
		CacheStateRaw: raw,
		CacheHeader:   header,
	}

	if c.throttle != nil {
		c.throttle.observe(resp.StatusCode)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 400:
		return res, nil
	case resp.StatusCode == stdhttp.StatusTooManyRequests, resp.StatusCode >= 500:
		return res, fmt.Errorf("transient status %d for %s", resp.StatusCode, target)
	default:
		return res, retry.PermanentError(fmt.Errorf("permanent status %d for %s", resp.StatusCode, target))
	}
}

// classify resolves the configured cache-detection rules (or the built-in
// defaults) against the response headers.
func (c *Client) classify(h stdhttp.Header) (config.CacheState, string, string) {
	cfg := c.cacheCfg
	if cfg == nil {
		cfg = &config.CacheDetectionConfig{}
	}
	state, hdr, raw := cfg.Classify(func(name string) string { return h.Get(name) })
	return state, hdr, raw
}

func (c *Client) buildRequest(ctx context.Context, method, target string, headers stdhttp.Header, cookies []*stdhttp.Cookie) (*stdhttp.Request, error) {
	if _, err := url.Parse(target); err != nil {
		return nil, fmt.Errorf("invalid url %q: %w", target, err)
	}
	req, err := stdhttp.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}

	// Default headers (lowest priority) → request headers (highest).
	for k, vs := range c.defaultHdrs {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	for k, vs := range headers {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if c.userAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if req.Header.Get("Cache-Control") == "" {
		req.Header.Set("Cache-Control", "no-cache")
	}

	// Merge cookies: defaults first, request overrides by name.
	merged := mergeCookies(c.defaultCookie, cookies)
	for _, ck := range merged {
		req.AddCookie(ck)
	}

	return req, nil
}

func mergeCookies(defaults, overrides []*stdhttp.Cookie) []*stdhttp.Cookie {
	byName := make(map[string]*stdhttp.Cookie, len(defaults)+len(overrides))
	for _, c := range defaults {
		if c == nil {
			continue
		}
		byName[c.Name] = c
	}
	for _, c := range overrides {
		if c == nil {
			continue
		}
		byName[c.Name] = c
	}
	out := make([]*stdhttp.Cookie, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	return out
}
