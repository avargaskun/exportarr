package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/onedr0p/exportarr/internal/assert"
	"github.com/onedr0p/exportarr/internal/fixtures"
)

const harnessTimeout = 10 * time.Second

var harnessEnv = regexp.MustCompile(`^(TARGET_.*|URL|API_KEY|API_KEY_FILE|AUTH_USERNAME|AUTH_PASSWORD|FORM_AUTH|MAX_UPSTREAM_REQUESTS|SERIES_CONCURRENCY|DISABLE_.*|ENABLE_UNKNOWN_QUEUE_ITEMS|PROWLARR__.*|BAZARR__.*|PORT|INTERFACE|LOG_LEVEL|LOG_FORMAT|SCRAPE_TIMEOUT|REQUEST_TIMEOUT|PROXY_FROM_ENV)$`)

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

type commandHooks struct {
	listen        func(network, address string) (net.Listener, error)
	notifySignals func(c chan<- os.Signal, sig ...os.Signal)
}

// saveLogging also restores the log package, which slog.SetDefault redirects.
func saveLogging(t *testing.T) {
	logger, writer, flags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(logger)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
}

func resetFlags(c *cobra.Command) {
	for _, fs := range []*pflag.FlagSet{c.Flags(), c.PersistentFlags()} {
		fs.VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	}
	for _, sub := range c.Commands() {
		resetFlags(sub)
	}
}

// setContexts is needed because cobra only gives a subcommand the root's context while its own is nil.
func setContexts(ctx context.Context, c *cobra.Command) {
	c.SetContext(ctx)
	for _, sub := range c.Commands() {
		setContexts(ctx, sub)
	}
}

func isolateCommand(ctx context.Context, t *testing.T, env map[string]string, hooks commandHooks) (logs, out *syncBuffer) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if harnessEnv.MatchString(name) {
			t.Setenv(name, "")
			assert.NoError(t, os.Unsetenv(name))
		}
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	resetFlags(rootCmd)

	saveLogging(t)
	savedConf, savedAppInfo := conf, appInfo
	savedListen, savedLogOutput, savedNotify := listen, logOutput, notifySignals
	t.Cleanup(func() {
		resetFlags(rootCmd)
		setContexts(context.Background(), rootCmd)
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		conf, appInfo = savedConf, savedAppInfo
		listen, logOutput, notifySignals = savedListen, savedLogOutput, savedNotify
	})

	logs, out = &syncBuffer{}, &syncBuffer{}
	appInfo = &AppInfo{Name: "exportarr", Version: "development"}
	listen, logOutput, notifySignals = hooks.listen, logs, hooks.notifySignals
	setContexts(ctx, rootCmd)
	rootCmd.SetOut(out)
	rootCmd.SetErr(out)
	return logs, out
}

type runningCommand struct {
	t      *testing.T
	url    string
	logs   *syncBuffer
	out    *syncBuffer
	client *http.Client
	cancel context.CancelFunc
	sigc   chan<- os.Signal
	done   chan struct{}
	err    error
}

