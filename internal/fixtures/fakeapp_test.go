package fixtures

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	arrclient "github.com/onedr0p/exportarr/internal/arr/client"
	"github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/arr/model"
	"github.com/onedr0p/exportarr/internal/assert"
)

type recorder struct {
	testing.TB

	mu     sync.Mutex
	errs   []string
	fatals []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

func (r *recorder) errors() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.errs)
}

var noRedirect = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Timeout:       10 * time.Second,
}

type response struct {
	code   int
	body   string
	header http.Header
}

func send(t *testing.T, method, rawURL string, header http.Header, body io.Reader) response {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, body)
	assert.NoError(t, err)
	maps.Copy(req.Header, header)
	resp, err := noRedirect.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	return response{code: resp.StatusCode, body: string(b), header: resp.Header}
}

func keyHeader(key string) http.Header {
	return http.Header{"X-Api-Key": {key}}
}

func getArr(t *testing.T, f *FakeApp, path string) response {
	t.Helper()
	return send(t, http.MethodGet, f.URL+path, keyHeader(APIKey), nil)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readFixture(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // fixture path
	assert.NoError(t, err)
	return string(b)
}

func TestFakeApp_ArrRequiresAPIKey(t *testing.T) {
	f := NewFakeApp(t, FakeAppOptions{App: "radarr", APIKey: APIKey})

	assert.Equal(t, send(t, http.MethodGet, f.URL+"/api/v3/movie", nil, nil).code, http.StatusUnauthorized)
	assert.Equal(t, send(t, http.MethodGet, f.URL+"/api/v3/movie", keyHeader("wrong"), nil).code, http.StatusUnauthorized)
	assert.Equal(t, send(t, http.MethodGet, f.URL+"/api/v3/movie?apikey="+APIKey, nil, nil).code, http.StatusUnauthorized)

	resp := getArr(t, f, "/api/v3/movie")
	assert.Equal(t, resp.code, http.StatusOK)
	assert.Equal(t, resp.header.Get("Content-Type"), "application/json")
	assert.Equal(t, resp.body, readFixture(t, ArrTestdata("radarr"), "v3_movie.json"))
}

func TestFakeApp_SabnzbdRequiresAPIKeyParam(t *testing.T) {
	f := NewFakeApp(t, FakeAppOptions{App: "sabnzbd", APIKey: APIKey})

	assert.Equal(t, send(t, http.MethodGet, f.URL+"/api?mode=queue", nil, nil).code, http.StatusUnauthorized)
	assert.Equal(t, send(t, http.MethodGet, f.URL+"/api?mode=queue&apikey=wrong", nil, nil).code, http.StatusUnauthorized)
	assert.Equal(t, send(t, http.MethodGet, f.URL+"/api?mode=queue", keyHeader(APIKey), nil).code, http.StatusUnauthorized)

	resp := send(t, http.MethodGet, f.URL+"/api?mode=queue&output=json&apikey="+APIKey, nil, nil)
	assert.Equal(t, resp.code, http.StatusOK)
	assert.Equal(t, resp.body, readFixture(t, SabnzbdTestdata(), "queue.json"))
}

func TestFakeApp_ArrFallsBackToCommonFixtures(t *testing.T) {
	f := NewFakeApp(t, FakeAppOptions{App: "sonarr", APIKey: APIKey})
	resp := getArr(t, f, "/api/v3/system/status")
	assert.Equal(t, resp.code, http.StatusOK)
	assert.Equal(t, resp.body, readFixture(t, ArrTestdata("common"), "v3_system_status.json"))
}

func TestFakeApp_Actions(t *testing.T) {
	movie := readFixture(t, ArrTestdata("radarr"), "v3_movie.json")
	tests := []struct {
		name     string
		action   Action
		wantCode int
		wantBody string
		minDelay time.Duration
	}{
		{name: "serve", action: Serve(), wantCode: http.StatusOK, wantBody: movie},
		{name: "zero value serves", action: Action{}, wantCode: http.StatusOK, wantBody: movie},
		{name: "status", action: Status(http.StatusServiceUnavailable), wantCode: http.StatusServiceUnavailable},
		{name: "body", action: Body([]byte("not json")), wantCode: http.StatusOK, wantBody: "not json"},
		{name: "delay", action: Delay(50 * time.Millisecond), wantCode: http.StatusOK, wantBody: movie, minDelay: 50 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			f := NewFakeApp(t, FakeAppOptions{App: "radarr", APIKey: APIKey, Behavior: func(r *http.Request) Action {
				mu.Lock()
				defer mu.Unlock()
				seen = append(seen, r.URL.Path)
				return tt.action
			}})
			start := time.Now()
			resp := getArr(t, f, "/api/v3/movie")
			assert.Equal(t, resp.code, tt.wantCode)
			assert.Equal(t, resp.body, tt.wantBody)
			assert.GreaterOrEqual(t, time.Since(start), tt.minDelay)
			mu.Lock()
			defer mu.Unlock()
			assert.DeepEqual(t, seen, []string{"/api/v3/movie"})
		})
	}
}

