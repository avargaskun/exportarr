# Changelog

## [3.2.0](https://github.com/avargaskun/exportarr/compare/v3.1.0...v3.2.0) (2026-09-14)


### Features

* serve several named targets from one process ([#14](https://github.com/avargaskun/exportarr/issues/14)) ([028d5ad](https://github.com/avargaskun/exportarr/commit/028d5ad80415b350674befd8fe1c752d34dc8190))

## [3.1.0](https://github.com/avargaskun/exportarr/compare/52bb6eb...v3.1.0) (2026-09-13)


### Bug Fixes

* add read, write and idle timeouts to the HTTP server ([a93693c](https://github.com/avargaskun/exportarr/commit/a93693cd4e5ad79e6996ff42a6787a269fabcd00))
* bound every collector by the scrape deadline ([a93693c](https://github.com/avargaskun/exportarr/commit/a93693cd4e5ad79e6996ff42a6787a269fabcd00))
* bound queue pagination ([a93693c](https://github.com/avargaskun/exportarr/commit/a93693cd4e5ad79e6996ff42a6787a269fabcd00))
* bound the sonarr fan-out by errors, time and a configurable limit ([a93693c](https://github.com/avargaskun/exportarr/commit/a93693cd4e5ad79e6996ff42a6787a269fabcd00))
* build the listen address with net.JoinHostPort ([5c78ee2](https://github.com/avargaskun/exportarr/commit/5c78ee2e6da8169a65bc27dd53b33b723ac5115d))
* cap upstream response bodies at 256 MiB ([a93693c](https://github.com/avargaskun/exportarr/commit/a93693cd4e5ad79e6996ff42a6787a269fabcd00))
* correct the env-scrub claim and unset form-auth credentials ([49a332c](https://github.com/avargaskun/exportarr/commit/49a332cb09116c812b462283ab27811faa657697))
* decode only the prowlarr indexer settings that are read ([49a332c](https://github.com/avargaskun/exportarr/commit/49a332cb09116c812b462283ab27811faa657697))
* **deps:** bump github.com/klauspost/compress to v1.18.7 ([98322f4](https://github.com/avargaskun/exportarr/commit/98322f46b3448102139f7823ac7cb7bc632b93d3))
* disable shell completion, including cobra's hidden __complete ([5c78ee2](https://github.com/avargaskun/exportarr/commit/5c78ee2e6da8169a65bc27dd53b33b723ac5115d))
* **docker:** harden the container image ([#3](https://github.com/avargaskun/exportarr/issues/3)) ([5d5b272](https://github.com/avargaskun/exportarr/commit/5d5b27207ad69de273f6bc22b4b1bc9ffc936dfa))
* ignore HTTP(S)_PROXY unless PROXY_FROM_ENV is set ([49a332c](https://github.com/avargaskun/exportarr/commit/49a332cb09116c812b462283ab27811faa657697))
* limit concurrent and overlong /metrics scrapes ([a93693c](https://github.com/avargaskun/exportarr/commit/a93693cd4e5ad79e6996ff42a6787a269fabcd00))
* never log upstream response bodies ([49a332c](https://github.com/avargaskun/exportarr/commit/49a332cb09116c812b462283ab27811faa657697))
* redact redirect locations in request errors ([49a332c](https://github.com/avargaskun/exportarr/commit/49a332cb09116c812b462283ab27811faa657697))
* reject URLs with credentials or a query string ([49a332c](https://github.com/avargaskun/exportarr/commit/49a332cb09116c812b462283ab27811faa657697))
* release the prowlarr stats lock on panic ([a93693c](https://github.com/avargaskun/exportarr/commit/a93693cd4e5ad79e6996ff42a6787a269fabcd00))
* stop a bare PROWLARR__ variable from setting the backfill time ([5c78ee2](https://github.com/avargaskun/exportarr/commit/5c78ee2e6da8169a65bc27dd53b33b723ac5115d))
* stop reading API_VERSION from the environment ([5c78ee2](https://github.com/avargaskun/exportarr/commit/5c78ee2e6da8169a65bc27dd53b33b723ac5115d))
* use the shared HTTP client in every collector ([a93693c](https://github.com/avargaskun/exportarr/commit/a93693cd4e5ad79e6996ff42a6787a269fabcd00))
* warn when secrets are passed as command-line flags ([49a332c](https://github.com/avargaskun/exportarr/commit/49a332cb09116c812b462283ab27811faa657697))


### Documentation

* add an About this fork section and point examples at the fork's image ([59fcb47](https://github.com/avargaskun/exportarr/commit/59fcb47fe2e9c52b2a76d4fb871874a792d3a8e4))
* correct the alerting guidance, stale comments and upgrade notes ([59fcb47](https://github.com/avargaskun/exportarr/commit/59fcb47fe2e9c52b2a76d4fb871874a792d3a8e4))
