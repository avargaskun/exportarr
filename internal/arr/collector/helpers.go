package collector

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
)

// goRecoverable runs fn on the group, converting a worker panic into an
// error surfaced through Wait — errgroup itself does not recover worker
// panics, and a worker goroutine's panic would otherwise crash the process
// past every collector-level recover.
func goRecoverable(eg *errgroup.Group, fn func() error) {
	eg.Go(func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("worker panicked: %v", r)
			}
		}()
		return fn()
	})
}

// emitError logs a collection failure and emits the collector's error gauge —
// the shared failure path of every collector. args follow the slog key-value
// convention.
func emitError(log *slog.Logger, ch chan<- prometheus.Metric, errorMetric *prometheus.Desc, msg string, args ...any) {
	log.Error(msg, args...)
	ch <- prometheus.MustNewConstMetric(errorMetric, prometheus.GaugeValue, 1)
}

// recoverCollect converts a collector panic into an error-gauge emission and a
// log line, so one unexpected payload can never crash the whole exporter —
// prometheus gathers collectors in goroutines that HTTP middleware cannot
// recover for us.
func recoverCollect(log *slog.Logger, ch chan<- prometheus.Metric, errorMetric *prometheus.Desc) {
	if r := recover(); r != nil {
		log.Error("collector panicked", "panic", r)
		ch <- prometheus.MustNewConstMetric(errorMetric, prometheus.GaugeValue, 1)
	}
}

// seriesConcurrency is the per-item API fan-out used by the sonarr and lidarr
// collectors, falling back to the default for configs not built by
// LoadArrConfig.
func seriesConcurrency(conf *config.ArrConfig) int {
	if conf.SeriesConcurrency < 1 {
		return config.DefaultSeriesConcurrency
	}
	return conf.SeriesConcurrency
}

// collectContext bounds one collection's upstream requests by the configured
// collect timeout, if any.
func collectContext(conf *config.ArrConfig) (context.Context, context.CancelFunc) {
	if conf.CollectTimeout <= 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), conf.CollectTimeout)
}

// newDesc builds a Desc namespaced to the app, with the instance URL attached
// as a constant label — the shape shared by every *arr metric.
func newDesc(app, name, help string, variableLabels []string, url string) *prometheus.Desc {
	return prometheus.NewDesc(
		prometheus.BuildFQName(app, "", name),
		help,
		variableLabels,
		prometheus.Labels{"url": url},
	)
}
