package commands

import (
	"errors"
	"flag"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/onedr0p/exportarr/internal/assert"
	"github.com/onedr0p/exportarr/internal/fixtures"
)

var update = flag.Bool("update", false, "rewrite golden files (S1 only)")

const (
	goldenDir     = "testdata/golden"
	goldenFakeURL = "http://fixture"
)

// compareGolden rewrites the golden file under -update and compares otherwise.
func compareGolden(t *testing.T, path, got string) {
	t.Helper()
	if *update {
		assert.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		assert.NoError(t, os.WriteFile(path, []byte(got), 0o600))
		return
	}
	assertGolden(t, path, got)
}

// assertGolden compares against a golden file and never rewrites it.
func assertGolden(t *testing.T, path, got string) {
	t.Helper()
	want, err := os.ReadFile(path) //nolint:gosec // path built from test constants
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the output (-want +got):\n%s", path, lineDiff(string(want), got))
	}
}

// lineDiff renders a unified-style line diff with three lines of context.
func lineDiff(want, got string) string {
	a, b := strings.Split(want, "\n"), strings.Split(got, "\n")
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i, ai := range slices.Backward(a) {
		for j, bj := range slices.Backward(b) {
			if ai == bj {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	var lines []string
	for i, j := 0, 0; i < len(a) || j < len(b); {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			lines = append(lines, " "+a[i])
			i++
			j++
		case i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]):
			lines = append(lines, "-"+a[i])
			i++
		default:
			lines = append(lines, "+"+b[j])
			j++
		}
	}

	const context = 3
	keep := make([]bool, len(lines))
	for k, l := range lines {
		if l[0] == ' ' {
			continue
		}
		for c := max(0, k-context); c <= min(len(lines)-1, k+context); c++ {
			keep[c] = true
		}
	}
	var sb strings.Builder
	sb.WriteString("--- want\n+++ got\n")
	skipped := false
	for k, l := range lines {
		if !keep[k] {
			skipped = true
			continue
		}
		if skipped {
			sb.WriteString("@@\n")
			skipped = false
		}
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	return sb.String()
}

func metricName(line string) string {
	if rest, ok := strings.CutPrefix(line, "# "); ok {
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			return ""
		}
		return fields[1]
	}
	if i := strings.IndexAny(line, "{ "); i >= 0 {
		return line[:i]
	}
	return line
}

func isTimedFamily(name string) bool {
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		name = strings.TrimSuffix(name, suffix)
	}
	return strings.HasSuffix(name, "_scrape_duration_seconds") || strings.HasSuffix(name, "_query_duration_seconds")
}

