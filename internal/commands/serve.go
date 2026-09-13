package commands

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	arrconfig "github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/config"
	"github.com/onedr0p/exportarr/internal/targets"
)

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
