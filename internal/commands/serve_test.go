package commands

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"

	arrclient "github.com/onedr0p/exportarr/internal/arr/client"
	arrconfig "github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/assert"
	"github.com/onedr0p/exportarr/internal/client"
	"github.com/onedr0p/exportarr/internal/config"
	"github.com/onedr0p/exportarr/internal/fixtures"
	sabconfig "github.com/onedr0p/exportarr/internal/sabnzbd/config"
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

	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), serveApps, testPool(t, cfg))
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

	ts, err := buildTargets(cfg, process, serveDefaults(), serveApps, testPool(t, cfg))
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
	ts, err = buildTargets(cfg, process, serveDefaults(), serveApps, testPool(t, cfg))
	assert.NoError(t, err)
	assert.Equal(t, maxScrapeTimeout(ts), 3*time.Minute)
	assert.Equal(t, newServer(maxScrapeTimeout(ts)).WriteTimeout, 3*time.Minute+10*time.Second)
	assert.Equal(t, maxScrapeTimeout(nil), time.Duration(0))
}

// testPool is a slot pool for cfg's targets with the default capacity.
func testPool(t *testing.T, cfg *targets.Config) *client.SlotPool {
	t.Helper()
	names := make([]string, len(cfg.Targets))
	for i, tg := range cfg.Targets {
		names[i] = tg.Name
	}
	pool, err := client.NewSlotPool(max(64, len(names)+1), names)
	assert.NoError(t, err)
	return pool
}

// stubApps builds every target of app "stub" from the given collectors.
func stubApps(cs ...prometheus.Collector) map[string]appBuilder {
	return map[string]appBuilder{
		"stub": func(t *targets.Target, _ config.Config, _ arrconfig.ArrConfig, _ client.Limiter) (string, []prometheus.Collector, error) {
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

	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), stubApps(blocking), testPool(t, cfg))
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

	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), serveApps, testPool(t, cfg))
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

	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), stubApps(constCollector{desc}, constCollector{desc}), testPool(t, cfg))
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

	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), stubApps(constCollector{prometheus.NewDesc("fine", "test", nil, nil)}), testPool(t, cfg))
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

	ts, err := buildTargets(cfg, process, serveDefaults(), stubApps(), testPool(t, cfg))
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
	), testPool(t, cfg))
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

const notFoundBody = "404 page not found\n"

type stubHandler string

func (s stubHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "stub:"+string(s))
}

// serveHandlerClient serves h and returns a non-redirecting client for it.
func serveHandlerClient(t *testing.T, h http.Handler) *runningCommand {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hc := newHarnessClient()
	t.Cleanup(hc.CloseIdleConnections)
	return &runningCommand{t: t, url: srv.URL, client: hc}
}

func routingClient(t *testing.T) *runningCommand {
	t.Helper()
	useAppInfo(t)
	ts := []*target{
		{name: "sonarr-hd", handler: stubHandler("sonarr-hd")},
		{name: "radarr", handler: stubHandler("radarr")},
	}
	return serveHandlerClient(t, newServeHandler(ts, newSelfRegistry()))
}

func assertNotFound(t *testing.T, code int, body string, header http.Header, probe string) {
	t.Helper()
	assert.Equal(t, code, http.StatusNotFound)
	assert.Equal(t, body, notFoundBody)
	assert.Equal(t, header.Get("Content-Type"), "text/plain; charset=utf-8")
	assert.NotContains(t, body, probe)
}

