package config

import (
	"github.com/onedr0p/exportarr/internal/assert"
	"testing"
	"time"

	base_config "github.com/onedr0p/exportarr/internal/config"
	"github.com/spf13/pflag"
)

func TestLoadProwlarrConfig(t *testing.T) {
	flags := pflag.FlagSet{}
	RegisterProwlarrFlags(&flags)

	_ = flags.Set("backfill", "true")
	_ = flags.Set("backfill-since-date", "2021-01-01")
	c := ArrConfig{
		URL:              "http://localhost",
		APIKey:           "abcdef0123456789abcdef0123456789",
		DisableSSLVerify: true,
	}
	_ = c.LoadProwlarrConfig(&flags)
	assert.True(t, c.Prowlarr.Backfill)
	assert.Equal(t, c.Prowlarr.BackfillSinceDate, "2021-01-01")
	assert.Equal(t, c.Prowlarr.BackfillSinceTime.Format("2006-01-02"), "2021-01-01")
	assert.Equal(t, c.URL, "http://localhost")
	assert.Equal(t, c.APIKey, "abcdef0123456789abcdef0123456789")
	assert.True(t, c.DisableSSLVerify)
}

func TestValidateProwlarr(t *testing.T) {
	tm, _ := time.Parse("2006-01-02", "2021-01-01")
	parameters := []struct {
		name        string
		config      *ProwlarrConfig
		shouldError bool
	}{
		{
			name: "good",
			config: &ProwlarrConfig{
				Backfill:          true,
				BackfillSinceTime: tm,
				BackfillSinceDate: "2021-01-01",
			},
		},
		{
			name: "bad-date",
			config: &ProwlarrConfig{
				Backfill:          true,
				BackfillSinceTime: tm,
				BackfillSinceDate: "2021-31-31",
			},
			shouldError: true,
		},
	}

	for _, parameter := range parameters {
		t.Run(parameter.name, func(t *testing.T) {
			err := parameter.config.Validate()
			if parameter.shouldError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestLoadArrConfig_BarePrefixDoesNotSetBackfillTime(t *testing.T) {
	flags := testFlagSet()
	RegisterProwlarrFlags(flags)

	t.Setenv("PROWLARR__", "2020-01-01T00:00:00Z")
	c, err := LoadArrConfig(base_config.Config{}, flags)
	assert.NoError(t, err)
	assert.True(t, c.Prowlarr.BackfillSinceTime.IsZero(), "PROWLARR__ must not set the backfill time")

	t.Setenv("PROWLARR__", "not-a-time")
	_, err = LoadArrConfig(base_config.Config{}, flags)
	assert.NoError(t, err, "a malformed PROWLARR__ must not abort every subcommand")
}
