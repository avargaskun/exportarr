package commands

import (
	"context"
	"errors"
	"fmt"
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

const gracefulTimeout = 5 * time.Second

var (
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

	var handler slog.Handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	if conf.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
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
type promhttpLogger struct{}

// Println implements promhttp.Logger.
func (promhttpLogger) Println(v ...any) {
	slog.Error(fmt.Sprintln(v...))
}

type registerFunc func(registry prometheus.Registerer)

// newServer bounds every phase of a connection so a stalled or slow client
// cannot pin it open. Writes get the scrape budget plus a margin, so a scrape
// that times out can still deliver its 503.
func newServer(conf *config.Config) *http.Server {
	return &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      conf.ScrapeTimeout + 10*time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func serveHTTP(fn registerFunc) error {
	srv := newServer(conf)

	idleConnsClosed := make(chan struct{})
	go func() {
		sigchan := make(chan os.Signal, 1)
		signal.Notify(sigchan, os.Interrupt)
		signal.Notify(sigchan, syscall.SIGTERM)
		sig := <-sigchan
		slog.Info("Shutting down due to signal", "signal", sig.String())

		ctx, cancel := context.WithTimeout(context.Background(), gracefulTimeout)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("Server shutdown failed", "error", err)
			os.Exit(1)
		}
		close(idleConnsClosed)
	}()

	registry := prometheus.NewRegistry()
	registerAppInfoMetric(registry)
	// The exporter's own runtime health: go_* and process_* metrics make its
	// CPU, memory, and GC behavior visible to the operator.
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	fn(registry)

	slog.Info("Starting HTTP Server",
		"interface", conf.Interface,
		"port", conf.Port)
	srv.Addr = listenAddr(conf)
	srv.Handler = newHandler(conf, registry)

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("failed to start HTTP server: %w", err)
	}
	<-idleConnsClosed
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

func newHandler(conf *config.Config, registry *prometheus.Registry) http.Handler {
	// Serve partial metrics when a collector fails rather than failing the
	// whole scrape. Collectors report failures via *_collector_error gauges,
	// except system status, which reports <app>_system_status 0. Scrape
	// bookkeeping wraps only /metrics so health probes don't pollute it.
	metricsHandler := promhttp.HandlerFor(&sharedGatherer{inner: registry}, promhttp.HandlerOpts{
		ErrorHandling:       promhttp.ContinueOnError,
		ErrorLog:            promhttpLogger{},
		MaxRequestsInFlight: maxScrapesInFlight,
		Timeout:             conf.ScrapeTimeout,
		// Exposes promhttp_metric_handler_errors_total for gather errors.
		Registry: registry,
	})
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", handlers.MetricsHandler(conf, registry, metricsHandler))
	mux.HandleFunc("GET /healthz", handlers.HealthzHandler)
	mux.HandleFunc("GET /", handlers.IndexHandler)

	return handlers.LogHandler(handlers.RecoveryHandler(mux))
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
