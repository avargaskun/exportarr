package collector

import (
	"fmt"
	"github.com/onedr0p/exportarr/internal/assert"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	client "github.com/onedr0p/exportarr/internal/arr/client"
	"github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/fixtures"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestQueueCollect(t *testing.T) {
	var tests = []struct {
		name   string
		config *config.ArrConfig
		path   string
	}{
		{
			name: "radarr",
			config: &config.ArrConfig{
				App:        "radarr",
				APIVersion: "v3",
			},
			path: "/api/v3/queue",
		},
		{
			name: "sonarr",
			config: &config.ArrConfig{
				App:        "sonarr",
				APIVersion: "v3",
			},
			path: "/api/v3/queue",
		},
		{
			name: "lidarr",
			config: &config.ArrConfig{
				App:        "lidarr",
				APIVersion: "v1",
			},
			path: "/api/v1/queue",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, err := fixtures.NewTestSharedServer(t, func(_ http.ResponseWriter, r *http.Request) {
				assert.Contains(t, r.URL.Path, tt.path)
				assert.Equal(t, r.URL.Query().Get("pageSize"), "250")
			})
			assert.NoError(t, err)

			defer ts.Close()

			tt.config.URL = ts.URL
			tt.config.APIKey = fixtures.APIKey

			cl, err := client.NewClient(tt.config)
			assert.NoError(t, err)
			collector := NewQueueCollector(cl, tt.config)

			b, err := os.ReadFile(fixtures.CommonFixturesPath + "expected_queue_metrics.txt")
			assert.NoError(t, err)

			expected := strings.ReplaceAll(string(b), "SOMEURL", ts.URL)
			expected = strings.ReplaceAll(expected, "APP", tt.config.App)

			f := strings.NewReader(expected)

			assert.NotPanics(t, func() {
				err = testutil.CollectAndCompare(collector, f)
			})
			assert.NoError(t, err)
		})
	}
}

func TestQueueCollect_FailureDoesntPanic(t *testing.T) {

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
	collector := NewQueueCollector(cl, config)

	f := strings.NewReader("")

	assert.NotPanics(t, func() {
		err := testutil.CollectAndCompare(collector, f)
		assert.Error(t, err)
	}, "Collecting metrics should not panic on failure")
}

// TestQueueCollect_EmptyQueue pins https://github.com/onedr0p/exportarr/issues/389:
// an empty queue must emit an explicit zero series, not vanish.
func TestQueueCollect_EmptyQueue(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"page":1,"pageSize":250,"totalRecords":0,"records":[]}`))
	}))
	defer ts.Close()

	config := &config.ArrConfig{
		App:        "sonarr",
		APIVersion: "v3",
		URL:        ts.URL,
		APIKey:     fixtures.APIKey,
	}
	cl, err := client.NewClient(config)
	assert.NoError(t, err)
	collector := NewQueueCollector(cl, config)

	expected := strings.NewReader(`# HELP sonarr_queue_total Total number of items in the queue by status, download_status, and download_state
# TYPE sonarr_queue_total gauge
sonarr_queue_total{download_state="",download_status="",status="",url="` + ts.URL + `"} 0
`)
	assert.NoError(t, testutil.CollectAndCompare(collector, expected))
}

// pagedQueueServer answers every queue page with one record whose id comes
// from idForPage, advertising the given pagination totals.
func pagedQueueServer(t *testing.T, totalRecords, pageSize int, idForPage func(page int) int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		records := "[]"
		if id := idForPage(page); id >= 0 {
			records = fmt.Sprintf(`[{"id":%d,"status":"downloading"}]`, id)
		}
		_, _ = fmt.Fprintf(w, `{"page":%d,"pageSize":%d,"totalRecords":%d,"records":%s}`, page, pageSize, totalRecords, records)
	}))
	return ts, &requests
}

func queueTotal(t *testing.T, ts *httptest.Server) (float64, bool) {
	t.Helper()
	conf := &config.ArrConfig{App: "sonarr", APIVersion: "v3", URL: ts.URL, APIKey: fixtures.APIKey}
	cl, err := client.NewClient(conf)
	assert.NoError(t, err)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(NewQueueCollector(cl, conf))
	families, err := registry.Gather()
	assert.NoError(t, err)
	var total float64
	var failed bool
	for _, mf := range families {
		switch mf.GetName() {
		case "sonarr_queue_total":
			for _, m := range mf.GetMetric() {
				total += m.GetGauge().GetValue()
			}
		case "sonarr_queue_collector_error":
			failed = true
		}
	}
	return total, failed
}

func TestQueueCollect_PaginationBounds(t *testing.T) {
	t.Run("page parameter ignored", func(t *testing.T) {
		ts, requests := pagedQueueServer(t, math.MaxInt32, 1, func(int) int { return 7 })
		defer ts.Close()
		total, failed := queueTotal(t, ts)
		assert.False(t, failed)
		assert.Equal(t, total, 1.0)
		assert.Equal(t, requests.Load(), int32(2))
	})

	t.Run("page count capped", func(t *testing.T) {
		ts, requests := pagedQueueServer(t, math.MaxInt32, 1, func(page int) int { return page })
		defer ts.Close()
		total, failed := queueTotal(t, ts)
		assert.False(t, failed)
		assert.Equal(t, total, float64(maxQueuePages))
		assert.Equal(t, requests.Load(), int32(maxQueuePages))
	})

	t.Run("empty page stops paging", func(t *testing.T) {
		ts, requests := pagedQueueServer(t, 10, 1, func(page int) int {
			if page > 3 {
				return -1
			}
			return page
		})
		defer ts.Close()
		total, failed := queueTotal(t, ts)
		assert.False(t, failed)
		assert.Equal(t, total, 3.0)
		assert.Equal(t, requests.Load(), int32(4))
	})

	t.Run("negative totals rejected", func(t *testing.T) {
		ts, requests := pagedQueueServer(t, -5, 1, func(page int) int { return page })
		defer ts.Close()
		_, failed := queueTotal(t, ts)
		assert.True(t, failed)
		assert.Equal(t, requests.Load(), int32(1))
	})
}
