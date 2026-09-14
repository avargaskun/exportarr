package commands

import (
	"errors"
	"maps"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/onedr0p/exportarr/internal/assert"
	"github.com/onedr0p/exportarr/internal/client"
	"github.com/onedr0p/exportarr/internal/fixtures"
)

const smokeKey = "smokeKey0123456789abcdef"

func TestServe_Smoke(t *testing.T) {
	sonarr := fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: "sonarr", APIKey: smokeKey})
	sab := fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: "sabnzbd", APIKey: smokeKey})
	rc := startCommand(t, map[string]string{
		"TARGET_0_NAME":    "sonarr-hd",
		"TARGET_0_APP":     "sonarr",
		"TARGET_0_URL":     sonarr.URL,
		"TARGET_0_API_KEY": smokeKey,
		"TARGET_1_NAME":    "sab",
		"TARGET_1_APP":     "sabnzbd",
		"TARGET_1_URL":     sab.URL,
		"TARGET_1_API_KEY": smokeKey,
	}, "serve")

	code, body := rc.Get("/metrics/sonarr-hd")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, body, "sonarr_series_total")
	assert.NotContains(t, body, "sabnzbd_")
	assert.NotContains(t, body, "go_goroutines")
	_, body = rc.Get("/metrics/sonarr-hd")
	assert.Contains(t, body, `sonarr_scrape_requests_total{code="200",url="`+sonarr.URL+`"} 1`)

	code, body = rc.Get("/metrics/sab")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, body, "sabnzbd_info")
	assert.NotContains(t, body, "sonarr_")

	code, body = rc.Get("/metrics")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, body, `exportarr_app_info{app_name="exportarr",build_time="",revision="",version="development"} 1`)
	assert.Contains(t, body, "go_goroutines")
	assert.NotContains(t, body, "sonarr_")
	assert.NotContains(t, body, "sabnzbd_")
	assert.Contains(t, body, "exportarr_upstream_requests_max 64\n")
	for _, name := range []string{"sonarr-hd", "sab"} {
		assert.Contains(t, body, `exportarr_upstream_requests_in_flight{target="`+name+`"} 0`+"\n")
		m := regexp.MustCompile(`exportarr_upstream_slot_wait_seconds_count\{target="` + name + `"\} (\d+)\n`).FindStringSubmatch(body)
		assert.True(t, m != nil, "no slot wait count for %s", name)
		waits, err := strconv.Atoi(m[1])
		assert.NoError(t, err)
		assert.True(t, waits > 0, "%s took no upstream slot", name)
	}

	code, body = rc.Get("/")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, body, "<a href='/metrics/sonarr-hd'>sonarr-hd</a>")
	assert.Contains(t, body, "<a href='/metrics/sab'>sab</a>")

	code, body = rc.Get("/metrics/nope")
	assert.Equal(t, code, http.StatusNotFound)
	assert.Equal(t, body, notFoundBody)

	code, body = rc.Get("/healthz")
	assert.Equal(t, code, http.StatusOK)
	assert.Equal(t, body, "OK")

	assert.Equal(t, rc.Server().WriteTimeout, 2*time.Minute+10*time.Second)

	rc.Stop()
	logs := rc.Logs()
	assert.Contains(t, logs, `msg="Configured target" target=sonarr-hd app=sonarr url=`+sonarr.URL+"\n")
	assert.Contains(t, logs, `msg="Configured target" target=sab app=sabnzbd url=`+sab.URL+"\n")
	assert.Equal(t, strings.Count(logs, `msg="Configured target"`), 2)
	assert.Contains(t, logs, "Starting HTTP Server")
	assert.Contains(t, logs, `msg="Shutting down due to signal" signal=terminated`)
	assert.NotContains(t, logs, smokeKey)
	assert.NotContains(t, logs, "level=ERROR")
}

func TestServe_ConfiguredTargetLogsRedactedURL(t *testing.T) {
	sonarr := fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: "sonarr", APIKey: smokeKey})
	// ValidateURL rejects userinfo and queries, so a fragment is what tells the redacted URL apart.
	rc := startCommand(t, map[string]string{
		"TARGET_0_NAME":    "sonarr-hd",
		"TARGET_0_APP":     "sonarr",
		"TARGET_0_URL":     sonarr.URL + "/base#fragment-marker",
		"TARGET_0_API_KEY": smokeKey,
	}, "serve")
	rc.Stop()

	logs := rc.Logs()
	assert.Contains(t, logs, `msg="Configured target" target=sonarr-hd app=sonarr url=`+sonarr.URL+"/base\n")
	assert.NotContains(t, logs, "fragment-marker")
}

