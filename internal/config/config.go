// Package config loads and validates exportarr's base configuration from
// environment variables and flags.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	flag "github.com/spf13/pflag"
)

// RegisterConfigFlags registers the base exportarr flags on the given FlagSet.
func RegisterConfigFlags(flags *flag.FlagSet) {
	flags.StringP("log-level", "l", "info", "Log level (debug, info, warn, error)")
	flags.String("log-format", "console", "Log format (console, json)")
	flags.StringP("url", "u", "", "URL to *arr instance")
	flags.StringP("api-key", "a", "", "API Key for *arr instance")
	flags.Bool("disable-ssl-verify", false, "Disable SSL verification")
	flags.Bool("proxy-from-env", false, "Send requests to the target app through HTTP_PROXY/HTTPS_PROXY (off by default: the proxy sees the credentials)")
	flags.StringP("interface", "i", "", "IP address to listen on")
	flags.IntP("port", "p", 0, "Port to listen on")
	flags.Duration("request-timeout", 0, "HTTP timeout per request to the target app")
	flags.Duration("scrape-timeout", 0, "Upper bound on one /metrics scrape; keep it at or below Prometheus's scrape_timeout")
}

// Config is the base configuration shared by every exportarr subcommand.
type Config struct {
	App       string `env:"-"`
	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	LogFormat string `env:"LOG_FORMAT" envDefault:"console"`
	URL       string `env:"URL"`
	// Secret-bearing variables carry the `unset` option: the env library
	// removes them from the process environment after parsing, so they are
	// not visible in /proc/<pid>/environ or inherited by child processes.
	APIKey string `env:"API_KEY,unset"`
	// APIKeyFromFile receives the *contents* of the file named by API_KEY_FILE
	// (the env library's `file` option) — Docker/Kubernetes secrets mounts.
	APIKeyFromFile   string        `env:"API_KEY_FILE,file,unset"`
	Port             int           `env:"PORT" envDefault:"9707"`
	Interface        string        `env:"INTERFACE" envDefault:"0.0.0.0"`
	DisableSSLVerify bool          `env:"DISABLE_SSL_VERIFY"`
	ProxyFromEnv     bool          `env:"PROXY_FROM_ENV"`
	RequestTimeout   time.Duration `env:"REQUEST_TIMEOUT" envDefault:"60s"`
	ScrapeTimeout    time.Duration `env:"SCRAPE_TIMEOUT" envDefault:"2m"`
}

// collectGrace is the part of the scrape budget reserved for serving
// whatever the collectors gathered before their deadline.
const collectGrace = 5 * time.Second

// CollectTimeout is the deadline for a collector's upstream requests: the
// scrape budget minus a grace period, so a slow collector still yields a
// partial response instead of the scrape timing out as a whole.
func (c *Config) CollectTimeout() time.Duration {
	return max(c.ScrapeTimeout-collectGrace, c.ScrapeTimeout/2)
}

// OverlayFlag copies the value of an explicitly-set flag into dst, so flags
// win over environment-derived configuration. The getter is one of the typed
// FlagSet accessors (GetString, GetInt, GetBool, ...).
func OverlayFlag[T any](flags *flag.FlagSet, name string, get func(string) (T, error), dst *T) {
	if !flags.Changed(name) {
		return
	}
	if v, err := get(name); err == nil {
		*dst = v
	}
}

// LoadConfig parses environment variables into a Config, then overlays any
// explicitly-set flags (flags win over environment).
func LoadConfig(flags *flag.FlagSet) (*Config, error) {
	out := &Config{}
	if err := env.Parse(out); err != nil {
		return nil, err
	}

	OverlayFlag(flags, "log-level", flags.GetString, &out.LogLevel)
	OverlayFlag(flags, "log-format", flags.GetString, &out.LogFormat)
	OverlayFlag(flags, "url", flags.GetString, &out.URL)
	OverlayFlag(flags, "api-key", flags.GetString, &out.APIKey)
	OverlayFlag(flags, "interface", flags.GetString, &out.Interface)
	OverlayFlag(flags, "port", flags.GetInt, &out.Port)
	OverlayFlag(flags, "disable-ssl-verify", flags.GetBool, &out.DisableSSLVerify)
	OverlayFlag(flags, "proxy-from-env", flags.GetBool, &out.ProxyFromEnv)
	OverlayFlag(flags, "request-timeout", flags.GetDuration, &out.RequestTimeout)
	OverlayFlag(flags, "scrape-timeout", flags.GetDuration, &out.ScrapeTimeout)

	// A mounted secret wins over any inline API_KEY. Secrets commonly end
	// with a newline, which the env library preserves: trim it.
	if out.APIKeyFromFile != "" {
		out.APIKey = strings.TrimSpace(out.APIKeyFromFile)
	}
	return out, nil
}

// ValidateURL checks a target app URL. Credentials and query strings are
// rejected because the URL is the url label on every series and is logged;
// errors never echo the URL for the same reason.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Scheme == "" || u.Host == "":
		return errors.New("url must be an absolute URL (scheme://host[:port][/path])")
	case u.User != nil:
		return errors.New("url must not contain credentials; use API_KEY_FILE/API_KEY or form auth")
	case u.RawQuery != "" || u.ForceQuery:
		return errors.New("url must not contain a query string")
	}
	return nil
}

// Validate checks the configuration against its validation rules.
func (c *Config) Validate() error {
	var errs []error
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.LogLevel)); err != nil {
		errs = append(errs, errors.New("log-level must be one of: debug, info, warn, error"))
	}
	if c.LogFormat != "console" && c.LogFormat != "json" {
		errs = append(errs, errors.New("log-format must be one of: console, json"))
	}
	if c.Port == 0 {
		errs = append(errs, errors.New("port is required"))
	}
	if c.ScrapeTimeout <= 0 {
		errs = append(errs, errors.New("scrape-timeout must be greater than zero"))
	}
	if net.ParseIP(c.Interface) == nil {
		errs = append(errs, fmt.Errorf("interface must be a valid IP address: %q", c.Interface))
	}
	return errors.Join(errs...)
}
