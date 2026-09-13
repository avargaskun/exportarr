package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/onedr0p/exportarr/internal/assert"
)

func TestHealthzHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	HealthzHandler(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	assert.Equal(t, rec.Code, http.StatusOK)
	assert.Equal(t, rec.Body.String(), "OK")
}

func TestIndexHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	IndexHandler(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, rec.Code, http.StatusOK)
	assert.Contains(t, rec.Body.String(), "/metrics")
}

func TestTargetIndexHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	TargetIndexHandler([]string{"sonarr-hd", "radarr", "sab_1"}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, rec.Code, http.StatusOK)
	assert.Equal(t, rec.Header().Get("Content-Type"), "text/html; charset=utf-8")
	assert.Equal(t, rec.Body.String(), "<h1>Exportarr</h1><ul>"+
		"<li><a href='/metrics/sonarr-hd'>sonarr-hd</a></li>"+
		"<li><a href='/metrics/radarr'>radarr</a></li>"+
		"<li><a href='/metrics/sab_1'>sab_1</a></li>"+
		"</ul>")
}

func TestTargetIndexHandler_Empty(t *testing.T) {
	rec := httptest.NewRecorder()
	TargetIndexHandler(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, rec.Code, http.StatusOK)
	assert.Equal(t, rec.Body.String(), "<h1>Exportarr</h1><ul></ul>")
}

func TestTargetIndexHandler_EscapesNames(t *testing.T) {
	rec := httptest.NewRecorder()
	TargetIndexHandler([]string{"<script>alert('x')</script>&"}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	assert.NotContains(t, body, "<script>")
	assert.NotContains(t, body, "alert('x')")
	assert.Equal(t, body, "<h1>Exportarr</h1><ul>"+
		"<li><a href='/metrics/&lt;script&gt;alert(&#39;x&#39;)&lt;/script&gt;&amp;'>"+
		"&lt;script&gt;alert(&#39;x&#39;)&lt;/script&gt;&amp;</a></li></ul>")
}

func TestNotFoundHandler_IsConstant(t *testing.T) {
	cases := []struct {
		method, target string
	}{
		{http.MethodGet, "/x"},
		{http.MethodGet, "/metrics/nope"},
		{http.MethodPost, "/"},
		{http.MethodPut, "/metrics/sonarr"},
		{http.MethodDelete, "/metrics/http:%2F%2Fevil"},
		{http.MethodGet, "/probe?target=http://evil.example/%3Cscript%3E"},
		{http.MethodGet, "/%3Cscript%3Ealert(1)%3C/script%3E?q=%3Cb%3E"},
		{http.MethodHead, "/secret-path-marker"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader("request-body-marker"))
			rec := httptest.NewRecorder()
			NotFoundHandler(rec, req)

			assert.Equal(t, rec.Code, http.StatusNotFound)
			assert.Equal(t, rec.Header().Get("Content-Type"), "text/plain; charset=utf-8")
			assert.Equal(t, rec.Header().Get("X-Content-Type-Options"), "nosniff")
			body := rec.Body.String()
			assert.Equal(t, body, "404 page not found\n")
			for _, part := range []string{req.URL.Path, req.URL.RawQuery, "<", "evil", "marker"} {
				if part != "" {
					assert.NotContains(t, body, part)
				}
			}
			assert.Equal(t, len(rec.Header()), 2, "unexpected headers %v", rec.Header())
		})
	}
}

func TestRecoveryHandler(t *testing.T) {
	h := RecoveryHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	assert.NotPanics(t, func() {
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	})
	assert.Equal(t, rec.Code, http.StatusInternalServerError)
}

func TestLogHandler_PassesThroughStatusAndBody(t *testing.T) {
	h := LogHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("nope"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))

	assert.Equal(t, rec.Code, http.StatusNotFound)
	assert.Equal(t, rec.Body.String(), "nope")
}

// labelValue returns the value of the named label on the first metric of the
// named family, failing the test if the family is not in mfs.
func labelValue(t *testing.T, reg *prometheus.Registry, family, label string) string {
	t.Helper()
	mfs, err := reg.Gather()
	assert.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != family {
			continue
		}
		for _, lp := range mf.GetMetric()[0].GetLabel() {
			if lp.GetName() == label {
				return lp.GetValue()
			}
		}
	}
	t.Fatalf("metric family %q with label %q not gathered", family, label)
	return ""
}

func TestMetricsHandler_RecordsDurationAndStatusCode(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := MetricsHandler("radarr", "http://radarr:7878", reg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	// The wrapped writer must pass the inner handler's status through.
	assert.Equal(t, rec.Code, http.StatusTeapot)
	// The counter labels the request with the inner handler's status code.
	assert.Equal(t, labelValue(t, reg, "radarr_scrape_requests_total", "code"), "418")

	mfs, err := reg.Gather()
	assert.NoError(t, err)
	var sawDuration bool
	for _, mf := range mfs {
		if mf.GetName() == "radarr_scrape_duration_seconds" {
			sawDuration = true
			assert.Equal(t, mf.GetMetric()[0].GetHistogram().GetSampleCount(), uint64(1))
		}
	}
	assert.True(t, sawDuration, "radarr_scrape_duration_seconds not gathered")
}

func TestMetricsHandler_DefaultsToStatusOK(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := MetricsHandler("sonarr", "http://sonarr:8989", reg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok")) // no explicit WriteHeader
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	assert.Equal(t, labelValue(t, reg, "sonarr_scrape_requests_total", "code"), "200")
}

func TestMetricsHandler_LabelsBothFamiliesWithURL(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := MetricsHandler("lidarr", "http://lidarr-hd:8686", reg, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))

	assert.Equal(t, labelValue(t, reg, "lidarr_scrape_requests_total", "url"), "http://lidarr-hd:8686")
	assert.Equal(t, labelValue(t, reg, "lidarr_scrape_duration_seconds", "url"), "http://lidarr-hd:8686")
}
