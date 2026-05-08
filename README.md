# gowarm — generic sitemap-driven cache warmer

A small, fast Go service that streams URLs out of a sitemap and warms each
one across every cookie/header combination you declare, populating origin and
CDN caches with one entry per (URL, combination) tuple.

gowarm is **content-agnostic**: the program itself knows nothing about
"region" or "language". You declare one or more `axes` in YAML, each axis
projects its values onto a cookie and/or a header, and gowarm takes the
cartesian product across them.

The default `config.yaml` in this repo is configured for example.com (4 regions ×
2 languages with `Vary: region` caching), but you can adapt it to any site by
replacing the `axes` block.

## What gowarm does

1. Streams `sitemap_url` with `encoding/xml` (constant memory).
2. Optionally filters URLs via `url_filter` (substring + regex include/exclude).
3. For every remaining URL, computes the cartesian product of axis values,
   per-value URL-binding rules permitting.
4. Submits each combination as a job into a fixed-size goroutine pool.
5. Each job is a `GET` (or `HEAD`) with the right cookies and headers set.
6. Failed responses are retried with jittered exponential backoff (transient)
   or short-circuited (permanent 4xx).
7. SIGINT/SIGTERM cancels gracefully; `-timeout` enforces a hard deadline.

## Quick start

```bash
make build               # produces ./bin/gowarm
./bin/gowarm -dry-run    # list every (URL, combo) it would warm
./bin/gowarm             # warm everything
```

A successful run looks like:

```
INFO  gowarm starting    sitemap=https://example.com/sitemap.xml axes=region(4),language(2) workers=50 method=GET
INFO  sitemap parsed    url_count=912
INFO  progress          completed=1758 success=1758 failed=0 cached=1633
…
INFO  warming complete  total=3648 success=3636 failed=12 cache_hits=2718 bytes=901293012 avg_latency=224ms
```

Hot re-runs report ~75% cache_hits because the previous run already populated
the CDN.

## The axis model

Each axis is one dimension of the warming cartesian. An axis declares:

| Field          | Required? | Meaning                                                                  |
| -------------- | --------- | ------------------------------------------------------------------------ |
| `name`         | yes       | human label (used in logs, dry-run, CLI overrides)                       |
| `cookie_name`  | one of    | when set, value is sent as `Cookie: <cookie_name>=<value>`               |
| `header_name`  | one of    | when set, value is sent as a header                                      |
| `transform`    | no        | `upper`, `lower`, or `none` (default) — applied to each value            |
| `values`       | yes       | list of values (shorthand strings or full objects with URL filters)      |

**Both `cookie_name` and `header_name` may be set on the same axis** — the
same (transformed) value is then projected onto both a cookie and a header.

### Value shorthand vs full form

```yaml
# shorthand: just strings
values: [on, mb, qc, ab]

# full form: objects with optional URL bindings
values:
  - value: en
    when_url_contains: ["/en/"]    # substring match
    when_url_matches: ["/en$"]      # regex match
  - value: fr
    when_url_matches: ["/fr(/|$)"]
```

A value with no URL filters applies to every URL. A value with filters only
applies to URLs that satisfy at least one of them.

