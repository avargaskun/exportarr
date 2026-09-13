package commands

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
// e2eKey(i) unless extra sets API_KEY_FILE; a fake given its own APIKey keeps
// it, which makes a bad-auth target.
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
		if _, fromFile := tg.extra["API_KEY_FILE"]; !fromFile {
			all[prefix+"API_KEY"] = e2eKey(i)
		}
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

func TestServeE2E_GlobalCap(t *testing.T) {
	const capacity = 10
	tracker := &fixtures.ConcurrencyTracker{}
	ts := everyAppTargets()[1:]
	for i := range ts {
		ts[i].opts.Tracker = tracker
		ts[i].opts.Behavior = func(*http.Request) fixtures.Action { return fixtures.Delay(50 * time.Millisecond) }
	}
	rc, fakes := startServe(t, ts, map[string]string{"MAX_UPSTREAM_REQUESTS": strconv.Itoa(capacity)})

	res := scrapeConcurrently(rc, metricsPaths(ts)...)
	for _, tg := range ts {
		assertHealthyScrape(t, tg, fakes[tg.name], res["/metrics/"+tg.name])
	}
	assert.True(t, tracker.Peak() <= capacity, "%d upstream requests in flight at once, cap %d", tracker.Peak(), capacity)
	assert.True(t, tracker.Peak() > 1, "the scrapes never overlapped upstream")

	assertNoSlotLeak(t, rc, slices.Sorted(maps.Keys(fakes))...)
	code, self := rc.Get("/metrics")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, self, "exportarr_upstream_requests_max "+strconv.Itoa(capacity)+"\n")
	waited := false
	for _, tg := range ts {
		count, ok := metricValue(self, `exportarr_upstream_slot_wait_seconds_count{target="`+tg.name+`"}`)
		assert.True(t, ok && count > 0, "%s took no upstream slot", tg.name)
		assert.Equal(t, int(count), len(fakes[tg.name].Requests()), "%s: slots taken vs requests received", tg.name)
		fast, ok := metricValue(self, `exportarr_upstream_slot_wait_seconds_bucket{target="`+tg.name+`",le="0.001"}`)
		assert.True(t, ok, "no le=0.001 bucket for %s", tg.name)
		if fast < count {
			waited = true
		}
	}
	assert.True(t, waited, "no request waited for a slot under the cap:\n%s", self)

	assertE2EInvariants(t, rc, fakes)
	rc.Stop()
}

func TestServeE2E_FormAuthUnderCap(t *testing.T) {
	const formUser, formPhrase = "e2e-user", "e2e-formpw"
	tracker := &fixtures.ConcurrencyTracker{}
	ts := []e2eTarget{
		{name: "radarr-form", app: "radarr",
			opts: fixtures.FakeAppOptions{
				FormAuth: &fixtures.FormAuthCreds{Username: formUser, Password: formPhrase},
				Tracker:  tracker,
			},
			extra: map[string]string{"FORM_AUTH": "true", "AUTH_USERNAME": formUser, "AUTH_PASSWORD": formPhrase},
		},
		{name: "sonarr", app: "sonarr", opts: fixtures.FakeAppOptions{Tracker: tracker}},
	}
	capacity := len(ts) + 1
	rc, fakes := startServe(t, ts, map[string]string{"MAX_UPSTREAM_REQUESTS": strconv.Itoa(capacity)})
	form := fakes["radarr-form"]

	var (
		mu      sync.Mutex
		results []scrapeResult
		wg      sync.WaitGroup
	)
	start := make(chan struct{})
	for range 5 {
		wg.Go(func() {
			<-start
			code, body := rc.Get("/metrics/radarr-form")
			mu.Lock()
			results = append(results, scrapeResult{code: code, body: body})
			mu.Unlock()
		})
	}
	var other scrapeResult
	wg.Go(func() {
		<-start
		other.code, other.body = rc.Get("/metrics/sonarr")
	})
	close(start)
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(harnessTimeout):
		t.Fatalf("scrapes did not finish within %s under a cap of %d", harnessTimeout, capacity)
	}

	ok := 0
	for _, r := range results {
		assert.True(t, r.code == http.StatusOK || r.code == http.StatusServiceUnavailable, "unexpected status %d", r.code)
		if r.code == http.StatusOK {
			ok++
			assertHealthyScrape(t, ts[0], form, r)
		}
	}
	assert.True(t, ok > 0, "no concurrent scrape of the form-auth target succeeded")
	assertHealthyScrape(t, ts[1], fakes["sonarr"], other)

	code, body := rc.Get("/metrics/radarr-form")
	assertHealthyScrape(t, ts[0], form, scrapeResult{code: code, body: body})

	assert.Equal(t, form.Logins(), 1)
	logins := 0
	for _, r := range form.Requests() {
		if r.Method == http.MethodPost && r.Path == "/login" {
			logins++
		}
	}
	assert.Equal(t, logins, 1, "login requests")
	assert.True(t, tracker.Peak() <= int64(capacity), "%d upstream requests in flight at once, cap %d", tracker.Peak(), capacity)

	assertNoSlotLeak(t, rc, "radarr-form", "sonarr")
	_, self := rc.Get("/metrics")
	count, found := metricValue(self, `exportarr_upstream_slot_wait_seconds_count{target="radarr-form"}`)
	assert.True(t, found, "no slot wait count for radarr-form")
	assert.Equal(t, int(count), len(form.Requests()), "slots taken (login included) vs requests received")

	assertE2EInvariants(t, rc, fakes)
	rc.Stop()
}

