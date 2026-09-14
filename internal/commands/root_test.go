package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
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

func metricsStackServer(t *testing.T, o stackOpts, cs ...prometheus.Collector) *httptest.Server {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(cs...)
	ts := httptest.NewServer(newMetricsHandler(o, registry))
	t.Cleanup(ts.Close)
	return ts
}

func TestNewMetricsHandler_ScrapeTimeout(t *testing.T) {
	blocking := newBlockingCollector()
	defer close(blocking.release)
	ts := metricsStackServer(t, stackOpts{app: "test", url: "http://target", scrapeTimeout: 100 * time.Millisecond}, blocking)

	code, body := get(t, http.MethodGet, ts.URL)
	assert.Equal(t, code, http.StatusServiceUnavailable)
	assert.True(t, strings.Contains(body, "timeout"), "unexpected body %q", body)
}

func TestNewMetricsHandler_MaxRequestsInFlight(t *testing.T) {
	blocking := newBlockingCollector()
	o := stackOpts{app: "test", url: "http://target", scrapeTimeout: time.Minute}
	registry := prometheus.NewRegistry()
	registry.MustRegister(blocking)
	handler := newMetricsHandler(o, registry)
	var arrived atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Add(1)
		handler.ServeHTTP(w, r)
	}))
	defer ts.Close()

	codes := make(chan int, maxScrapesInFlight)
	for range maxScrapesInFlight {
		go func() {
			resp, err := http.Get(ts.URL)
			if err != nil {
				codes <- 0
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	<-blocking.started
	for arrived.Load() < maxScrapesInFlight {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	code, body := get(t, http.MethodGet, ts.URL)
	assert.Equal(t, code, http.StatusServiceUnavailable)
	assert.Contains(t, body, "Limit of concurrent requests reached")

	close(blocking.release)
	for range maxScrapesInFlight {
		assert.Equal(t, <-codes, http.StatusOK)
	}

	_, body = get(t, http.MethodGet, ts.URL)
	assert.Contains(t, body, `test_scrape_requests_total{code="200",url="http://target"} 2`)
	assert.Contains(t, body, `test_scrape_requests_total{code="503",url="http://target"} 1`)
}

func TestServerTimeouts(t *testing.T) {
	srv := newServer(2 * time.Minute)
	assert.Equal(t, srv.ReadHeaderTimeout, 10*time.Second)
	assert.Equal(t, srv.ReadTimeout, 30*time.Second)
	assert.Equal(t, srv.IdleTimeout, 60*time.Second)
	assert.Equal(t, srv.WriteTimeout, 2*time.Minute+10*time.Second)
}

func useAppInfo(t *testing.T) {
	t.Helper()
	saved := appInfo
	t.Cleanup(func() { appInfo = saved })
	appInfo = &AppInfo{Name: "exportarr", Version: "1.2.3", BuildTime: "now", Revision: "abc"}
}

func TestNewSelfRegistry(t *testing.T) {
	useAppInfo(t)
	mfs, err := newSelfRegistry().Gather()
	assert.NoError(t, err)
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	assert.True(t, names["exportarr_app_info"], "missing exportarr_app_info in %v", names)
	assert.True(t, names["go_goroutines"], "missing go_goroutines in %v", names)
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
		assert.True(t, names["process_cpu_seconds_total"], "missing process_cpu_seconds_total in %v", names)
	}
}

type constCollector struct{ desc *prometheus.Desc }

func (c constCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c constCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 42)
}

func TestSingleTargetHandler(t *testing.T) {
	useAppInfo(t)
	savedConf := conf
	t.Cleanup(func() { conf = savedConf })
	conf = &config.Config{App: "radarr", URL: "http://radarr:7878", ScrapeTimeout: time.Minute}

	ts := httptest.NewServer(singleTargetHandler(constCollector{prometheus.NewDesc("extra_series", "test", nil, nil)}))
	defer ts.Close()

	code, body := get(t, http.MethodGet, ts.URL+"/metrics")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, body, "extra_series 42")
	assert.Contains(t, body, `exportarr_app_info{app_name="exportarr",build_time="now",revision="abc",version="1.2.3"} 1`)
	assert.Contains(t, body, "go_goroutines")

	_, body = get(t, http.MethodGet, ts.URL+"/metrics")
	assert.Contains(t, body, `radarr_scrape_requests_total{code="200",url="http://radarr:7878"} 1`)

	code, body = get(t, http.MethodGet, ts.URL+"/healthz")
	assert.Equal(t, code, http.StatusOK)
	assert.Equal(t, body, "OK")
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

func TestListenAddr(t *testing.T) {
	for _, tc := range []struct{ iface, want string }{
		{"0.0.0.0", "0.0.0.0:9707"},
		{"127.0.0.1", "127.0.0.1:9707"},
		{"::1", "[::1]:9707"},
		{"::", "[::]:9707"},
	} {
		assert.Equal(t, listenAddr(&config.Config{Interface: tc.iface, Port: 9707}), tc.want)
	}
}

func TestShellCompletionDisabled(t *testing.T) {
	debugFile := filepath.Join(t.TempDir(), "comp-debug")
	t.Setenv("BASH_COMP_DEBUG_FILE", debugFile)
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	for _, args := range [][]string{
		{"completion", "bash"},
		{"__complete", "sonarr", "--bogus", ""},
		{"__completeNoDesc", "sonarr", "--bogus", ""},
	} {
		rootCmd.SetArgs(args)
		assert.Error(t, rootCmd.Execute(), "%v should fail", args)
	}
	_, err := os.Stat(debugFile)
	assert.True(t, os.IsNotExist(err), "BASH_COMP_DEBUG_FILE must not be written")
}

// expvar is linked in and registers /debug/vars on http.DefaultServeMux, which must never be served.
func TestHandler_DoesNotServeDebugEndpoints(t *testing.T) {
	ts := testServer(t, time.Minute)
	for _, path := range []string{"/debug/vars", "/debug/pprof/"} {
		_, body := get(t, http.MethodGet, ts.URL+path)
		assert.NotContains(t, body, "memstats")
		assert.NotContains(t, body, "cmdline")
		assert.NotContains(t, body, "goroutine")
	}
}

func useServeHooks(t *testing.T, listenFn func(network, address string) (net.Listener, error)) (logs *syncBuffer, sigcc chan chan<- os.Signal) {
	t.Helper()
	saveLogging(t)
	savedConf, savedListen, savedNotify := conf, listen, notifySignals
	t.Cleanup(func() { conf, listen, notifySignals = savedConf, savedListen, savedNotify })

	logs = &syncBuffer{}
	sigcc = make(chan chan<- os.Signal, 1)
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	conf = &config.Config{App: "test", URL: "http://target", Interface: "127.0.0.1", Port: 9707, ScrapeTimeout: time.Minute}
	listen = listenFn
	notifySignals = func(c chan<- os.Signal, sig ...os.Signal) {
		if !slices.Equal(sig, []os.Signal{os.Interrupt, syscall.SIGTERM}) {
			t.Errorf("notifySignals got %v", sig)
		}
		sigcc <- c
	}
	return logs, sigcc
}

func loopbackListen() (func(string, string) (net.Listener, error), chan string) {
	addrc := make(chan string, 1)
	return func(network, _ string) (net.Listener, error) {
		ln, err := net.Listen(network, "127.0.0.1:0")
		if err == nil {
			addrc <- ln.Addr().String()
		}
		return ln, err
	}, addrc
}

func startServeHTTP(ctx context.Context, h http.Handler) chan error {
	errc := make(chan error, 1)
	go func() { errc <- serveHTTP(ctx, time.Minute, h) }()
	return errc
}

func waitErr(t *testing.T, errc chan error) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("serveHTTP did not return within 10s")
		return nil
	}
}

