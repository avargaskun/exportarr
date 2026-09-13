package targets

import (
	"errors"
	"fmt"

	"github.com/spf13/pflag"

	arrconfig "github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/config"
	sabconfig "github.com/onedr0p/exportarr/internal/sabnzbd/config"
)

var loadSabnzbdConfig = sabconfig.LoadSabnzbdConfig

// BaseConfig returns the process base config with this target's URL, key and
// connection/timeout overrides applied (App = t.App).
func (t *Target) BaseConfig(process config.Config) config.Config {
	c := process
	c.App = t.App
	c.URL = t.URL
	c.APIKey = t.APIKey
	c.APIKeyFromFile = ""
	if t.ScrapeTimeout != nil {
		c.ScrapeTimeout = *t.ScrapeTimeout
	}
	if t.RequestTimeout != nil {
		c.RequestTimeout = *t.RequestTimeout
	}
	if t.DisableSSLVerify != nil {
		c.DisableSSLVerify = *t.DisableSSLVerify
	}
	if t.ProxyFromEnv != nil {
		c.ProxyFromEnv = *t.ProxyFromEnv
	}
	return c
}

// ArrConfig resolves this target into an ordinary *ArrConfig: copy of the process
// defaults → ApplyBase(t.BaseConfig(process)) → non-nil overrides → FormAuth/AUTH_* →
// Target = t.Name → ResolveBackfillSince(). The caller sets APIVersion and runs Validate.
func (t *Target) ArrConfig(defaults arrconfig.ArrConfig, process config.Config) (*arrconfig.ArrConfig, error) {
	c := defaults
	c.ApplyBase(t.BaseConfig(process))
	override(&c.EnableUnknownQueueItems, t.EnableUnknownQueueItems)
	override(&c.DisableQualityMetrics, t.DisableQualityMetrics)
	override(&c.DisableEpisodeMetrics, t.DisableEpisodeMetrics)
	override(&c.DisableAlbumMetrics, t.DisableAlbumMetrics)
	override(&c.DisableHistoryMetrics, t.DisableHistoryMetrics)
	override(&c.DisableWantedMetrics, t.DisableWantedMetrics)
	override(&c.SeriesConcurrency, t.SeriesConcurrency)
	override(&c.Prowlarr.Backfill, t.Prowlarr.Backfill)
	override(&c.Prowlarr.BackfillSinceDate, t.Prowlarr.BackfillSinceDate)
	override(&c.Bazarr.SeriesBatchSize, t.Bazarr.SeriesBatchSize)
	override(&c.Bazarr.SeriesBatchConcurrency, t.Bazarr.SeriesBatchConcurrency)
	c.FormAuth = t.FormAuth
	c.AuthUsername = t.AuthUsername
	c.AuthPassword = t.AuthPassword
	c.Target = t.Name
	if err := c.ResolveBackfillSince(); err != nil {
		return nil, fmt.Errorf("%s: %w", t.Label(), err)
	}
	return &c, nil
}

// SabnzbdConfig resolves this target via the existing LoadSabnzbdConfig(t.BaseConfig(process)).
func (t *Target) SabnzbdConfig(process config.Config) (*sabconfig.SabnzbdConfig, error) {
	c, err := loadSabnzbdConfig(t.BaseConfig(process))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Label(), err)
	}
	c.Target = t.Name
	return c, nil
}

func override[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// CheckProcessConfig rejects single-target settings that are meaningless (and would be
// silently ignored) in serve mode.
func CheckProcessConfig(base *config.Config, arrDefaults *arrconfig.ArrConfig, flags *pflag.FlagSet) error {
	var errs []error
	reject := func(setting, perTarget string) {
		errs = append(errs, fmt.Errorf("%s is not supported by serve: URLs and credentials are set per target (%s)", setting, perTarget))
	}
	if base.URL != "" {
		setting := "URL"
		if flags.Changed("url") {
			setting = "--url"
		}
		reject(setting, "TARGET_<n>_URL")
	}
	if base.APIKey != "" || base.APIKeyFromFile != "" {
		setting := "API_KEY"
		switch {
		case base.APIKeyFromFile != "":
			setting = "API_KEY_FILE"
		case flags.Changed("api-key"):
			setting = "--api-key"
		}
		reject(setting, "TARGET_<n>_API_KEY or TARGET_<n>_API_KEY_FILE")
	}
	if arrDefaults.FormAuth {
		reject("FORM_AUTH", "TARGET_<n>_FORM_AUTH")
	}
	if arrDefaults.AuthUsername != "" {
		reject("AUTH_USERNAME", "TARGET_<n>_AUTH_USERNAME")
	}
	if arrDefaults.AuthPassword != "" {
		reject("AUTH_PASSWORD", "TARGET_<n>_AUTH_PASSWORD")
	}
	return errors.Join(errs...)
}
