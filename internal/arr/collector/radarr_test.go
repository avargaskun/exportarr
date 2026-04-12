package collector

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/test_util"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

const radarr_test_fixtures_path = "../test_fixtures/radarr/"

func newTestRadarrServer(t *testing.T, fn func(http.ResponseWriter, *http.Request)) (*httptest.Server, error) {
	return test_util.NewTestServer(t, radarr_test_fixtures_path, fn)
}

func TestRadarrCollect(t *testing.T) {
	tests := []struct {
		name                  string
		config                *config.ArrConfig
		expected_metrics_file string
		disallowedEndpoints   []string
	}{
		{
			name: "basic",
			config: &config.ArrConfig{
				App:        "radarr",
				ApiVersion: "v3",
			},
			expected_metrics_file: "expected_metrics.txt",
		},
		{
			name: "disable_wanted",
			config: &config.ArrConfig{
				App:           "radarr",
				ApiVersion:    "v3",
				DisableWanted: true,
			},
			expected_metrics_file: "expected_metrics_disable_wanted.txt",
			disallowedEndpoints:   []string{"/api/v3/wanted/cutoff"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			ts, err := newTestRadarrServer(t, func(w http.ResponseWriter, r *http.Request) {
				require.Contains(r.URL.Path, "/api/")
				for _, ep := range tt.disallowedEndpoints {
					require.NotEqual(ep, r.URL.Path, "endpoint %s should not be called when disabled", ep)
				}
			})
			require.NoError(err)

			defer ts.Close()

			tt.config.URL = ts.URL
			tt.config.ApiKey = test_util.API_KEY

			collector := NewRadarrCollector(tt.config)
			require.NoError(err)

			b, err := os.ReadFile(radarr_test_fixtures_path + tt.expected_metrics_file)
			require.NoError(err)

			expected := strings.ReplaceAll(string(b), "SOMEURL", ts.URL)
			f := strings.NewReader(expected)

			require.NotPanics(func() {
				err = testutil.CollectAndCompare(collector, f)
			})
			require.NoError(err)
		})
	}
}

func TestRadarrCollect_FailureDoesntPanic(t *testing.T) {
	require := require.New(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	config := &config.ArrConfig{
		URL:    ts.URL,
		ApiKey: test_util.API_KEY,
	}
	collector := NewRadarrCollector(config)

	f := strings.NewReader("")

	require.NotPanics(func() {
		err := testutil.CollectAndCompare(collector, f)
		require.Error(err)
	}, "Collecting metrics should not panic on failure")
}