If a URL has zero applicable values for any axis, that URL is skipped (gowarm
can't construct a complete combination). The skip count is reported
on the `all jobs queued` log line.

### Examples

#### Region cookie + language cookie tied to URL path

```yaml
axes:
  - name: region
    cookie_name: region
    transform: upper           # server expects ON/MB/QC/AB
    values: [on, mb, qc, ab]

  - name: language
    cookie_name: lang
    values:
      - value: en
        when_url_matches: ["/en(/|$)"]
      - value: fr
        when_url_matches: ["/fr(/|$)"]
```

#### Currency cookie × Accept-Language header

```yaml
axes:
  - name: currency
    cookie_name: currency
    values: [usd, eur, gbp]

  - name: locale
    header_name: Accept-Language
    values: [en-US, fr-FR, de-DE]
```

Generates 3×3 = 9 requests per URL. Cookie + header work side by side.

#### Same value applied to both a cookie and a header

```yaml
axes:
  - name: region
    cookie_name: region
    header_name: X-Region        # same value, sent both ways
    transform: upper
    values: [on, qc, mb, ab]
```

## URL filter

Optional. All four lists are independently usable; excludes always win.

```yaml
url_filter:
  include_substrings: ["/en/", "/fr/"]
  exclude_substrings: ["/admin/", "/preview/"]
  include_regex: ["/(en|fr)$"]
  exclude_regex: ["\\.(json|xml)$"]
```

If no includes are configured, every non-excluded URL passes.

## CLI

| Flag                              | Effect                                                                |
| --------------------------------- | --------------------------------------------------------------------- |
| `-config <path>`                  | YAML config file (default `config.yaml`)                              |
| `-sitemap <url>`                  | override `sitemap_url`                                                |
| `-workers <n>`                    | override `worker_count`                                               |
| `-method GET\|HEAD`               | override `method`                                                     |
| `-timeout 5m`                     | hard deadline                                                         |
| `-dry-run`                        | print TSV `(URL, axis=value, …)` and exit                             |
| `-force`                          | bypass `limits.max_jobs` / `limits.max_combinations_per_url` guards   |
| `-axis name=v1,v2`                | override an axis's values; **repeatable**                             |
| `-log-level debug\|info\|warn\|…` | log level                                                             |
| `-log-file <path>`                | tee slog text output to this file (in addition to stdout)             |
| `-per-job-file <path>`            | write one JSONL record per warm job                                   |
| `-summary-file <path>`            | write a JSON summary report at the end of the run                     |

`-axis` is smart: if the override value name matches an existing config
value, the original per-value URL filters are **preserved**. Use it to
restrict a run to a single region/language for testing without editing the
YAML:

```bash
./bin/gowarm -axis region=qc -axis language=en -workers 5 -log-level debug
```

## Config reference

```yaml
sitemap_url: "https://example.com/sitemap.xml"
worker_count: 50
method: GET                       # GET | HEAD
user_agent: "gowarm/1.0"

http:
  timeout: 20s
  max_idle_conns: 200
  max_idle_conns_per_host: 100
  idle_conn_timeout: 90s

  # Rate limiting + per-host concurrency. 0 = disabled.
  rate_limit_rps: 0               # global token-bucket rate (req/sec)
  burst: 0                        # bucket capacity (defaults to rate_limit_rps)
  max_conns_per_host: 0           # in-flight cap per hostname (semaphore)

  # Adaptive slowdown: on 429/503 the rate is multiplied by
  # adaptive_slowdown_factor for adaptive_slowdown_duration before recovering.
  adaptive_slowdown: false
  adaptive_slowdown_factor: 0.5
  adaptive_slowdown_duration: 30s

retry:
  max_attempts: 3
  base_delay: 200ms
  max_delay: 5s
  jitter: 200ms
  factor: 2.0

url_filter:                       # optional
  include_substrings: []
  exclude_substrings: []
  include_regex: []
  exclude_regex: []

axes:                             # required, ≥ 1
  - name: ...
    cookie_name: ...              # one of cookie_name / header_name (or both)
    header_name: ...
    transform: upper              # upper | lower | none
    values: [...]                 # shorthand or full-form objects

global_headers: {}                # applied to every request
global_cookies: []                # applied to every request

follow_index: true                # recurse into <sitemapindex>
stop_on_error: false              # exit non-zero if any job fails
log_level: info

report:                           # optional artefacts written to disk
  log_file: ""                    # tee slog output to this file
  per_job_file: ""                # JSONL: one object per warm job
  summary_file: ""                # JSON:  aggregate counters + failures

# Cartesian-explosion guards. Bypass with `-force` on the CLI.
limits:
  max_jobs: 0                     # 0 = unlimited
  max_combinations_per_url: 0     # 0 = unlimited

# Sampling: trim the URL list before the cartesian product is generated.
# Percent and first_n are mutually exclusive (percent wins).
sampling:
  percent: 0                      # 0 disables; (0, 100] keeps that share
  first_n: 0                      # 0 disables; > 0 keeps the first N URLs
  random: false                   # false = deterministic stride
  seed: 0                         # used when random=true

# priority_urls: warm hot paths first.
priority_urls:
  include_first: false
  include_substrings: []
  include_regex: []

# Cache verification rules. When `rules` is empty, gowarm uses a built-in
# default that recognises CF-Cache-Status, X-Drupal-(Dynamic-)Cache, X-Cache,
# X-Cache-Status, and Age. See the "Cache verification" section for details.
cache_detection:
  rules: []
```

`${VAR}` placeholders are expanded against the process environment in any
string field, so secrets stay out of the file.

## Reports & logs

All three artefacts are independently opt-in. They can be configured via the
`report:` block in YAML or the matching CLI flags (CLI wins). The CLI flags
make it trivial to wire into a CI cron without editing the config.

### `log_file` — slog text tee

Mirrors the same human-readable `INFO`/`WARN` lines that go to stdout. Useful
when you want to capture a run's full log without piping stdout in your cron
wrapper.

### `per_job_file` — JSON Lines (one object per job)

Streamed during the run; one job, one line. Each record looks like:

```json
{"time":"2026-05-07T01:30:00Z","url":"https://example.com/en/phones",
 "status":200,"bytes":15842,"duration_ms":118,
 "cache_state":"hit","cache_state_raw":"HIT","cache_header":"CF-Cache-Status",
 "tags":{"region":"QC","language":"en"}}
```

`cache_state` is gowarm's normalised classification (`hit` / `miss` /
`bypass` / `unknown`). `cache_state_raw` and `cache_header` keep the
original header value/name so you can still see provider-specific nuance
("EXPIRED", "REVALIDATED" …).

