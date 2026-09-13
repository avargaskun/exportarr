package collector

import (
	"github.com/onedr0p/exportarr/internal/assert"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	client "github.com/onedr0p/exportarr/internal/arr/client"
	"github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/arr/model"
	"github.com/onedr0p/exportarr/internal/fixtures"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type testCollector struct {
	emitter ExtraHealthMetricEmitter
	msg     model.SystemHealthMessage
}

func (c *testCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.emitter.Describe()
}

func (c *testCollector) Collect(ch chan<- prometheus.Metric) {
	for _, metric := range c.emitter.Emit(c.msg) {
		ch <- metric
	}
}

func TestUnavailableIndexerEmitter(t *testing.T) {
	emitter := NewUnavailableIndexerEmitter("http://localhost:9117")
	assert.NotNil(t, emitter.Describe())

	msg := model.SystemHealthMessage{
		Source:  "IndexerLongTermStatusCheck",
		Type:    "warning",
		WikiURL: "https://wiki.servarr.com/prowlarr/system#indexers-are-unavailable-due-to-failures",
		Message: "Indexers unavailable due to failures for more than 6 hours: Server1, ServerTwo, ServerTHREE, Server.four",
	}
	metrics := emitter.Emit(msg)
	assert.Len(t, metrics, 4)

	testCol := &testCollector{
		emitter: emitter,
		msg:     msg,
	}

	expected := strings.NewReader(
		`# HELP prowlarr_indexer_unavailable Indexers marked unavailable due to repeated errors
		# TYPE prowlarr_indexer_unavailable gauge
		prowlarr_indexer_unavailable{indexer="Server.four",url="http://localhost:9117"} 1
		prowlarr_indexer_unavailable{indexer="Server1",url="http://localhost:9117"} 1
		prowlarr_indexer_unavailable{indexer="ServerTHREE",url="http://localhost:9117"} 1
		prowlarr_indexer_unavailable{indexer="ServerTwo",url="http://localhost:9117"} 1
		`)
	err := testutil.CollectAndCompare(testCol, expected)
	assert.NoError(t, err)
}

func TestProwlarrCollect_PanicReleasesStatsLock(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/indexer":
			_, _ = w.Write([]byte(`[{"name":"a","enable":true}]`))
		case "/api/v1/indexerstats":
			_, _ = w.Write([]byte(`{"indexers":[{"indexerName":"a","numberOfQueries":1}],"userAgents":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	conf := &config.ArrConfig{App: "prowlarr", APIVersion: "v1", URL: ts.URL, APIKey: fixtures.APIKey}
	cl, err := client.NewClient(conf)
	assert.NoError(t, err)
	collector := NewProwlarrCollector(cl, conf).(*prowlarrCollector)

	collector.indexerStatCache = newStatCache(func(model.IndexerStats, model.IndexerStats) model.IndexerStats {
		panic("boom")
	})
	assert.True(t, collectorFailed(t, collector), "a panicking collection should raise the error gauge")

	collector.indexerStatCache = newStatCache(mergeIndexerStats)
	done := make(chan bool)
	go func() {
		failed, err := gatherErrorGauge(collector)
		done <- err == nil && !failed
	}()
	select {
	case ok := <-done:
		assert.True(t, ok, "the next collection should succeed")
	case <-time.After(5 * time.Second):
		t.Fatal("the next collection is stuck on the stats lock")
	}
}