func TestServeHandler_NotFound(t *testing.T) {
	rc := routingClient(t)
	cases := []struct{ name, method, path string }{
		{"unknown name", http.MethodGet, "/metrics/nope"},
		{"empty name", http.MethodGet, "/metrics/"},
		{"trailing slash", http.MethodGet, "/metrics/sonarr-hd/"},
		{"wrong case", http.MethodGet, "/metrics/SONARR-HD"},
		{"escaped slash inside the name", http.MethodGet, "/metrics/sonarr%2Fhd"},
		{"escaped slash after the name", http.MethodGet, "/metrics/sonarr-hd%2F"},
		{"raw url", http.MethodGet, "/metrics/http:%2F%2Fevil"},
		{"deeper path", http.MethodGet, "/metrics/sonarr-hd/extra"},
		{"post to a target", http.MethodPost, "/metrics/sonarr-hd"},
		{"post to the index", http.MethodPost, "/"},
		{"post to self metrics", http.MethodPost, "/metrics"},
		{"delete a target", http.MethodDelete, "/metrics/radarr"},
		{"other path", http.MethodGet, "/x"},
		{"markup in the path", http.MethodGet, "/%3Cscript%3Ealert(1)%3C/script%3E"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body, header := rc.Do(tc.method, tc.path, nil)
			assertNotFound(t, code, body, header, tc.path)
			assert.NotContains(t, body, "stub:")
		})
	}
}

func TestServeHandler_ServesConfiguredNames(t *testing.T) {
	rc := routingClient(t)
	cases := []struct{ name, path, want string }{
		{"sonarr", "/metrics/sonarr-hd", "stub:sonarr-hd"},
		{"radarr", "/metrics/radarr", "stub:radarr"},
		{"query ignored", "/metrics/sonarr-hd?target=http://evil", "stub:sonarr-hd"},
		{"escaped hyphen", "/metrics/sonarr%2Dhd", "stub:sonarr-hd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body, _ := rc.Do(http.MethodGet, tc.path, nil)
			assert.Equal(t, code, http.StatusOK)
			assert.Equal(t, body, tc.want)
		})
	}
}

func TestServeHandler_NonCleanPathsRedirectToSameHost(t *testing.T) {
	rc := routingClient(t)
	cases := []struct{ path, location string }{
		{"//metrics/x", "/metrics/x"},
		{"/metrics/../metrics/x", "/metrics/x"},
		{"//metrics/sonarr-hd", "/metrics/sonarr-hd"},
		{"/metrics/./radarr", "/metrics/radarr"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			code, body, header := rc.Do(http.MethodGet, tc.path, nil)
			assert.Equal(t, code, http.StatusTemporaryRedirect)
			loc := header.Get("Location")
			assert.Equal(t, loc, tc.location)
			assert.True(t, strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//"), "Location %q leaves the host", loc)
			assert.NotContains(t, body, "stub:")
		})
	}
}

func TestServeHandler_IndexHealthzAndSelfMetrics(t *testing.T) {
	rc := routingClient(t)

	code, body, header := rc.Do(http.MethodGet, "/", nil)
	assert.Equal(t, code, http.StatusOK)
	assert.Equal(t, header.Get("Content-Type"), "text/html; charset=utf-8")
	assert.Equal(t, body, "<h1>Exportarr</h1><ul><li><a href='/metrics/sonarr-hd'>sonarr-hd</a></li><li><a href='/metrics/radarr'>radarr</a></li></ul>")

	code, body = rc.Get("/healthz")
	assert.Equal(t, code, http.StatusOK)
	assert.Equal(t, body, "OK")

	code, body = rc.Get("/metrics")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, body, `exportarr_app_info{app_name="exportarr",build_time="now",revision="abc",version="1.2.3"} 1`)
	assert.Contains(t, body, "go_goroutines")
	assert.NotContains(t, body, "stub:")
}

func TestServeHandler_SelfMetricsSeparateFromTargets(t *testing.T) {
	useAppInfo(t)
	cfg := &targets.Config{Targets: []targets.Target{
		{Index: 0, Name: "one", App: "stub", URL: "http://one:1"},
	}}
	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), stubApps(constCollector{prometheus.NewDesc("target_series", "test", nil, nil)}), testPool(t, cfg))
	assert.NoError(t, err)
	rc := serveHandlerClient(t, newServeHandler(ts, newSelfRegistry()))

	code, body := rc.Get("/metrics/one")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, body, "target_series 42")
	for _, family := range []string{"go_", "process_", "exportarr_"} {
		assert.NotContains(t, body, "\n"+family)
	}

	code, body = rc.Get("/metrics")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, body, "exportarr_app_info")
	assert.Contains(t, body, "go_goroutines")
	assert.NotContains(t, body, "target_series")
	assert.NotContains(t, body, "stub_scrape_")
	assert.NotContains(t, body, "promhttp_metric_handler_errors_total")
}

