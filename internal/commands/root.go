package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/sync/singleflight"

	"github.com/onedr0p/exportarr/internal/config"
	"github.com/onedr0p/exportarr/internal/handlers"
)

var (
	gracefulTimeout = 5 * time.Second

	listen                  = net.Listen
	logOutput     io.Writer = os.Stdout
	notifySignals           = signal.Notify

	conf    = &config.Config{}
	appInfo = &AppInfo{}
	rootCmd = &cobra.Command{
		Use:   "exportarr",
		Short: "exportarr is a AIO Prometheus exporter for *arr applications",
		Long: `exportarr is a Prometheus exporter for *arr applications.
It can export metrics from Radarr, Sonarr, Lidarr, Bazarr, Prowlarr and SABnzbd.
More information available at the Github Repo (https://github.com/onedr0p/exportarr)`,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		// Load + validate config and install the logger before any subcommand
		// runs, returning errors instead of exiting so cobra can report them
		// and defers still run. Help skips config entirely.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			switch cmd.Name() {
			case "help":
				return nil
			case cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
				// cobra adds these hidden commands even with completion disabled,
				// and they append to the file named by BASH_COMP_DEBUG_FILE.
				return errors.New("shell completion is not supported")
			}
			var err error
			conf, err = config.LoadConfig(cmd.Root().PersistentFlags())
			if err != nil {
				return err
			}
			conf.App = cmd.Name()
			if err := conf.Validate(); err != nil {
				return err
			}
			initLogger()
			warnSecretFlags(slog.Default(), cmd.Flags())
			return nil
		},
	}
)

// AppInfo carries build metadata stamped into the binary at release time.
type AppInfo struct {
	Name      string
	Version   string
	BuildTime string
	Revision  string
}

// Execute runs the exportarr root command.
func Execute(a AppInfo) error {
	appInfo = &a
	return rootCmd.Execute()
}

func init() {
	config.RegisterConfigFlags(rootCmd.PersistentFlags())
}

func initLogger() {
	// Install the handler at info first so config failures are loggable, then
	// lower/raise the level once the configured value parses.
	lvl := new(slog.LevelVar)

	var handler slog.Handler = slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: lvl})
	if conf.LogFormat == "json" {
		handler = slog.NewJSONHandler(logOutput, &slog.HandlerOptions{Level: lvl})
	}
	slog.SetDefault(slog.New(handler))

	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(conf.LogLevel)); err != nil {
		slog.Error("Invalid log level, using default level: info", "log-level", conf.LogLevel)
		parsed = slog.LevelInfo
	}
	lvl.Set(parsed)

	slog.Info(
		fmt.Sprintf("Starting %s", appInfo.Name),
		"app_name", appInfo.Name,
		"version", appInfo.Version,
		"buildTime", appInfo.BuildTime,
		"revision", appInfo.Revision,
	)
}

// secretFlags hold credentials; argv is readable by every local user.
var secretFlags = []string{"api-key", "auth-password"}

func warnSecretFlags(log *slog.Logger, flags *pflag.FlagSet) {
	for _, name := range secretFlags {
		if f := flags.Lookup(name); f != nil && f.Changed {
			log.Warn("Secret passed as a command-line flag is visible to every local user in the process list; use the environment or API_KEY_FILE instead",
				"flag", "--"+name)
		}
	}
}

// promhttpLogger routes promhttp's internal gather errors to slog.
type promhttpLogger struct {
	target string
}

// Println implements promhttp.Logger.
func (p promhttpLogger) Println(v ...any) {
	if p.target == "" {
		slog.Error(fmt.Sprintln(v...))
		return
	}
	slog.Error(fmt.Sprintln(v...), "target", p.target)
}

