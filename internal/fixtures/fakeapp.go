package fixtures

import (
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ActionKind selects how a FakeApp answers a request.
type ActionKind int

// Action kinds; the zero value serves the fixture.
const (
	KindServe ActionKind = iota
	KindHang
	KindDelay
	KindStatus
	KindBody
)

// Action is a FakeApp's per-request behavior.
type Action struct {
	Kind    ActionKind
	D       time.Duration
	Code    int
	Payload []byte
}

// Serve answers with the app's fixture.
func Serve() Action { return Action{Kind: KindServe} }

// Hang blocks until the client goes away or the test ends.
func Hang() Action { return Action{Kind: KindHang} }

// Delay waits d, then serves the fixture.
func Delay(d time.Duration) Action { return Action{Kind: KindDelay, D: d} }

// Status answers with code and an empty body.
func Status(code int) Action { return Action{Kind: KindStatus, Code: code} }

// Body answers 200 with b.
func Body(b []byte) Action { return Action{Kind: KindBody, Payload: b} }

// FormAuthCreds are the credentials a FakeApp's login form accepts.
type FormAuthCreds struct {
	Username, Password string
}

// FakeAppOptions configures NewFakeApp.
type FakeAppOptions struct {
	App      string
	APIKey   string
	FormAuth *FormAuthCreds
	Behavior func(*http.Request) Action
	Tracker  *ConcurrencyTracker
}

// Recorded is one request a FakeApp received. QueryKeys holds sorted
// parameter names only, never values.
type Recorded struct {
	Method    string
	Path      string
	QueryKeys []string
	HadKey    bool
}

// ConcurrencyTracker counts in-flight requests and their peak; it can be
// shared by several fakes.
type ConcurrencyTracker struct {
	current atomic.Int64
	peak    atomic.Int64
}

// Current returns the number of requests in flight.
func (c *ConcurrencyTracker) Current() int64 { return c.current.Load() }

// Peak returns the highest number of requests seen in flight at once.
func (c *ConcurrencyTracker) Peak() int64 { return c.peak.Load() }

func (c *ConcurrencyTracker) enter() {
	n := c.current.Add(1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

func (c *ConcurrencyTracker) leave() { c.current.Add(-1) }

// FakeApp is an *arr or SABnzbd backend serving the repo's fixtures.
type FakeApp struct {
	URL string

	t        testing.TB
	opts     FakeAppOptions
	sabnzbd  bool
	cookie   string
	token    string
	done     chan struct{}
	inFlight ConcurrencyTracker

	mu       sync.Mutex
	requests []Recorded
	logins   int
}

var arrApps = []string{"radarr", "sonarr", "lidarr", "prowlarr", "bazarr"}

// NewFakeApp starts a FakeApp that is closed when the test ends.
func NewFakeApp(t testing.TB, o FakeAppOptions) *FakeApp {
	t.Helper()
	sabnzbd := o.App == "sabnzbd"
	switch {
	case !sabnzbd && !slices.Contains(arrApps, o.App):
		t.Fatalf("fakeapp: unknown app %q", o.App)
		return nil
	case o.APIKey == "":
		t.Fatalf("fakeapp %s: APIKey is required", o.App)
		return nil
	case sabnzbd && o.FormAuth != nil:
		t.Fatalf("fakeapp %s: form auth is not supported", o.App)
		return nil
	}
	f := &FakeApp{
		t:       t,
		opts:    o,
		sabnzbd: sabnzbd,
		cookie:  strings.ToUpper(o.App[:1]) + o.App[1:] + "Auth",
		token:   rand.Text(),
		done:    make(chan struct{}),
	}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	f.URL = srv.URL
	t.Cleanup(func() {
		close(f.done)
		srv.Close()
	})
	return f
}

// Requests returns a copy of every request received so far.
func (f *FakeApp) Requests() []Recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// InFlight returns the number of requests this fake is handling.
func (f *FakeApp) InFlight() int64 { return f.inFlight.Current() }

// PeakInFlight returns the most requests this fake handled at once.
func (f *FakeApp) PeakInFlight() int64 { return f.inFlight.Peak() }

// HasFormAuth reports whether the fake serves a login form.
func (f *FakeApp) HasFormAuth() bool { return f.opts.FormAuth != nil }

// Logins returns the number of successful form logins.
func (f *FakeApp) Logins() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

func (f *FakeApp) handle(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	f.inFlight.enter()
	defer f.inFlight.leave()
	if f.opts.Tracker != nil {
		f.opts.Tracker.enter()
		defer f.opts.Tracker.leave()
	}

	if !f.sabnzbd && f.opts.FormAuth != nil && r.Method == http.MethodPost && r.URL.Path == "/login" {
		f.login(w, r)
		return
	}
	if !f.authorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	a := Serve()
	if f.opts.Behavior != nil {
		a = f.opts.Behavior(r)
	}
	switch a.Kind {
	case KindServe:
		f.serveFixture(w, r)
	case KindHang:
		select {
		case <-r.Context().Done():
		case <-f.done:
		}
	case KindDelay:
		timer := time.NewTimer(a.D)
		defer timer.Stop()
		select {
		case <-timer.C:
			f.serveFixture(w, r)
		case <-r.Context().Done():
		case <-f.done:
		}
	case KindStatus:
		w.WriteHeader(a.Code)
	case KindBody:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(a.Payload) //nolint:gosec // test payload, not user input
	default:
		f.t.Errorf("fakeapp %s: unknown action kind %d", f.opts.App, a.Kind)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (f *FakeApp) record(r *http.Request) {
	q := r.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	rec := Recorded{Method: r.Method, Path: r.URL.Path, QueryKeys: keys, HadKey: f.hasKey(r)}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	f.mu.Unlock()
}

func (f *FakeApp) hasKey(r *http.Request) bool {
	if f.sabnzbd {
		return r.URL.Query().Get("apikey") == f.opts.APIKey
	}
	return r.Header.Get("X-Api-Key") == f.opts.APIKey
}

func (f *FakeApp) authorized(r *http.Request) bool {
	if !f.hasKey(r) {
		return false
	}
	if f.opts.FormAuth == nil {
		return true
	}
	c, err := r.Cookie(f.cookie)
	return err == nil && c.Value == f.token
}

func (f *FakeApp) login(w http.ResponseWriter, r *http.Request) {
	creds := f.opts.FormAuth
	if r.PostFormValue("username") != creds.Username || r.PostFormValue("password") != creds.Password {
		w.Header().Set("Location", "/login?loginFailed=true")
		w.WriteHeader(http.StatusFound)
		return
	}
	f.mu.Lock()
	f.logins++
	f.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: f.cookie, Value: f.token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Location", "/")
	w.WriteHeader(http.StatusFound)
}

func (f *FakeApp) serveFixture(w http.ResponseWriter, r *http.Request) {
	var dirs []string
	var name, what string
	if f.sabnzbd {
		mode := r.URL.Query().Get("mode")
		what = r.URL.Path + "?mode=" + mode
		if r.URL.Path == "/api" && mode != "" && !strings.ContainsAny(mode, `/\.`) {
			dirs, name = []string{SabnzbdTestdata()}, mode+".json"
		}
	} else {
		what = r.URL.Path
		name = strings.ReplaceAll(strings.TrimPrefix(r.URL.Path, "/api/"), "/", "_") + ".json"
		dirs = []string{ArrTestdata(f.opts.App), ArrTestdata("common")}
	}
	for _, dir := range dirs {
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // fixture name has no path separators
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b) //nolint:gosec // fixture bytes, not user input
			return
		}
	}
	f.t.Errorf("fakeapp %s: no fixture for %s", f.opts.App, what)
	w.WriteHeader(http.StatusNotFound)
}

// Canary is a server that fails the test on any request it receives.
type Canary struct {
	URL string

	hits atomic.Int64
}

// NewCanary starts a Canary that is closed when the test ends.
func NewCanary(t testing.TB) *Canary {
	t.Helper()
	c := &Canary{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		t.Errorf("canary received %s %s", r.Method, r.URL.RequestURI())
		w.WriteHeader(http.StatusNotFound)
	}))
	c.URL = srv.URL
	t.Cleanup(srv.Close)
	return c
}

// Hits returns the number of requests the canary received.
func (c *Canary) Hits() int { return int(c.hits.Load()) }

func fixturesDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

// ArrTestdata returns the absolute path of an *arr app's fixture directory.
func ArrTestdata(app string) string {
	return filepath.Join(fixturesDir(), "..", "arr", "testdata", app)
}

// SabnzbdTestdata returns the absolute path of the SABnzbd fixture directory.
func SabnzbdTestdata() string {
	return filepath.Join(fixturesDir(), "..", "sabnzbd", "testdata")
}