func TestServeHandler_HandlerPanicIs500(t *testing.T) {
	useAppInfo(t)
	logs := captureSlog(t)
	ts := []*target{
		{name: "boom", handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("handler boom") })},
		{name: "fine", handler: stubHandler("fine")},
	}
	rc := serveHandlerClient(t, newServeHandler(ts, newSelfRegistry()))

	code, _ := rc.Get("/metrics/boom")
	assert.Equal(t, code, http.StatusInternalServerError)
	assert.Contains(t, logs.String(), `msg="panic recovered" error="handler boom"`)

	code, body := rc.Get("/metrics/fine")
	assert.Equal(t, code, http.StatusOK)
	assert.Equal(t, body, "stub:fine")
}

func TestRedactTargetURL(t *testing.T) {
	assert.Equal(t, redactTargetURL("http://sonarr:8989/base"), "http://sonarr:8989/base")
	assert.Equal(t, redactTargetURL("http://user:pw@sonarr:8989/base?apikey=x#frag"), "http://sonarr:8989/base") //nolint:gosec // redaction fixture
	assert.Equal(t, redactTargetURL("http://[::1"), "")
}

func TestBuildTargets_PassesEachTargetItsLimiter(t *testing.T) {
	cfg := &targets.Config{Targets: []targets.Target{
		{Index: 0, Name: "one", App: "stub", URL: "http://one:1"},
		{Index: 1, Name: "two", App: "stub", URL: "http://two:1"},
	}}
	pool := testPool(t, cfg)
	got := map[string]client.Limiter{}
	apps := map[string]appBuilder{
		"stub": func(t *targets.Target, _ config.Config, _ arrconfig.ArrConfig, lim client.Limiter) (string, []prometheus.Collector, error) {
			got[t.Name] = lim
			return t.URL, nil, nil
		},
	}

	_, err := buildTargets(cfg, serveProcess(), serveDefaults(), apps, pool)
	assert.NoError(t, err)
	assert.Len(t, slices.Collect(maps.Keys(got)), 2)
	for name, lim := range got {
		assert.True(t, lim == pool.For(name), "target %s did not get its own limiter", name)
	}
}

type slotStats struct {
	waits    uint64
	inFlight float64
}

// poolStats reads each target's acquire count and in-flight gauge from the pool's metrics.
func poolStats(t *testing.T, pool *client.SlotPool) map[string]slotStats {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(pool)
	mfs, err := reg.Gather()
	assert.NoError(t, err)
	out := map[string]slotStats{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			var name string
			for _, l := range m.GetLabel() {
				if l.GetName() == "target" {
					name = l.GetValue()
				}
			}
			st := out[name]
			switch mf.GetName() {
			case "exportarr_upstream_slot_wait_seconds":
				st.waits = m.GetHistogram().GetSampleCount()
			case "exportarr_upstream_requests_in_flight":
				st.inFlight = m.GetGauge().GetValue()
			}
			out[name] = st
		}
	}
	return out
}

func TestBuildTargets_PoolSeesEveryUpstreamRequest(t *testing.T) {
	creds := &fixtures.FormAuthCreds{Username: "admin", Password: "s3cret"}
	fakes := map[string]*fixtures.FakeApp{
		"sonarr-hd":  fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: "sonarr", APIKey: fixtures.APIKey}),
		"radarr-sso": fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: "radarr", APIKey: fixtures.APIKey, FormAuth: creds}),
		"sab":        fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: "sabnzbd", APIKey: fixtures.APIKey}),
	}
	cfg := &targets.Config{MaxUpstreamRequests: 64, Targets: []targets.Target{
		{Index: 0, Name: "sonarr-hd", App: "sonarr", URL: fakes["sonarr-hd"].URL, APIKey: fixtures.APIKey},
		{Index: 1, Name: "radarr-sso", App: "radarr", URL: fakes["radarr-sso"].URL, APIKey: fixtures.APIKey,
			FormAuth: true, AuthUsername: creds.Username, AuthPassword: creds.Password},
		{Index: 2, Name: "sab", App: "sabnzbd", URL: fakes["sab"].URL, APIKey: fixtures.APIKey},
	}}
	pool := testPool(t, cfg)

	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), serveApps, pool)
	assert.NoError(t, err)
	for _, tg := range ts {
		code, body := scrapeHandler(t, tg.handler)
		assert.Equal(t, code, http.StatusOK, tg.name)
		assert.NotContains(t, body, "_collector_error{", tg.name)
		assert.NotContains(t, body, "exportarr_upstream_", tg.name)
	}

	assert.Equal(t, fakes["radarr-sso"].Logins(), 1)
	stats := poolStats(t, pool)
	for name, fake := range fakes {
		requests := len(fake.Requests())
		assert.True(t, requests > 1, "%s saw %d requests", name, requests)
		assert.Equal(t, stats[name].waits, uint64(requests), "%s: every upstream request must take a slot", name)
		assert.Equal(t, stats[name].inFlight, float64(0), "%s leaked a slot", name)
	}
}