func newHarnessClient() *http.Client {
	return &http.Client{
		Timeout:       time.Minute,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func startCommand(t *testing.T, env map[string]string, args ...string) *runningCommand {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan string, 1)
	sigcc := make(chan chan<- os.Signal, 1)
	logs, out := isolateCommand(ctx, t, env, commandHooks{
		listen: func(string, string) (net.Listener, error) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			select {
			case addrc <- ln.Addr().String():
			default:
			}
			return ln, nil
		},
		notifySignals: func(c chan<- os.Signal, _ ...os.Signal) {
			select {
			case sigcc <- c:
			default:
			}
		},
	})
	rc := &runningCommand{
		t:      t,
		logs:   logs,
		out:    out,
		client: newHarnessClient(),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	rootCmd.SetArgs(args)
	go func() {
		defer close(rc.done)
		rc.err = rootCmd.ExecuteContext(ctx)
	}()
	t.Cleanup(func() {
		defer rc.client.CloseIdleConnections()
		_ = rc.Cancel()
	})

	select {
	case addr := <-addrc:
		rc.url = "http://" + addr
	case <-rc.done:
		t.Fatalf("%v exited before listening: %v\n%s", args, rc.err, out.String())
	case <-time.After(harnessTimeout):
		t.Fatalf("%v did not listen within %s\n%s", args, harnessTimeout, out.String())
	}
	select {
	case rc.sigc = <-sigcc:
	default:
		t.Fatalf("%v listened without registering for signals", args)
	}
	return rc
}

func (rc *runningCommand) URL() string { return rc.url }

func (rc *runningCommand) Logs() string { return rc.logs.String() }

func (rc *runningCommand) Get(path string) (int, string) {
	rc.t.Helper()
	code, body, _ := rc.Do(http.MethodGet, path, nil)
	return code, body
}

// Do never follows redirects and reports failures with Errorf, so it is safe
// to call from other goroutines. Go's client ignores Host in req.Header.
func (rc *runningCommand) Do(method, path string, header http.Header) (int, string, http.Header) {
	rc.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, rc.url+path, nil)
	if err != nil {
		rc.t.Errorf("%s %s: %v", method, path, err)
		return 0, "", nil
	}
	for k, vs := range header {
		if http.CanonicalHeaderKey(k) == "Host" {
			if len(vs) > 0 {
				req.Host = vs[0]
			}
			continue
		}
		req.Header[k] = vs
	}
	resp, err := rc.client.Do(req)
	if err != nil {
		rc.t.Errorf("%s %s: %v", method, path, err)
		return 0, "", nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		rc.t.Errorf("%s %s: reading body: %v", method, path, err)
	}
	return resp.StatusCode, string(body), resp.Header
}

// Stop shuts the command down through its signal channel and asserts a clean exit.
func (rc *runningCommand) Stop() {
	rc.t.Helper()
	select {
	case rc.sigc <- syscall.SIGTERM:
	default:
		rc.t.Fatalf("signal channel is full")
	}
	assert.NoError(rc.t, rc.wait())
}

func (rc *runningCommand) Cancel() error {
	rc.t.Helper()
	rc.cancel()
	return rc.wait()
}

func (rc *runningCommand) wait() error {
	rc.t.Helper()
	select {
	case <-rc.done:
		return rc.err
	case <-time.After(harnessTimeout):
		rc.t.Fatalf("command did not exit within %s\n%s", harnessTimeout, rc.Logs())
		return nil
	}
}

type commandResult struct {
	Out, Logs string
	Err       error
}

// runCommandOutput fails the test if the command gets as far as listening.
func runCommandOutput(t *testing.T, env map[string]string, args ...string) commandResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listened := false
	logs, out := isolateCommand(ctx, t, env, commandHooks{
		listen: func(string, string) (net.Listener, error) {
			listened = true
			return nil, errors.New("listen is not allowed in runCommandOutput")
		},
		notifySignals: func(chan<- os.Signal, ...os.Signal) {},
	})
	rootCmd.SetArgs(args)
	err := rootCmd.ExecuteContext(ctx)
	if listened {
		t.Errorf("%v: listen was called", args)
	}
	return commandResult{Out: out.String(), Logs: logs.String(), Err: err}
}

func runCommandErr(t *testing.T, env map[string]string, args ...string) error {
	t.Helper()
	return runCommandOutput(t, env, args...).Err
}

func TestHarness_Sonarr(t *testing.T) {
	fake := fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: "sonarr", APIKey: fixtures.APIKey})
	rc := startCommand(t, map[string]string{"URL": fake.URL, "API_KEY": fixtures.APIKey}, "sonarr")

	code, body := rc.Get("/metrics")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, body, "sonarr_series_total")
	assert.Contains(t, body, `exportarr_app_info{app_name="exportarr",build_time="",revision="",version="development"} 1`)

	code, body = rc.Get("/healthz")
	assert.Equal(t, code, http.StatusOK)
	assert.Equal(t, body, "OK")

	rc.Stop()
	assert.Contains(t, rc.Logs(), "Starting HTTP Server")
	assert.Contains(t, rc.Logs(), `msg="Shutting down due to signal" signal=terminated`)
}

