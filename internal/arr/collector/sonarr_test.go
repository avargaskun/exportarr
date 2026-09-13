package collector

import (
	"fmt"
	"github.com/onedr0p/exportarr/internal/assert"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	client "github.com/onedr0p/exportarr/internal/arr/client"
	"github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/fixtures"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const sonarrTestFixturesPath = "../testdata/sonarr/"

func newTestSonarrServer(t *testing.T, fn func(http.ResponseWriter, *http.Request)) (*httptest.Server, error) {
	return fixtures.NewTestServer(t, sonarrTestFixturesPath, fn)
}

func TestSonarrCollect(t *testing.T) {
	tests := []struct {
		name                string
		config              *config.ArrConfig
		expectedMetricsFile string
	}{
		{
			name: "basic",
			config: &config.ArrConfig{
				App:                   "sonarr",
				APIVersion:            "v3",
				DisableQualityMetrics: true,
				DisableEpisodeMetrics: true,
			},
			expectedMetricsFile: "expected_metrics.txt",
		},
		{
			name: "default_collects_everything",
			config: &config.ArrConfig{
				App:        "sonarr",
				APIVersion: "v3",
			},
			expectedMetricsFile: "expected_metrics_extended.txt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, err := newTestSonarrServer(t, func(_ http.ResponseWriter, r *http.Request) {
				assert.Contains(t, r.URL.Path, "/api/")
			})
			assert.NoError(t, err)

			defer ts.Close()

			tt.config.URL = ts.URL
			tt.config.APIKey = fixtures.APIKey

			cl, err := client.NewClient(tt.config)
			assert.NoError(t, err)
			collector := NewSonarrCollector(cl, tt.config)
			assert.NoError(t, err)

			b, err := os.ReadFile(sonarrTestFixturesPath + tt.expectedMetricsFile)
			assert.NoError(t, err)

			expected := strings.ReplaceAll(string(b), "SOMEURL", ts.URL)
			f := strings.NewReader(expected)

			assert.NotPanics(t, func() {
				err = testutil.CollectAndCompare(collector, f)
			})
			assert.NoError(t, err)
		})
	}
}

func TestSonarrCollect_FailureDoesntPanic(t *testing.T) {

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()

	config := &config.ArrConfig{
		URL:    ts.URL,
		APIKey: fixtures.APIKey,
	}
	cl, err := client.NewClient(config)
	assert.NoError(t, err)
	collector := NewSonarrCollector(cl, config)

	f := strings.NewReader("")

	assert.NotPanics(t, func() {
		err := testutil.CollectAndCompare(collector, f)
		assert.Error(t, err)
	}, "Collecting metrics should not panic on failure")
}

// TestSonarrCollect_DisableWantedMetrics proves the wanted endpoints are never
// queried when disabled — their totals force full table counts that can hang
// multi-year instances.
func TestSonarrCollect_DisableWantedMetrics(t *testing.T) {
	ts, err := newTestSonarrServer(t, func(_ http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/wanted/") {
			t.Errorf("wanted endpoint %q must not be queried when disabled", r.URL.Path)
		}
	})
	assert.NoError(t, err)
	defer ts.Close()

	config := &config.ArrConfig{
		App:                  "sonarr",
		APIVersion:           "v3",
		URL:                  ts.URL,
		APIKey:               fixtures.APIKey,
		DisableWantedMetrics: true,
	}
	cl, err := client.NewClient(config)
	assert.NoError(t, err)
	collector := NewSonarrCollector(cl, config)

	assert.GreaterOrEqual(t, testutil.CollectAndCount(collector), 5)
	assert.Equal(t, testutil.CollectAndCount(collector, "sonarr_episode_missing_total", "sonarr_episode_cutoff_unmet_total"), 0,
		"wanted series must be absent when disabled")
	assert.Equal(t, testutil.CollectAndCount(collector, "sonarr_collector_error"), 0)
}

// fanoutServer serves n series and answers each per-series episodefile lookup
// with episodeFile, recording how many lookups arrived and the peak number
// in flight at once.
func fanoutServer(t *testing.T, n int, episodeFile http.HandlerFunc) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var calls, inFlight, peak atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/series":
			ids := make([]string, n)
			for i := range ids {
				ids[i] = fmt.Sprintf(`{"id":%d}`, i+1)
			}
			_, _ = fmt.Fprintf(w, "[%s]", strings.Join(ids, ","))
		case "/api/v3/episodefile":
			calls.Add(1)
			cur := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			episodeFile(w, r)
		default:
			_, _ = w.Write([]byte("[]"))
		}
	}))
	return ts, &calls, &peak
}

// collectorFailed gathers c and reports whether it emitted an error gauge.
func collectorFailed(t *testing.T, c prometheus.Collector) bool {
	t.Helper()
	failed, err := gatherErrorGauge(c)
	assert.NoError(t, err)
	return failed
}

// gatherErrorGauge is collectorFailed without the test assertions, for use
// off the test goroutine.
func gatherErrorGauge(c prometheus.Collector) (bool, error) {
	registry := prometheus.NewRegistry()
	if err := registry.Register(c); err != nil {
		return false, err
	}
	families, err := registry.Gather()
	if err != nil {
		return false, err
	}
	for _, mf := range families {
		if strings.HasSuffix(mf.GetName(), "collector_error") {
			return true, nil
		}
	}
	return false, nil
}

func sonarrFanoutConfig(url string) *config.ArrConfig {
	return &config.ArrConfig{
		App:                   "sonarr",
		APIVersion:            "v3",
		URL:                   url,
		APIKey:                fixtures.APIKey,
		DisableEpisodeMetrics: true,
		DisableWantedMetrics:  true,
	}
}

func TestSonarrCollect_FanoutStopsOnFirstError(t *testing.T) {
	ts, calls, _ := fanoutServer(t, 100, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	defer ts.Close()

	conf := sonarrFanoutConfig(ts.URL)
	conf.SeriesConcurrency = 1
	cl, err := client.NewClient(conf)
	assert.NoError(t, err)

	assert.True(t, collectorFailed(t, NewSonarrCollector(cl, conf)))
	assert.True(t, calls.Load() <= 2, "expected the fan-out to stop after the first failure, got %d lookups", calls.Load())
}

func TestSonarrCollect_FanoutStopsAtCollectTimeout(t *testing.T) {
	ts, calls, _ := fanoutServer(t, 100, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	defer ts.Close()

	conf := sonarrFanoutConfig(ts.URL)
	conf.SeriesConcurrency = 2
	conf.CollectTimeout = 200 * time.Millisecond
	cl, err := client.NewClient(conf)
	assert.NoError(t, err)

	start := time.Now()
	assert.True(t, collectorFailed(t, NewSonarrCollector(cl, conf)))
	assert.True(t, time.Since(start) < 5*time.Second, "collection should end at the collect timeout, took %s", time.Since(start))
	assert.True(t, calls.Load() <= 2, "no lookups should start after the timeout, got %d", calls.Load())
}

func TestSonarrCollect_FanoutRespectsConcurrency(t *testing.T) {
	ts, calls, peak := fanoutServer(t, 40, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte("[]"))
	})
	defer ts.Close()

	conf := sonarrFanoutConfig(ts.URL)
	conf.SeriesConcurrency = 3
	cl, err := client.NewClient(conf)
	assert.NoError(t, err)

	assert.False(t, collectorFailed(t, NewSonarrCollector(cl, conf)))
	assert.Equal(t, calls.Load(), int32(40))
	assert.True(t, peak.Load() <= 3, "peak concurrency %d exceeds the configured 3", peak.Load())
}