func TestServeE2E_NameOnlySelection(t *testing.T) {
	c := fixtures.NewCanary(t)
	canaryHost := strings.TrimPrefix(c.URL, "http://")
	ts := []e2eTarget{
		{name: "sonarr-hd", app: "sonarr"},
		{name: "sabnzbd", app: "sabnzbd"},
	}
	rc, fakes := startServe(t, ts, nil)
	byName := map[string]e2eTarget{}
	for _, tg := range ts {
		byName[tg.name] = tg
	}

	q := url.QueryEscape(c.URL)
	probes := []struct {
		name, path string
		header     http.Header
		serves     string
	}{
		{"escaped canary url as the name", "/metrics/" + url.PathEscape(c.URL), nil, ""},
		{"canary host as the name", "/metrics/" + canaryHost, nil, ""},
		{"target query", "/metrics/sonarr-hd?target=" + q, nil, "sonarr-hd"},
		{"url query", "/metrics/sonarr-hd?url=" + q, nil, "sonarr-hd"},
		{"repeated target query", "/metrics/sabnzbd?target=" + q + "&target=" + q + "%2Fapi", nil, "sabnzbd"},
		{"empty target query", "/metrics/sonarr-hd?target=", nil, "sonarr-hd"},
		{"target query on an unknown name", "/metrics/nope?target=" + q, nil, ""},
		{"target query on the empty name", "/metrics/?target=" + q, nil, ""},
		{"X-Target header", "/metrics/sonarr-hd", http.Header{"X-Target": {c.URL}}, "sonarr-hd"},
		{"Host header", "/metrics/sabnzbd", http.Header{"Host": {canaryHost}}, "sabnzbd"},
		{"X-Target header on an unknown name", "/metrics/nope", http.Header{"X-Target": {c.URL}}, ""},
		{"Host header on an unknown name", "/metrics/nope", http.Header{"Host": {canaryHost}}, ""},
	}
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			code, body, header := rc.Do(http.MethodGet, p.path, p.header)
			if p.serves == "" {
				assertNotFound(t, code, body, header, c.URL)
				assert.NotContains(t, body, canaryHost)
				return
			}
			tg := byName[p.serves]
			assertHealthyScrape(t, tg, fakes[tg.name], scrapeResult{code: code, body: body})
		})
	}
	assert.Equal(t, c.Hits(), 0)

	assertE2EInvariants(t, rc, fakes)
	rc.Stop()
}