Failed jobs include an `err` string; context-cancelled jobs additionally
carry `"cancelled":true` so you can filter them out post-run:

```bash
jq 'select(.cancelled != true and .err)' jobs.jsonl  # real failures only
jq 'select(.cache_state == "hit") | .url' jobs.jsonl | sort -u
jq 'select(.cache_state_raw == "EXPIRED") | .url' jobs.jsonl
```

### `summary_file` — JSON aggregate report

A single JSON document written once, after the worker pool has drained.
Contains run timestamps, axis summary, normalised cache counters
(`cache_hits`, `cache_misses`, `cache_bypass`, `unknown_cache_state`,
plus the derived `hit_ratio`), the same counters that go on the
`warming complete` log line, and a `failures` array (always present,
possibly empty) with `{url, status, tags, err}` for every job that
ultimately failed (cancellations excluded). Friendly for dashboards and CI
artefact uploads.

## Rate limiting & adaptive slowdown

By default gowarm relies on `worker_count` as its only throttle, which is
fine for a healthy origin. For sensitive environments you can opt into a
token-bucket rate limiter and a per-host concurrency cap:

```yaml
http:
  rate_limit_rps: 50         # global token-bucket: 50 req/sec
  burst: 50                  # bucket capacity (defaults to rate_limit_rps)
  max_conns_per_host: 8      # cap in-flight requests per hostname
  adaptive_slowdown: true
  adaptive_slowdown_factor: 0.5     # halve the rate when 429/503 is observed
  adaptive_slowdown_duration: 30s   # for this long before recovering
```

* `rate_limit_rps` and `burst` are evaluated globally across all workers, so
  `worker_count: 50` + `rate_limit_rps: 10` actually issues ~10 requests/sec
  even though 50 goroutines are running.
* `max_conns_per_host` is a *concurrency* limit (different from
  `max_idle_conns_per_host`, which only tunes the connection pool). It is
  the right knob when you want N workers but don't want to hammer a single
  hostname with all of them.
* `adaptive_slowdown` is a back-pressure circuit-breaker: whenever the
  origin returns a 429 or 503, the limiter's effective rate is multiplied
  by `adaptive_slowdown_factor` (default `0.5`) for
  `adaptive_slowdown_duration` (default `30s`). Subsequent 429/503s extend
  the slowdown; once the origin recovers, the limiter restores the base
  rate automatically.

The `-dry-run` mode does NOT apply the limiter (it never sends real
traffic); only the actual run paces requests.

## Cache verification (provider-agnostic)

gowarm classifies every response into one of four normalised cache states:
`hit`, `miss`, `bypass`, `unknown`. The classification is driven by the
`cache_detection.rules` block, which is a first-match-wins ordered list of
rules. Each rule pairs a header name with a match strategy:

| Strategy         | Behaviour                                            |
| ---------------- | ---------------------------------------------------- |
| `match_equals`   | exact value, case-insensitive                        |
| `match_contains` | substring, case-insensitive                          |
| `match_regex`    | regex against the header value                       |
| *(none of the above)* | header presence is enough (e.g. `Age`)          |

When the block is empty, gowarm falls back to a built-in default that
recognises Cloudflare (`CF-Cache-Status`), Drupal (`X-Drupal-Cache`,
`X-Drupal-Dynamic-Cache`), Fastly/Varnish/nginx (`X-Cache`,
`X-Cache-Status`), and an `Age` header. The legacy `cf-cache-status: HIT`
example in earlier versions of this README still works without any
configuration change.

Custom example for a non-CF, non-Drupal CDN:

```yaml
cache_detection:
  rules:
    - header: X-Foo-Cache
      match_equals: HIT
      state: hit
    - header: X-Foo-Cache
      match_regex: "^MISS-\\d+$"
      state: miss
    - header: X-Foo-Cache
      match_contains: bypass
      state: bypass
    - header: Age              # presence-only rule
      state: hit
```

The summary report and per-job JSONL include both the normalised
`cache_state` and the original `cache_state_raw` + `cache_header`, so you
can build dashboards on the normalised counter set without losing the
provider-specific signal.

## Cartesian explosion guards