// normalizeMetrics drops the go_/process_ families, replaces the fake's URL and
// masks the values of timing samples.
func normalizeMetrics(body, fakeURL string) string {
	var sb strings.Builder
	for line := range strings.Lines(body) {
		line = strings.TrimSuffix(line, "\n")
		name := metricName(line)
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			continue
		}
		line = strings.ReplaceAll(line, fakeURL, goldenFakeURL)
		if !strings.HasPrefix(line, "#") && isTimedFamily(name) {
			if i := strings.LastIndexByte(line, ' '); i >= 0 {
				line = line[:i+1] + "X"
			}
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// normalizeLogs strips timestamps and the fake's URL, then sorts the lines
// because collectors log from concurrent goroutines.
func normalizeLogs(logs, fakeURL string) string {
	var lines []string
	for line := range strings.Lines(logs) {
		line = strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(line, "time=") {
			if i := strings.IndexByte(line, ' '); i >= 0 {
				line = line[i+1:]
			}
		}
		line = strings.ReplaceAll(line, fakeURL, goldenFakeURL)
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n") + "\n"
}

func hasSample(metrics, name string) bool {
	for line := range strings.Lines(metrics) {
		if !strings.HasPrefix(line, "#") && metricName(line) == name {
			return true
		}
	}
	return false
}

func hasSampleSuffix(metrics, suffix string) bool {
	for line := range strings.Lines(metrics) {
		if !strings.HasPrefix(line, "#") && strings.HasSuffix(metricName(line), suffix) {
			return true
		}
	}
	return false
}

type goldenApp struct {
	app  string
	fail func(*http.Request) bool
}

func failPath(path string) func(*http.Request) bool {
	return func(r *http.Request) bool { return r.URL.Path == path }
}

var goldenApps = []goldenApp{
	{app: "radarr", fail: failPath("/api/v3/movie")},
	{app: "sonarr", fail: failPath("/api/v3/series")},
	{app: "lidarr", fail: failPath("/api/v1/artist")},
	{app: "prowlarr", fail: failPath("/api/v1/indexer")},
	{app: "bazarr", fail: failPath("/api/badges")},
	{app: "sabnzbd", fail: func(r *http.Request) bool {
		return r.URL.Path == "/api" && r.URL.Query().Get("mode") == "server_stats"
	}},
}

func goldenPath(app string, failing bool, ext string) string {
	name := app
	if failing {
		name += "_error"
	}
	return filepath.Join(goldenDir, name+ext)
}

// runGoldenCase runs one subcommand against its fake, scrapes /metrics twice
// and returns the normalized second body and logs.
func runGoldenCase(t *testing.T, g goldenApp, failing bool, extraEnv map[string]string) (metrics, logs string) {
	t.Helper()
	opts := fixtures.FakeAppOptions{App: g.app, APIKey: fixtures.APIKey}
	if failing {
		opts.Behavior = func(r *http.Request) fixtures.Action {
			if g.fail(r) {
				return fixtures.Status(http.StatusInternalServerError)
			}
			return fixtures.Serve()
		}
	}
	fake := fixtures.NewFakeApp(t, opts)
	env := map[string]string{"URL": fake.URL, "API_KEY": fixtures.APIKey}
	maps.Copy(env, extraEnv)
	rc := startCommand(t, env, g.app)

	var body string
	for range 2 {
		var code int
		code, body = rc.Get("/metrics")
		assert.Equal(t, code, http.StatusOK)
	}
	code, health := rc.Get("/healthz")
	assert.Equal(t, code, http.StatusOK)
	assert.Equal(t, health, "OK")
	code, index := rc.Get("/")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, index, "href='/metrics'")
	code, _, _ = rc.Do(http.MethodPost, "/metrics", nil)
	assert.Equal(t, code, http.StatusMethodNotAllowed)
	rc.Stop()

	assert.Contains(t, body, "go_goroutines")
	metrics = normalizeMetrics(body, fake.URL)
	logs = normalizeLogs(rc.Logs(), fake.URL)
	if failing {
		assert.True(t, hasSample(metrics, g.app+"_collector_error"), "no %s_collector_error sample", g.app)
		assert.Contains(t, logs, "level=ERROR")
	} else {
		assert.False(t, hasSampleSuffix(metrics, "_collector_error"), "unexpected _collector_error sample")
		assert.NotContains(t, logs, "level=ERROR")
	}
	return metrics, logs
}

func TestGolden(t *testing.T) {
	for _, g := range goldenApps {
		for _, failing := range []bool{false, true} {
			name := g.app + "/ok"
			if failing {
				name = g.app + "/error"
			}
			t.Run(name, func(t *testing.T) {
				metrics, logs := runGoldenCase(t, g, failing, nil)
				compareGolden(t, goldenPath(g.app, failing, ".metrics"), metrics)
				compareGolden(t, goldenPath(g.app, failing, ".log"), logs)
			})
		}
	}
}

func TestGolden_TargetEnvIgnoredInSingleTargetMode(t *testing.T) {
	extra := map[string]string{
		"TARGET_0_NAME":         "other",
		"TARGET_0_APP":          "sonarr",
		"TARGET_0_URL":          "http://127.0.0.1:1",
		"TARGET_1_URLL":         "typo",
		"MAX_UPSTREAM_REQUESTS": "1",
	}
	for _, g := range goldenApps {
		t.Run(g.app, func(t *testing.T) {
			metrics, logs := runGoldenCase(t, g, false, extra)
			assertGolden(t, goldenPath(g.app, false, ".metrics"), metrics)
			assertGolden(t, goldenPath(g.app, false, ".log"), logs)
		})
	}
}

type startupErrorCase struct {
	name string
	args []string
	env  map[string]string
}

// withValidTarget adds a well-formed URL and API key, so only the case's own error fires.
func withValidTarget(env map[string]string) map[string]string {
	out := map[string]string{"URL": "http://host:7878", "API_KEY": fixtures.APIKey}
	maps.Copy(out, env)
	return out
}

var startupErrorCases = []startupErrorCase{
	{name: "radarr without url or api key", args: []string{"radarr"}},
	{name: "radarr url with credentials", args: []string{"radarr"}, env: map[string]string{"URL": "http://user:pw@host:7878", "API_KEY": fixtures.APIKey}}, //nolint:gosec // rejected-credentials fixture
	{name: "radarr url with query", args: []string{"radarr"}, env: map[string]string{"URL": "http://host:7878/?apikey=x", "API_KEY": fixtures.APIKey}},
	{name: "radarr short api key", args: []string{"radarr"}, env: map[string]string{"URL": "http://host:7878", "API_KEY": "short"}},
	{name: "sonarr series concurrency zero", args: []string{"sonarr"}, env: withValidTarget(map[string]string{"SERIES_CONCURRENCY": "0"})},
	{name: "sonarr series concurrency not a number", args: []string{"sonarr"}, env: withValidTarget(map[string]string{"SERIES_CONCURRENCY": "abc"})},
	{name: "lidarr form auth without credentials", args: []string{"lidarr"}, env: withValidTarget(map[string]string{"FORM_AUTH": "true"})},
	{name: "lidarr username without form auth", args: []string{"lidarr"}, env: withValidTarget(map[string]string{"AUTH_USERNAME": "u"})},
	{name: "prowlarr invalid backfill date from env", args: []string{"prowlarr"}, env: withValidTarget(map[string]string{"PROWLARR__BACKFILL_SINCE_DATE": "2024-13-01"})},
	{name: "prowlarr invalid backfill date from flag", args: []string{"prowlarr", "--backfill-since-date", "2024-13-01"}, env: withValidTarget(nil)},
	{name: "bazarr series batch size zero", args: []string{"bazarr"}, env: withValidTarget(map[string]string{"BAZARR__SERIES_BATCH_SIZE": "0"})},
	{name: "sabnzbd without api key", args: []string{"sabnzbd"}, env: map[string]string{"URL": "http://host:8080"}},
	{name: "sabnzbd url not absolute", args: []string{"sabnzbd"}, env: map[string]string{"URL": "not-a-url", "API_KEY": fixtures.APIKey}},
	{name: "port zero", args: []string{"radarr"}, env: withValidTarget(map[string]string{"PORT": "0"})},
	{name: "unknown log level", args: []string{"radarr"}, env: withValidTarget(map[string]string{"LOG_LEVEL": "verbose"})},
	{name: "unknown log format", args: []string{"radarr"}, env: withValidTarget(map[string]string{"LOG_FORMAT": "xml"})},
	{name: "interface not an ip", args: []string{"radarr"}, env: withValidTarget(map[string]string{"INTERFACE": "not-an-ip"})},
	{name: "scrape timeout zero", args: []string{"radarr"}, env: withValidTarget(map[string]string{"SCRAPE_TIMEOUT": "0s"})},
	{name: "api key file missing", args: []string{"radarr"}, env: map[string]string{"URL": "http://host:7878", "API_KEY_FILE": "/nonexistent/exportarr-key"}},
	{name: "sonarr unknown flag", args: []string{"sonarr", "--bogus"}, env: withValidTarget(nil)},
	{name: "every base setting invalid", args: []string{"radarr"}, env: withValidTarget(map[string]string{
		"PORT": "0", "LOG_LEVEL": "verbose", "LOG_FORMAT": "xml", "INTERFACE": "not-an-ip", "SCRAPE_TIMEOUT": "0s",
	})},
	{name: "base error before app error", args: []string{"radarr"}, env: map[string]string{"PORT": "0"}},
}

func renderStartupError(c startupErrorCase, err error) string {
	env := "(none)"
	if len(c.env) > 0 {
		var pairs []string
		for _, k := range slices.Sorted(maps.Keys(c.env)) {
			pairs = append(pairs, k+"="+c.env[k])
		}
		env = strings.Join(pairs, " ")
	}
	msg := "(no error)"
	if err != nil {
		msg = err.Error()
	}
	return "=== " + c.name + "\nargs: " + strings.Join(c.args, " ") + "\nenv: " + env + "\n" + msg + "\n"
}

func TestGolden_StartupErrors(t *testing.T) {
	var blocks []string
	for _, c := range startupErrorCases {
		t.Run(c.name, func(t *testing.T) {
			res := runCommandOutput(t, c.env, c.args...)
			if res.Err == nil {
				t.Errorf("expected a startup error")
			} else {
				assert.True(t, strings.HasPrefix(res.Out, "Error: "+res.Err.Error()+"\n"), "cobra output %q", res.Out)
			}
			blocks = append(blocks, renderStartupError(c, res.Err))
		})
	}
	compareGolden(t, filepath.Join(goldenDir, "startup_errors.txt"), strings.Join(blocks, "\n"))
}

func TestGolden_Help(t *testing.T) {
	for _, g := range goldenApps {
		t.Run(g.app, func(t *testing.T) {
			res := runCommandOutput(t, nil, g.app, "--help")
			assert.NoError(t, res.Err)
			assert.Equal(t, res.Logs, "")
			compareGolden(t, filepath.Join(goldenDir, "help_"+g.app+".txt"), res.Out)
		})
	}
}

func TestRenderStartupError(t *testing.T) {
	c := startupErrorCase{name: "n", args: []string{"radarr", "--x"}, env: map[string]string{"B": "2", "A": "1"}}
	assert.Equal(t, renderStartupError(c, errors.New("boom\nbang")), "=== n\nargs: radarr --x\nenv: A=1 B=2\nboom\nbang\n")
	assert.Equal(t, renderStartupError(startupErrorCase{name: "m", args: []string{"sonarr"}}, nil), "=== m\nargs: sonarr\nenv: (none)\n(no error)\n")
}

func TestNormalizeMetrics(t *testing.T) {
	const fakeURL = "http://127.0.0.1:41234"
	in := strings.Join([]string{
		"# HELP go_goroutines Number of goroutines that currently exist.",
		"# TYPE go_goroutines gauge",
		"go_goroutines 12",
		"# HELP process_open_fds Number of open file descriptors.",
		"# TYPE process_open_fds gauge",
		"process_open_fds 9",
		"# HELP radarr_movie_total Total number of movies",
		"# TYPE radarr_movie_total gauge",
		`radarr_movie_total{url="` + fakeURL + `"} 3`,
		"# HELP radarr_scrape_duration_seconds Distribution of scrape durations.",
		"# TYPE radarr_scrape_duration_seconds histogram",
		`radarr_scrape_duration_seconds_bucket{url="` + fakeURL + `",le="0.1"} 1`,
		`radarr_scrape_duration_seconds_sum{url="` + fakeURL + `"} 0.0042`,
		`radarr_scrape_duration_seconds_count{url="` + fakeURL + `"} 1`,
		`sabnzbd_queue_query_duration_seconds_bucket{le="+Inf"} 2`,
		`sabnzbd_queue_query_duration_seconds 0.5`,
		`prowlarr_indexer_queries_total{indexer="a b",url="` + fakeURL + `"} 7`,
	}, "\n") + "\n"
	want := strings.Join([]string{
		"# HELP radarr_movie_total Total number of movies",
		"# TYPE radarr_movie_total gauge",
		`radarr_movie_total{url="http://fixture"} 3`,
		"# HELP radarr_scrape_duration_seconds Distribution of scrape durations.",
		"# TYPE radarr_scrape_duration_seconds histogram",
		`radarr_scrape_duration_seconds_bucket{url="http://fixture",le="0.1"} X`,
		`radarr_scrape_duration_seconds_sum{url="http://fixture"} X`,
		`radarr_scrape_duration_seconds_count{url="http://fixture"} X`,
		`sabnzbd_queue_query_duration_seconds_bucket{le="+Inf"} X`,
		`sabnzbd_queue_query_duration_seconds X`,
		`prowlarr_indexer_queries_total{indexer="a b",url="http://fixture"} 7`,
	}, "\n") + "\n"
	assert.Equal(t, normalizeMetrics(in, fakeURL), want)
	assert.Equal(t, normalizeMetrics("", fakeURL), "")
}

func TestNormalizeLogs(t *testing.T) {
	const fakeURL = "http://127.0.0.1:41234"
	in := "time=2026-09-13T10:00:00.000Z level=INFO msg=b\n" +
		"\n" +
		"time=2026-09-13T10:00:01.000Z level=ERROR msg=a error=\"Get " + fakeURL + "/api/v3/movie\"\n" +
		"   \n" +
		"level=WARN msg=untimed\n"
	want := "level=ERROR msg=a error=\"Get http://fixture/api/v3/movie\"\n" +
		"level=INFO msg=b\n" +
		"level=WARN msg=untimed\n"
	assert.Equal(t, normalizeLogs(in, fakeURL), want)
}

func TestLineDiff(t *testing.T) {
	want := "a\nb\nc\nd\ne\nf\ng\nh\ni\n"
	got := "a\nb\nc\nd\ne\nF\ng\nh\ni\nj\n"
	diff := lineDiff(want, got)
	assert.Equal(t, diff, "--- want\n+++ got\n@@\n c\n d\n e\n-f\n+F\n g\n h\n i\n+j\n \n")
	assert.Equal(t, lineDiff("x\n", "x\n"), "--- want\n+++ got\n")
}

func TestGoldenPath(t *testing.T) {
	assert.Equal(t, goldenPath("radarr", false, ".log"), filepath.Join(goldenDir, "radarr.log"))
	assert.Equal(t, goldenPath("sabnzbd", true, ".metrics"), filepath.Join(goldenDir, "sabnzbd_error.metrics"))
}