func TestNewServeSelfRegistry_ExposesUpstreamMetrics(t *testing.T) {
	useAppInfo(t)
	cfg := &targets.Config{Targets: []targets.Target{
		{Index: 0, Name: "one", App: "stub", URL: "http://one:1"},
		{Index: 1, Name: "two", App: "stub", URL: "http://two:1"},
	}}
	pool, err := client.NewSlotPool(10, []string{"one", "two"})
	assert.NoError(t, err)
	ts, err := buildTargets(cfg, serveProcess(), serveDefaults(), stubApps(constCollector{prometheus.NewDesc("target_series", "test", nil, nil)}), pool)
	assert.NoError(t, err)
	rc := serveHandlerClient(t, newServeHandler(ts, newServeSelfRegistry(pool)))

	code, body := rc.Get("/metrics")
	assert.Equal(t, code, http.StatusOK)
	for _, want := range []string{
		"exportarr_app_info{",
		"go_goroutines",
		"exportarr_upstream_requests_max 10\n",
		`exportarr_upstream_requests_in_flight{target="one"} 0` + "\n",
		`exportarr_upstream_requests_in_flight{target="two"} 0` + "\n",
		`exportarr_upstream_slot_wait_seconds_count{target="one"} 0` + "\n",
		`exportarr_upstream_slot_wait_seconds_bucket{target="two",le="30"} 0` + "\n",
		`exportarr_upstream_slot_wait_timeouts_total{target="two"} 0` + "\n",
	} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, "target_series")

	for _, name := range []string{"one", "two"} {
		code, body = rc.Get("/metrics/" + name)
		assert.Equal(t, code, http.StatusOK)
		assert.Contains(t, body, "target_series 42")
		assert.NotContains(t, body, "exportarr_upstream_")
	}
}

func TestSingleTarget_LeavesUpstreamUnlimited(t *testing.T) {
	t.Setenv("FORM_AUTH", "true")
	t.Setenv("AUTH_USERNAME", "admin")
	t.Setenv("AUTH_PASSWORD", "s3cret")
	base := serveProcess()
	base.App = "radarr"
	base.URL = "http://radarr:7878"
	base.APIKey = fixtures.APIKey
	flags := pflag.NewFlagSet("radarr", pflag.ContinueOnError)
	arrconfig.RegisterArrFlags(flags)

	c, err := arrconfig.LoadArrConfig(base, flags)
	assert.NoError(t, err)
	_, err = radarrApp.build(c)
	assert.NoError(t, err)
	assert.True(t, c.UpstreamLimiter == nil, "single-target mode must not set a limiter")
	auth, err := arrclient.NewAuth(c)
	assert.NoError(t, err)
	rt := auth.(*arrclient.FormAuth).Transport
	_, ok := rt.(*http.Transport)
	assert.True(t, ok, "single-target transport must stay *http.Transport, got %T", rt)

	sc, err := sabconfig.LoadSabnzbdConfig(base)
	assert.NoError(t, err)
	_, err = buildSabnzbd(sc)
	assert.NoError(t, err)
	assert.True(t, sc.UpstreamLimiter == nil, "single-target sabnzbd must not set a limiter")
}
