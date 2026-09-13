package collector

import (
	"bytes"
	"github.com/onedr0p/exportarr/internal/assert"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onedr0p/exportarr/internal/sabnzbd/config"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const testAPIKey = "abcdef0123456789abcdef0123456789"

func newTestServer(t *testing.T, fn func(http.ResponseWriter, *http.Request)) (*httptest.Server, error) {
	queue, err := os.ReadFile("../testdata/queue.json")
	assert.NoError(t, err)
	serverStats, err := os.ReadFile("../testdata/server_stats.json")
	assert.NoError(t, err)

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fn(w, r)
		assert.NotEmpty(t, r.URL.Query().Get("mode"))
		switch r.URL.Query().Get("mode") {
		case "queue":
			w.WriteHeader(http.StatusOK)
			_, err := w.Write(queue)
			assert.NoError(t, err)
		case "server_stats":
			w.WriteHeader(http.StatusOK)
			_, err := w.Write(serverStats)
			assert.NoError(t, err)
		}
	})), nil
}

func TestCollect(t *testing.T) {
	ts, err := newTestServer(t, func(_ http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.URL.Path, "/api")
		assert.Equal(t, r.URL.Query().Get("apikey"), testAPIKey)
		assert.Equal(t, r.URL.Query().Get("output"), "json")
	})
	assert.NoError(t, err)

	defer ts.Close()

	config := &config.SabnzbdConfig{
		URL:    ts.URL,
		APIKey: testAPIKey,
	}
	collector, err := NewSabnzbdCollector(config)
	assert.NoError(t, err)

	b, err := os.ReadFile("../testdata/expected_metrics.txt")
	assert.NoError(t, err)

	expected := strings.ReplaceAll(string(b), "http://127.0.0.1:39965", ts.URL)
	f := strings.NewReader(expected)

	assert.NotPanics(t, func() {
		err = testutil.CollectAndCompare(collector, f,
			"sabnzbd_downloaded_bytes",
			"sabnzbd_server_downloaded_bytes",
			"sabnzbd_server_articles_total",
			"sabnzbd_server_articles_success",
			"sabnzbd_info",
			"sabnzbd_paused",
			"sabnzbd_paused_all",
			"sabnzbd_pause_duration_seconds",
			"sabnzbd_disk_used_bytes",
			"sabnzbd_disk_total_bytes",
			"sabnzbd_remaining_quota_bytes",
			"sabnzbd_quota_bytes",
			"sabnzbd_article_cache_articles",
			"sabnzbd_article_cache_bytes",
			"sabnzbd_speed_bps",
			"sabnzbd_speed_limit_bps",
			"sabnzbd_speed_limit_percent",
			"sabnzbd_remaining_bytes",
			"sabnzbd_total_bytes",
			"sabnzbd_queue_size",
			"sabnzbd_status",
			"sabnzbd_time_estimate_seconds",
			"sabnzbd_queue_length",
			"sabnzbd_warnings",
		)
	})
	assert.NoError(t, err)

	assert.GreaterOrEqual(t, 31, testutil.CollectAndCount(collector))
}

func TestCollect_FailureDoesntPanic(t *testing.T) {

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()

	config := &config.SabnzbdConfig{
		URL:    ts.URL,
		APIKey: testAPIKey,
	}
	collector, err := NewSabnzbdCollector(config)
	assert.NoError(t, err)

	f := strings.NewReader("")

	assert.NotPanics(t, func() {
		err = testutil.CollectAndCompare(collector, f)
		assert.Error(t, err)
	}, "Collecting metrics should not panic on failure")
	assert.Error(t, err)
}

func TestCollect_StopsAtCollectTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer ts.Close()

	collector, err := NewSabnzbdCollector(&config.SabnzbdConfig{
		URL:            ts.URL,
		APIKey:         testAPIKey,
		CollectTimeout: 200 * time.Millisecond,
	})
	assert.NoError(t, err)

	start := time.Now()
	assert.Equal(t, testutil.CollectAndCount(collector, "sabnzbd_collector_error"), 1)
	assert.True(t, time.Since(start) < 5*time.Second, "collection took %s", time.Since(start))
}

func captureDefaultLog(t *testing.T) *syncBuffer {
	t.Helper()
	saved := slog.Default()
	t.Cleanup(func() { slog.SetDefault(saved) })
	buf := &syncBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	return buf
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestCollect_ErrorLinesCarryTarget(t *testing.T) {
	for _, target := range []string{"", "sab-main"} {
		t.Run("target="+target, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer ts.Close()

			collector, err := NewSabnzbdCollector(&config.SabnzbdConfig{
				URL:    ts.URL,
				APIKey: testAPIKey,
				Target: target,
			})
			assert.NoError(t, err)

			logs := captureDefaultLog(t)
			assert.Equal(t, testutil.CollectAndCount(collector, "sabnzbd_collector_error"), 1)

			var errorLines int
			for line := range strings.Lines(logs.String()) {
				if !strings.Contains(line, "level=ERROR") {
					continue
				}
				errorLines++
				if target == "" {
					assert.NotContains(t, line, "target=")
				} else {
					assert.Contains(t, line, "target="+target)
				}
			}
			assert.True(t, errorLines > 0, "no error lines in:\n%s", logs.String())
		})
	}
}
