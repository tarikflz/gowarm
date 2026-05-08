package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tarikflz/gowarm/internal/config"
	"github.com/tarikflz/gowarm/internal/retry"
)

func TestWarm_MergesHeadersAndCookies(t *testing.T) {
	t.Parallel()

	var (
		gotMethod  string
		gotHeaders http.Header
		gotCookies []*http.Cookie
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotHeaders = r.Header.Clone()
		gotCookies = r.Cookies()
		w.Header().Set("cf-cache-status", "HIT")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cli := New(Options{
		Timeout:        2 * time.Second,
		Method:         http.MethodGet,
		UserAgent:      "test-ua",
		DefaultHeaders: http.Header{"X-Default": []string{"yes"}},
		DefaultCookies: []*http.Cookie{{Name: "session", Value: "abc"}},
	})

	res, err := cli.Warm(context.Background(), srv.URL,
		http.Header{"X-Per-Req": []string{"1"}},
		[]*http.Cookie{{Name: "region", Value: "QC"}},
	)
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if res.Status != 200 {
		t.Errorf("status: got %d want 200", res.Status)
	}
	if res.CacheState != config.CacheStateHit {
		t.Errorf("cache state: got %q want %q", res.CacheState, config.CacheStateHit)
	}
	if res.CacheStateRaw != "HIT" {
		t.Errorf("cache state raw: got %q want HIT", res.CacheStateRaw)
	}
	if !strings.EqualFold(res.CacheHeader, "CF-Cache-Status") {
		t.Errorf("cache header: got %q want CF-Cache-Status", res.CacheHeader)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method: got %s want GET", gotMethod)
	}
	if gotHeaders.Get("X-Default") != "yes" {
		t.Errorf("default header missing: %v", gotHeaders)
	}
	if gotHeaders.Get("X-Per-Req") != "1" {
		t.Errorf("per-req header missing")
	}
	if gotHeaders.Get("User-Agent") != "test-ua" {
		t.Errorf("user-agent: got %q", gotHeaders.Get("User-Agent"))
	}
	if gotHeaders.Get("Cache-Control") != "no-cache" {
		t.Errorf("cache-control not set: %q", gotHeaders.Get("Cache-Control"))
	}

	names := cookieNames(gotCookies)
	want := []string{"region", "session"}
	if !equalSlices(names, want) {
		t.Errorf("cookies: got %v want %v", names, want)
	}
}

func TestWarm_PermanentOn4xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	cli := New(Options{Timeout: 1 * time.Second, Method: http.MethodGet})
	res, err := cli.Warm(context.Background(), srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !retry.IsPermanent(err) {
		t.Fatalf("expected permanent error, got %v", err)
	}
	if res == nil || res.Status != 403 {
		t.Errorf("expected 403 result, got %+v", res)
	}
}

func TestWarm_TransientOn5xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	cli := New(Options{Timeout: 1 * time.Second, Method: http.MethodGet})
	_, err := cli.Warm(context.Background(), srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if retry.IsPermanent(err) {
		t.Fatalf("5xx must be transient, got permanent: %v", err)
	}
}

func TestWarm_RequestOverridesDefaultHeader(t *testing.T) {
	t.Parallel()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Override")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cli := New(Options{
		Timeout:        time.Second,
		Method:         http.MethodGet,
		DefaultHeaders: http.Header{"X-Override": []string{"default"}},
	})
	if _, err := cli.Warm(context.Background(), srv.URL,
		http.Header{"X-Override": []string{"per-request"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got != "per-request" {
		t.Errorf("override failed; got %q", got)
	}
}

func cookieNames(cs []*http.Cookie) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}
