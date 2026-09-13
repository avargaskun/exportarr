# Multi-target exporter (`exportarr serve`)

- **Status:** Accepted: the owner approved the recommendation on 2026-09-13.
- **Issue:** [avargaskun/exportarr#13](https://github.com/avargaskun/exportarr/issues/13)
- **Prior art:** upstream PR [onedr0p/exportarr#431](https://github.com/onedr0p/exportarr/pull/431), which adds `exportarr serve` with one fixed target per app type at `/metrics/<app>`.

## 1. Problem and goal

Today one exportarr process exports exactly one app instance. Running N instances means N containers, N ports, N scrape entries and N image updates per release. The motivating setup runs five: 2× Sonarr, 2× Radarr and 1× Prowlarr.

**Goal:** one process on one port serves many **named** targets of all six app types (Radarr, Sonarr, Lidarr, Prowlarr, Bazarr, SABnzbd), including several instances of the same type. Each target stays its own Prometheus scrape target. A slow, failing or panicking target can't affect the others, a process-wide cap bounds the load on shared backends, and the single-target subcommands behave exactly as in 3.1.0.

## 2. Requirements

| #   | Requirement                                                                                                                                                         |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R1  | Several named targets per process, including several instances of the same app type                                                                                 |
| R2  | Isolation: a slow, failing or panicking target must not delay, blank or wedge the others (per-target timeout, overlap guard, error gauges)                          |
| R3  | Prometheus semantics: per-target `up`, `scrape_duration_seconds`, `scrape_timeout` and a stable `instance`; different intervals possible                            |
| R4  | Secrets: per-target credentials from files or env; never in argv, logs, labels, metric values or validation errors                                                  |
| R5  | Per-target tuning overrides with process defaults                                                                                                                   |
| R6  | A global bound on concurrent upstream requests                                                                                                                      |
| R7  | The #1 invariants still hold: outbound only to configured URLs; GET only, login excepted; no secrets in logs or metrics; a scrape can never choose an arbitrary URL |
| R8  | Single-target subcommands unchanged                                                                                                                                 |
| R9  | Small, additive, upstreamable                                                                                                                                       |

## 3. Constraints from today's code

| #   | Constraint                                                                                                                                                                                                                                | Consequence                                                                                                                       |
| --- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| 1   | **Collectors are stateful and long-lived:** the Prowlarr stats window, SABnzbd's accumulated server stats, the form-auth session cookie and the per-collector `TryLock` overlap guards                                                    | A registry built per request would reset that state every scrape. Registries must be long-lived, one per target, built at startup |
| 2   | **One shared registry can't hold two SABnzbd collectors:** their descriptors are package-level and identical. Two *arr targets of one type only coexist because their const `url` label differs                                           | Per-target registries avoid the collision; a shared registry would need a `target` label on every series                          |
| 3   | **`Collector.Collect` takes no context.** Deadlines are fixed at construction from config                                                                                                                                                 | A deadline derived from each request (the Prometheus timeout header) needs extra plumbing                                         |
| 4   | **Three timeouts must agree:** the collector deadline (`max(SCRAPE_TIMEOUT − 5s, SCRAPE_TIMEOUT / 2)`), promhttp's `Timeout` (a 503, but the gather keeps running) and the server's `WriteTimeout` (`SCRAPE_TIMEOUT + 10s`)               | All three must come from the **target's** scrape timeout; `WriteTimeout` from the maximum. PR #431 loses the collector deadline   |
| 5   | **Fate sharing:** an OOM, file-descriptor exhaustion or an unrecovered panic in one process takes down every target in it. Memory adds up: roughly 25–100 MB RSS per single-target exporter today, and each response may be up to 256 MiB | Inherent to every single-process option; accepted and documented (section 10)                                                     |
| 6   | **The #1 invariants live in specific constructors:** `ValidateURL`, GET-only `DoRequestContext`, no-follow `CheckRedirect`, redacted client logs, `unset` env tags, `warnSecretFlags`                                                     | Build every target through the same config, validation and client path as single-target mode, so R7 holds by construction         |
| 7   | **Base flags mean nothing in multi mode:** `--url`, `--api-key`, `URL`, `API_KEY`, `AUTH_*` and `FORM_AUTH` configure a single target                                                                                                     | Multi mode must reject them (fail closed); PR #431 silently ignores them                                                          |
| 8   | **The env library fails open on indexed slices:** `TARGET_0_*` plus `TARGET_2_*` silently yields one target, unknown keys are ignored, and a parse error echoes the value                                                                 | The loader scans for gaps and unknown keys itself and rewrites every error without the value                                      |

## 4. Exposure alternatives

| Option  | Approach                                                                                                               | Pros                                                                                                                                               | Cons                                                                                                                                                                                                |
| ------- | ---------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **A**   | Env prefix per app instance plus `/metrics/<name>` (#431 extended, e.g. `SONARR_HD__URL`)                              | Closest to #431; env names read naturally                                                                                                          | Really C plus a name-keyed env source (config source 2). Case and `_`/`-` mapping ambiguity; collides with the existing `PROWLARR__`/`BAZARR__` prefixes; needs a custom environment scan           |
| **B**   | Multi-target pattern `/probe?target=<name>`, names only                                                                | The standard Prometheus pattern (blackbox, snmp, mysqld); one job lists bare names; well-known relabel recipe                                      | Operators expect `target` to be a host or URL, so the names-only rule needs explicit rejection code (empty, repeated, unknown, URL-like) and tests; relabel boilerplate                             |
| **B+C** | Serve both routes over one name-to-handler map                                                                         | About ten more lines; operators pick either scrape style                                                                                           | Two routes to document and test; no single obvious way to scrape                                                                                                                                    |
| **C**   | Static path per named target, `/metrics/<name>`                                                                        | No query parsing: the mux matches a fixed, validated set. Extends #431's `/metrics/<app>` naturally. Prometheus needs only `static_configs` labels | Rare among exporters, so no well-known recipe; one `static_configs` entry with two labels per target                                                                                                |
| **D**   | One merged `/metrics` for all targets with a `target` label                                                            | Simplest scrape config: one job, one target                                                                                                        | One `up`, duration, timeout and interval for everything, so no per-target `instance`. The response waits for the slowest target, and a 503 blanks all of them. Against Prometheus exporter guidance |
| **E**   | One process, one port per target, each running today's exact stack                                                     | Per-target behavior identical to single-target mode by construction; Prometheus config and `instance` unchanged                                    | N ports to allocate, publish and keep unique; port becomes per-target config; not a known pattern; the cap and fate sharing still apply                                                             |
| **E2**  | Supervisor: `serve` forks N single-target children                                                                     | True fault isolation, including OOM; per-target code is exactly 3.1.0                                                                              | Process management in a `scratch` image (signals, restarts, reaping, log multiplexing); secrets pass through child environments; a global cap needs IPC; heaviest and least upstreamable            |
| **F**   | Do nothing in code: group image updates (e.g. a Renovate `groupName`) and deduplicate compose config with YAML anchors | Zero code and zero divergence; strongest isolation                                                                                                 | Doesn't meet the one-process goal; N× memory, ports and containers; no cross-target cap                                                                                                             |

**Requirements matrix** (`yes` / `partial` / `no`):

| Requirement                   | A       | B       | B+C     | C   | D       | E       | E2      | F   |
| ----------------------------- | ------- | ------- | ------- | --- | ------- | ------- | ------- | --- |
| R1 named targets              | yes     | yes     | yes     | yes | yes     | yes     | yes     | no  |
| R2 isolation                  | yes     | yes     | yes     | yes | no      | yes     | yes     | yes |
| R3 Prometheus semantics       | yes     | yes     | yes     | yes | no      | yes     | yes     | yes |
| R4 secrets                    | yes     | yes     | yes     | yes | yes     | yes     | partial | yes |
| R5 per-target tuning          | partial | yes     | yes     | yes | yes     | yes     | yes     | yes |
| R6 global cap                 | yes     | yes     | yes     | yes | yes     | yes     | partial | no  |
| R7 #1 invariants              | yes     | partial | partial | yes | yes     | yes     | yes     | yes |
| R8 single-target unchanged    | yes     | yes     | yes     | yes | yes     | yes     | yes     | yes |
| R9 small, upstreamable        | partial | partial | partial | yes | partial | partial | no      | yes |
| Owner scope: one process/port | yes     | yes     | yes     | yes | yes     | no      | no      | no  |

Notes: A's R5 is `partial` because the shared `PROWLARR__`/`BAZARR__` prefixes stay process-wide (#431's pitfall). B's R7 is `partial` because a query parameter invites URLs and needs dedicated rejection code. E2's R4 is `partial` because secrets reach children through their environment. F's R9 is trivially `yes`: there is nothing to build.

**Status:**

| Option | Status            | Reason                                                                                                 |
| ------ | ----------------- | ------------------------------------------------------------------------------------------------------ |
| **C**  | **Chosen**        | Meets every requirement; closest to #431, which weighs high for R9                                     |
| B, B+C | Not chosen for v1 | A `/probe?target=` alias can be added later without redesign: it looks up the same name-to-handler map |
| A      | Not chosen        | Its routing is C; its config source (name-keyed env) is not chosen                                     |
| D      | Rejected          | Fails R3                                                                                               |
| E, E2  | Ruled out         | The owner chose one port                                                                               |
| F      | Ruled out         | Doesn't meet the one-process goal; grouping image updates into one review stays useful as a stopgap    |

## 5. Config sources

| #   | Source                                                          | R4 secrets | R5 overrides | R9 upstreamable | Container ergonomics                                           | Pros                                                                                                            | Cons                                                                                                                           |
| --- | --------------------------------------------------------------- | ---------- | ------------ | --------------- | -------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| 1   | **Indexed env vars** `TARGET_<n>_*`                             | yes        | yes          | yes             | Good: plain compose `environment:` and Kubernetes `env:` lists | No new dependency; same library and tags as today (`API_KEY_FILE` with `file,unset`, `unset` on inline secrets) | Verbose (about four variables per target); the loader must close the gap and unknown-key holes; "inherit" needs pointer fields |
| 2   | **Name-keyed env vars**, e.g. `EXPORTARR_TARGET_SONARR_HD__URL` | yes        | yes          | partial         | Good, and self-describing                                      | No indices, so no gaps                                                                                          | Custom discovery scan; names derived from env var names (uppercase, `_`/`-` mapping, case collisions)                          |
| 3   | **YAML file** with `api_key_file:`                              | yes        | yes          | no              | Fair: the file must be mounted into a `scratch` container      | Most readable for many targets; comments; strict decoding catches typos                                         | New dependency; upstream v3 dropped its config file; two config mechanisms in one binary                                       |
| 4   | **JSON file**                                                   | yes        | yes          | partial         | Fair: mounted file                                             | Stdlib only; strict decoding                                                                                    | No comments; unpleasant by hand; two config mechanisms                                                                         |
| 5   | **Hybrid:** a file for topology, env for process defaults       | yes        | yes          | no              | Good for a Kubernetes ConfigMap plus Secret split              | Structure in the file, defaults in env                                                                          | Two sources with precedence rules; largest config surface                                                                      |

**Status:** **1 is chosen**: it is the only option with no dependency and no custom parser. 3 is disfavored (new dependency, and upstream v3 removed its config file). 2, 4 and 5 are viable but not chosen.

## 6. Smaller choices

**Registry structure**

| Option                                | Trade-off                                                                                                         | Status     |
| ------------------------------------- | ----------------------------------------------------------------------------------------------------------------- | ---------- |
| Per-target long-lived registry        | Own singleflight, timeout and in-flight limit per target; no descriptor collisions; collector state keeps working | **Chosen** |
| Shared registry plus a `target` label | One gather for everything, which only D needs; a shared singleflight couples all targets                          | Not chosen |
| Registry per request (blackbox style) | Per-request deadlines for free, but resets collector state (constraint 1) unless that state is moved out first    | Not chosen |

**Global cap semantics (R6)**

| Option | Semantics                                                                   | Trade-off                                                                                   | Status                    |
| ------ | --------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------- | ------------------------- |
| a      | One process-wide semaphore in the transport, acquired per attempt           | Couples targets: a hung backend holds its slots until its deadline and starves the others   | Not chosen                |
| **b**  | (a) plus **one reserved slot per target**                                   | Every target keeps making progress while the total stays bounded                            | **Chosen**, refined below |
| c      | Caps per backend group (`GROUP=db1`)                                        | Models shared database servers precisely; an extra config concept                           | Deferred                  |
| d      | Startup check only: reject configs whose worst-case fan-out exceeds the cap | No runtime coupling, but coarse; doesn't bound retries or logins                            | Not chosen                |
| e      | Cap concurrent scrapes rather than requests                                 | A queued scrape eats its own budget and blanks the target (fails R2); doesn't bound fan-out | Not chosen                |

**b refined:** the reserved slots are carved **out of** the cap, not added on top, so the bound is exact.

- `MAX_UPSTREAM_REQUESTS` (default **64**) is the process-wide ceiling on concurrent upstream HTTP attempts.
- Each of the N targets owns one reserved slot; the rest form a **shared pool of `cap − N`** slots.
- Validation requires **`cap ≥ N + 1`**, so the shared pool is never empty.
- A request takes its own reserved slot first, then any free shared slot, waiting under its collector's deadline.
- A slot is held **until the response body is closed**, and released between retry attempts.

**Timeout source**

| Option                                                | Trade-off                                                                                   | Status     |
| ----------------------------------------------------- | ------------------------------------------------------------------------------------------- | ---------- |
| Static per target (`TARGET_<n>_SCRAPE_TIMEOUT`)       | Works with long-lived collectors today; the operator keeps it at or below Prometheus's own  | **Chosen** |
| From the `X-Prometheus-Scrape-Timeout-Seconds` header | Follows Prometheus automatically, but needs deadline plumbing into `Collect` (constraint 3) | Deferred   |
| `min(header − offset, configured)`                    | Safest bound; same plumbing                                                                 | Deferred   |

**Series labels**

| Option                      | Trade-off                                                                                                 | Status     |
| --------------------------- | --------------------------------------------------------------------------------------------------------- | ---------- |
| No new label                | Series identical to single-target mode; identity comes from Prometheus's `instance`                       | **Chosen** |
| Add `target=<name>`         | Identity survives a bad scrape config, but series differ from single-target mode and duplicate `instance` | Not chosen |
| Replace `url` with `target` | Hides internal URLs, but breaks existing dashboards and alerts                                            | Not chosen |

**Other forks:** serve mode is an explicit `serve` subcommand (named like #431), not auto-detected. Exporter self-metrics live only on the exporter's own `/metrics`, not duplicated onto every target as #431 does.

## 7. Prometheus examples

**C (chosen):** one `static_configs` entry per target, with `__metrics_path__` and `instance`.

```yaml
- job_name: exportarr
  scrape_interval: 30s
  scrape_timeout: 25s
  static_configs:
    - targets: ["exportarr:9707"]
      labels: { __metrics_path__: /metrics/sonarr-hd, instance: sonarr-hd }
    - targets: ["exportarr:9707"]
      labels: { __metrics_path__: /metrics/radarr-hd, instance: radarr-hd }
```

**B (for comparison, not built):** bare names plus the standard relabel recipe.

```yaml
- job_name: exportarr
  metrics_path: /probe
  static_configs:
    - targets: [sonarr-hd, radarr-hd]
  relabel_configs:
    - source_labels: [__address__]
      target_label: __param_target
    - source_labels: [__param_target]
      target_label: instance
    - target_label: __address__
      replacement: exportarr:9707
```

**D (for comparison, rejected):** one job and one target, so one `up` and one timeout for everything.

```yaml
- job_name: exportarr
  static_configs:
    - targets: ["exportarr:9707"]
```

**C needs `instance` set just as B does.** Without it, every target shares `instance="exportarr:9707"` and their `up` series collide. C's simplicity is only that the labels live in `static_configs` instead of `relabel_configs`. A target that needs a different interval goes in its own job with its own `scrape_interval` and `scrape_timeout`.

## 8. Worked example: 2× Sonarr, 2× Radarr, 1× Prowlarr

Run `exportarr serve` with this environment. Each `API_KEY_FILE` points at a mounted secret; `sonarr-4k` overrides one tuning setting.

```sh
# Process-wide defaults (existing names, same meaning)
SCRAPE_TIMEOUT=25s
MAX_UPSTREAM_REQUESTS=64

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

The matching Prometheus job:

```yaml
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
```

Scrape `exportarr:9707/metrics` in a separate job to collect the exporter's own metrics.

## 9. Recommendation and resulting design

**Recommendation: C + indexed env vars + a reserved-slot cap.** It generalizes #431 rather than competing with it: naming each target after its app type (`TARGET_0_NAME=sonarr`) reproduces #431's `/metrics/sonarr` exactly.

| Decision          | Choice                                                                                                                               |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| Process and port  | One process, one port; all six app types can be targets                                                                              |
| Activation        | New subcommand `exportarr serve`; single-target subcommands untouched                                                                |
| Exposure          | `GET /metrics/<name>`, one exact route per configured target                                                                         |
| Config            | Indexed env vars `TARGET_<n>_*`, env only, no new dependency, no hot reload                                                          |
| Names             | `^[a-z0-9][a-z0-9_-]{0,62}$`, unique; no `:` or `/`, so a URL can never pass as a name                                               |
| Unknown selection | `404` with `Content-Type: text/plain; charset=utf-8` and the constant body `404 page not found\n`; input never echoed                |
| Self-metrics      | Only on the exporter's own `GET /metrics`: app info, Go and process metrics, the cap metrics                                         |
| Index             | `GET /` (exact root only) lists the target names as links                                                                            |
| Scrape admission  | `MaxRequestsInFlight: 2` per target, as today                                                                                        |
| Series labels     | No `target` label; the `url` label is unchanged                                                                                      |
| Timeouts          | Static per target; `TARGET_<n>_SCRAPE_TIMEOUT` drives the collector deadline and promhttp `Timeout`; `WriteTimeout` is the max + 10s |
| Global cap        | `MAX_UPSTREAM_REQUESTS=64`; one reserved slot per target inside the cap; `cap ≥ N + 1`; slot held until body close                   |
| Duplicate URLs    | Startup error                                                                                                                        |
| Fate sharing      | Accepted at process level; memory sizing documented                                                                                  |

**Routes in serve mode:**

| Request                                                                                             | Result                                               |
| --------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `GET /metrics/<name>`                                                                               | That target's own stack; the query string is ignored |
| `GET /metrics`                                                                                      | Exporter self-metrics                                |
| `GET /healthz`                                                                                      | Unchanged                                            |
| `GET /`                                                                                             | Index of target names                                |
| Anything else: unknown name, `/metrics/`, trailing slash, wrong case, `%2F` in the name, any `POST` | Constant `404`                                       |

**Per-target keys** (`<KEY>` in `TARGET_<n>_<KEY>`):

| Key                                                                                                                                                                                    | Apps          | Notes                                                                                  |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------- | -------------------------------------------------------------------------------------- |
| `NAME`, `APP`, `URL`                                                                                                                                                                   | all           | Required; `APP` is one of `radarr sonarr lidarr prowlarr bazarr sabnzbd`; `URL` unique |
| `API_KEY`, `API_KEY_FILE`                                                                                                                                                              | all           | Removed from the environment after reading; the file wins                              |
| `FORM_AUTH`, `AUTH_USERNAME`, `AUTH_PASSWORD`                                                                                                                                          | *arr          | Per target only; the `AUTH_*` values are removed after reading                         |
| `SCRAPE_TIMEOUT`, `REQUEST_TIMEOUT`, `DISABLE_SSL_VERIFY`, `PROXY_FROM_ENV`                                                                                                            | all           | Unset means the process value                                                          |
| `ENABLE_UNKNOWN_QUEUE_ITEMS`, `DISABLE_QUALITY_METRICS`, `DISABLE_EPISODE_METRICS`, `DISABLE_ALBUM_METRICS`, `DISABLE_HISTORY_METRICS`, `DISABLE_WANTED_METRICS`, `SERIES_CONCURRENCY` | *arr          | Unset means the process value                                                          |
| `PROWLARR__BACKFILL`, `PROWLARR__BACKFILL_SINCE_DATE`                                                                                                                                  | prowlarr only | Unset means the process value                                                          |
| `BAZARR__SERIES_BATCH_SIZE`, `BAZARR__SERIES_BATCH_CONCURRENCY`                                                                                                                        | bazarr only   | Unset means the process value                                                          |

Process-wide defaults use the existing variable names with the same meaning. `serve` has no flags of its own: it reads env plus the root persistent flags (`--port`, `--interface`, `--log-*`, `--scrape-timeout`, `--request-timeout`, `--disable-ssl-verify`, `--proxy-from-env`).

**Strict, fail-closed validation.** Startup fails, and nothing listens, on any of the following. All errors are reported together, each labeled `target <index>/<name>`, and **no error ever contains a value** from the environment:

- no targets; a gap in the indices (`TARGET_0_*` and `TARGET_2_*`); a malformed variable (`TARGET_01_NAME`, `TARGET_x_NAME`); an unknown key (`TARGET_0_URLL`);
- an invalid or duplicate name, an unknown app, a missing or duplicate URL, an unparsable value (`TARGET_0_SERIES_CONCURRENCY: invalid int`) or an unreadable key file;
- a key that doesn't apply to the target's app (`PROWLARR__*` on a non-Prowlarr target; `FORM_AUTH`, `AUTH_*`, `DISABLE_*` and so on on SABnzbd);
- `MAX_UPSTREAM_REQUESTS` below the number of targets + 1;
- a single-target setting in serve mode: base `URL`/`--url`, `API_KEY`/`API_KEY_FILE`/`--api-key`, `FORM_AUTH`, `AUTH_USERNAME` or `AUTH_PASSWORD`. Credentials are per target.

Every resolved target then goes through the **existing** config validation, client construction and collector builders, so the #1 invariants hold by construction (R7).

**Cap metrics** (self-registry only; a `target` label is fine here because these describe the exporter):

| Metric                                                | Type      |
| ----------------------------------------------------- | --------- |
| `exportarr_upstream_requests_max`                     | gauge     |
| `exportarr_upstream_requests_in_flight{target}`       | gauge     |
| `exportarr_upstream_slot_wait_seconds{target}`        | histogram |
| `exportarr_upstream_slot_wait_timeouts_total{target}` | counter   |

The limiter wraps the inner HTTP transport, **below** the auth step, and also wraps the form-auth login. A goroutine waiting for the form-auth lock therefore never holds a slot, so a saturated cap can't deadlock. Collector log lines gain a `target` attribute in serve mode only, and startup logs one `Configured target` line per target with a redacted URL.

## 10. Isolation, fate sharing and sizing

**Isolation.** Every layer of a scrape is per target except the cap:

| Layer                                           | Scope         | On hitting it                                                 |
| ----------------------------------------------- | ------------- | ------------------------------------------------------------- |
| Scrape admission (`MaxRequestsInFlight: 2`)     | per target    | Immediate 503 for that target only                            |
| Gather dedup (singleflight)                     | per target    | Overlapping scrapes share one gather                          |
| Scrape deadline (promhttp `Timeout`)            | per target    | 503 for that scrape                                           |
| Collector deadline                              | per target    | Requests cancelled, error gauge raised, partial result served |
| Panic recovery (collectors, serve-only wrapper) | per collector | Logged with the target; other targets unaffected              |
| **Global upstream cap**                         | **process**   | The request waits within its own collector deadline           |

A hung target can drain the shared pool but never another target's reserved slot. **The reserved slot guarantees progress, not speed:** under a hung neighbor, a healthy Sonarr can be squeezed to one request at a time and miss its own deadline. Its error gauge then reports that correctly. The reserved share stays at one slot.

**Fate sharing.** An OOM, file-descriptor exhaustion or an unrecovered panic in a goroutine takes down every target. Panics are already well contained, and serve adds a recover wrapper around every collector. Isolation beyond panics would need one process per target (E2 or F).

**Sizing.** Worst-case upstream concurrency per target at default settings:

| Target         | Estimate                                                         |
| -------------- | ---------------------------------------------------------------- |
| Radarr         | about 7                                                          |
| Sonarr, Lidarr | about 6 + `SERIES_CONCURRENCY` (16 at the default, 38 max)       |
| Prowlarr       | about 4                                                          |
| Bazarr         | about `BAZARR__SERIES_BATCH_CONCURRENCY` + 2 (12 at the default) |
| SABnzbd        | 2                                                                |

The five targets of the worked example peak around **50** concurrent upstream requests, so the default cap of 64 doesn't bind in normal operation. It engages when a target hangs or tuning is raised.

Memory adds up across targets: a Bazarr episode walk and a large Radarr movie list decoding at the same time need the sum of both. Size the container for the concurrent peaks, set **`GOMEMLIMIT`** to the container memory limit so GC stays ahead of decode spikes, and watch the exporter's own `go_*`/`process_*` metrics on `/metrics`.

A scrape misconfigured to hit bare `/metrics` gets the self-metrics with `up=1` and no app series. Alert on `absent(<app>_system_status)` to catch it.

## 11. Deferred and out of scope

| Item                                                    | Why deferred                                                                   |
| ------------------------------------------------------- | ------------------------------------------------------------------------------ |
| `/probe?target=<name>` alias (B)                        | Can be added later over the same name-to-handler map                           |
| Header-derived timeouts                                 | Needs deadline plumbing into `Collect`                                         |
| Per-group caps (option c)                               | The natural extension if targets ever span unrelated database servers          |
| Hot reload                                              | A restart is fine for a handful of targets                                     |
| Scaling the reserved share with `SERIES_CONCURRENCY`    | One reserved slot guarantees progress; revisit if squeezed targets prove noisy |
| Config files (YAML or JSON), registry per request       | Not chosen (sections 5 and 6)                                                  |
| Tightening single-target mode's catch-all `GET /` index | Would change single-target behavior (R8)                                       |

## 12. Delivery

The work lands as self-contained slices. Each compiles and passes tests, lint and the coverage gate on its own, so each can become its own PR.

| Slice | Content                                                                                                                                                                                               |
| ----- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| S0    | This design doc                                                                                                                                                                                       |
| S1    | Test tooling: coverage gate, write-once golden files pinning 3.1.0 single-target output, in-process command harness, fake upstream apps                                                               |
| S2    | Behavior-preserving refactors: shared collector builders, `MetricsHandler(app, url, …)`, the per-target handler stack split out, `newServer(scrapeTimeout)`, config helpers for per-target resolution |
| S3    | Target-aware logging, byte-identical in single-target mode                                                                                                                                            |
| S4    | Target config package `internal/targets`: parsing, strict checks, resolution                                                                                                                          |
| S5    | `exportarr serve`: per-target stacks, routing, self-metrics, index, 404, panic wrapper                                                                                                                |
| S6    | Global upstream limiter in the transport (including form auth), cap metrics                                                                                                                           |
| S7    | Acceptance suite: isolation, cap, name-only selection, secrets, five-plus targets; fuzz targets                                                                                                       |
| S8    | README section, compose and Prometheus examples for serve mode, migration and Grafana notes, final coverage floors                                                                                    |

Migration is additive: translate each container's env into a `TARGET_<n>_*` block and point Prometheus at `/metrics/<name>` with `instance: <name>`. Series are unchanged apart from `job` and `instance`. The shipped Grafana dashboards sum across series, so two targets of one type are summed, as they already are with two containers.
