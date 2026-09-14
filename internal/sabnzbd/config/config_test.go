package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/onedr0p/exportarr/internal/assert"
	base_config "github.com/onedr0p/exportarr/internal/config"
)

func TestLoadSabnzbdConfig_CopiesBaseFields(t *testing.T) {
	base := base_config.Config{
		App:              "sabnzbd",
		LogLevel:         "debug",
		URL:              "http://sabnzbd:8080",
		APIKey:           "abcdef0123456789abcdef0123456789",
		APIKeyFromFile:   "ignored",
		Port:             1234,
		DisableSSLVerify: true,
		ProxyFromEnv:     true,
		RequestTimeout:   7 * time.Second,
		ScrapeTimeout:    30 * time.Second,
	}
	got, err := LoadSabnzbdConfig(base)
	assert.NoError(t, err)
	assert.DeepEqual(t, *got, SabnzbdConfig{
		URL:              "http://sabnzbd:8080",
		APIKey:           "abcdef0123456789abcdef0123456789",
		DisableSSLVerify: true,
		ProxyFromEnv:     true,
		RequestTimeout:   7 * time.Second,
		CollectTimeout:   base.CollectTimeout(),
	})
	assert.Equal(t, got.CollectTimeout, 25*time.Second)

	v := reflect.ValueOf(*got)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		switch name {
		case "Target":
			assert.Equal(t, got.Target, "", "the target name is set by the caller")
			continue
		case "UpstreamLimiter":
			assert.Nil(t, got.UpstreamLimiter, "the limiter is set by the caller")
			continue
		}
		assert.False(t, v.Field(i).IsZero(), "field %s not copied from the base config", name)
	}
}

func TestLoadSabnzbdConfig_ShortScrapeTimeout(t *testing.T) {
	got, err := LoadSabnzbdConfig(base_config.Config{ScrapeTimeout: 6 * time.Second})
	assert.NoError(t, err)
	assert.Equal(t, got.CollectTimeout, 3*time.Second)
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		conf SabnzbdConfig
		want string
	}{
		{
			name: "valid",
			conf: SabnzbdConfig{URL: "http://localhost:8080", APIKey: "key"},
		},
		{
			name: "missing url",
			conf: SabnzbdConfig{APIKey: "key"},
			want: "url is required",
		},
		{
			name: "invalid url",
			conf: SabnzbdConfig{URL: "not-a-url", APIKey: "key"},
			want: "url must be an absolute URL (scheme://host[:port][/path])",
		},
		{
			name: "missing api key",
			conf: SabnzbdConfig{URL: "http://localhost:8080"},
			want: "api-key is required",
		},
		{
			name: "missing url and api key are joined",
			conf: SabnzbdConfig{},
			want: "url is required\napi-key is required",
		},
		{
			name: "invalid url and missing api key are joined",
			conf: SabnzbdConfig{URL: "not-a-url"},
			want: "url must be an absolute URL (scheme://host[:port][/path])\napi-key is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.conf.Validate()
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			assert.Error(t, err)
			assert.Equal(t, err.Error(), tc.want)
		})
	}
}

func TestValidate_RejectsSecretBearingURLs(t *testing.T) {
	for _, u := range []string{
		"http://user:hunter2@localhost:8080",
		"http://localhost:8080/?apikey=hunter2",
	} {
		err := (&SabnzbdConfig{URL: u, APIKey: "key"}).Validate()
		assert.Error(t, err)
		assert.NotContains(t, err.Error(), "hunter2")
	}
	assert.NoError(t, (&SabnzbdConfig{URL: "http://localhost:8080", APIKey: "key"}).Validate())
}