Cartesian products grow geometrically with axes × values × URLs. To prevent
"oops" runs, gowarm computes the planned job count up front and refuses to
launch when configured limits are exceeded. Two limits cooperate:

```yaml
limits:
  max_jobs: 50000              # total cap across the run
  max_combinations_per_url: 32 # per-URL cap; over-cap URLs are skipped
```

When `max_jobs` is exceeded, gowarm exits with a descriptive error and
leaves the origin alone. Pass `-force` to override (for one-off bulk runs
where you've made a conscious decision). The dry-run output's trailing
comment line always shows the planned `total_jobs` so you can sanity-check
before launching.

### Sampling

Need to warm only a fraction? Use `sampling`:

```yaml
sampling:
  percent: 10        # keep ~10% of URLs (deterministic stride by default)
  # or
  first_n: 200       # keep the first 200 URLs
```

`percent` and `first_n` are mutually exclusive (percent wins). With the
default `random: false` gowarm uses a deterministic stride (every Nth URL),
so re-runs warm the same set without setting a seed. With `random: true`
sampling becomes RNG-driven; set `seed: <int>` for reproducible CI runs.

### Priority URLs

Warm hot paths first by promoting URLs that match a substring or regex:

```yaml
priority_urls:
  include_first: true
  include_substrings: ["/checkout/", "/cart/"]
  include_regex: ["^https://example\\.com/$"]
```

The promoted URLs preserve their relative order; everything else follows.

## Project layout

```
gowarm/
├── cmd/                         # entry point + axes orchestration
│   ├── main.go
│   └── main_test.go             # cartesian + override unit tests
├── internal/
│   ├── config/                  # YAML config + axes + URL filter
│   ├── http/                    # CacheWarmer (cookie/header merge, transport)
│   ├── parser/                  # streaming XML sitemap parser
│   ├── retry/                   # exponential backoff + jitter
│   └── worker/                  # fixed-size pool + stats
├── docs/ARCHITECTURE.md         # design rationale + lifecycle diagram
├── config.yaml                  # default config (region + language axes)
├── Makefile
└── go.mod
```

`internal/http` declares `package httpclient` so callers can import the stdlib
`net/http` alongside it without aliases.

## Tests

```bash
make test          # plain
make test-race     # with the race detector
```

Coverage:

* parser: urlset, sitemapindex, context cancellation, IsIndex
* retry:  success after retries, give-up, permanent short-circuit, ctx cancel
* http:   header/cookie merge, override semantics, 4xx/5xx classification,
          token-bucket rate limit, per-host concurrency cap, adaptive
          slowdown on 429
* config: defaults, YAML load, env expansion, validation, axis shorthand
          + full form, URL filter rules, transform semantics, cache
          detection rules (defaults + custom regex/equals/contains/presence)
* worker: success, failures × retries, ctx cancellation drops jobs,
          normalised cache counters
* cmd:    cartesian product, applicable values, axis overrides preserve
          filters, plan computation (max_jobs, max_combinations_per_url,
          sampling, priority URLs)

## Verification

After a full run, fetch any warmed URL with the same cookies and look for a
cache-state header that maps to `hit` under `cache_detection`:

```bash
curl -I "https://example.com/en/phones/iphone/16-pro" \
     -H "Cookie: region=QC; lang=en" | grep -i cache
# cf-cache-status: HIT       (default rules → cache_state=hit)
# x-drupal-dynamic-cache: HIT (default rules → cache_state=hit)
# x-cache: HIT from edge      (default rules → cache_state=hit)
```

You can also inspect the JSONL report to verify which combinations actually
warmed:

```bash
# Cold combinations that came back as miss/bypass/unknown
jq 'select(.cache_state != "hit") | {url, tags, cache_state, cache_state_raw}' jobs.jsonl

# Aggregate per-region hit ratio from the per-job log
jq -s 'group_by(.tags.region)
       | map({region: .[0].tags.region,
              hits: ([.[] | select(.cache_state=="hit")] | length),
              total: length})' jobs.jsonl
```

## Operational notes

* The pool's job channel is `worker_count × 4` deep, so the producer naturally
  back-pressures the consumer if the origin slows down.
* `SIGINT` / `SIGTERM` cancels the root context. In-flight requests are
  cancelled (their `context.DeadlineExceeded` errors are not counted as
  failures); queued-but-unstarted jobs are silently dropped.
* `stop_on_error: true` makes the process exit non-zero if any job ultimately
  fails; the default is to exit 0 so transient failures don't break a CI cron.
* The `User-Agent` is descriptive on purpose — operations teams should be able
  to grep their logs and see that gowarm is the source.

## License

[MIT](./LICENSE) — see `LICENSE` for the full text.