func TestServeHTTP_ListenError(t *testing.T) {
	logs, _ := useServeHooks(t, func(string, string) (net.Listener, error) {
		return nil, errors.New("address in use")
	})
	err := serveHTTP(context.Background(), time.Minute, singleTargetHandler())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to start HTTP server: address in use")
	assert.Contains(t, logs.String(), "Starting HTTP Server")
}

func TestServeHTTP_ClosedListener(t *testing.T) {
	useServeHooks(t, func(network, _ string) (net.Listener, error) {
		ln, err := net.Listen(network, "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		_ = ln.Close()
		return ln, nil
	})
	err := serveHTTP(context.Background(), time.Minute, singleTargetHandler())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to start HTTP server")
}

func TestServeHTTP_ContextCancelled(t *testing.T) {
	listenFn, addrc := loopbackListen()
	logs, _ := useServeHooks(t, listenFn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := startServeHTTP(ctx, singleTargetHandler())
	addr := <-addrc
	code, body := get(t, http.MethodGet, "http://"+addr+"/healthz")
	assert.Equal(t, code, http.StatusOK)
	assert.Equal(t, body, "OK")

	cancel()
	assert.NoError(t, waitErr(t, errc))
	assert.NotContains(t, logs.String(), "Shutting down")
}

func TestServeHTTP_Signal(t *testing.T) {
	listenFn, addrc := loopbackListen()
	logs, sigcc := useServeHooks(t, listenFn)

	errc := startServeHTTP(context.Background(), singleTargetHandler())
	<-addrc
	(<-sigcc) <- os.Interrupt
	assert.NoError(t, waitErr(t, errc))
	assert.Contains(t, logs.String(), `msg="Shutting down due to signal" signal=interrupt`)
}

func TestServeHTTP_GracefulShutdownFailure(t *testing.T) {
	listenFn, addrc := loopbackListen()
	logs, _ := useServeHooks(t, listenFn)
	savedTimeout := gracefulTimeout
	gracefulTimeout = 50 * time.Millisecond
	t.Cleanup(func() { gracefulTimeout = savedTimeout })

	blocking := newBlockingCollector()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := startServeHTTP(ctx, singleTargetHandler(blocking))
	addr := <-addrc

	scraped := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err != nil {
			scraped <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		scraped <- resp.StatusCode
	}()
	<-blocking.started

	cancel()
	err := waitErr(t, errc)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "unexpected error %v", err)
	assert.Contains(t, logs.String(), `msg="Server shutdown failed"`)

	close(blocking.release)
	assert.Equal(t, <-scraped, http.StatusOK)
}

// captureSlog swaps slog.Default for a text handler without timestamps.
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	saveLogging(t)
	buf := &syncBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})))
	return buf
}

