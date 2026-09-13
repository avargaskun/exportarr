package config

import (
	"github.com/onedr0p/exportarr/internal/assert"
	"os"
	"reflect"
	"testing"
	"time"

	base_config "github.com/onedr0p/exportarr/internal/config"
	"github.com/spf13/pflag"
)

func testFlagSet() *pflag.FlagSet {
	ret := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterArrFlags(ret)
	return ret
}

func TestUseFormAuth(t *testing.T) {
	c := ArrConfig{
		AuthUsername: "user",
		AuthPassword: "pass",
	}
	assert.False(t, c.UseFormAuth())
	c.FormAuth = true
	assert.True(t, c.UseFormAuth())
}

func TestBaseURL(t *testing.T) {
	c := ArrConfig{
		URL:        "http://localhost:8080",
		APIVersion: "v1",
	}
	assert.Equal(t, c.BaseURL(), "http://localhost:8080/api/v1")
}

func TestLoadConfig_Defaults(t *testing.T) {
	flags := testFlagSet()
	c := base_config.Config{
		URL:              "http://localhost",
		APIKey:           "abcdef0123456789abcdef0123456789",
		DisableSSLVerify: true,
	}

	config, err := LoadArrConfig(c, flags)
	assert.NoError(t, err)

	assert.Equal(t, config.SeriesConcurrency, DefaultSeriesConcurrency)

	// base config values are not overwritten
	assert.Equal(t, config.URL, "http://localhost")
	assert.Equal(t, config.APIKey, "abcdef0123456789abcdef0123456789")
	assert.True(t, config.DisableSSLVerify)
}

func TestLoadConfig_SeriesConcurrency(t *testing.T) {
	t.Setenv("SERIES_CONCURRENCY", "4")
	config, err := LoadArrConfig(base_config.Config{}, testFlagSet())
	assert.NoError(t, err)
	assert.Equal(t, config.SeriesConcurrency, 4)

	flags := testFlagSet()
	_ = flags.Set("series-concurrency", "2")
	config, err = LoadArrConfig(base_config.Config{}, flags)
	assert.NoError(t, err)
	assert.Equal(t, config.SeriesConcurrency, 2)
}

func TestLoadConfig_ProxyFromBase(t *testing.T) {
	config, err := LoadArrConfig(base_config.Config{ProxyFromEnv: true}, testFlagSet())
	assert.NoError(t, err)
	assert.True(t, config.ProxyFromEnv)
}

func TestLoadConfig_CollectTimeoutFromBase(t *testing.T) {
	config, err := LoadArrConfig(base_config.Config{ScrapeTimeout: time.Minute}, testFlagSet())
	assert.NoError(t, err)
	assert.Equal(t, config.CollectTimeout, 55*time.Second)
}

func TestLoadConfig_Environment(t *testing.T) {
	flags := testFlagSet()
	c := base_config.Config{
		URL:              "http://localhost",
		APIKey:           "abcdef0123456789abcdef0123456789",
		DisableSSLVerify: true,
	}
	t.Setenv("AUTH_USERNAME", "user")
	t.Setenv("AUTH_PASSWORD", "pass")
	t.Setenv("FORM_AUTH", "true")
	t.Setenv("ENABLE_UNKNOWN_QUEUE_ITEMS", "true")
	t.Setenv("DISABLE_QUALITY_METRICS", "true")

	config, err := LoadArrConfig(c, flags)
	assert.NoError(t, err)

	assert.Equal(t, config.AuthUsername, "user")
	assert.Equal(t, config.AuthPassword, "pass")
	assert.True(t, config.FormAuth)
	assert.True(t, config.EnableUnknownQueueItems)
	assert.True(t, config.DisableQualityMetrics)

	// defaults are not overwritten
	assert.Equal(t, config.SeriesConcurrency, DefaultSeriesConcurrency)

	// base config values are not overwritten
	assert.Equal(t, config.URL, "http://localhost")
	assert.Equal(t, config.APIKey, "abcdef0123456789abcdef0123456789")
	assert.True(t, config.DisableSSLVerify)

}

func TestLoadConfig_IgnoresAPIVersionEnv(t *testing.T) {
	t.Setenv("API_VERSION", "v9")
	config, err := LoadArrConfig(base_config.Config{}, testFlagSet())
	assert.NoError(t, err)
	assert.Equal(t, config.APIVersion, "")
}

func TestLoadConfig_UnsetsFormAuthCredentials(t *testing.T) {
	t.Setenv("AUTH_USERNAME", "user")
	t.Setenv("AUTH_PASSWORD", "pass")

	config, err := LoadArrConfig(base_config.Config{}, testFlagSet())
	assert.NoError(t, err)
	assert.Equal(t, config.AuthUsername, "user")
	assert.Equal(t, config.AuthPassword, "pass")

	_, ok := os.LookupEnv("AUTH_USERNAME")
	assert.False(t, ok, "AUTH_USERNAME should be removed from the environment")
	_, ok = os.LookupEnv("AUTH_PASSWORD")
	assert.False(t, ok, "AUTH_PASSWORD should be removed from the environment")
}