func TestFakeApp_BehaviorSelectsByRequest(t *testing.T) {
	f := NewFakeApp(t, FakeAppOptions{App: "radarr", APIKey: APIKey, Behavior: func(r *http.Request) Action {
		if r.URL.Path == "/api/v3/movie" {
			return Status(http.StatusInternalServerError)
		}
		return Serve()
	}})
	assert.Equal(t, getArr(t, f, "/api/v3/movie").code, http.StatusInternalServerError)
	assert.Equal(t, getArr(t, f, "/api/v3/health").code, http.StatusOK)
}

func TestFakeApp_BehaviorNotConsultedWhenUnauthorized(t *testing.T) {
	var called atomic.Bool
	f := NewFakeApp(t, FakeAppOptions{App: "radarr", APIKey: APIKey, Behavior: func(*http.Request) Action {
		called.Store(true)
		return Serve()
	}})
	assert.Equal(t, send(t, http.MethodGet, f.URL+"/api/v3/movie", nil, nil).code, http.StatusUnauthorized)
	assert.False(t, called.Load())
}

func TestFakeApp_UnknownActionKind(t *testing.T) {
	rec := &recorder{TB: t}
	f := NewFakeApp(rec, FakeAppOptions{App: "radarr", APIKey: APIKey, Behavior: func(*http.Request) Action {
		return Action{Kind: ActionKind(99)}
	}})
	assert.Equal(t, getArr(t, f, "/api/v3/movie").code, http.StatusInternalServerError)
	assert.DeepEqual(t, rec.errors(), []string{"fakeapp radarr: unknown action kind 99"})
}

func requestAsync(ctx context.Context, f *FakeApp, path string) <-chan error {
	done := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL+path, nil)
		if err != nil {
			done <- err
			return
		}
		req.Header.Set("X-Api-Key", APIKey)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done <- err
	}()
	return done
}

func awaitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("request did not return")
		return nil
	}
}

