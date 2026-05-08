# Architecture

`gowarm` is a horizontally-streaming, vertically-bounded cache warmer:
horizontally, it streams URLs from the sitemap so memory stays flat regardless
of catalogue size; vertically, it caps concurrency with a fixed-size goroutine
pool so the origin is never overwhelmed.

The core abstraction is the **axis**: a generic dimension along which the
warming cartesian is expanded. The program has no built-in concept of region,
language, currency, or locale — those are just user-declared axes.

```
                ┌──────────────────────────┐
                │        cmd/main.go        │
                │  (config + orchestration) │
                └────────────┬──────────────┘
                             │ Config (axes, url_filter, http, retry…)
       ┌─────────────────────┴──────────────────────┐
       │                                            │
┌──────▼──────┐                              ┌──────▼──────┐
│  parser     │  ──URLs (channel)──▶  filter │  axes       │
│  (xml.Token │                              │  cartesian  │
│   stream)   │                              │  generator  │
└─────────────┘                              └──────┬──────┘
                                                    │ Job{URL, Cookies, Headers, Tags}
                                             ┌──────▼──────┐
                                             │  worker.    │
                                             │  Pool       │
                                             │  (N goroutines)
                                             └──────┬──────┘
                                                    │
                                         ┌──────────▼─────────┐
                                         │  retry.Retrier     │
                                         │  (exp. backoff +   │
                                         │   jitter, ctx-aware)
                                         └──────────┬─────────┘
                                                    │
                                         ┌──────────▼─────────┐
                                         │  httpclient.Client │
                                         │  (cookie + header  │
                                         │   merge, transport)│
                                         └────────────────────┘
```

## 1. Sitemap Parser (`internal/parser`)

* Uses `encoding/xml.NewDecoder` and consumes one token at a time.
* Emits results over a buffered channel (`make(chan string, 1024)`).
* Memory footprint is constant: only the current token plus the channel buffer.
* Handles both `<urlset>` and `<sitemapindex>` documents. The caller (in
  `cmd/main.go`) peeks the first 256 bytes of the body to decide which list to
  populate, then re-feeds those bytes into the parser via a `prefixReader`.
* Context-aware: cancelling `ctx` immediately stops parsing and closes the
  channel. The downstream `range` loop terminates cleanly.

## 2. Retry (`internal/retry`)

* `ExponentialBackoff` implements `Retrier.Execute`.
* Delay = `BaseDelay × Factor^attempt`, capped at `MaxDelay`, plus uniform
  jitter ∈ `[0, Jitter)`.
* Sleep is implemented with `time.Timer` + `select` on `ctx.Done()` — never
  blocks past cancellation.
* `PermanentError` short-circuits retries. The HTTP client wraps `4xx`
  responses with it so we don't hammer 403/404 endpoints.

## 3. HTTP Client (`internal/http`, package `httpclient`)

* Wraps `net/http.Client` with a tuned `Transport` (max idle conns, idle
  timeout, HTTP/2 attempt).
* `Warm()` builds a request, applies a header / cookie merge, dispatches it,
  and consumes the body so the connection can be reused.
* Header / cookie merge: defaults (lowest priority) → per-request overrides
  (highest priority). Merge happens by header name.
* Status code classification:
  | Status              | Outcome                              |
  | ------------------- | ------------------------------------ |
  | 2xx, 3xx            | success                              |
  | 429, 5xx            | transient → retry                    |
  | other 4xx           | permanent → wrapped in `*Permanent`  |
* Cache-state extraction: reads `cf-cache-status`, `x-drupal-dynamic-cache`,
  or `x-cache` and exposes a normalised `CacheState` string for stats.

## 4. Worker Pool (`internal/worker`)

* Fixed-size pool of N goroutines reading from a buffered job channel
  (`size × 4` capacity).
* `Submit()` blocks if the channel is full so the producer naturally throttles
  to consumer capacity. It also bails immediately on `ctx.Done()`.
* `Stop()` closes the job channel via `sync.Once`. Workers drain remaining
  jobs and exit; their `for j := range p.jobs` loop terminates naturally.
* `Wait(ctx)` blocks until every worker exits or the supplied context fires.
* Per-job concurrency is wrapped in `Retrier.Execute`. The retrier owns the
  retry loop; the pool owns lifecycle.
* Live counters (`Stats`) are atomic; a non-atomic `Snapshot` is used for
  logging. Cache hits are detected from the `WarmResult.CacheState`.
* Each `Job` carries a free-form `Tags map[string]string` (axis name → value)
  that is folded into the slog output, so log lines look like
  `region=QC language=en` and are grep-friendly.

## 5. Configuration (`internal/config`)

* Loads YAML into a typed `Config` struct via `gopkg.in/yaml.v3`.
* After unmarshalling, `expandEnvInStruct` walks the value and applies
  `os.ExpandEnv` to every string and `[]string` recursively. This makes
  `${VAR}` work in any string field.
* `Validate()` normalises the input and enforces invariants:
  - non-empty `sitemap_url`,
  - positive `worker_count` and `http.timeout`,
  - method is GET or HEAD,
  - at least one axis,
  - each axis has a name, at least one of `cookie_name`/`header_name`, a
    valid `transform`, and at least one non-empty value,
  - each value's `when_url_matches` regex compiles,
  - each `url_filter` regex compiles.