func TestPromhttpLogger_NoTargetIsByteIdentical(t *testing.T) {
	logs := captureSlog(t)
	promhttpLogger{}.Println("x")
	slog.Error("x\n")
	lines := strings.SplitAfter(logs.String(), "\n")
	assert.Len(t, lines, 3)
	assert.Equal(t, lines[0], lines[1])
	assert.Equal(t, lines[0], "level=ERROR msg=\"x\\n\"\n")
}

func TestPromhttpLogger_WithTarget(t *testing.T) {
	logs := captureSlog(t)
	promhttpLogger{target: "t"}.Println("error gathering metrics:", errors.New("boom"))
	assert.Equal(t, logs.String(), "level=ERROR msg=\"error gathering metrics: boom\\n\" target=t\n")
}

// invalidCollector sends a metric that fails the gather.
type invalidCollector struct{ desc *prometheus.Desc }

func (c invalidCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c invalidCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(c.desc, errors.New("broken metric"))
}

func TestNewMetricsHandler_GatherErrorLogsTarget(t *testing.T) {
	for _, target := range []string{"", "sonarr-hd"} {
		t.Run("target="+target, func(t *testing.T) {
			logs := captureSlog(t)
			registry := prometheus.NewRegistry()
			registry.MustRegister(
				invalidCollector{desc: prometheus.NewDesc("invalid", "test", nil, nil)},
				constCollector{desc: prometheus.NewDesc("fine", "test", nil, nil)},
			)
			ts := httptest.NewServer(newMetricsHandler(stackOpts{
				app:           "sonarr",
				url:           "http://sonarr:8989",
				target:        target,
				scrapeTimeout: time.Minute,
			}, registry))
			t.Cleanup(ts.Close)

			code, body := get(t, http.MethodGet, ts.URL)
			assert.Equal(t, code, http.StatusOK)
			assert.Contains(t, body, "fine ")

			var errorLines int
			for line := range strings.Lines(logs.String()) {
				if !strings.HasPrefix(line, "level=ERROR") {
					continue
				}
				errorLines++
				assert.Contains(t, line, "broken metric")
				if target == "" {
					assert.NotContains(t, line, "target=")
				} else {
					assert.True(t, strings.HasSuffix(line, " target="+target+"\n"), "line %q lacks target", line)
				}
			}
			assert.Equal(t, errorLines, 1)
		})
	}
}
