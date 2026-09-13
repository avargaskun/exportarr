package commands

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"

	arrconfig "github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/assert"
	"github.com/onedr0p/exportarr/internal/config"
)

// blockingCollector holds every Collect until release is closed.
type blockingCollector struct {
	desc    *prometheus.Desc
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func newBlockingCollector() *blockingCollector {
	return &blockingCollector{
		desc:    prometheus.NewDesc("blocking", "test", nil, nil),
		started: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
}

func (b *blockingCollector) Describe(ch chan<- *prometheus.Desc) { ch <- b.desc }

func (b *blockingCollector) Collect(ch chan<- prometheus.Metric) {
	b.calls.Add(1)
	b.started <- struct{}{}
	<-b.release
	ch <- prometheus.MustNewConstMetric(b.desc, prometheus.GaugeValue, 1)
}

func testServer(t *testing.T, scrapeTimeout time.Duration, cs ...prometheus.Collector) *httptest.Server {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(cs...)
	conf := &config.Config{App: "test", URL: "http://target", ScrapeTimeout: scrapeTimeout}
	ts := httptest.NewServer(newHandler(conf, registry))
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, method, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	assert.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	return resp.StatusCode, string(body)
}

func TestHandler_Routes(t *testing.T) {
	ts := testServer(t, time.Minute)

	code, body := get(t, http.MethodGet, ts.URL+"/metrics")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, body, "promhttp_metric_handler_errors_total")

	code, _ = get(t, http.MethodPost, ts.URL+"/metrics")
	assert.Equal(t, code, http.StatusMethodNotAllowed)

	code, body = get(t, http.MethodGet, ts.URL+"/healthz")
	assert.Equal(t, code, http.StatusOK)
	assert.Equal(t, body, "OK")

	code, _ = get(t, http.MethodGet, ts.URL+"/")
	assert.Equal(t, code, http.StatusOK)
}

func TestHandler_ConcurrentScrapesShareOneGather(t *testing.T) {
	blocking := newBlockingCollector()
	registry := prometheus.NewRegistry()
	registry.MustRegister(blocking)
	conf := &config.Config{App: "test", URL: "http://target", ScrapeTimeout: time.Minute}
	var arrived atomic.Int32
	handler := newHandler(conf, registry)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Add(1)
		handler.ServeHTTP(w, r)
	}))
	defer ts.Close()

	type result struct {
		code int
		body string
	}
	results := make(chan result, maxScrapesInFlight)
	for range maxScrapesInFlight {
		go func() {
			resp, err := http.Get(ts.URL + "/metrics")
			if err != nil {
				results <- result{}
				return
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			results <- result{resp.StatusCode, string(body)}
		}()
	}
	<-blocking.started
	for arrived.Load() < maxScrapesInFlight {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	code, _ := get(t, http.MethodGet, ts.URL+"/metrics")
	assert.Equal(t, code, http.StatusServiceUnavailable)

	close(blocking.release)
	for range maxScrapesInFlight {
		r := <-results
		assert.Equal(t, r.code, http.StatusOK)
		assert.Contains(t, r.body, "blocking 1")
	}
	assert.Equal(t, blocking.calls.Load(), int32(1))
}

func TestHandler_ScrapeTimeout(t *testing.T) {
	blocking := newBlockingCollector()
	defer close(blocking.release)
	ts := testServer(t, 100*time.Millisecond, blocking)

	code, body := get(t, http.MethodGet, ts.URL+"/metrics")
	assert.Equal(t, code, http.StatusServiceUnavailable)
	assert.True(t, strings.Contains(body, "timeout"), "unexpected body %q", body)
}

func TestServerTimeouts(t *testing.T) {
	srv := newServer(&config.Config{ScrapeTimeout: 2 * time.Minute})
	assert.Equal(t, srv.ReadHeaderTimeout, 10*time.Second)
	assert.Equal(t, srv.ReadTimeout, 30*time.Second)
	assert.Equal(t, srv.IdleTimeout, 60*time.Second)
	assert.True(t, srv.WriteTimeout > 2*time.Minute, "WriteTimeout %s must exceed the scrape timeout", srv.WriteTimeout)
}

func TestWarnSecretFlags(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	config.RegisterConfigFlags(flags)
	arrconfig.RegisterArrFlags(flags)

	warnSecretFlags(log, flags)
	assert.Equal(t, logs.String(), "")

	_ = flags.Set("api-key", "abcdef0123456789abcdef0123456789")
	_ = flags.Set("auth-password", "hunter2")
	warnSecretFlags(log, flags)
	assert.Contains(t, logs.String(), "flag=--api-key")
	assert.Contains(t, logs.String(), "flag=--auth-password")
	assert.NotContains(t, logs.String(), "hunter2")
}