### 5.1 The Axis model (the core data type)

```go
type Axis struct {
    Name       string      // log + override label
    CookieName string      // optional; sent as Cookie
    HeaderName string      // optional; sent as a header
    Transform  string      // upper | lower | none
    Values     []AxisValue
}

type AxisValue struct {
    Value           string
    WhenURLContains []string
    WhenURLMatches  []string  // pre-compiled regexes
}
```

The YAML supports two equivalent forms for `Values`:

```yaml
values: [on, mb, qc, ab]                # shorthand
values:
  - value: en
    when_url_contains: ["/en/"]         # full form
```

The shorthand is implemented via `AxisValue.UnmarshalYAML`, which accepts
either a scalar (treated as `Value`) or a mapping.

### 5.2 URL filter

```go
type URLFilter struct {
    IncludeSubstrings []string
    ExcludeSubstrings []string
    IncludeRegex      []string
    ExcludeRegex      []string
}
```

Order of precedence:
1. If a URL matches any exclude (substring or regex), it's rejected.
2. If no includes are declared, every non-excluded URL passes.
3. Otherwise the URL must match at least one include.

## 6. Main entry-point (`cmd/main.go`)

The `run()` function wires everything together. Order of operations:

1. Parse CLI flags. They take priority over `config.yaml`.
2. `config.Load(cfgPath)` — read YAML, expand `${VAR}`, validate.
3. Apply `-axis name=v1,v2` overrides. Smart merge: if a value name matches
   an existing config value, its per-value URL filters are preserved.
4. Build root context (with optional `-timeout`).
5. Register `SIGINT/SIGTERM` handler that calls `cancel()`.
6. Build HTTP client with merged defaults.
7. Fetch + parse sitemap (recursing into nested indexes if `follow_index`).
   URLs are gated by `URLFilter.Allow`.
8. Start worker pool, spawn progress reporter.
9. **Submit jobs** — see §6.1.
10. `Stop()` the pool to signal "no more jobs", then `Wait()`.
11. Log summary stats. Treat `context.Canceled` and
    `context.DeadlineExceeded` as graceful termination (exit 0).

### 6.1 Cartesian product (the heart of the orchestration)

Pseudocode:

```go
for u := range urls {
    applicable := per-axis values that apply to u
    if any axis has 0 applicable values, skip u
    forEachCombo(applicable, func(combo) {
        cookies, headers, tags := buildJobInputs(axes, combo)
        pool.Submit(ctx, Job{URL: u, Cookies: cookies, Headers: headers, Tags: tags})
    })
}
```

`forEachCombo` is a recursive cartesian generator that **streams** combos
into the pool — we never materialise the full list, so memory stays O(axes)
regardless of how big the cartesian gets.

`buildJobInputs` walks each axis, applies its `Transform` to the chosen
value, and projects it onto:

* a `*http.Cookie` (if `cookie_name` is set),
* an HTTP header (if `header_name` is set),
* a tag in the `Tags` map (always, for log context).

An axis with **both** `cookie_name` and `header_name` produces both a cookie
and a header carrying the same value.

### 6.2 Why a per-value URL filter?

Some axes are conceptually tied to URL structure. A `lang` cookie, for
example, must equal `en` for `/en/...` URLs and `fr` for `/fr/...` URLs — sending the
wrong combination is wasted (and may even be cached as a redirect to the
correct path).

Rather than special-case "language" in the program, we let the axis value
declare its own URL binding via `when_url_contains` / `when_url_matches`.
The cartesian generator then automatically excludes incompatible
combinations, so we never queue a `lang=fr` job for an `/en/...` URL.

If a URL has zero applicable values for *any* axis, the URL is skipped
entirely (and counted in `skipped_urls`).

## 7. Lifecycle & Shutdown

```
SIGINT/SIGTERM   ──►   cancel()
                          │
                          ├─► parser.Parse  → channel closes
                          ├─► retry sleeps  → return ctx.Err()
                          ├─► http requests → return ctx.Err()
                          └─► worker.Submit → noop on ctx.Done()

                       pool.Stop()  → close(jobs)
                       pool.Wait()  → wg.Wait()
                       summary log  → exit 0
```

Every blocking primitive (sleep, channel send, HTTP request) is
`select`-driven against `ctx.Done()`. No goroutines should leak.

## 8. Performance characteristics

| Metric                       | Order of magnitude (50 workers)              |
| ---------------------------- | ---------------------------------------- |
| Sitemap parse                | < 500 ms for 912 URLs                    |
| Throughput (cold)            | ~80–100 req/s                            |
| Throughput (warm)            | ~300+ req/s                              |
| Latency (CDN MISS)           | 500 – 800 ms                             |
| Latency (CDN HIT)            | 30 – 200 ms                              |
| RSS                          | < 30 MB                                  |
| Total time, 912 URL × 4 × 2  | ~2 minutes (cold), ~30 s (warm)          |

The cartesian product is generated lazily via recursion, so even axis
configurations producing millions of combinations stay flat in memory; the
job channel (size `worker_count × 4`) is the only buffer.
