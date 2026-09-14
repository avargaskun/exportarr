# Exportarr

AIO Prometheus Exporter for Sonarr, Radarr, Lidarr, Prowlarr, Bazarr and Sabnzbd

[![Go Report Card](https://goreportcard.com/badge/github.com/onedr0p/exportarr)](https://goreportcard.com/report/github.com/onedr0p/exportarr)

Note: Each app subcommand (`exportarr sonarr`, `exportarr radarr`, …) exports one app instance. To export several instances from one process, use [`exportarr serve`](#serving-several-targets-from-one-process-exportarr-serve). Be sure to see the examples below for more information.

## About this fork

This is [avargaskun/exportarr](https://github.com/avargaskun/exportarr), a fork of [onedr0p/exportarr](https://github.com/onedr0p/exportarr). It ships upstream's v3 rewrite, which upstream merged but has not released, plus security and availability hardening. It stays close to upstream so switching back is cheap once upstream releases v3.

- **Image:** `ghcr.io/avargaskun/exportarr` for `linux/amd64` and `linux/arm64`. It is built only by this repository's CI from reviewed source, and publishing requires tests and lint to pass on the release commit.
- **Tags:** `X.Y.Z` (pin this), `X.Y`, `X` and `latest`, with no `v` prefix. Git tags and GitHub releases are `vX.Y.Z`.
- **Releases:** [release-please](https://github.com/googleapis/release-please) cuts releases from conventional commits on `dev`: `fix:` bumps the patch version, `feat:` the minor. The major version tracks upstream's. The first release is `3.1.0`, because upstream's unreleased v3 would be `3.0.0`.
- **Differences from upstream v3 (`52bb6eb`):**
  - Image: no UPX or catatonit, `ENTRYPOINT ["/exportarr"]`, `scratch` plus the CA bundle, UID `65532`, embedded tzdata, patched Go.
  - The default port is `9707` for both the binary and the image (upstream's code said `8081`).
  - New settings: `SCRAPE_TIMEOUT`, `SERIES_CONCURRENCY` and `PROXY_FROM_ENV` (see [Configuration](#configuration)).
  - Hardening: environment proxies are ignored by default, and URLs with credentials or query strings are rejected. Secrets stay out of logs and redirect errors. Response bodies are capped at 256 MiB, and queue pagination and the Sonarr/Lidarr fan-out are bounded. Concurrent and overlong `/metrics` scrapes are limited, the HTTP server has timeouts, and shell completion is removed.

![image](.github/images/dashboard-2.png)

## Usage

### Docker Compose

See examples in the [examples/compose](./examples/compose/) directory.

### Kubernetes

See examples in the [examples/kubernetes](./examples/kubernetes/) directory.

### Docker CLI

_Replace `$app` and `$port` with one of the supported apps and its port, and put the app's API key in `./api_key`_

<!-- x-release-please-start-version -->

```sh
# PORT must be unique across all Exportarr instances.
# ./api_key must be readable by UID 65532, the container user.
docker run --name exportarr_$app \
  -e PORT=9707 \
  -e URL="http://x.x.x.x:$port" \
  -e API_KEY_FILE=/run/secrets/api_key \
  -v "$PWD/api_key:/run/secrets/api_key:ro" \
  --restart unless-stopped \
  -p 9707:9707 \
  -d ghcr.io/avargaskun/exportarr:3.2.0 $app
```

<!-- x-release-please-end -->

Visit http://127.0.0.1:9707/metrics to see the app metrics

### CLI

_Replace `$app` and `$port` with one of the supported apps and its port, and put the app's API key in `./api_key`_

```sh
./exportarr $app --help

# --port must be unique across all Exportarr instances
API_KEY_FILE=./api_key ./exportarr $app \
  --port 9707 \
  --url "http://x.x.x.x:$port"
```

Visit http://127.0.0.1:9707/metrics to see the app metrics

## Configuration

|        Environment Variable        | CLI Flag                       | Description                                                                                                               | Default              | Required |
| :--------------------------------: | ------------------------------ | ------------------------------------------------------------------------------------------------------------------------- | -------------------- | :------: |
|               `PORT`               | `--port` or `-p`               | The port Exportarr will listen on                                                                                         | `9707`               |    ❌    |
|               `URL`                | `--url` or `-u`                | The full URL to the app being exported, without credentials or a query string (e.g. `http://sonarr:8989`)                 |                      |    ✅    |
|             `API_KEY`              | `--api-key` or `-a`            | API Key for the app being exported (prefer `API_KEY_FILE`; the flag is visible in the process list)                       |                      |    ✅    |
|           `API_KEY_FILE`           | —                              | Path to a file containing the API key (Docker/Kubernetes secrets); overrides `API_KEY`                                    |                      |    ❌    |
|            `INTERFACE`             | `--interface` or `-i`          | IP address to listen on; set it (e.g. `127.0.0.1`, a pod IP or `::1`) to avoid listening on every interface               | `0.0.0.0`            |    ❌    |
|            `LOG_LEVEL`             | `--log-level` or `-l`          | Log level (`debug`, `info`, `warn`, `error`)                                                                              | `info`               |    ❌    |
|            `LOG_FORMAT`            | `--log-format`                 | Log format (`console`, `json`)                                                                                            | `console`            |    ❌    |
|        `DISABLE_SSL_VERIFY`        | `--disable-ssl-verify`         | Set to `true` to disable SSL verification                                                                                 | `false`              |    ❌    |
|          `PROXY_FROM_ENV`          | `--proxy-from-env`             | Send requests to the app through `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`; off by default because the proxy sees the API key | `false`              |    ❌    |
|         `REQUEST_TIMEOUT`          | `--request-timeout`            | HTTP timeout per request to the target app                                                                                | `60s`                |    ❌    |
|          `SCRAPE_TIMEOUT`          | `--scrape-timeout`             | Time budget for one scrape (`/metrics` answers 503 past it); keep it at or below Prometheus's `scrape_timeout`            | `2m`                 |    ❌    |
|          `AUTH_PASSWORD`           | `--auth-password`              | Password for form auth (prefer the environment variable; the flag is visible in the process list)                         |                      |    ❌    |
|          `AUTH_USERNAME`           | `--auth-username`              | Username for form auth                                                                                                    |                      |    ❌    |
|            `FORM_AUTH`             | `--form-auth`                  | Use form-based authentication                                                                                             | `false`              |    ❌    |
|    `ENABLE_UNKNOWN_QUEUE_ITEMS`    | `--enable-unknown-queue-items` | Set to `true` to enable gathering unknown queue items                                                                     | `false`              |    ❌    |
|     `DISABLE_QUALITY_METRICS`      | `--disable-quality-metrics`    | Skip per-item quality breakdowns (episodefile/trackfile lookups; ~1 API call per series/artist each scrape)               | `false`              |    ❌    |
|     `DISABLE_EPISODE_METRICS`      | `--disable-episode-metrics`    | Skip per-episode metrics (sonarr episode monitoring, bazarr episode-subtitle walk; load scales with library size)         | `false`              |    ❌    |
|      `DISABLE_ALBUM_METRICS`       | `--disable-album-metrics`      | Skip per-album metrics (lidarr album lookups; ~1 API call per artist each scrape)                                         | `false`              |    ❌    |
|     `DISABLE_HISTORY_METRICS`      | `--disable-history-metrics`    | Skip the history endpoint — its total forces a full count over the unprunable history table, slow on multi-year instances | `false`              |    ❌    |
|      `DISABLE_WANTED_METRICS`      | `--disable-wanted-metrics`     | Skip the wanted/missing and wanted/cutoff endpoints — their totals force full counts, slow on very large libraries        | `false`              |    ❌    |
|        `SERIES_CONCURRENCY`        | `--series-concurrency`         | Concurrent per-series (Sonarr) / per-artist (Lidarr) API calls, `1`–`32`; each can hold a database connection on the app  | `10`                 |    ❌    |
|        `PROWLARR__BACKFILL`        | `--backfill`                   | Set to `true` to enable backfill of historical metrics                                                                    | `false`              |    ❌    |
|  `PROWLARR__BACKFILL_SINCE_DATE`   | `--backfill-since-date`        | Set a date (`YYYY-MM-DD`) from which to start the backfill                                                                | `1970-01-01` (epoch) |    ❌    |
|    `BAZARR__SERIES_BATCH_SIZE`     | `--series-batch-size`          | Number of series per Bazarr episodes API call                                                                             | `300`                |    ❌    |
| `BAZARR__SERIES_BATCH_CONCURRENCY` | `--series-batch-concurrency`   | Concurrent Bazarr episodes API calls                                                                                      | `10`                 |    ❌    |

### Secrets

- Prefer `API_KEY_FILE` pointing at a read-only mounted secret (Docker or Kubernetes secrets) over an inline `API_KEY`.
- `API_KEY`, `AUTH_USERNAME` and `AUTH_PASSWORD` are removed from the exporter's own environment after they are read, so child processes and `os.Environ` never see them. They remain visible in `/proc/<pid>/environ` and `docker inspect`, so this is not a substitute for a mounted secret.
- Avoid `--api-key` and `--auth-password`: command-line arguments are readable by every local user through the process list. exportarr logs a warning when they are used.

### Prowlarr Backfill

The Prowlarr collector is a little different than other collectors as it's hitting an actual "stats" endpoint, collecting counters of events that happened in a small time window, rather than getting all-time statistics like the other collectors. This means that by default, when you start the Prowlarr collector, collected stats will start from that moment (all counters will start from zero).

To backfill all Prowlarr Data, either use `PROWLARR__BACKFILL` or `--backfill`.

Note that the first request can be extremely slow, depending on how long your Prowlarr instance has been running. You can also specify a start date to limit the backfill if the backfill is timing out:

`PROWLARR__BACKFILL_SINCE_DATE=2023-03-01` or `--backfill-since-date=2023-03-01`

## Serving several targets from one process (`exportarr serve`)

`exportarr serve` runs one process on one port that exports several **named targets**: any of the six app types, including several instances of the same type. Each target is still its own Prometheus scrape target at `/metrics/<name>`, with its own `up`, scrape duration, timeout and `instance`. A slow, failing or panicking target doesn't affect the others. Use it instead of one exporter container per app instance. The single-target subcommands are unchanged. The design and the alternatives considered are in [docs/design/multi-target.md](./docs/design/multi-target.md).

### Process-wide settings

`serve` is configured through the environment. It has no flags of its own and accepts only the global flags (`--port`, `--interface`, `--log-level`, `--log-format`, `--scrape-timeout`, `--request-timeout`, `--disable-ssl-verify`, `--proxy-from-env`); the app flags such as `--series-concurrency` don't exist on `serve`.

These settings from [Configuration](#configuration) keep their meaning. The per-app ones become the default for every target of that app:

- `PORT`, `INTERFACE`, `LOG_LEVEL`, `LOG_FORMAT`
- `SCRAPE_TIMEOUT`, `REQUEST_TIMEOUT`, `DISABLE_SSL_VERIFY`, `PROXY_FROM_ENV`
- `SERIES_CONCURRENCY`, `ENABLE_UNKNOWN_QUEUE_ITEMS`, the `DISABLE_*_METRICS` settings, `PROWLARR__*` and `BAZARR__*`

One setting is new and only used by `serve`:

| Environment Variable    | Description                                                                                                               | Default |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------- | ------- |
| `MAX_UPSTREAM_REQUESTS` | Process-wide limit on concurrent HTTP requests from all targets to their apps; must be at least the number of targets + 1 | `64`    |

`URL`, `API_KEY`, `API_KEY_FILE`, `FORM_AUTH`, `AUTH_USERNAME`, `AUTH_PASSWORD`, `--url` and `--api-key` describe a single target, so `serve` refuses to start when any of them is set. Set them per target instead.

### Per-target settings

Each target is a block of `TARGET_<n>_<KEY>` variables, with `<n>` counting up from `0`:

| Key                                                                                                                                                                                    | Apps               | Description                                                                                                                                   |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------ | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `NAME`                                                                                                                                                                                 | all                | Required. Served at `/metrics/<name>`. Lowercase letters, digits, `_` and `-`, starting with a letter or digit, at most 63 characters; unique |
| `APP`                                                                                                                                                                                  | all                | Required. One of `radarr`, `sonarr`, `lidarr`, `prowlarr`, `bazarr`, `sabnzbd`                                                                |
| `URL`                                                                                                                                                                                  | all                | Required. Same rules as `URL` above; no two targets may share a URL                                                                           |
| `API_KEY`                                                                                                                                                                              | all                | API key, inline                                                                                                                               |
| `API_KEY_FILE`                                                                                                                                                                         | all                | Path to a file containing the API key; surrounding whitespace is trimmed, and it overrides `API_KEY`                                          |
| `FORM_AUTH`, `AUTH_USERNAME`, `AUTH_PASSWORD`                                                                                                                                          | all except sabnzbd | Form auth for this target; there is no process-wide default                                                                                   |
| `SCRAPE_TIMEOUT`, `REQUEST_TIMEOUT`, `DISABLE_SSL_VERIFY`, `PROXY_FROM_ENV`                                                                                                            | all                | Override the process-wide value                                                                                                               |
| `ENABLE_UNKNOWN_QUEUE_ITEMS`, `DISABLE_QUALITY_METRICS`, `DISABLE_EPISODE_METRICS`, `DISABLE_ALBUM_METRICS`, `DISABLE_HISTORY_METRICS`, `DISABLE_WANTED_METRICS`, `SERIES_CONCURRENCY` | all except sabnzbd | Override the process-wide value                                                                                                               |
| `PROWLARR__BACKFILL`, `PROWLARR__BACKFILL_SINCE_DATE`                                                                                                                                  | prowlarr           | Override the process-wide value                                                                                                               |
| `BAZARR__SERIES_BATCH_SIZE`, `BAZARR__SERIES_BATCH_CONCURRENCY`                                                                                                                        | bazarr             | Override the process-wide value                                                                                                               |

An override that is unset **or empty** inherits the process-wide value: `TARGET_1_SERIES_CONCURRENCY=` is the same as leaving it out. A target's `SCRAPE_TIMEOUT` also sets its collector deadline, exactly as the process-wide one does in single-target mode.

`TARGET_<n>_API_KEY`, `TARGET_<n>_API_KEY_FILE`, `TARGET_<n>_AUTH_USERNAME` and `TARGET_<n>_AUTH_PASSWORD` are removed from the exporter's environment after they are read, like their single-target counterparts (see [Secrets](#secrets)). Prefer `API_KEY_FILE` with a mounted secret.

### Validation

Startup is strict and fails closed. If anything below is wrong, `serve` reports every problem at once, exits with status 1 and never starts listening. Errors about targets name the variable or the target (`target <n>/<name>`), but never contain the value of a `TARGET_*` variable:

- no targets at all, a gap in the indices (`TARGET_0_*` and `TARGET_2_*` without `TARGET_1_*`), a malformed variable (`TARGET_01_NAME`, `TARGET_x_URL`) or an unknown key (`TARGET_0_URLL`);
- a value that doesn't parse (`TARGET_0_SERIES_CONCURRENCY: invalid int`) or a key file that can't be read;
- an invalid or duplicate name, an unknown app, a missing URL, or two targets with the same URL (compared case-insensitively on scheme and host, ignoring a trailing `/`);
- a key that doesn't apply to the target's app (`PROWLARR__*` on a non-Prowlarr target, `BAZARR__*` on a non-Bazarr target, the form-auth, `DISABLE_*`, `ENABLE_UNKNOWN_QUEUE_ITEMS` and `SERIES_CONCURRENCY` keys on SABnzbd);
- `MAX_UPSTREAM_REQUESTS` below the number of targets + 1;
- a single-target setting (`URL`, `API_KEY`, `API_KEY_FILE`, `FORM_AUTH`, `AUTH_USERNAME`, `AUTH_PASSWORD`, `--url`, `--api-key`);
- anything the app's own subcommand would reject, such as a malformed API key or `SERIES_CONCURRENCY` outside `1`–`32`.

For example:

```text
Error: URL is not supported by serve: URLs and credentials are set per target (TARGET_<n>_URL)
TARGET_0_URLL: unknown setting
TARGET_2_*: target indices must be contiguous from 0 (missing TARGET_1_*)
TARGET_0_SERIES_CONCURRENCY: invalid int
target 0/sonarr-hd: PROWLARR__BACKFILL is not valid for app sonarr
```

### Routes

| Request                                                                                      | Response                                                                                                   |
| -------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| `GET /metrics/<name>`                                                                        | That target's metrics; the query string is ignored                                                         |
| `GET /metrics`                                                                               | The exporter's own metrics: `exportarr_app_info`, `go_*`, `process_*` and the upstream-limit metrics below |
| `GET /healthz`                                                                               | `OK`                                                                                                       |
| `GET /`                                                                                      | An HTML index linking to every target                                                                      |
| Anything else: an unknown name, `/metrics/`, a trailing slash, an uppercase name, any `POST` | `404` with the constant body `404 page not found`                                                          |

Each target keeps the single-target scrape behavior: at most two concurrent scrapes (`503` beyond that), a `503` past its `SCRAPE_TIMEOUT`, and overlapping scrapes share one collection. Target series are exactly the single-target series, with no extra label. Collector log lines carry `target=<name>`, and startup logs one `Configured target` line per target, with the URL's credentials and query removed.

### Prometheus configuration

The worked example below exports 2× Sonarr, 2× Radarr and 1× Prowlarr from one `serve` process:

```sh
SCRAPE_TIMEOUT=25s

TARGET_0_NAME=sonarr-hd
TARGET_0_APP=sonarr
TARGET_0_URL=http://sonarr-hd:8989
TARGET_0_API_KEY_FILE=/run/secrets/sonarr_hd

TARGET_1_NAME=sonarr-4k
TARGET_1_APP=sonarr
TARGET_1_URL=http://sonarr-4k:8989
TARGET_1_API_KEY_FILE=/run/secrets/sonarr_4k
TARGET_1_SERIES_CONCURRENCY=4

TARGET_2_NAME=radarr-hd
TARGET_2_APP=radarr
TARGET_2_URL=http://radarr-hd:7878
TARGET_2_API_KEY_FILE=/run/secrets/radarr_hd

TARGET_3_NAME=radarr-4k
TARGET_3_APP=radarr
TARGET_3_URL=http://radarr-4k:7878
TARGET_3_API_KEY_FILE=/run/secrets/radarr_4k

TARGET_4_NAME=prowlarr
TARGET_4_APP=prowlarr
TARGET_4_URL=http://prowlarr:9696
TARGET_4_API_KEY_FILE=/run/secrets/prowlarr
```

Give every target its own `static_configs` entry. `__metrics_path__` selects the target and `instance` names it:

```yaml
scrape_configs:
  - job_name: exportarr
    scrape_interval: 30s
    scrape_timeout: 25s
    static_configs:
      - targets: ["exportarr:9707"]
        labels: { __metrics_path__: /metrics/sonarr-hd, instance: sonarr-hd }
      - targets: ["exportarr:9707"]
        labels: { __metrics_path__: /metrics/sonarr-4k, instance: sonarr-4k }
      - targets: ["exportarr:9707"]
        labels: { __metrics_path__: /metrics/radarr-hd, instance: radarr-hd }
      - targets: ["exportarr:9707"]
        labels: { __metrics_path__: /metrics/radarr-4k, instance: radarr-4k }
      - targets: ["exportarr:9707"]
        labels: { __metrics_path__: /metrics/prowlarr, instance: prowlarr }
  # The exporter's own metrics, including the upstream-limit metrics.
  - job_name: exportarr-self
    static_configs:
      - targets: ["exportarr:9707"]
```

- **Always set `instance`.** Without it every target gets `instance="exportarr:9707"`, and their `up` series collide.
- Targets that need a different `scrape_interval` go in their own job with their own `scrape_timeout`. Keep each target's `SCRAPE_TIMEOUT` at or below its job's `scrape_timeout`.
- A target listed without `__metrics_path__` scrapes the exporter's own `/metrics`: `up` is `1` but no app series arrive. See [Alerting](#alerting) for a rule that catches it.

See [examples/compose](./examples/compose/docker-compose.yaml) and [examples/prometheus](./examples/prometheus/prometheus.yml) for a complete setup.

### Sizing and the upstream limit

`MAX_UPSTREAM_REQUESTS` bounds the concurrent HTTP requests from all targets to their apps, including retries and form-auth logins. Each target owns one reserved slot inside the limit, and the remaining `MAX_UPSTREAM_REQUESTS − <number of targets>` slots are shared. A request takes its own slot first, then any free shared slot. It holds the slot until its response body is closed, and gives it back between retries. A request that can't get a slot waits within its collector deadline and then fails into that target's `*_collector_error` gauge.

Rough peak of concurrent requests per target at the default settings:

| Target         | Peak                                                             |
| -------------- | ---------------------------------------------------------------- |
| Radarr         | about 7                                                          |
| Sonarr, Lidarr | about 6 + `SERIES_CONCURRENCY` (16 at the default)               |
| Prowlarr       | about 4                                                          |
| Bazarr         | about `BAZARR__SERIES_BATCH_CONCURRENCY` + 2 (12 at the default) |
| SABnzbd        | 2                                                                |

- The worked example peaks around **50** concurrent requests, so the default of 64 doesn't bind in normal operation. It engages when a target hangs or tuning is raised.
- The reserved slot guarantees progress, not speed: while a neighbor hangs, a healthy Sonarr can be squeezed to one request at a time and miss its own deadline, which its error gauge then reports. If `exportarr_upstream_slot_wait_timeouts_total` keeps growing, raise the limit or lower the per-target concurrency.
- **Memory adds up** across targets: a Bazarr episode walk and a large Radarr movie list decoding at the same time need the sum of both (see [Scrape performance and sizing](#scrape-performance-and-sizing)). Size the container for the concurrent peaks and set `GOMEMLIMIT` to its memory limit.
- **Fate sharing:** all targets share one process. Panics inside collectors are recovered and logged with `target=<name>`, but an out-of-memory kill or a crash takes every target down at once. Keep separate containers for targets that need hard isolation.

The upstream-limit metrics are served on the exporter's own `/metrics` only:

| Metric                                                | Type      | Description                                 |
| ----------------------------------------------------- | --------- | ------------------------------------------- |
| `exportarr_upstream_requests_max`                     | gauge     | The configured `MAX_UPSTREAM_REQUESTS`      |
| `exportarr_upstream_requests_in_flight{target}`       | gauge     | Requests currently holding a slot           |
| `exportarr_upstream_slot_wait_seconds{target}`        | histogram | Time requests waited for a slot             |
| `exportarr_upstream_slot_wait_timeouts_total{target}` | counter   | Requests abandoned while waiting for a slot |

### Migrating from one container per app

1. For each exporter container, add a `TARGET_<n>_*` block: pick a `NAME`, set `APP` to the container's subcommand, and move `URL` and `API_KEY_FILE` (or `API_KEY`) to `TARGET_<n>_URL` and `TARGET_<n>_API_KEY_FILE`. Form auth moves to `TARGET_<n>_FORM_AUTH`, `TARGET_<n>_AUTH_USERNAME` and `TARGET_<n>_AUTH_PASSWORD`.
2. Settings that all the containers share can stay process-wide. Settings that differ become per-target overrides, such as `TARGET_<n>_SERIES_CONCURRENCY`.
3. Replace the per-container scrape jobs with one job like the one above.

Series are unchanged apart from their `job` and `instance` labels, and the `url` label is the same as before. Update alerts and dashboards that select on `job` or `instance`, such as `up{job="sonarr-exporter"}`.

**Grafana:** the shipped dashboards sum across series, so two targets of the same type are added together, just as two containers already are. [Dashboard 2](./examples/grafana/dashboard2.json) also filters on `instance`: with `instance: <name>`, each target appears in its app's instance picker and can be viewed on its own.

## Scrape performance and sizing

Measured against real instances with **every metric enabled** — use these scaling rules to pick scrape intervals and container limits:

| App      | Typical scrape       | What it scales with                                                                                                                         |
| -------- | -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| SABnzbd  | tens of milliseconds | flat — two small API calls                                                                                                                  |
| Radarr   | a second or two      | one large movie-list payload; mostly JSON transfer + decode                                                                                 |
| Sonarr   | a few seconds        | per-series fan-out (two API calls per series, `SERIES_CONCURRENCY` at a time, default 10)                                                   |
| Lidarr   | a few seconds        | per-artist fan-out (two API calls per artist, `SERIES_CONCURRENCY` at a time, default 10)                                                   |
| Prowlarr | fast                 | stats endpoint (see the backfill note above for the first scrape)                                                                           |
| Bazarr   | tens of seconds      | full episode-subtitle walk; the time is spent _inside bazarr_ generating the batched responses, so batch/concurrency tuning barely moves it |

- **Set `scrape_interval` longer than your worst scrape.** Bazarr with episode metrics enabled commonly needs `60s` or more; if a scrape arrives while the previous one is still running, exportarr skips it and raises the collector's error gauge (see "Changed scrape behavior" below). The other apps are comfortable at `15–30s`.
- The first scrape after startup is the slowest (TLS handshakes); connections are pooled and reused afterwards.
- **Memory** scales with the largest API payload decoded: expect roughly 25–100 MB RSS, with the high end during bazarr's episode walk or a large radarr movie list. In Kubernetes, set `GOMEMLIMIT` to the container memory limit so GC stays ahead of the decode spike, and watch the exporter's own `go_*`/`process_*` metrics.
- If a scrape is too slow, reach for the `DISABLE_*` flags above rather than a shorter `REQUEST_TIMEOUT` — they remove the expensive endpoints entirely instead of cutting requests off mid-flight.

## Alerting

`up` only tells you the exporter answered. Whether the app is reachable and its collectors succeed is reported inside the scrape:

- `<app>_system_status` is `1` while the app answers its status endpoint and `0` when it doesn't.
- `<app>_*collector_error` is `1` for each failing collector and absent otherwise.

Write rules over a window of at least **two scrape intervals**, and don't rely on `for:`. That matters with long intervals such as 15 minutes:

- An instant expression only sees a series for Prometheus's 5-minute lookback after each scrape, so a `for:` longer than that never fires, and `*_collector_error > 0` flickers at best.
- `max_over_time({__name__=~".+_collector_error"}[30m])` drops the metric name, so all of a target's error gauges collapse into one labelset and the query fails. Copy the name into a label first, as below, or write one rule per gauge.

```yaml
groups:
  - name: exportarr
    rules:
      # 30m = 2 × a 15m scrape interval; adjust the job and app names.
      - alert: ArrUnreachable
        expr: max_over_time(sonarr_system_status[30m]) < 1
      - alert: ArrCollectorFailing
        expr: |
          max_over_time(
            label_replace({__name__=~"sonarr_.*collector_error"}, "collector", "$1", "__name__", "(.+)")[30m:]
          ) > 0
      - alert: ExporterDown
        expr: max_over_time(up{job="sonarr-exporter"}[30m]) < 1
```

With [`exportarr serve`](#serving-several-targets-from-one-process-exportarr-serve), a target whose scrape config lacks `__metrics_path__` scrapes the exporter's own `/metrics`. `up` stays `1`, but none of the app's series arrive, so the rules above never fire. Add an `absent(<app>_system_status)` rule per target, over the same window:

```yaml
# Add to the rules above, once per serve target; instance is its static_configs label.
- alert: ArrSeriesMissing
  expr: absent_over_time(sonarr_system_status{instance="sonarr-hd"}[30m])
```

## Upgrading from v2 to v3

v3 is a breaking release. Review each section before upgrading.

### Removed

- **Readarr support** — the `readarr` command, its metrics, and its dashboard panels are gone (Readarr was retired upstream).
- **Basic auth** — HTTP basic auth and the `--basic-auth-username`/`--basic-auth-password` flags are removed. `AUTH_USERNAME`/`AUTH_PASSWORD` now apply to form auth only and require `FORM_AUTH=true`; setting credentials without form auth is a startup error.
- **config.xml parsing** — `CONFIG`/`--config` is removed; exportarr no longer reads the \*arr's config file. Provide the key via `API_KEY_FILE` (environment-only; the `--api-key-file` flag is gone) for Docker and Kubernetes secrets mounted as files, or via `API_KEY`.
- **Legacy variable aliases** — `APIKEY`, `APIKEY_FILE`, `BASIC_AUTH_USERNAME` and `BASIC_AUTH_PASSWORD` no longer work.
- **Log levels** — `fatal`, `panic` and `dpanic` are gone; valid levels are `debug`, `info`, `warn`, `error`.
- **`ENABLE_ADDITIONAL_METRICS`** — removed. The metrics it bundled are now collected **by default** (the per-item fan-out is parallelized and ~10× faster), with granular opt-outs instead: `DISABLE_QUALITY_METRICS`, `DISABLE_EPISODE_METRICS`, `DISABLE_ALBUM_METRICS`. Set all that apply to restore v2's default-off behavior.

| v2                                   | v3                                                                                         |
| ------------------------------------ | ------------------------------------------------------------------------------------------ |
| `APIKEY`                             | `API_KEY`                                                                                  |
| `API_KEY_FILE=/path`                 | still supported as an environment variable; the `--api-key-file` flag was removed          |
| `CONFIG=/path/config.xml`            | `URL` + `API_KEY`                                                                          |
| `BASIC_AUTH_USERNAME`/`..._PASSWORD` | removed — form auth uses `AUTH_USERNAME`/`AUTH_PASSWORD` + `FORM_AUTH=true`                |
| `LOG_LEVEL=fatal`                    | `LOG_LEVEL=error`                                                                          |
| `ENABLE_ADDITIONAL_METRICS=true`     | (default behavior — remove it)                                                             |
| `ENABLE_ADDITIONAL_METRICS` unset    | `DISABLE_QUALITY_METRICS` + `DISABLE_EPISODE_METRICS` + `DISABLE_ALBUM_METRICS` all `true` |

### Changed scrape behavior — update your alerts

- A failing collector no longer fails the whole scrape with HTTP 500. `/metrics` now returns 200 with everything that succeeded, plus a per-collector error gauge (e.g. `radarr_collector_error`, `radarr_queue_collector_error`) set to `1` for whatever failed; the gauge is absent while the collector is healthy. System status is the exception: an unreachable app shows up as `<app>_system_status 0`, while `<app>_status_collector_error` only appears if that collector panics. Alerts that relied on `up == 0` when the app was down need new rules; see [Alerting](#alerting).
- `sabnzbd_collector_error` renamed its `target` label to `url`, matching every other metric.
- Overlapping scrapes never stack walks onto the app, the failure mode behind bazarr CPU drainage ([#380](https://github.com/onedr0p/exportarr/issues/380)): a scrape that arrives while a collection is still running waits for it and is served the same result.
- `/metrics` serves at most two scrapes at once and answers `503` to any more, and to a scrape that exceeds `SCRAPE_TIMEOUT`. Every collector abandons its requests shortly before that deadline and raises its error gauge (system status reports `0`), so the scrape still returns whatever finished. Only `GET` (and `HEAD`) is accepted.

### Changed metrics

- `bazarr_subtitles_score_total{score="93.45%"}` (one series per distinct score — unbounded cardinality) is replaced by a **histogram**, `bazarr_subtitles_score`, with percentage buckets `10..90, 95, 100`. Use `histogram_quantile()` or the bucket series directly ([#239](https://github.com/onedr0p/exportarr/issues/239)).
- `<app>_queue_total` now emits one series per `(status, download_status, download_state)` combination with accurate counts. An empty queue emits a single zero series (empty label values) instead of no series at all, so dashboards can tell "zero items" from "scrape failed". v2 emitted a single series carrying the total queue size under whichever labels the last queue item happened to have — sums still work, per-label panels will show corrected values.
- The self-instrumentation duration gauges (`<app>_scrape_duration_seconds`, `sabnzbd_queue_query_duration_seconds`, `sabnzbd_server_stats_query_duration_seconds`) are now **histograms**, so `histogram_quantile()` works across scrapes instead of only seeing the last value. They additionally expose sparse **native histograms** to scrapers that negotiate them; classic buckets remain for everyone else.
- Log output is structured slog (`time=… level=… msg=…`, or JSON with `--log-format json`); update anything parsing exporter logs.

### New in v3

No action needed, but worth knowing:

- `<app>_diskspace_free_bytes` / `<app>_diskspace_total_bytes` (per disk, with totals — disk usage is finally computable).
- `sabnzbd_speed_limit_bps` / `sabnzbd_speed_limit_percent`.
- `bazarr_throttled_providers` and `bazarr_signalr_connected{app="sonarr"|"radarr"}`.
- `bazarr_episode_subtitles_missing_total` stays exported even with `DISABLE_EPISODE_METRICS=true` (sourced from bazarr's cheap badges endpoint), and `bazarr_subtitles_missing_total` always includes the episode count instead of misleadingly reporting movies-only ([#407](https://github.com/onedr0p/exportarr/issues/407)).
- `REQUEST_TIMEOUT` / `--request-timeout` (default `60s`) caps each request to the target app.
- Huge-library relief: `DISABLE_HISTORY_METRICS` and `DISABLE_WANTED_METRICS` skip the endpoints whose totals force full table counts (the queries that can hang a multi-year instance's UI during scrapes). Combined with v3's `pageSize=1`, no-sort requests and partial scrapes, large sonarr/radarr instances scrape reliably again.
- Resiliency: a panic inside any collector — including concurrent fan-out workers — now degrades to that collector's error gauge instead of crashing the exporter, and scraping an empty instance (e.g. bazarr with no series, [#244](https://github.com/onedr0p/exportarr/issues/244)) is clean.
- Retries of failed requests now back off with jitter (~250ms, then ~500ms) instead of re-sending immediately, giving a struggling instance breathing room.
- The exporter exposes its own runtime metrics (`go_*`, `process_*`), so its CPU, memory, and GC behavior are visible alongside the app metrics.
- Large-library scrapes are dramatically faster: per-series/artist lookups now run concurrently, and the per-item metrics on a ~500-series library drop from ~30s to a few seconds.
