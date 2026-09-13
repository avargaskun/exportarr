package commands

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"

	arrconfig "github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/client"
	"github.com/onedr0p/exportarr/internal/config"
	"github.com/onedr0p/exportarr/internal/handlers"
	"github.com/onedr0p/exportarr/internal/targets"
)

func init() {
	rootCmd.AddCommand(serveCmd)
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Prometheus exporter for several named targets in one process",
	Long: `Prometheus exporter for several named targets in one process.
Targets are configured with TARGET_<n>_NAME, TARGET_<n>_APP, TARGET_<n>_URL and
TARGET_<n>_API_KEY or TARGET_<n>_API_KEY_FILE; each is served on /metrics/<name>.`,
	RunE: runServe,
}

func runServe(cmd *cobra.Command, _ []string) error {
	var errs []error
	arrDefaults, err := arrconfig.LoadArrConfig(*conf, cmd.Flags())
	if err != nil {
		errs = append(errs, err)
	} else {
		errs = append(errs, targets.CheckProcessConfig(conf, arrDefaults, cmd.Flags()))
	}
	// Load runs even after a process-level error so every target's secrets are unset.
	cfg, err := targets.Load(os.Environ())
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return err
	}

	ts, err := buildTargets(cfg, *conf, *arrDefaults, serveApps)
	if err != nil {
		return err
	}
	for _, t := range ts {
		slog.Info("Configured target", "target", t.name, "app", t.app, "url", redactTargetURL(t.url))
	}
	return serveHTTP(cmd.Context(), maxScrapeTimeout(ts), newServeHandler(ts, newSelfRegistry()))
}

func redactTargetURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return client.RedactURL(u)
}

// newServeHandler routes GET /metrics/<name> to each target's stack and answers
// every other path with a constant 404; self is the exporter's own /metrics.
func newServeHandler(ts []*target, self *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		mux.Handle("GET /metrics/"+t.name, t.handler)
		names = append(names, t.name)
	}
	mux.Handle("GET /metrics", promhttp.HandlerFor(self, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
		ErrorLog:      promhttpLogger{},
	}))
	mux.HandleFunc("GET /healthz", handlers.HealthzHandler)
	mux.Handle("GET /{$}", handlers.TargetIndexHandler(names))
	mux.HandleFunc("/", handlers.NotFoundHandler)
	return handlers.LogHandler(handlers.RecoveryHandler(mux))
}

// appBuilder resolves a target into its app's collectors and the URL they scrape.
type appBuilder func(t *targets.Target, process config.Config, arrDefaults arrconfig.ArrConfig) (url string, cs []prometheus.Collector, err error)

// serveApps maps every targets.AppNames entry to its builder.
var serveApps = map[string]appBuilder{
	"radarr":   arrBuilder(radarrApp),
	"sonarr":   arrBuilder(sonarrApp),
	"lidarr":   arrBuilder(lidarrApp),
	"prowlarr": arrBuilder(prowlarrApp),
	"bazarr":   arrBuilder(bazarrApp),
	"sabnzbd":  sabnzbdBuilder,
}

func arrBuilder(app arrCommand) appBuilder {
	return func(t *targets.Target, process config.Config, arrDefaults arrconfig.ArrConfig) (string, []prometheus.Collector, error) {
		c, err := t.ArrConfig(arrDefaults, process)
		if err != nil {
			return "", nil, err
		}
		cs, err := app.build(c)
		if err != nil {
			return "", nil, labelErr(t.Label(), err)
		}
		return c.URL, cs, nil
	}
}

func sabnzbdBuilder(t *targets.Target, process config.Config, _ arrconfig.ArrConfig) (string, []prometheus.Collector, error) {
	c, err := t.SabnzbdConfig(process)
	if err != nil {
		return "", nil, err
	}
	cs, err := buildSabnzbd(c)
	if err != nil {
		return "", nil, labelErr(t.Label(), err)
	}
	return c.URL, cs, nil
}

// labelErr prefixes err with label; each error of an errors.Join gets its own prefix.
func labelErr(label string, err error) error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		errs := joined.Unwrap()
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		if strings.Join(msgs, "\n") == err.Error() {
			labeled := make([]error, len(errs))
			for i, e := range errs {
				labeled[i] = labelErr(label, e)
			}
			return errors.Join(labeled...)
		}
	}
	return fmt.Errorf("%s: %w", label, err)
}

// target is one configured serve target: its own registry and /metrics stack.
type target struct {
	name, app, url string
	scrapeTimeout  time.Duration
	registry       *prometheus.Registry
	handler        http.Handler
}

// buildTargets builds every target, joining all errors; it returns no targets if any failed.
func buildTargets(cfg *targets.Config, process config.Config, arrDefaults arrconfig.ArrConfig, apps map[string]appBuilder) ([]*target, error) {
	var (
		out  []*target
		errs []error
	)
	for i := range cfg.Targets {
		t, err := buildTarget(&cfg.Targets[i], process, arrDefaults, apps)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, t)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

func buildTarget(t *targets.Target, process config.Config, arrDefaults arrconfig.ArrConfig, apps map[string]appBuilder) (*target, error) {
	build, ok := apps[t.App]
	if !ok {
		return nil, fmt.Errorf("%s: unsupported app", t.Label())
	}
	scrapeTimeout := process.ScrapeTimeout
	if t.ScrapeTimeout != nil {
		scrapeTimeout = *t.ScrapeTimeout
	}
	if scrapeTimeout <= 0 {
		return nil, fmt.Errorf("%s: SCRAPE_TIMEOUT must be greater than zero", t.Label())
	}
	url, cs, err := build(t, process, arrDefaults)
	if err != nil {
		return nil, err
	}
	registry := prometheus.NewRegistry()
	for _, c := range cs {
		if err := registry.Register(safeCollector{inner: c, target: t.Name}); err != nil {
			return nil, labelErr(t.Label(), err)
		}
	}
	return &target{
		name:          t.Name,
		app:           t.App,
		url:           url,
		scrapeTimeout: scrapeTimeout,
		registry:      registry,
		handler: newMetricsHandler(stackOpts{
			app:           t.App,
			url:           url,
			target:        t.Name,
			scrapeTimeout: scrapeTimeout,
		}, registry),
	}, nil
}

// safeCollector keeps a panic in one target's collector from crashing every target.
type safeCollector struct {
	inner  prometheus.Collector
	target string
}

// Describe implements prometheus.Collector.
func (s safeCollector) Describe(ch chan<- *prometheus.Desc) {
	s.inner.Describe(ch)
}

// Collect implements prometheus.Collector.
func (s safeCollector) Collect(ch chan<- prometheus.Metric) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("collector panicked", "target", s.target, "collector", fmt.Sprintf("%T", s.inner), "panic", r)
		}
	}()
	s.inner.Collect(ch)
}

func maxScrapeTimeout(ts []*target) time.Duration {
	var d time.Duration
	for _, t := range ts {
		d = max(d, t.scrapeTimeout)
	}
	return d
}