// newServer bounds every phase of a connection so a stalled or slow client
// cannot pin it open. Writes get the scrape budget plus a margin, so a scrape
// that times out can still deliver its 503.
var newServer = func(scrapeTimeout time.Duration) *http.Server {
	return &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      scrapeTimeout + 10*time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func serveHTTP(ctx context.Context, scrapeTimeout time.Duration, h http.Handler) error {
	sigc := make(chan os.Signal, 1)
	notifySignals(sigc, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigc)

	srv := newServer(scrapeTimeout)

	slog.Info("Starting HTTP Server",
		"interface", conf.Interface,
		"port", conf.Port)
	srv.Handler = h

	ln, err := listen("tcp", listenAddr(conf))
	if err != nil {
		return fmt.Errorf("failed to start HTTP server: %w", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return fmt.Errorf("failed to start HTTP server: %w", err)
	case sig := <-sigc:
		slog.Info("Shutting down due to signal", "signal", sig.String())
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("Server shutdown failed", "error", err)
		return err
	}
	return nil
}

// listenAddr joins the interface and port, bracketing IPv6 addresses.
func listenAddr(conf *config.Config) string {
	return net.JoinHostPort(conf.Interface, strconv.Itoa(conf.Port))
}

// sharedGatherer lets concurrent scrapes share one in-flight gather, so an
// overlapping request never stacks another authenticated walk onto the target.
type sharedGatherer struct {
	inner prometheus.Gatherer
	group singleflight.Group
}

// Gather implements prometheus.Gatherer.
func (g *sharedGatherer) Gather() ([]*dto.MetricFamily, error) {
	v, err, _ := g.group.Do("gather", func() (any, error) {
		return g.inner.Gather()
	})
	mfs, _ := v.([]*dto.MetricFamily)
	return mfs, err
}

// maxScrapesInFlight bounds concurrent /metrics requests; further requests
// get a 503 instead of stacking more authenticated walks onto the target.
const maxScrapesInFlight = 2

// stackOpts describes one scrape target's /metrics stack.
type stackOpts struct {
	app, url, target string
	scrapeTimeout    time.Duration
}

func newMetricsHandler(o stackOpts, registry *prometheus.Registry) http.Handler {
	// Serve partial metrics when a collector fails rather than failing the
	// whole scrape. Collectors report failures via *_collector_error gauges,
	// except system status, which reports <app>_system_status 0.
	h := promhttp.HandlerFor(&sharedGatherer{inner: registry}, promhttp.HandlerOpts{
		ErrorHandling:       promhttp.ContinueOnError,
		ErrorLog:            promhttpLogger{target: o.target},
		MaxRequestsInFlight: maxScrapesInFlight,
		Timeout:             o.scrapeTimeout,
		// Exposes promhttp_metric_handler_errors_total for gather errors.
		Registry: registry,
	})
	return handlers.MetricsHandler(o.app, o.url, registry, h)
}

func newHandler(conf *config.Config, registry *prometheus.Registry) http.Handler {
	// Scrape bookkeeping wraps only /metrics so health probes don't pollute it.
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", newMetricsHandler(stackOpts{
		app:           conf.App,
		url:           conf.URL,
		scrapeTimeout: conf.ScrapeTimeout,
	}, registry))
	mux.HandleFunc("GET /healthz", handlers.HealthzHandler)
	mux.HandleFunc("GET /", handlers.IndexHandler)

	return handlers.LogHandler(handlers.RecoveryHandler(mux))
}

// newSelfRegistry holds the exporter's own health: app info, go_* and process_*.
func newSelfRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registerAppInfoMetric(registry)
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return registry
}

// singleTargetHandler serves the self metrics and cs from one registry.
func singleTargetHandler(cs ...prometheus.Collector) http.Handler {
	registry := newSelfRegistry()
	registry.MustRegister(cs...)
	return newHandler(conf, registry)
}

func registerAppInfoMetric(registry prometheus.Registerer) {
	registry.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: appInfo.Name,
			Name:      "app_info",
			Help:      "A metric with a constant '1' value labeled by app name, version, build time, and revision.",
			ConstLabels: prometheus.Labels{
				"app_name":   appInfo.Name,
				"version":    appInfo.Version,
				"build_time": appInfo.BuildTime,
				"revision":   appInfo.Revision,
			},
		},
		func() float64 { return 1 },
	))
}