func TestLoadConfig_PartialEnvironment(t *testing.T) {
	flags := testFlagSet()
	_ = flags.Set("auth-username", "user")
	_ = flags.Set("auth-password", "pass")

	t.Setenv("ENABLE_UNKNOWN_QUEUE_ITEMS", "true")
	t.Setenv("DISABLE_QUALITY_METRICS", "true")

	c := base_config.Config{
		URL:    "http://localhost",
		APIKey: "abcdef0123456789abcdef0123456789",
	}
	config, err := LoadArrConfig(c, flags)
	assert.NoError(t, err)

	assert.Equal(t, config.AuthUsername, "user")
	assert.Equal(t, config.AuthPassword, "pass")
	assert.True(t, config.EnableUnknownQueueItems)
	assert.True(t, config.DisableQualityMetrics)

	assert.Equal(t, config.URL, "http://localhost")
	assert.Equal(t, config.APIKey, "abcdef0123456789abcdef0123456789")

	assert.Equal(t, config.SeriesConcurrency, DefaultSeriesConcurrency)

}

func TestLoadConfig_Flags(t *testing.T) {
	flags := testFlagSet()
	_ = flags.Set("auth-username", "user")
	_ = flags.Set("auth-password", "pass")
	_ = flags.Set("form-auth", "true")
	_ = flags.Set("enable-unknown-queue-items", "true")
	_ = flags.Set("disable-episode-metrics", "true")
	c := base_config.Config{}

	// should be overridden by flags
	t.Setenv("AUTH_USERNAME", "user2")
	config, err := LoadArrConfig(c, flags)
	assert.NoError(t, err)
	assert.Equal(t, config.AuthUsername, "user")
	assert.Equal(t, config.AuthPassword, "pass")
	assert.True(t, config.FormAuth)
	assert.True(t, config.EnableUnknownQueueItems)
	assert.True(t, config.DisableEpisodeMetrics)

	// defaults fall through
	assert.Equal(t, config.SeriesConcurrency, DefaultSeriesConcurrency)
}

