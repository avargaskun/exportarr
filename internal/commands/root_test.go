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

func noCollectors(prometheus.Registerer) {}

func startServeHTTP(ctx context.Context, fn registerFunc) chan error {
	errc := make(chan error, 1)
	go func() { errc <- serveHTTP(ctx, fn) }()
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
	err := serveHTTP(context.Background(), noCollectors)
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
	err := serveHTTP(context.Background(), noCollectors)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to start HTTP server")
}

func TestServeHTTP_ContextCancelled(t *testing.T) {
	listenFn, addrc := loopbackListen()
	logs, _ := useServeHooks(t, listenFn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := startServeHTTP(ctx, noCollectors)
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

	errc := startServeHTTP(context.Background(), noCollectors)
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
	errc := startServeHTTP(ctx, func(r prometheus.Registerer) { r.MustRegister(blocking) })
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