func TestServe_FailsClosed(t *testing.T) {
	sonarr := fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: "sonarr", APIKey: smokeKey})
	validTarget := func(env map[string]string) map[string]string {
		out := map[string]string{
			"TARGET_0_NAME":    "sonarr-hd",
			"TARGET_0_APP":     "sonarr",
			"TARGET_0_URL":     sonarr.URL,
			"TARGET_0_API_KEY": smokeKey,
		}
		maps.Copy(out, env)
		return out
	}
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"no targets", nil, nil, "serve requires at least one target (TARGET_0_NAME, TARGET_0_APP, TARGET_0_URL, …)"},
		{"base url", validTarget(map[string]string{"URL": "http://sonarr:8989"}), nil,
			"URL is not supported by serve: URLs and credentials are set per target (TARGET_<n>_URL)"},
		{"url flag", validTarget(nil), []string{"--url", "http://sonarr:8989"},
			"--url is not supported by serve: URLs and credentials are set per target (TARGET_<n>_URL)"},
		{"process parse error", validTarget(map[string]string{"SERIES_CONCURRENCY": "abc"}), nil,
			`env: parse error on field "SeriesConcurrency" of type "int"`},
		{"invalid per-target key", validTarget(map[string]string{"TARGET_0_API_KEY": "short"}), nil,
			"target 0/sonarr-hd: api-key must be a 20-32 character alphanumeric string"},
		{"gap", validTarget(map[string]string{"TARGET_2_NAME": "late"}), nil,
			"TARGET_2_*: target indices must be contiguous from 0 (missing TARGET_1_*)"},
		{"base config error", validTarget(map[string]string{"PORT": "0"}), nil, "port is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runCommandOutput(t, tc.env, append([]string{"serve"}, tc.args...)...)
			assert.Error(t, res.Err)
			assert.Contains(t, res.Err.Error(), tc.want)
			assert.True(t, strings.HasPrefix(res.Out, "Error: "+res.Err.Error()+"\n"), "cobra output %q", res.Out)
			assert.NotContains(t, res.Logs, "Configured target")
		})
	}
}

func TestServe_ProcessErrorStillScrubsTargetSecrets(t *testing.T) {
	res := runCommandOutput(t, map[string]string{
		"SERIES_CONCURRENCY":     "abc",
		"TARGET_0_NAME":          "radarr",
		"TARGET_0_APP":           "radarr",
		"TARGET_0_URL":           "http://radarr:7878",
		"TARGET_0_API_KEY":       smokeKey,
		"TARGET_0_AUTH_PASSWORD": "hunter2",
	}, "serve")
	assert.Error(t, res.Err)
	assert.Contains(t, res.Err.Error(), "SeriesConcurrency")
	for _, name := range []string{"TARGET_0_API_KEY", "TARGET_0_AUTH_PASSWORD"} {
		_, set := os.LookupEnv(name)
		assert.False(t, set, "%s still set", name)
	}
	assert.NotContains(t, res.Err.Error(), smokeKey)
	assert.NotContains(t, res.Err.Error(), "hunter2")
}

func TestServe_SlotPoolErrorFailsClosed(t *testing.T) {
	saved := newSlotPool
	t.Cleanup(func() { newSlotPool = saved })
	var gotCap int
	var gotNames []string
	newSlotPool = func(capacity int, names []string) (*client.SlotPool, error) {
		gotCap, gotNames = capacity, names
		return nil, errors.New("pool refused")
	}

	res := runCommandOutput(t, map[string]string{
		"MAX_UPSTREAM_REQUESTS": "7",
		"TARGET_0_NAME":         "sonarr-hd",
		"TARGET_0_APP":          "sonarr",
		"TARGET_0_URL":          "http://sonarr:8989",
		"TARGET_0_API_KEY":      smokeKey,
		"TARGET_1_NAME":         "sab",
		"TARGET_1_APP":          "sabnzbd",
		"TARGET_1_URL":          "http://sab:8080",
		"TARGET_1_API_KEY":      smokeKey,
	}, "serve")
	assert.Error(t, res.Err)
	assert.Equal(t, res.Err.Error(), "pool refused")
	assert.Equal(t, gotCap, 7)
	assert.DeepEqual(t, gotNames, []string{"sonarr-hd", "sab"})
	assert.NotContains(t, res.Logs, "Configured target")
}
