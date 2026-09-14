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

func TestResolveBackfillSince(t *testing.T) {
	preset := time.Date(2020, 5, 6, 0, 0, 0, 0, time.UTC)
	params := []struct {
		name    string
		date    string
		want    time.Time
		wantErr string
	}{
		{name: "valid", date: "2021-01-01", want: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "empty resets", date: "", want: time.Time{}},
		{name: "invalid", date: "2021-31-31", want: preset, wantErr: "backfill-since-date must be in the format YYYY-MM-DD"},
		{name: "wrong layout", date: "01/02/2021", want: preset, wantErr: "backfill-since-date must be in the format YYYY-MM-DD"},
	}
	for _, p := range params {
		t.Run(p.name, func(t *testing.T) {
			c := ArrConfig{Prowlarr: ProwlarrConfig{BackfillSinceDate: p.date, BackfillSinceTime: preset}}
			err := c.ResolveBackfillSince()
			if p.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Equal(t, err.Error(), p.wantErr)
			}
			assert.True(t, c.Prowlarr.BackfillSinceTime.Equal(p.want), "got %v, want %v", c.Prowlarr.BackfillSinceTime, p.want)
			assert.Equal(t, c.Prowlarr.BackfillSinceDate, p.date)
		})
	}
}

func TestLoadProwlarrConfig_InvalidFlag(t *testing.T) {
	flags := pflag.FlagSet{}
	RegisterProwlarrFlags(&flags)
	_ = flags.Set("backfill-since-date", "2024-13-01")

	c := ArrConfig{Prowlarr: ProwlarrConfig{BackfillSinceDate: "2021-01-01"}}
	err := c.LoadProwlarrConfig(&flags)
	assert.Error(t, err)
	assert.Equal(t, err.Error(), "backfill-since-date must be in the format YYYY-MM-DD")
	assert.Equal(t, c.Prowlarr.BackfillSinceDate, "2024-13-01")
}

func TestLoadProwlarrConfig_NoDate(t *testing.T) {
	flags := pflag.FlagSet{}
	RegisterProwlarrFlags(&flags)

	c := ArrConfig{}
	assert.NoError(t, c.LoadProwlarrConfig(&flags))
	assert.False(t, c.Prowlarr.Backfill)
	assert.True(t, c.Prowlarr.BackfillSinceTime.IsZero())
}
