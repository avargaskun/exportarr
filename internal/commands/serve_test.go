package commands

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	arrconfig "github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/assert"
	"github.com/onedr0p/exportarr/internal/config"
	"github.com/onedr0p/exportarr/internal/fixtures"
	"github.com/onedr0p/exportarr/internal/targets"
)

func serveProcess() config.Config {
	return config.Config{
		App:            "serve",
		LogLevel:       "info",
		LogFormat:      "console",
		Port:           9707,
		Interface:      "0.0.0.0",
		RequestTimeout: 60 * time.Second,
		ScrapeTimeout:  2 * time.Minute,
	}
}

func serveDefaults() arrconfig.ArrConfig {
	c := arrconfig.ArrConfig{
		SeriesConcurrency: arrconfig.DefaultSeriesConcurrency,
		Bazarr:            arrconfig.BazarrConfig{SeriesBatchSize: 300, SeriesBatchConcurrency: 10},
	}
	c.ApplyBase(serveProcess())
	return c
}

func scrapeHandler(t *testing.T, h http.Handler) (int, string) {
	t.Helper()
	ts := httptest.NewServer(h)
	defer ts.Close()
	return get(t, http.MethodGet, ts.URL)
}

var urlLabel = regexp.MustCompile(`[{,]url="([^"]*)"`)

func TestServeApps_MatchesAppNames(t *testing.T) {
	got := slices.Sorted(maps.Keys(serveApps))
	want := slices.Sorted(slices.Values(targets.AppNames))
	assert.DeepEqual(t, got, want)
}

func TestBuildTargets_EveryApp(t *testing.T) {
	type spec struct {
		name, app, family string
	}
	specs := []spec{
		{"radarr", "radarr", "radarr_movie_total"},
		{"sonarr-hd", "sonarr", "sonarr_series_total"},
		{"sonarr-4k", "sonarr", "sonarr_series_total"},
		{"lidarr", "lidarr", "lidarr_artists_total"},
		{"prowlarr", "prowlarr", "prowlarr_indexer_total"},
		{"bazarr", "bazarr", "bazarr_system_status"},
		{"sab-a", "sabnzbd", "sabnzbd_info"},
		{"sab-b", "sabnzbd", "sabnzbd_info"},
	}
	cfg := &targets.Config{MaxUpstreamRequests: 64}
	for i, s := range specs {
		fake := fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: s.app, APIKey: fixtures.APIKey})
		cfg.Targets = append(cfg.Targets, targets.Target{Index: i, Name: s.name, App: s.app, URL: fake.URL, APIKey: fixtures.APIKey})
	}

	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), serveApps)
	assert.NoError(t, err)
	assert.Len(t, ts, len(specs))

	for i, s := range specs {
		tg := ts[i]
		t.Run(s.name, func(t *testing.T) {
			own := cfg.Targets[i].URL
			assert.Equal(t, tg.name, s.name)
			assert.Equal(t, tg.app, s.app)
			assert.Equal(t, tg.url, own)

			code, body := scrapeHandler(t, tg.handler)
			assert.Equal(t, code, http.StatusOK)
			assert.Contains(t, body, s.family+"{")
			assert.NotContains(t, body, "_collector_error{")
			labels := urlLabel.FindAllStringSubmatch(body, -1)
			assert.True(t, len(labels) > 0, "no url labels in %s", s.name)
			for _, m := range labels {
				assert.Equal(t, m[1], own, "foreign url label in %s", s.name)
			}

			mfs, err := tg.registry.Gather()
			assert.NoError(t, err)
			var families []string
			for _, mf := range mfs {
				families = append(families, mf.GetName())
				for _, prefix := range []string{"go_", "process_", "exportarr_"} {
					assert.False(t, strings.HasPrefix(mf.GetName(), prefix), "%s gathers %s", s.name, mf.GetName())
				}
			}
			assert.True(t, slices.Contains(families, s.family), "%s registry lacks %s", s.name, s.family)
			assert.True(t, slices.Contains(families, s.app+"_scrape_requests_total"), "%s registry lacks the scrape counter", s.name)
		})
	}
}

