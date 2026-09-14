package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

var fuzzTargetNames = []string{"sonarr-hd", "radarr"}

const fuzzIndexBody = "<h1>Exportarr</h1><ul><li><a href='/metrics/sonarr-hd'>sonarr-hd</a></li><li><a href='/metrics/radarr'>radarr</a></li></ul>"

func FuzzServeRouting(f *testing.F) {
	seeds := []struct{ method, path, query string }{
		{http.MethodGet, "/metrics/sonarr-hd", ""},
		{http.MethodGet, "/metrics/radarr", ""},
		{http.MethodHead, "/metrics/radarr", ""},
		{"", "/metrics/radarr", ""},
		{http.MethodGet, "/metrics/nope", ""},
		{http.MethodGet, "/metrics/", ""},
		{http.MethodGet, "/metrics/sonarr-hd/", ""},
		{http.MethodGet, "/metrics/SONARR-HD", ""},
		{http.MethodGet, "/metrics/sonarr%2Fhd", ""},
		{http.MethodGet, "/metrics/sonarr-hd%2F", ""},
		{http.MethodGet, "/metrics%2Fradarr", ""},
		{http.MethodGet, "/metrics/http:%2F%2Fevil", ""},
		{http.MethodGet, "/metrics/sonarr-hd/extra", ""},
		{http.MethodPost, "/metrics/sonarr-hd", ""},
		{http.MethodPost, "/", ""},
		{http.MethodPost, "/metrics", ""},
		{http.MethodDelete, "/metrics/radarr", ""},
		{"get", "/metrics/radarr", ""},
		{http.MethodConnect, "//metrics/radarr", ""},
		{http.MethodGet, "/metrics/sonarr-hd", "target=http://evil"},
		{http.MethodGet, "/metrics/radarr", "url=http://evil&target=a&target=b"},
		{http.MethodGet, "/metrics/nope", "target=http://evil"},
		{http.MethodGet, "/metrics/sonarr%2Dhd", ""},
		{http.MethodGet, "/%6detrics/radar%72", ""},
		{http.MethodGet, "//metrics/x", ""},
		{http.MethodGet, "/metrics/../metrics/x", ""},
		{http.MethodGet, "//metrics/sonarr-hd", "a=b"},
		{http.MethodGet, "/metrics/./radarr", ""},
		{http.MethodGet, "//metrics/a%2Fb", ""},
		{http.MethodGet, "//\\evil.example/", ""},
		{http.MethodGet, "", ""},
		{http.MethodGet, "/", ""},
		{http.MethodGet, "/%2F", ""},
		{http.MethodGet, "/healthz", ""},
		{http.MethodGet, "/metrics", ""},
		{http.MethodGet, "/x", ""},
		{http.MethodGet, "/%3Cscript%3Ealert(1)%3C/script%3E", "<script>"},
		{http.MethodGet, "@evil.example/metrics/radarr", ""},
		{http.MethodGet, ":8080/metrics/radarr", ""},
		{http.MethodGet, "/metrics/radarr#frag", ""},
	}
	for _, s := range seeds {
		f.Add(s.method, s.path, s.query)
	}

	ts := make([]*target, len(fuzzTargetNames))
	for i, name := range fuzzTargetNames {
		ts[i] = &target{name: name, handler: stubHandler(name)}
	}
	self := prometheus.NewRegistry()
	self.MustRegister(constCollector{prometheus.NewDesc("self_stub", "test", nil, nil)})
	h := newServeHandler(ts, self)

	f.Fuzz(func(t *testing.T, method, path, query string) {
		req, err := http.NewRequest(method, "http://exportarr.test"+path+"?"+query, nil)
		if err != nil {
			return
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		res := rec.Result()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		checkServeRoute(t, req, res.StatusCode, res.Header, string(body))
	})
}

// checkServeRoute is the routing oracle: a stub answers only for its own exact
// name, and everything else is one of the fixed, input-free responses.
func checkServeRoute(t *testing.T, req *http.Request, code int, header http.Header, body string) {
	t.Helper()
	getLike := req.Method == http.MethodGet || req.Method == http.MethodHead
	if name, ok := strings.CutPrefix(body, "stub:"); ok {
		if !slices.Contains(fuzzTargetNames, name) || !getLike || req.URL.Path != "/metrics/"+name || code != http.StatusOK {
			t.Fatalf("%s %q (path %q) reached stub %q with %d", req.Method, req.URL, req.URL.Path, name, code)
		}
		return
	}
	if name, ok := strings.CutPrefix(req.URL.Path, "/metrics/"); ok && getLike && req.URL.RawPath == "" && slices.Contains(fuzzTargetNames, name) {
		t.Fatalf("%s %q did not reach configured target %q: %d %q", req.Method, req.URL, name, code, body)
	}
	switch {
	case code == http.StatusNotFound:
		if body != notFoundBody || header.Get("Content-Type") != "text/plain; charset=utf-8" || header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s %q: 404 is not the constant response: %q %v", req.Method, req.URL, body, header)
		}
	case code == http.StatusTemporaryRedirect:
		loc := header.Get("Location")
		u, err := url.Parse(loc)
		if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(loc, "/") ||
			strings.HasPrefix(loc, "//") || strings.HasPrefix(loc, "/\\") {
			t.Fatalf("%s %q: redirect leaves the host: Location %q", req.Method, req.URL, loc)
		}
	case code == http.StatusOK && body == "OK":
		if !getLike || req.URL.Path != "/healthz" {
			t.Fatalf("%s %q (path %q) served /healthz", req.Method, req.URL, req.URL.Path)
		}
	case code == http.StatusOK && body == fuzzIndexBody:
		if !getLike || strings.Trim(req.URL.Path, "/") != "" {
			t.Fatalf("%s %q (path %q) served the index", req.Method, req.URL, req.URL.Path)
		}
	case code == http.StatusOK && strings.Contains(body, "self_stub 42"):
		if !getLike || req.URL.Path != "/metrics" {
			t.Fatalf("%s %q (path %q) served self-metrics", req.Method, req.URL, req.URL.Path)
		}
	default:
		t.Fatalf("%s %q: unexpected response %d %q", req.Method, req.URL, code, body)
	}
}
