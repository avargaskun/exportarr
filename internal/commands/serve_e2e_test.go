package commands

import (
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	arrconfig "github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/assert"
	"github.com/onedr0p/exportarr/internal/client"
	"github.com/onedr0p/exportarr/internal/config"
	"github.com/onedr0p/exportarr/internal/fixtures"
	"github.com/onedr0p/exportarr/internal/targets"
)

// e2eTarget is one serve target backed by its own FakeApp; extra holds
// TARGET_<i>_<KEY> settings keyed by <KEY>.
type e2eTarget struct {
	name, app string
	opts      fixtures.FakeAppOptions
	extra     map[string]string
}

func e2eKey(i int) string { return fmt.Sprintf("e2e%02dKey0123456789abcdef", i) }

// startServe runs serve against one FakeApp per target. Target i uses
// e2eKey(i); a fake given its own APIKey keeps it, which makes a bad-auth target.
func startServe(t *testing.T, ts []e2eTarget, env map[string]string) (*runningCommand, map[string]*fixtures.FakeApp) {
	t.Helper()
	all := map[string]string{}
	fakes := make(map[string]*fixtures.FakeApp, len(ts))
	for i, tg := range ts {
		opts := tg.opts
		opts.App = tg.app
		if opts.APIKey == "" {
			opts.APIKey = e2eKey(i)
		}
		fake := fixtures.NewFakeApp(t, opts)
		fakes[tg.name] = fake
		prefix := fmt.Sprintf("TARGET_%d_", i)
		all[prefix+"NAME"] = tg.name
		all[prefix+"APP"] = tg.app
		all[prefix+"URL"] = fake.URL
		all[prefix+"API_KEY"] = e2eKey(i)
		for k, v := range tg.extra {
			all[prefix+k] = v
		}
	}
	maps.Copy(all, env)
	return startCommand(t, all, "serve"), fakes
}

type scrapeResult struct {
	code int
	body string
	dur  time.Duration
}

