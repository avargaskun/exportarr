package collector

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/onedr0p/exportarr/internal/arr/client"
	"github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/assert"
	"github.com/onedr0p/exportarr/internal/fixtures"
	"github.com/prometheus/client_golang/prometheus"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// captureDefaultLog swaps slog.Default for a text handler without timestamps.
func captureDefaultLog(t *testing.T) *lockedBuffer {
	t.Helper()
	saved := slog.Default()
	t.Cleanup(func() { slog.SetDefault(saved) })
	buf := &lockedBuffer{}
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

func TestCollectorLogger_NoTargetIsByteIdentical(t *testing.T) {
	buf := captureDefaultLog(t)
	for _, name := range []string{"queue", "system_status", "systemHealth", "bazarr"} {
		buf.Reset()
		slog.With("collector", name).Error("Error getting queue", "error", "boom")
		want := buf.String()

		buf.Reset()
		collectorLogger(&config.ArrConfig{}, name).Error("Error getting queue", "error", "boom")
		assert.Equal(t, buf.String(), want)
		assert.NotContains(t, buf.String(), "target=")
	}
}

func TestCollectorLogger_WithTarget(t *testing.T) {
	buf := captureDefaultLog(t)
	collectorLogger(&config.ArrConfig{Target: "sonarr-hd"}, "queue").Info("hello")
	assert.Equal(t, buf.String(), "level=INFO msg=hello collector=queue target=sonarr-hd\n")
}

func TestCollectorLogger_FailingCollectorsLogTarget(t *testing.T) {
	cases := []struct {
		name       string
		app        string
		apiVersion string
		build      func(*client.Client, *config.ArrConfig) prometheus.Collector
	}{
		{name: "queue", app: "sonarr", apiVersion: "v3", build: NewQueueCollector},
		{name: "sonarr", app: "sonarr", apiVersion: "v3", build: NewSonarrCollector},
		{name: "bazarr", app: "bazarr", apiVersion: "", build: NewBazarrCollector},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := fixtures.NewFakeApp(t, fixtures.FakeAppOptions{
				App:      tc.app,
				APIKey:   fixtures.APIKey,
				Behavior: func(*http.Request) fixtures.Action { return fixtures.Status(http.StatusInternalServerError) },
			})
			conf := &config.ArrConfig{
				App:        tc.app,
				APIVersion: tc.apiVersion,
				URL:        fake.URL,
				APIKey:     fixtures.APIKey,
				Target:     "my-target",
				Bazarr:     config.BazarrConfig{SeriesBatchSize: 1, SeriesBatchConcurrency: 1},
			}
			cl, err := client.NewClient(conf)
			assert.NoError(t, err)

			buf := captureDefaultLog(t)
			reg := prometheus.NewRegistry()
			reg.MustRegister(tc.build(cl, conf))
			_, _ = reg.Gather()

			var errorLines int
			for line := range strings.Lines(buf.String()) {
				if strings.Contains(line, "collector=") {
					assert.Contains(t, line, "target=my-target")
				}
				if strings.HasPrefix(line, "level=ERROR") && strings.Contains(line, "target=my-target") {
					errorLines++
				}
			}
			assert.True(t, errorLines > 0, "no level=ERROR line with target=my-target in:\n%s", buf.String())
		})
	}
}
