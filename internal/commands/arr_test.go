package commands

import (
	"github.com/onedr0p/exportarr/internal/assert"
	"testing"

	"github.com/onedr0p/exportarr/internal/arr/config"
	base_config "github.com/onedr0p/exportarr/internal/config"
	"github.com/onedr0p/exportarr/internal/fixtures"
	sabcollector "github.com/onedr0p/exportarr/internal/sabnzbd/collector"
	sabconfig "github.com/onedr0p/exportarr/internal/sabnzbd/config"
	"github.com/spf13/pflag"
)

func TestAuthFlagsRegistered(t *testing.T) {
	params := []struct {
		name  string
		flags *pflag.FlagSet
	}{
		{
			name:  "radarr",
			flags: radarrCmd.PersistentFlags(),
		},
		{
			name:  "sonarr",
			flags: sonarrCmd.PersistentFlags(),
		},
		{
			name:  "lidarr",
			flags: lidarrCmd.PersistentFlags(),
		},
		{
			name:  "prowlarr",
			flags: prowlarrCmd.PersistentFlags(),
		},
		{
			name:  "bazarr",
			flags: bazarrCmd.PersistentFlags(),
		},
	}
	for _, p := range params {
		t.Run(p.name, func(t *testing.T) {
			t.Cleanup(func() {
				for _, name := range []string{"auth-username", "auth-password"} {
					f := p.flags.Lookup(name)
					_ = f.Value.Set(f.DefValue)
					f.Changed = false
				}
			})
			_ = p.flags.Set("auth-username", "user")
			_ = p.flags.Set("auth-password", "pass")
			config, err := config.LoadArrConfig(base_config.Config{}, p.flags)
			assert.NoError(t, err)
			assert.Equal(t, config.AuthUsername, "user")
			assert.Equal(t, config.AuthPassword, "pass")
		})
	}

}

func validArrConfig() *config.ArrConfig {
	return &config.ArrConfig{
		URL:               "http://localhost:7878",
		APIKey:            fixtures.APIKey,
		SeriesConcurrency: config.DefaultSeriesConcurrency,
		Bazarr:            config.BazarrConfig{SeriesBatchSize: 300, SeriesBatchConcurrency: 10},
	}
}

func TestArrCommandBuild(t *testing.T) {
	params := []struct {
		name       string
		app        arrCommand
		apiVersion string
		want       int
		wantNoHist int
	}{
		{name: "radarr", app: radarrApp, apiVersion: "v3", want: 7, wantNoHist: 6},
		{name: "sonarr", app: sonarrApp, apiVersion: "v3", want: 7, wantNoHist: 6},
		{name: "lidarr", app: lidarrApp, apiVersion: "v1", want: 7, wantNoHist: 6},
		{name: "prowlarr", app: prowlarrApp, apiVersion: "v1", want: 4, wantNoHist: 3},
		{name: "bazarr", app: bazarrApp, apiVersion: "", want: 1, wantNoHist: 1},
	}
	for _, p := range params {
		t.Run(p.name, func(t *testing.T) {
			c := validArrConfig()
			c.APIVersion = "stale"
			cs, err := p.app.build(c)
			assert.NoError(t, err)
			assert.Len(t, cs, p.want)
			assert.Equal(t, c.APIVersion, p.apiVersion)
			for _, col := range cs {
				assert.NotNil(t, col)
			}

			c = validArrConfig()
			c.DisableHistoryMetrics = true
			cs, err = p.app.build(c)
			assert.NoError(t, err)
			assert.Len(t, cs, p.wantNoHist)
		})
	}
}

func TestArrCommandBuild_ValidateBeforeValidateExtra(t *testing.T) {
	c := validArrConfig()
	c.URL = "not-a-url"
	c.Prowlarr.BackfillSinceDate = "2024-13-01"
	cs, err := prowlarrApp.build(c)
	assert.Error(t, err)
	assert.Nil(t, cs)
	assert.Equal(t, err.Error(), "url must be an absolute URL (scheme://host[:port][/path])")
}

func TestArrCommandBuild_ValidateExtra(t *testing.T) {
	c := validArrConfig()
	c.Bazarr.SeriesBatchSize = 0
	cs, err := bazarrApp.build(c)
	assert.Error(t, err)
	assert.Nil(t, cs)
	assert.Equal(t, err.Error(), "series-batch-size must be greater than zero")

	c = validArrConfig()
	c.Prowlarr.BackfillSinceDate = "2024-13-01"
	_, err = prowlarrApp.build(c)
	assert.Error(t, err)
	assert.Equal(t, err.Error(), "backfill-since-date must be in the format YYYY-MM-DD")
}

func TestArrCommandBuild_ValidateErrorsJoined(t *testing.T) {
	c := validArrConfig()
	c.URL = ""
	c.APIKey = "short"
	_, err := radarrApp.build(c)
	assert.Error(t, err)
	assert.Equal(t, err.Error(), "url is required\napi-key must be a 20-32 character alphanumeric string")
}

func TestBuildSabnzbd(t *testing.T) {
	cs, err := buildSabnzbd(&sabconfig.SabnzbdConfig{URL: "not-a-url"})
	assert.Error(t, err)
	assert.Nil(t, cs)
	assert.Equal(t, err.Error(), "url must be an absolute URL (scheme://host[:port][/path])\napi-key is required")

	cs, err = buildSabnzbd(&sabconfig.SabnzbdConfig{URL: "http://localhost:8080", APIKey: fixtures.APIKey})
	assert.NoError(t, err)
	assert.Len(t, cs, 1)
	_, ok := cs[0].(*sabcollector.SabnzbdCollector)
	assert.True(t, ok, "got %T, want *collector.SabnzbdCollector", cs[0])
}