// scrapeConcurrently GETs every path at once; each path may appear only once.
func scrapeConcurrently(rc *runningCommand, paths ...string) map[string]scrapeResult {
	rc.t.Helper()
	out := make(map[string]scrapeResult, len(paths))
	for _, p := range paths {
		if _, dup := out[p]; dup {
			rc.t.Fatalf("scrapeConcurrently: duplicate path %s", p)
		}
		out[p] = scrapeResult{}
	}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	start := make(chan struct{})
	for _, p := range paths {
		wg.Go(func() {
			<-start
			began := time.Now()
			code, body := rc.Get(p)
			res := scrapeResult{code: code, body: body, dur: time.Since(began)}
			mu.Lock()
			out[p] = res
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	return out
}

// metricValue returns the value of the sample whose name and labels are exactly sample.
func metricValue(body, sample string) (float64, bool) {
	for line := range strings.Lines(body) {
		line = strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 || line[:i] != sample {
			continue
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		return v, err == nil
	}
	return 0, false
}

// assertOnlyGET checks every fake saw only GETs, plus the login POST on form-auth fakes.
func assertOnlyGET(t *testing.T, fakes map[string]*fixtures.FakeApp) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(fakes)) {
		f := fakes[name]
		for _, r := range f.Requests() {
			if r.Method == http.MethodGet || (r.Method == http.MethodPost && r.Path == "/login" && f.HasFormAuth()) {
				continue
			}
			t.Errorf("%s received %s %s", name, r.Method, r.Path)
		}
	}
}

// assertNoSlotLeak waits up to 5s, since a hung target's requests release only
// at its collect deadline, for every target's in-flight gauge to reach 0.
func assertNoSlotLeak(t *testing.T, rc *runningCommand, names ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := rc.Get("/metrics")
		assert.Equal(t, code, http.StatusOK)
		busy := map[string]float64{}
		for _, name := range names {
			v, ok := metricValue(body, `exportarr_upstream_requests_in_flight{target="`+name+`"}`)
			if !ok {
				t.Fatalf("no in-flight gauge for target %s", name)
			}
			if v != 0 {
				busy[name] = v
			}
		}
		if len(busy) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("upstream slots still held after 5s: %v", busy)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// assertE2EInvariants runs the checks every E2E test ends with.
func assertE2EInvariants(t *testing.T, rc *runningCommand, fakes map[string]*fixtures.FakeApp) {
	t.Helper()
	assertOnlyGET(t, fakes)
	assertNoSlotLeak(t, rc, slices.Sorted(maps.Keys(fakes))...)
}

// e2eFamilies holds one family every healthy scrape of each app exposes.
var e2eFamilies = map[string]string{
	"radarr":   "radarr_movie_total",
	"sonarr":   "sonarr_series_total",
	"lidarr":   "lidarr_artists_total",
	"prowlarr": "prowlarr_indexer_total",
	"bazarr":   "bazarr_system_status",
	"sabnzbd":  "sabnzbd_info",
}

// everyAppTargets covers all six app types, with two instances of sonarr and radarr.
func everyAppTargets() []e2eTarget {
	return []e2eTarget{
		{name: "sonarr-hd", app: "sonarr"},
		{name: "sonarr-4k", app: "sonarr"},
		{name: "radarr-hd", app: "radarr"},
		{name: "radarr-4k", app: "radarr"},
		{name: "prowlarr", app: "prowlarr"},
		{name: "lidarr", app: "lidarr"},
		{name: "bazarr", app: "bazarr"},
		{name: "sabnzbd", app: "sabnzbd"},
	}
}

func metricsPaths(ts []e2eTarget) []string {
	out := make([]string, len(ts))
	for i, tg := range ts {
		out[i] = "/metrics/" + tg.name
	}
	return out
}

// assertHealthyScrape checks a 200 with the app's family, no error gauge and only own url labels.
func assertHealthyScrape(t *testing.T, tg e2eTarget, fake *fixtures.FakeApp, res scrapeResult) {
	t.Helper()
	assert.Equal(t, res.code, http.StatusOK, tg.name)
	assert.Contains(t, res.body, e2eFamilies[tg.app]+"{", tg.name)
	assert.False(t, hasSampleSuffix(res.body, "_collector_error"), "%s has an error gauge:\n%s", tg.name, res.body)
	labels := urlLabel.FindAllStringSubmatch(res.body, -1)
	assert.True(t, len(labels) > 0, "no url labels in %s", tg.name)
	for _, m := range labels {
		assert.Equal(t, m[1], fake.URL, "foreign url label in %s", tg.name)
	}
}

var versionedAPIPath = regexp.MustCompile(`^/api/v[0-9]+/`)

func ownAPIPath(app, path string) bool {
	switch app {
	case "sabnzbd":
		return path == "/api"
	case "lidarr", "prowlarr":
		return strings.HasPrefix(path, "/api/v1/")
	case "radarr", "sonarr":
		return strings.HasPrefix(path, "/api/v3/")
	case "bazarr":
		return strings.HasPrefix(path, "/api/") && !versionedAPIPath.MatchString(path)
	}
	return false
}

func TestServeE2E_EveryAppSideBySide(t *testing.T) {
	ts := everyAppTargets()
	rc, fakes := startServe(t, ts, nil)

	res := scrapeConcurrently(rc, metricsPaths(ts)...)
	for _, tg := range ts {
		assertHealthyScrape(t, tg, fakes[tg.name], res["/metrics/"+tg.name])
	}

	code, index := rc.Get("/")
	assert.Equal(t, code, http.StatusOK)
	for _, tg := range ts {
		assert.Contains(t, index, "<li><a href='/metrics/"+tg.name+"'>"+tg.name+"</a></li>")
	}

	for _, tg := range ts {
		reqs := fakes[tg.name].Requests()
		assert.True(t, len(reqs) > 0, "%s received no requests", tg.name)
		for _, r := range reqs {
			assert.True(t, ownAPIPath(tg.app, r.Path), "%s (%s) received %s", tg.name, tg.app, r.Path)
			assert.True(t, r.HadKey, "%s received %s without its own key", tg.name, r.Path)
		}
	}

	assertE2EInvariants(t, rc, fakes)
	rc.Stop()
}

func TestServeE2E_HungTarget(t *testing.T) {
	cases := []struct {
		name    string
		maxReqs string
		bound   time.Duration
	}{
		{"default cap", "", 2 * time.Second},
		{"tightest cap", strconv.Itoa(len(everyAppTargets()) + 1), 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := everyAppTargets()
			ts[0].opts.Behavior = func(*http.Request) fixtures.Action { return fixtures.Hang() }
			ts[0].extra = map[string]string{"SCRAPE_TIMEOUT": "2s"}
			env := map[string]string{}
			if tc.maxReqs != "" {
				env["MAX_UPSTREAM_REQUESTS"] = tc.maxReqs
			}
			rc, fakes := startServe(t, ts, env)
			hung := fakes[ts[0].name]

			hungDone := make(chan scrapeResult, 1)
			go func() {
				began := time.Now()
				code, body := rc.Get("/metrics/" + ts[0].name)
				hungDone <- scrapeResult{code: code, body: body, dur: time.Since(began)}
			}()
			waitUntil(t, 5*time.Second, func() bool { return hung.InFlight() > 0 }, "the hung target received no request")
			select {
			case <-hungDone:
				t.Fatal("the hung scrape finished before the healthy scrapes started")
			default:
			}

			res := scrapeConcurrently(rc, metricsPaths(ts[1:])...)
			for _, tg := range ts[1:] {
				r := res["/metrics/"+tg.name]
				assertHealthyScrape(t, tg, fakes[tg.name], r)
				assert.True(t, r.dur < tc.bound, "%s took %s next to a hung target (bound %s)", tg.name, r.dur, tc.bound)
			}

			var h scrapeResult
			select {
			case h = <-hungDone:
			case <-time.After(harnessTimeout):
				t.Fatal("the hung scrape never returned")
			}
			assert.True(t, h.dur < 2500*time.Millisecond, "the hung scrape took %s", h.dur)
			if h.code == http.StatusOK {
				v, ok := metricValue(h.body, `sonarr_collector_error{url="`+hung.URL+`"}`)
				assert.True(t, ok && v == 1, "the hung target has no error gauge:\n%s", h.body)
			} else {
				assert.Equal(t, h.code, http.StatusServiceUnavailable)
			}

			assertE2EInvariants(t, rc, fakes)
			rc.Stop()
		})
	}
}

func TestServeE2E_FailingTarget(t *testing.T) {
	ts := []e2eTarget{
		{name: "radarr-broken", app: "radarr", opts: fixtures.FakeAppOptions{
			Behavior: func(*http.Request) fixtures.Action { return fixtures.Status(http.StatusInternalServerError) },
		}},
		{name: "radarr-ok", app: "radarr"},
		{name: "sonarr", app: "sonarr"},
		{name: "sabnzbd", app: "sabnzbd"},
	}
	rc, fakes := startServe(t, ts, nil)
	broken := fakes["radarr-broken"]
	label := `{url="` + broken.URL + `"}`

	for scrape := 1; scrape <= 2; scrape++ {
		res := scrapeConcurrently(rc, metricsPaths(ts)...)
		b := res["/metrics/radarr-broken"]
		assert.Equal(t, b.code, http.StatusOK)
		for _, gauge := range []string{
			"radarr_collector_error",
			"radarr_queue_collector_error",
			"radarr_rootfolder_collector_error",
			"radarr_diskspace_collector_error",
			"radarr_health_collector_error",
			"radarr_history_collector_error",
		} {
			v, ok := metricValue(b.body, gauge+label)
			assert.True(t, ok && v == 1, "scrape %d: %s missing from the failing target:\n%s", scrape, gauge, b.body)
		}
		v, ok := metricValue(b.body, "radarr_system_status"+label)
		assert.True(t, ok && v == 0, "scrape %d: radarr_system_status should be 0", scrape)

		for _, tg := range ts[1:] {
			assertHealthyScrape(t, tg, fakes[tg.name], res["/metrics/"+tg.name])
		}

		movies := 0
		for _, r := range broken.Requests() {
			if r.Path == "/api/v3/movie" {
				movies++
			}
		}
		assert.Equal(t, movies, 3*scrape, "scrape %d: /api/v3/movie attempts", scrape)
	}

	assertE2EInvariants(t, rc, fakes)
	rc.Stop()
}

type panicOnCollect struct{ desc *prometheus.Desc }

func (c panicOnCollect) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c panicOnCollect) Collect(chan<- prometheus.Metric) { panic("e2e boom") }

func TestServeE2E_PanickingCollector(t *testing.T) {
	saved := serveApps
	t.Cleanup(func() { serveApps = saved })
	serveApps = maps.Clone(saved)
	realSonarr := saved["sonarr"]
	serveApps["sonarr"] = func(tg *targets.Target, process config.Config, d arrconfig.ArrConfig, lim client.Limiter) (string, []prometheus.Collector, error) {
		u, cs, err := realSonarr(tg, process, d, lim)
		if err == nil && tg.Name == "sonarr-panics" {
			cs = append(cs, panicOnCollect{prometheus.NewDesc("e2e_panicky", "Panics in Collect.", nil, nil)})
		}
		return u, cs, err
	}

	ts := []e2eTarget{
		{name: "sonarr-panics", app: "sonarr"},
		{name: "sonarr-ok", app: "sonarr"},
		{name: "radarr", app: "radarr"},
		{name: "sabnzbd", app: "sabnzbd"},
	}
	rc, fakes := startServe(t, ts, nil)

	res := scrapeConcurrently(rc, metricsPaths(ts)...)
	for _, tg := range ts {
		assertHealthyScrape(t, tg, fakes[tg.name], res["/metrics/"+tg.name])
	}

	var panics []string
	for line := range strings.Lines(rc.Logs()) {
		if strings.Contains(line, `msg="collector panicked"`) {
			panics = append(panics, line)
		}
	}
	assert.Len(t, panics, 1)
	assert.Contains(t, panics[0], "target=sonarr-panics ")
	assert.Contains(t, panics[0], "panic=\"e2e boom\"")

	assertE2EInvariants(t, rc, fakes)
	rc.Stop()
}

func TestServeE2E_PerTargetScrapeTimeout(t *testing.T) {
	ts := []e2eTarget{
		{name: "slow", app: "sonarr", extra: map[string]string{"SCRAPE_TIMEOUT": "3s"}, opts: fixtures.FakeAppOptions{
			Behavior: func(*http.Request) fixtures.Action { return fixtures.Delay(10 * time.Second) },
		}},
		{name: "fast", app: "radarr"},
	}
	rc, fakes := startServe(t, ts, nil)

	res := scrapeConcurrently(rc, metricsPaths(ts)...)
	slow := res["/metrics/slow"]
	assert.True(t, slow.dur < 3500*time.Millisecond, "the slow target took %s", slow.dur)
	if slow.code == http.StatusOK {
		v, ok := metricValue(slow.body, `sonarr_collector_error{url="`+fakes["slow"].URL+`"}`)
		assert.True(t, ok && v == 1, "the slow target's partial result has no error gauge:\n%s", slow.body)
	} else {
		assert.Equal(t, slow.code, http.StatusServiceUnavailable)
	}
	fast := res["/metrics/fast"]
	assertHealthyScrape(t, ts[1], fakes["fast"], fast)
	assert.True(t, fast.dur < 2*time.Second, "the fast target took %s", fast.dur)
	assert.Equal(t, rc.Server().WriteTimeout, 2*time.Minute+10*time.Second)

	assertE2EInvariants(t, rc, fakes)
	rc.Stop()
}

func TestServeE2E_WriteTimeoutFollowsLongestTarget(t *testing.T) {
	ts := []e2eTarget{
		{name: "sonarr", app: "sonarr"},
		{name: "sabnzbd", app: "sabnzbd", extra: map[string]string{"SCRAPE_TIMEOUT": "5m"}},
	}
	rc, fakes := startServe(t, ts, nil)
	assert.Equal(t, rc.Server().WriteTimeout, 5*time.Minute+10*time.Second)

	res := scrapeConcurrently(rc, metricsPaths(ts)...)
	for _, tg := range ts {
		assertHealthyScrape(t, tg, fakes[tg.name], res["/metrics/"+tg.name])
	}

	assertE2EInvariants(t, rc, fakes)
	rc.Stop()
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s within %s", msg, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