func TestBuildTargets_ScrapeTimeouts(t *testing.T) {
	process := serveProcess()
	cfg := &targets.Config{Targets: []targets.Target{
		{Index: 0, Name: "sonarr-fast", App: "sonarr", URL: "http://sonarr-fast:8989", APIKey: fixtures.APIKey, ScrapeTimeout: new(30 * time.Second)},
		{Index: 1, Name: "sonarr-default", App: "sonarr", URL: "http://sonarr-default:8989", APIKey: fixtures.APIKey},
		{Index: 2, Name: "sab-fast", App: "sabnzbd", URL: "http://sab-fast:8080", APIKey: fixtures.APIKey, ScrapeTimeout: new(6 * time.Second)},
		{Index: 3, Name: "sab-default", App: "sabnzbd", URL: "http://sab-default:8080", APIKey: fixtures.APIKey},
	}}
	wantScrape := []time.Duration{30 * time.Second, 2 * time.Minute, 6 * time.Second, 2 * time.Minute}
	wantCollect := []time.Duration{25 * time.Second, 115 * time.Second, 3 * time.Second, 115 * time.Second}

	ts, err := buildTargets(cfg, process, serveDefaults(), serveApps)
	assert.NoError(t, err)
	assert.Len(t, ts, 4)
	for i, tg := range ts {
		assert.Equal(t, tg.scrapeTimeout, wantScrape[i], tg.name)
		tgt := cfg.Targets[i]
		if tgt.App == "sabnzbd" {
			c, err := tgt.SabnzbdConfig(process)
			assert.NoError(t, err)
			assert.Equal(t, c.CollectTimeout, wantCollect[i], tg.name)
			continue
		}
		c, err := tgt.ArrConfig(serveDefaults(), process)
		assert.NoError(t, err)
		assert.Equal(t, c.CollectTimeout, wantCollect[i], tg.name)
	}

	assert.Equal(t, maxScrapeTimeout(ts), 2*time.Minute)
	assert.Equal(t, newServer(maxScrapeTimeout(ts)).WriteTimeout, 2*time.Minute+10*time.Second)

	cfg.Targets[0].ScrapeTimeout = new(3 * time.Minute)
	ts, err = buildTargets(cfg, process, serveDefaults(), serveApps)
	assert.NoError(t, err)
	assert.Equal(t, maxScrapeTimeout(ts), 3*time.Minute)
	assert.Equal(t, newServer(maxScrapeTimeout(ts)).WriteTimeout, 3*time.Minute+10*time.Second)
	assert.Equal(t, maxScrapeTimeout(nil), time.Duration(0))
}

// stubApps builds every target of app "stub" from the given collectors.
func stubApps(cs ...prometheus.Collector) map[string]appBuilder {
	return map[string]appBuilder{
		"stub": func(t *targets.Target, _ config.Config, _ arrconfig.ArrConfig) (string, []prometheus.Collector, error) {
			return t.URL, cs, nil
		},
	}
}

func TestBuildTargets_HandlerAppliesTargetScrapeTimeout(t *testing.T) {
	blocking := newBlockingCollector()
	defer close(blocking.release)
	cfg := &targets.Config{Targets: []targets.Target{
		{Index: 0, Name: "slow", App: "stub", URL: "http://slow:1", ScrapeTimeout: new(100 * time.Millisecond)},
	}}

	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), stubApps(blocking))
	assert.NoError(t, err)
	code, body := scrapeHandler(t, ts[0].handler)
	assert.Equal(t, code, http.StatusServiceUnavailable)
	assert.Contains(t, body, "timeout")
}

func TestBuildTargets_JoinsLabeledErrors(t *testing.T) {
	cfg := &targets.Config{Targets: []targets.Target{
		{Index: 0, Name: "radarr-key", App: "radarr", URL: "http://radarr:7878", APIKey: "short"},
		{Index: 1, Name: "sonarr-conc", App: "sonarr", URL: "http://sonarr:8989", APIKey: fixtures.APIKey, SeriesConcurrency: new(99)},
		{Index: 2, Name: "bazarr-batch", App: "bazarr", URL: "http://bazarr:6767", APIKey: fixtures.APIKey,
			Bazarr: targets.BazarrOverrides{SeriesBatchSize: new(0)}},
		{Index: 3, Name: "prowl-date", App: "prowlarr", URL: "http://prowlarr:9696", APIKey: fixtures.APIKey,
			Prowlarr: targets.ProwlarrOverrides{BackfillSinceDate: new("2024-13-01")}},
		{Index: 4, Name: "sab", App: "sabnzbd", URL: "not-a-url"},
		{Index: 5, Name: "fine", App: "sonarr", URL: "http://fine:8989", APIKey: fixtures.APIKey},
	}}

	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), serveApps)
	assert.Nil(t, ts)
	assert.Error(t, err)
	want := strings.Join([]string{
		"target 0/radarr-key: api-key must be a 20-32 character alphanumeric string",
		"target 1/sonarr-conc: series-concurrency must be between 1 and 32",
		"target 2/bazarr-batch: series-batch-size must be greater than zero",
		"target 3/prowl-date: backfill-since-date must be in the format YYYY-MM-DD",
		"target 4/sab: url must be an absolute URL (scheme://host[:port][/path])",
		"target 4/sab: api-key is required",
	}, "\n")
	assert.Equal(t, err.Error(), want)
	for line := range strings.Lines(err.Error()) {
		assert.Equal(t, strings.Count(line, "target "), 1, "double prefix in %q", line)
	}
	assert.NotContains(t, err.Error(), "short")
}