func TestServeE2E_Secrets(t *testing.T) {
	const (
		inlineKey    = "Secretkeyzero0000000000000000000"
		fileKey      = "Secretkeyfile1111111111111111111"
		formUser     = "e2e-formuser"
		formPassword = "hunter2-formpw"
		fakeOnlyKey  = "Fakeonlykey222222222222222222222"
	)
	keyFile := filepath.Join(t.TempDir(), "radarr.key")
	assert.NoError(t, os.WriteFile(keyFile, []byte(fileKey+"\n"), 0o600))
	status500 := func(*http.Request) fixtures.Action { return fixtures.Status(http.StatusInternalServerError) }
	ts := []e2eTarget{
		{name: "sonarr-inline", app: "sonarr", opts: fixtures.FakeAppOptions{APIKey: inlineKey}},
		{name: "radarr-file", app: "radarr", opts: fixtures.FakeAppOptions{APIKey: fileKey},
			extra: map[string]string{"API_KEY_FILE": keyFile}},
		{name: "radarr-form", app: "radarr",
			opts:  fixtures.FakeAppOptions{FormAuth: &fixtures.FormAuthCreds{Username: formUser, Password: formPassword}},
			extra: map[string]string{"FORM_AUTH": "true", "AUTH_USERNAME": formUser, "AUTH_PASSWORD": formPassword},
		},
		{name: "lidarr-failing", app: "lidarr", opts: fixtures.FakeAppOptions{Behavior: status500}},
		{name: "sabnzbd-badauth", app: "sabnzbd", opts: fixtures.FakeAppOptions{APIKey: fakeOnlyKey}},
		{name: "sabnzbd", app: "sabnzbd"},
	}
	rc, fakes := startServe(t, ts, map[string]string{"LOG_LEVEL": "debug", "TARGET_0_API_KEY": inlineKey})
	secrets := []string{inlineKey, fileKey, formUser, formPassword, fakeOnlyKey}
	for i := 2; i < len(ts); i++ {
		secrets = append(secrets, e2eKey(i))
	}

	for i := range ts {
		for _, key := range []string{"API_KEY", "API_KEY_FILE", "AUTH_USERNAME", "AUTH_PASSWORD"} {
			name := fmt.Sprintf("TARGET_%d_%s", i, key)
			v, set := os.LookupEnv(name)
			assert.False(t, set, "%s is still set (%d bytes)", name, len(v))
		}
	}

	bodies := map[string]string{}
	for scrape := 1; scrape <= 2; scrape++ {
		res := scrapeConcurrently(rc, metricsPaths(ts)...)
		for _, tg := range ts {
			r := res["/metrics/"+tg.name]
			bodies[fmt.Sprintf("scrape %d of %s", scrape, tg.name)] = r.body
			switch tg.name {
			case "lidarr-failing", "sabnzbd-badauth":
				assert.Equal(t, r.code, http.StatusOK, tg.name)
				v, ok := metricValue(r.body, tg.app+`_collector_error{url="`+fakes[tg.name].URL+`"}`)
				assert.True(t, ok && v == 1, "%s has no error gauge:\n%s", tg.name, r.body)
			default:
				assertHealthyScrape(t, tg, fakes[tg.name], r)
			}
		}
	}
	for _, path := range []string{"/metrics", "/", "/metrics/nope", "/nope"} {
		_, bodies[path] = rc.Get(path)
	}
	assert.Equal(t, bodies["/metrics/nope"], notFoundBody)
	assert.Equal(t, bodies["/nope"], notFoundBody)

	assertE2EInvariants(t, rc, fakes)
	rc.Stop()

	logs := rc.Logs()
	assert.Contains(t, logs, "level=DEBUG")
	assert.Contains(t, logs, "level=ERROR")
	for _, s := range secrets {
		assert.NotContains(t, logs, s, "logs")
		for what, body := range bodies {
			assert.NotContains(t, body, s, what)
		}
	}
}

func TestServeE2E_SecretFlagRejected(t *testing.T) {
	const flagKey = "Secretflagkey3333333333333333333"
	res := runCommandOutput(t, map[string]string{
		"TARGET_0_NAME":    "sonarr",
		"TARGET_0_APP":     "sonarr",
		"TARGET_0_URL":     "http://sonarr:8989",
		"TARGET_0_API_KEY": e2eKey(0),
	}, "serve", "--api-key", flagKey)
	assert.Error(t, res.Err)
	assert.Contains(t, res.Err.Error(),
		"--api-key is not supported by serve: URLs and credentials are set per target (TARGET_<n>_API_KEY or TARGET_<n>_API_KEY_FILE)")
	assert.Contains(t, res.Logs, `level=WARN msg="Secret passed as a command-line flag`)
	assert.Contains(t, res.Logs, "flag=--api-key")
	for what, s := range map[string]string{"error": res.Err.Error(), "output": res.Out, "logs": res.Logs} {
		assert.NotContains(t, s, flagKey, what)
		assert.NotContains(t, s, e2eKey(0), what)
	}
}