func TestValidate(t *testing.T) {
	params := []struct {
		name   string
		config *ArrConfig
		valid  bool
	}{
		{
			name: "creds-without-form-auth",
			config: &ArrConfig{
				URL:          "http://localhost",
				APIKey:       "abcdef0123456789abcdef0123456789",
				APIVersion:   "v3",
				AuthUsername: "user",
				AuthPassword: "pass",
			},
			valid: false,
		},
		{
			name: "good-form-auth",
			config: &ArrConfig{
				URL:               "http://localhost",
				APIKey:            "abcdef0123456789abcdef0123456789",
				APIVersion:        "v3",
				AuthUsername:      "user",
				AuthPassword:      "pass",
				FormAuth:          true,
				SeriesConcurrency: 10,
			},
			valid: true,
		},
		{
			name: "good-api-key-32-len",
			config: &ArrConfig{
				URL:               "http://localhost",
				APIKey:            "abcdefABCDEF0123456789abcdef0123",
				APIVersion:        "v3",
				SeriesConcurrency: 10,
			},
			valid: true,
		},
		{
			name: "good-api-key-32-len",
			config: &ArrConfig{
				URL:               "http://localhost",
				APIKey:            "abcdefABCDEF01234567",
				APIVersion:        "v3",
				SeriesConcurrency: 10,
			},
			valid: true,
		},
		{
			name: "bad-api-key",
			config: &ArrConfig{
				URL:        "http://localhost",
				APIKey:     "abcdef0123456789abc",
				APIVersion: "v3",
			},
			valid: false,
		},
		{
			name: "no-api-version",
			config: &ArrConfig{
				URL:               "http://localhost",
				APIKey:            "abcdef0123456789abcdef0123456789",
				APIVersion:        "",
				SeriesConcurrency: 10,
			},
			valid: true,
		},
		{
			name: "password-needs-username",
			config: &ArrConfig{
				URL:          "http://localhost",
				APIKey:       "abcdef0123456789abcdef0123456789",
				APIVersion:   "v3",
				AuthPassword: "password",
			},
			valid: false,
		},
		{
			name: "username-needs-password",
			config: &ArrConfig{
				URL:          "http://localhost",
				APIKey:       "abcdef0123456789abcdef0123456789",
				APIVersion:   "v3",
				AuthUsername: "username",
			},
			valid: false,
		},
		{
			name: "series-concurrency-too-low",
			config: &ArrConfig{
				URL:               "http://localhost",
				APIKey:            "abcdef0123456789abcdef0123456789",
				SeriesConcurrency: 0,
			},
			valid: false,
		},
		{
			name: "series-concurrency-too-high",
			config: &ArrConfig{
				URL:               "http://localhost",
				APIKey:            "abcdef0123456789abcdef0123456789",
				SeriesConcurrency: MaxSeriesConcurrency + 1,
			},
			valid: false,
		},
		{
			name: "series-concurrency-max",
			config: &ArrConfig{
				URL:               "http://localhost",
				APIKey:            "abcdef0123456789abcdef0123456789",
				SeriesConcurrency: MaxSeriesConcurrency,
			},
			valid: true,
		},
		{
			name: "url-with-credentials",
			config: &ArrConfig{ //nolint:gosec // rejected-credentials fixture
				URL:               "http://user:pass@localhost",
				APIKey:            "abcdef0123456789abcdef0123456789",
				SeriesConcurrency: 10,
			},
			valid: false,
		},
		{
			name: "url-with-query",
			config: &ArrConfig{
				URL:               "http://localhost/?apikey=abcdef0123456789abcdef0123456789",
				APIKey:            "abcdef0123456789abcdef0123456789",
				SeriesConcurrency: 10,
			},
			valid: false,
		},
		{
			name: "form-auth-needs-user-and-password",
			config: &ArrConfig{
				URL:        "http://localhost",
				APIKey:     "abcdef0123456789abcdef0123456789",
				APIVersion: "v3",
				FormAuth:   true,
			},
			valid: false,
		},
	}
	for _, p := range params {
		t.Run(p.name, func(t *testing.T) {
			err := p.config.Validate()
			if p.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestApplyBase(t *testing.T) {
	copied := []string{"App", "URL", "APIKey", "DisableSSLVerify", "ProxyFromEnv", "RequestTimeout", "CollectTimeout"}
	filled := ArrConfig{
		App:                     "radarr",
		APIVersion:              "v3",
		AuthUsername:            "user",
		AuthPassword:            "pass",
		FormAuth:                true,
		EnableUnknownQueueItems: true,
		DisableQualityMetrics:   true,
		DisableEpisodeMetrics:   true,
		DisableAlbumMetrics:     true,
		DisableHistoryMetrics:   true,
		DisableWantedMetrics:    true,
		SeriesConcurrency:       4,
		URL:                     "http://radarr:7878",
		APIKey:                  "0123456789abcdef0123456789abcdef",
		DisableSSLVerify:        true,
		ProxyFromEnv:            true,
		RequestTimeout:          3 * time.Second,
		CollectTimeout:          4 * time.Second,
		Prowlarr: ProwlarrConfig{
			Backfill:          true,
			BackfillSinceDate: "2021-01-01",
			BackfillSinceTime: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		Bazarr: BazarrConfig{SeriesBatchSize: 7, SeriesBatchConcurrency: 8},
	}
	empty := filled
	empty.DisableSSLVerify = false
	empty.ProxyFromEnv = false

	params := []struct {
		name   string
		before ArrConfig
		base   base_config.Config
		want   map[string]any
	}{
		{
			name:   "sets every base field",
			before: empty,
			base: base_config.Config{
				App:              "sonarr",
				LogLevel:         "debug",
				URL:              "http://sonarr:8989",
				APIKey:           "abcdef0123456789abcdef0123456789",
				Port:             1234,
				DisableSSLVerify: true,
				ProxyFromEnv:     true,
				RequestTimeout:   7 * time.Second,
				ScrapeTimeout:    time.Minute,
			},
			want: map[string]any{
				"App":              "sonarr",
				"URL":              "http://sonarr:8989",
				"APIKey":           "abcdef0123456789abcdef0123456789",
				"DisableSSLVerify": true,
				"ProxyFromEnv":     true,
				"RequestTimeout":   7 * time.Second,
				"CollectTimeout":   55 * time.Second,
			},
		},
		{
			name:   "zero base clears every base field",
			before: filled,
			base:   base_config.Config{},
			want: map[string]any{
				"App":              "",
				"URL":              "",
				"APIKey":           "",
				"DisableSSLVerify": false,
				"ProxyFromEnv":     false,
				"RequestTimeout":   time.Duration(0),
				"CollectTimeout":   time.Duration(0),
			},
		},
	}
	for _, p := range params {
		t.Run(p.name, func(t *testing.T) {
			assert.Equal(t, len(p.want), len(copied))
			got := p.before
			got.ApplyBase(p.base)
			gv, bv := reflect.ValueOf(got), reflect.ValueOf(p.before)
			for i := range gv.NumField() {
				name := gv.Type().Field(i).Name
				if want, ok := p.want[name]; ok {
					assert.DeepEqual(t, gv.Field(i).Interface(), want, name)
					continue
				}
				assert.False(t, bv.Field(i).IsZero(), "fixture must set %s so a clobber is visible", name)
				assert.DeepEqual(t, gv.Field(i).Interface(), bv.Field(i).Interface(), name)
			}
		})
	}
}

func TestLoadConfig_SeedsFromApplyBase(t *testing.T) {
	base := base_config.Config{
		App:              "lidarr",
		URL:              "http://lidarr:8686",
		APIKey:           "abcdef0123456789abcdef0123456789",
		DisableSSLVerify: true,
		ProxyFromEnv:     true,
		RequestTimeout:   9 * time.Second,
		ScrapeTimeout:    30 * time.Second,
	}
	got, err := LoadArrConfig(base, testFlagSet())
	assert.NoError(t, err)
	want := ArrConfig{SeriesConcurrency: DefaultSeriesConcurrency, Bazarr: BazarrConfig{SeriesBatchSize: 300, SeriesBatchConcurrency: 10}}
	want.ApplyBase(base)
	assert.DeepEqual(t, *got, want)
}