func TestHarness_Sabnzbd(t *testing.T) {
	fake := fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: "sabnzbd", APIKey: fixtures.APIKey})
	rc := startCommand(t, map[string]string{"URL": fake.URL, "API_KEY": fixtures.APIKey}, "sabnzbd")

	code, body := rc.Get("/metrics")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, body, "sabnzbd_info")
	rc.Stop()
}

func TestHarness_JSONLogs(t *testing.T) {
	fake := fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: "sonarr", APIKey: fixtures.APIKey})
	rc := startCommand(t, map[string]string{"URL": fake.URL, "API_KEY": fixtures.APIKey, "LOG_FORMAT": "json"}, "sonarr")
	rc.Stop()

	lines := strings.Split(strings.TrimSpace(rc.Logs()), "\n")
	assert.GreaterOrEqual(t, len(lines), 3)
	for _, line := range lines {
		assert.True(t, json.Valid([]byte(line)), "not a JSON log line: %q", line)
	}
	assert.Contains(t, rc.Logs(), `"msg":"Starting exportarr"`)
	assert.Contains(t, rc.Logs(), `"signal":"terminated"`)
}

func TestHarness_StartupError(t *testing.T) {
	t.Setenv("URL", "http://leaked-from-the-environment:7878")
	err := runCommandErr(t, nil, "radarr")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "url is required")
}

func TestHarness_ResetsFlagsBetweenRuns(t *testing.T) {
	err := runCommandErr(t, nil, "radarr", "--url", "http://radarr:7878")
	assert.Error(t, err)
	assert.NotContains(t, err.Error(), "url is required")

	err = runCommandErr(t, nil, "radarr")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "url is required")
}

func TestHarness_Help(t *testing.T) {
	res := runCommandOutput(t, nil, "sonarr", "--help")
	assert.NoError(t, res.Err)
	assert.Contains(t, res.Out, "Prometheus Exporter for Sonarr.")
	assert.Contains(t, res.Out, "--series-concurrency")

	res = runCommandOutput(t, nil, "sonarr")
	assert.Error(t, res.Err)
	assert.NotContains(t, res.Out, "Prometheus Exporter for Sonarr.")
}

func TestHarness_RepeatedRuns(t *testing.T) {
	fake := fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: "sonarr", APIKey: fixtures.APIKey})
	env := map[string]string{"URL": fake.URL, "API_KEY": fixtures.APIKey}

	first := startCommand(t, env, "sonarr")
	assert.NoError(t, first.Cancel())
	assert.NotContains(t, first.Logs(), "Shutting down due to signal")

	second := startCommand(t, env, "sonarr")
	code, body := second.Get("/metrics")
	assert.Equal(t, code, http.StatusOK)
	assert.Contains(t, body, "sonarr_series_total")
	second.Stop()
}

func TestHarness_DoMovesHostHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = io.WriteString(w, r.Method+" "+r.Host+" "+r.Header.Get("X-Target")+" "+r.URL.RequestURI()) //nolint:gosec // test echo server
	}))
	t.Cleanup(srv.Close)
	rc := &runningCommand{t: t, url: srv.URL, client: newHarnessClient()}

	code, body, header := rc.Do(http.MethodPost, "/metrics/x?target=y", http.Header{
		"Host":     {"canary.example"},
		"X-Target": {"canary"},
	})
	assert.Equal(t, code, http.StatusTemporaryRedirect)
	assert.Equal(t, header.Get("Location"), "/elsewhere")
	assert.Equal(t, body, "POST canary.example canary /metrics/x?target=y")
}