func TestFakeApp_DelayStopsWhenClientGoesAway(t *testing.T) {
	f := NewFakeApp(t, FakeAppOptions{App: "radarr", APIKey: APIKey, Behavior: func(*http.Request) Action {
		return Delay(time.Minute)
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := requestAsync(ctx, f, "/api/v3/movie")
	waitFor(t, "the request to arrive", func() bool { return f.InFlight() == 1 })
	cancel()
	assert.Error(t, awaitResult(t, done))
	waitFor(t, "the handler to return", func() bool { return f.InFlight() == 0 })
}

func TestFakeApp_HangReleasedByClientContext(t *testing.T) {
	f := NewFakeApp(t, FakeAppOptions{App: "radarr", APIKey: APIKey, Behavior: func(*http.Request) Action {
		return Hang()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := requestAsync(ctx, f, "/api/v3/movie")
	waitFor(t, "the request to arrive", func() bool { return f.InFlight() == 1 })
	cancel()
	assert.Error(t, awaitResult(t, done))
	waitFor(t, "the handler to return", func() bool { return f.InFlight() == 0 })
}

func TestFakeApp_ReleasedByCleanup(t *testing.T) {
	for _, action := range []Action{Hang(), Delay(time.Minute)} {
		t.Run(fmt.Sprintf("kind %d", action.Kind), func(t *testing.T) {
			var done <-chan error
			start := time.Now()
			t.Run("fake", func(t *testing.T) {
				f := NewFakeApp(t, FakeAppOptions{App: "radarr", APIKey: APIKey, Behavior: func(*http.Request) Action {
					return action
				}})
				done = requestAsync(context.Background(), f, "/api/v3/movie")
				waitFor(t, "the request to arrive", func() bool { return f.InFlight() == 1 })
			})
			_ = awaitResult(t, done)
			assert.True(t, time.Since(start) < 4*time.Second, "cleanup took %v", time.Since(start))
		})
	}
}

func TestFakeApp_FormAuth(t *testing.T) {
	creds := &FormAuthCreds{Username: "admin", Password: "s3cret"}
	f := NewFakeApp(t, FakeAppOptions{App: "radarr", APIKey: APIKey, FormAuth: creds})
	assert.True(t, f.HasFormAuth())
	form := func(user, pass string) io.Reader {
		return strings.NewReader(url.Values{"username": {user}, "password": {pass}}.Encode())
	}
	formHeader := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}

	failed := send(t, http.MethodPost, f.URL+"/login", formHeader, form("admin", "wrong"))
	assert.Equal(t, failed.code, http.StatusFound)
	assert.Equal(t, failed.header.Get("Location"), "/login?loginFailed=true")
	assert.Equal(t, failed.header.Get("Set-Cookie"), "")
	assert.Equal(t, f.Logins(), 0)

	ok := send(t, http.MethodPost, f.URL+"/login?ReturnUrl=%2Fgeneral%2Fsettings", formHeader, form("admin", "s3cret"))
	assert.Equal(t, ok.code, http.StatusFound)
	assert.Equal(t, f.Logins(), 1)
	cookies := (&http.Response{Header: ok.header}).Cookies()
	assert.Len(t, cookies, 1)
	cookie := cookies[0]
	assert.Equal(t, cookie.Name, "RadarrAuth")
	assert.True(t, strings.HasSuffix(cookie.Name, "arrAuth"))
	assert.NotEmpty(t, cookie.Value)

	withCookie := func(key, value string) http.Header {
		h := keyHeader(key)
		h.Set("Cookie", cookie.Name+"="+value)
		return h
	}
	assert.Equal(t, getArr(t, f, "/api/v3/movie").code, http.StatusUnauthorized)
	assert.Equal(t, send(t, http.MethodGet, f.URL+"/api/v3/movie", withCookie(APIKey, "forged"), nil).code, http.StatusUnauthorized)
	assert.Equal(t, send(t, http.MethodGet, f.URL+"/api/v3/movie", withCookie("wrong", cookie.Value), nil).code, http.StatusUnauthorized)
	assert.Equal(t, send(t, http.MethodGet, f.URL+"/api/v3/movie", withCookie(APIKey, cookie.Value), nil).code, http.StatusOK)
}

func TestFakeApp_LoginNeedsFormAuth(t *testing.T) {
	f := NewFakeApp(t, FakeAppOptions{App: "radarr", APIKey: APIKey})
	assert.False(t, f.HasFormAuth())
	resp := send(t, http.MethodPost, f.URL+"/login", nil, strings.NewReader("username=a&password=b"))
	assert.Equal(t, resp.code, http.StatusUnauthorized)
	assert.Equal(t, f.Logins(), 0)
}

func TestFakeApp_FormAuthWithArrClient(t *testing.T) {
	creds := &FormAuthCreds{Username: "admin", Password: "s3cret"}
	f := NewFakeApp(t, FakeAppOptions{App: "sonarr", APIKey: APIKey, FormAuth: creds})
	newClient := func(password string) *arrclient.Client {
		c, err := arrclient.NewClient(&config.ArrConfig{
			App: "sonarr", APIVersion: "v3", URL: f.URL, APIKey: APIKey,
			FormAuth: true, AuthUsername: creds.Username, AuthPassword: password,
			RequestTimeout: 5 * time.Second,
		})
		assert.NoError(t, err)
		return c
	}

	c := newClient(creds.Password)
	for range 3 {
		status, err := arrclient.Get[model.SystemStatus](c, "system/status")
		assert.NoError(t, err)
		assert.NotEmpty(t, status.Version)
	}
	assert.Equal(t, f.Logins(), 1)

	_, err := arrclient.Get[model.SystemStatus](newClient("wrong"), "system/status")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Login Failed")
	assert.Equal(t, f.Logins(), 1)
}

func TestFakeApp_RecordsRequests(t *testing.T) {
	f := NewFakeApp(t, FakeAppOptions{App: "radarr", APIKey: APIKey})
	getArr(t, f, "/api/v3/movie?zeta=value-zeta&alpha=value-alpha&apikey=value-key")
	send(t, http.MethodPost, f.URL+"/api/v3/queue", keyHeader("wrong"), nil)

	got := f.Requests()
	assert.DeepEqual(t, got, []Recorded{
		{Method: http.MethodGet, Path: "/api/v3/movie", QueryKeys: []string{"alpha", "apikey", "zeta"}, HadKey: true},
		{Method: http.MethodPost, Path: "/api/v3/queue", QueryKeys: []string{}, HadKey: false},
	})
	assert.NotContains(t, fmt.Sprintf("%+v", got), "value-")

	got[0].Path = "changed"
	assert.Equal(t, f.Requests()[0].Path, "/api/v3/movie")

	s := NewFakeApp(t, FakeAppOptions{App: "sabnzbd", APIKey: APIKey})
	send(t, http.MethodGet, s.URL+"/api?mode=queue&output=json&apikey="+APIKey, nil, nil)
	send(t, http.MethodGet, s.URL+"/api?mode=queue", keyHeader(APIKey), nil)
	assert.DeepEqual(t, s.Requests(), []Recorded{
		{Method: http.MethodGet, Path: "/api", QueryKeys: []string{"apikey", "mode", "output"}, HadKey: true},
		{Method: http.MethodGet, Path: "/api", QueryKeys: []string{"mode"}, HadKey: false},
	})
}

func TestFakeApp_ConcurrencyTracking(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	hold := func(*http.Request) Action {
		<-release
		return Serve()
	}
	tracker := &ConcurrencyTracker{}
	a := NewFakeApp(t, FakeAppOptions{App: "radarr", APIKey: APIKey, Behavior: hold, Tracker: tracker})
	b := NewFakeApp(t, FakeAppOptions{App: "sonarr", APIKey: APIKey, Behavior: hold, Tracker: tracker})

	var results []<-chan error
	for range 3 {
		results = append(results, requestAsync(context.Background(), a, "/api/v3/movie"))
	}
	for range 2 {
		results = append(results, requestAsync(context.Background(), b, "/api/v3/series"))
	}
	waitFor(t, "five requests in flight", func() bool { return tracker.Current() == 5 })
	assert.Equal(t, a.InFlight(), int64(3))
	assert.Equal(t, b.InFlight(), int64(2))

	once.Do(func() { close(release) })
	for _, done := range results {
		assert.NoError(t, awaitResult(t, done))
	}
	waitFor(t, "all handlers to return", func() bool { return tracker.Current() == 0 })
	assert.Equal(t, tracker.Peak(), int64(5))
	assert.Equal(t, a.PeakInFlight(), int64(3))
	assert.Equal(t, b.PeakInFlight(), int64(2))
	assert.Equal(t, a.InFlight(), int64(0))
}

func TestFakeApp_MissingFixture(t *testing.T) {
	rec := &recorder{TB: t}
	arr := NewFakeApp(rec, FakeAppOptions{App: "radarr", APIKey: APIKey})
	assert.Equal(t, getArr(t, arr, "/api/v3/nope").code, http.StatusNotFound)
	assert.Equal(t, getArr(t, arr, "/login").code, http.StatusNotFound)

	sab := NewFakeApp(rec, FakeAppOptions{App: "sabnzbd", APIKey: APIKey})
	for _, q := range []string{"/api?mode=nope", "/api?mode=..%2Fqueue", "/api?mode=", "/other?mode=queue"} {
		assert.Equal(t, send(t, http.MethodGet, sab.URL+q+"&apikey="+APIKey, nil, nil).code, http.StatusNotFound, q)
	}

	assert.DeepEqual(t, rec.errors(), []string{
		"fakeapp radarr: no fixture for /api/v3/nope",
		"fakeapp radarr: no fixture for /login",
		"fakeapp sabnzbd: no fixture for /api?mode=nope",
		"fakeapp sabnzbd: no fixture for /api?mode=../queue",
		"fakeapp sabnzbd: no fixture for /api?mode=",
		"fakeapp sabnzbd: no fixture for /other?mode=queue",
	})
}

func TestNewFakeApp_InvalidOptions(t *testing.T) {
	tests := []struct {
		name string
		opts FakeAppOptions
		want string
	}{
		{name: "unknown app", opts: FakeAppOptions{App: "plex", APIKey: APIKey}, want: `fakeapp: unknown app "plex"`},
		{name: "empty app", opts: FakeAppOptions{APIKey: APIKey}, want: `fakeapp: unknown app ""`},
		{name: "no api key", opts: FakeAppOptions{App: "radarr"}, want: "fakeapp radarr: APIKey is required"},
		{name: "sabnzbd form auth", opts: FakeAppOptions{App: "sabnzbd", APIKey: APIKey, FormAuth: &FormAuthCreds{}}, want: "fakeapp sabnzbd: form auth is not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{TB: t}
			assert.Nil(t, NewFakeApp(rec, tt.opts))
			assert.DeepEqual(t, rec.fatals, []string{tt.want})
		})
	}
}

func TestCanary(t *testing.T) {
	rec := &recorder{TB: t}
	c := NewCanary(rec)
	assert.Equal(t, c.Hits(), 0)

	resp := send(t, http.MethodGet, c.URL+"/metrics?target=x", nil, nil)
	assert.Equal(t, resp.code, http.StatusNotFound)
	send(t, http.MethodPost, c.URL+"/login", nil, nil)

	assert.Equal(t, c.Hits(), 2)
	assert.DeepEqual(t, rec.errors(), []string{
		"canary received GET /metrics?target=x",
		"canary received POST /login",
	})
}

func TestTestdataPaths(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, p := range []string{
		filepath.Join(ArrTestdata("common"), "v3_queue.json"),
		filepath.Join(ArrTestdata("prowlarr"), "v1_indexer.json"),
		filepath.Join(SabnzbdTestdata(), "server_stats.json"),
	} {
		assert.True(t, filepath.IsAbs(p), p)
		_, err := os.Stat(p)
		assert.NoError(t, err, p)
	}
}

func TestFakeApp_FixtureCompleteness(t *testing.T) {
	endpoints := map[string][]string{
		"radarr": {
			"/api/v3/queue", "/api/v3/rootfolder", "/api/v3/diskspace", "/api/v3/system/status",
			"/api/v3/health", "/api/v3/history", "/api/v3/movie", "/api/v3/tag/detail",
			"/api/v3/qualitydefinition", "/api/v3/wanted/cutoff",
		},
		"sonarr": {
			"/api/v3/queue", "/api/v3/rootfolder", "/api/v3/diskspace", "/api/v3/system/status",
			"/api/v3/health", "/api/v3/history", "/api/v3/series", "/api/v3/qualitydefinition",
			"/api/v3/episodefile", "/api/v3/episode", "/api/v3/wanted/missing", "/api/v3/wanted/cutoff",
			"/api/v3/tag/detail",
		},
		"lidarr": {
			"/api/v1/queue", "/api/v1/rootfolder", "/api/v1/diskspace", "/api/v1/system/status",
			"/api/v1/health", "/api/v1/history", "/api/v1/artist", "/api/v1/qualitydefinition",
			"/api/v1/trackfile", "/api/v1/album", "/api/v1/wanted/missing",
		},
		"prowlarr": {
			"/api/v1/indexer", "/api/v1/indexerstats", "/api/v1/system/status", "/api/v1/health",
			"/api/v1/history",
		},
		"bazarr": {
			"/api/badges", "/api/series", "/api/episodes", "/api/episodes/history",
			"/api/movies/history", "/api/movies", "/api/system/health", "/api/system/status",
		},
		"sabnzbd": {"/api?mode=queue", "/api?mode=server_stats"},
	}
	for app, paths := range endpoints {
		t.Run(app, func(t *testing.T) {
			f := NewFakeApp(t, FakeAppOptions{App: app, APIKey: APIKey})
			for _, p := range paths {
				var resp response
				if app == "sabnzbd" {
					resp = send(t, http.MethodGet, f.URL+p+"&output=json&apikey="+APIKey, nil, nil)
				} else {
					resp = getArr(t, f, p)
				}
				assert.Equal(t, resp.code, http.StatusOK, p)
				assert.NotEmpty(t, strings.TrimSpace(resp.body), p)
			}
		})
	}
}