func TestBuildTargets_RegistrationError(t *testing.T) {
	desc := prometheus.NewDesc("dup_series", "test", nil, nil)
	cfg := &targets.Config{Targets: []targets.Target{
		{Index: 0, Name: "dup", App: "stub", URL: "http://dup:1"},
	}}

	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), stubApps(constCollector{desc}, constCollector{desc}))
	assert.Nil(t, ts)
	assert.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "target 0/dup: "), "unlabeled: %q", err.Error())
	assert.Contains(t, err.Error(), "duplicate")
	assert.Equal(t, strings.Count(err.Error(), "target "), 1)
}

func TestBuildTargets_OneBadTargetBuildsNothing(t *testing.T) {
	cfg := &targets.Config{Targets: []targets.Target{
		{Index: 0, Name: "good", App: "stub", URL: "http://good:1"},
		{Index: 1, Name: "unknown", App: "nope", URL: "http://unknown:1"},
	}}

	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), stubApps(constCollector{prometheus.NewDesc("fine", "test", nil, nil)}))
	assert.Nil(t, ts)
	assert.Error(t, err)
	assert.Equal(t, err.Error(), "target 1/unknown: unsupported app")
}

func TestBuildTargets_RejectsNonPositiveScrapeTimeout(t *testing.T) {
	process := serveProcess()
	process.ScrapeTimeout = 0
	cfg := &targets.Config{Targets: []targets.Target{
		{Index: 0, Name: "inherits", App: "stub", URL: "http://a:1"},
		{Index: 1, Name: "overrides", App: "stub", URL: "http://b:1", ScrapeTimeout: new(time.Second)},
	}}

	ts, err := buildTargets(cfg, process, serveDefaults(), stubApps())
	assert.Nil(t, ts)
	assert.Error(t, err)
	assert.Equal(t, err.Error(), "target 0/inherits: SCRAPE_TIMEOUT must be greater than zero")
}

type panicCollector struct{ desc *prometheus.Desc }

func (c panicCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c panicCollector) Collect(chan<- prometheus.Metric) { panic("boom") }

func TestSafeCollector_RecoversPanic(t *testing.T) {
	logs := captureSlog(t)
	cfg := &targets.Config{Targets: []targets.Target{
		{Index: 0, Name: "sonarr-hd", App: "stub", URL: "http://sonarr-hd:8989"},
	}}
	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), stubApps(
		panicCollector{prometheus.NewDesc("panicky", "test", nil, nil)},
		constCollector{prometheus.NewDesc("fine", "test", nil, nil)},
	))
	assert.NoError(t, err)

	code, body := scrapeHandler(t, ts[0].handler)
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, body, "fine 42")
	assert.NotContains(t, body, "panicky")
	assert.Contains(t, logs.String(), `level=ERROR msg="collector panicked" target=sonarr-hd collector=commands.panicCollector panic=boom`)
}

func TestSafeCollector_DescribePassesThrough(t *testing.T) {
	desc := prometheus.NewDesc("described", "test", nil, nil)
	ch := make(chan *prometheus.Desc, 1)
	safeCollector{inner: constCollector{desc}, target: "x"}.Describe(ch)
	assert.True(t, <-ch == desc, "Describe must forward the inner descriptor")
}

// multiErr wraps several errors without being an errors.Join.
type multiErr []error

func (m multiErr) Error() string   { return "several: " + m[0].Error() }
func (m multiErr) Unwrap() []error { return m }

func TestLabelErr(t *testing.T) {
	a, b, c := errors.New("a failed"), errors.New("b failed"), errors.New("c failed")
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"single", a, "target 1/x: a failed"},
		{"joined", errors.Join(a, b), "target 1/x: a failed\ntarget 1/x: b failed"},
		{"nested join", errors.Join(a, errors.Join(b, c)), "target 1/x: a failed\ntarget 1/x: b failed\ntarget 1/x: c failed"},
		{"wrapped", fmt.Errorf("outer: %w", errors.Join(a, b)), "target 1/x: outer: a failed\nb failed"},
		{"multi-wrap is not a join", fmt.Errorf("%w and %w", a, b), "target 1/x: a failed and b failed"},
		{"custom multi-error", multiErr{a, b}, "target 1/x: several: a failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := labelErr("target 1/x", tc.err)
			assert.Equal(t, got.Error(), tc.want)
			assert.True(t, errors.Is(got, a), "labelErr must keep the chain")
		})
	}
}