func TestServeE2E_FailClosedStartup(t *testing.T) {
	const (
		keyA     = "Brokenkeyaaaaaaaaaaaaaaaaaaaaaa1"
		keyB     = "Brokenkeybbbbbbbbbbbbbbbbbbbbbb2"
		password = "hunter2-brokenpw"
		badKey   = "Shortsecret9"
	)
	target := func(i int, name, app, u, key string) map[string]string {
		p := fmt.Sprintf("TARGET_%d_", i)
		return map[string]string{p + "NAME": name, p + "APP": app, p + "URL": u, p + "API_KEY": key}
	}
	merge := func(ms ...map[string]string) map[string]string {
		out := map[string]string{}
		for _, m := range ms {
			maps.Copy(out, m)
		}
		return out
	}
	sonarr := target(0, "sonarr", "sonarr", "http://sonarr:8989", keyA)
	radarr := merge(target(1, "radarr", "radarr", "http://radarr:7878", keyB), map[string]string{
		"TARGET_1_FORM_AUTH": "true", "TARGET_1_AUTH_USERNAME": "admin", "TARGET_1_AUTH_PASSWORD": password,
	})
	two := merge(sonarr, radarr)
	missing := filepath.Join(t.TempDir(), "missing.key")

	cases := []struct {
		name    string
		env     map[string]string
		want    []string
		secrets []string
	}{
		{"gap", merge(sonarr, target(2, "radarr", "radarr", "http://radarr:7878", keyB)),
			[]string{"TARGET_2_*: target indices must be contiguous from 0 (missing TARGET_1_*)"}, nil},
		{"unknown key", merge(two, map[string]string{"TARGET_1_URLL": "http://radarr:7879"}),
			[]string{"TARGET_1_URLL: unknown setting"}, []string{"radarr:7879"}},
		{"duplicate name", merge(sonarr, target(1, "sonarr", "radarr", "http://radarr:7878", keyB)),
			[]string{"target 1/sonarr: duplicate name (also target 0)"}, nil},
		{"base url", merge(two, map[string]string{"URL": "http://base-secret:8989"}),
			[]string{"URL is not supported by serve: URLs and credentials are set per target (TARGET_<n>_URL)"}, []string{"base-secret"}},
		{"cap equals the number of targets", merge(two, map[string]string{"MAX_UPSTREAM_REQUESTS": "2"}),
			[]string{"MAX_UPSTREAM_REQUESTS must be at least the number of targets + 1 (3)"}, nil},
		{"invalid per-target api key", merge(two, map[string]string{"TARGET_0_API_KEY": badKey}),
			[]string{"target 0/sonarr: api-key must be a 20-32 character alphanumeric string"}, []string{badKey}},
		{"series concurrency not an int", merge(two, map[string]string{"TARGET_0_SERIES_CONCURRENCY": "abc"}),
			[]string{"TARGET_0_SERIES_CONCURRENCY: invalid int"}, []string{"abc"}},
		{"missing key file", merge(two, map[string]string{"TARGET_1_API_KEY_FILE": missing}),
			[]string{"TARGET_1_API_KEY_FILE: cannot read file " + missing}, nil},
		{"prowlarr setting on sonarr", merge(two, map[string]string{"TARGET_0_PROWLARR__BACKFILL": "true"}),
			[]string{"target 0/sonarr: PROWLARR__BACKFILL is not valid for app sonarr"}, nil},
		{"duplicate url", merge(sonarr, target(1, "sonarr-4k", "sonarr", "http://SONARR:8989/", keyB)),
			[]string{"target 1/sonarr-4k: same url as target 0/sonarr"}, []string{"sonarr:8989", "SONARR:8989"}},
		{"three faults", merge(two, map[string]string{
			"TARGET_0_FORM_AUTH": "maybe",
			"TARGET_1_NAME":      "Radarr <secret>",
			"TARGET_1_URLL":      "http://radarr:7879",
		}), []string{
			"TARGET_0_FORM_AUTH: invalid bool",
			"TARGET_1_URLL: unknown setting",
			"target 1: name must match ^[a-z0-9][a-z0-9_-]{0,62}$",
		}, []string{"maybe", "<secret>", "radarr:7879"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCommandErr(t, tc.env, "serve")
			assert.Error(t, err)
			msg := err.Error()
			for _, want := range tc.want {
				assert.Equal(t, strings.Count(msg, want), 1, "%q in:\n%s", want, msg)
			}
			assert.Equal(t, strings.Count(msg, "\n")+1, len(tc.want), "messages in:\n%s", msg)
			for _, s := range append([]string{keyA, keyB, password}, tc.secrets...) {
				assert.NotContains(t, msg, s)
			}
		})
	}
}
