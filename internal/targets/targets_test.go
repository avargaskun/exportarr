package targets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/onedr0p/exportarr/internal/assert"
)

const appList = "radarr, sonarr, lidarr, prowlarr, bazarr, sabnzbd"

// block returns a TARGET_<i>_* block; extra entries are KEY=VALUE without the prefix.
func block(i int, name, app, rawURL string, extra ...string) []string {
	prefix := fmt.Sprintf("TARGET_%d_", i)
	out := []string{prefix + "NAME=" + name, prefix + "APP=" + app, prefix + "URL=" + rawURL}
	for _, kv := range extra {
		out = append(out, prefix+kv)
	}
	return out
}

func environ(blocks ...[]string) []string {
	return slices.Concat(blocks...)
}

func messages(err error) []string {
	if err == nil {
		return nil
	}
	return strings.Split(err.Error(), "\n")
}

func writeKeyFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	assert.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

// clearTargetEnv removes serve settings inherited from the real environment.
func clearTargetEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "TARGET_") || name == "MAX_UPSTREAM_REQUESTS" {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
}

func assertUnset(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		_, ok := os.LookupEnv(name)
		assert.False(t, ok, "%s still set", name)
	}
}

func TestEnvLibrary_FieldParamsIncludeNestedPrefixes(t *testing.T) {
	params, err := env.GetFieldParams(&Target{})
	assert.NoError(t, err)
	var keys []string
	for _, p := range params {
		keys = append(keys, p.Key)
	}
	for _, want := range []string{"NAME", "API_KEY_FILE", "PROWLARR__BACKFILL", "PROWLARR__BACKFILL_SINCE_DATE", "BAZARR__SERIES_BATCH_SIZE"} {
		assert.True(t, slices.Contains(keys, want), "missing %s in %v", want, keys)
	}
	assert.False(t, slices.Contains(keys, "-"), "ignored Index field listed")
	assert.False(t, slices.Contains(keys, "BACKFILL"), "nested key without its prefix")
}

func TestEnvLibrary_PrefixReadsIndexedKey(t *testing.T) {
	var tgt Target
	err := env.ParseWithOptions(&tgt, env.Options{
		Environment: map[string]string{"TARGET_3_NAME": "sonarr-hd", "TARGET_0_NAME": "other", "NAME": "bare"},
		Prefix:      "TARGET_3_",
	})
	assert.NoError(t, err)
	assert.Equal(t, tgt.Name, "sonarr-hd")
}

func TestEnvLibrary_UnsetsPrefixedKey(t *testing.T) {
	clearTargetEnv(t)
	t.Setenv("TARGET_3_API_KEY", "inline-secret")
	var tgt Target
	assert.NoError(t, env.ParseWithOptions(&tgt, env.Options{Prefix: "TARGET_3_"}))
	assert.Equal(t, tgt.APIKey, "inline-secret")
	assert.Equal(t, os.Getenv("TARGET_3_API_KEY"), "")
	assertUnset(t, "TARGET_3_API_KEY")
}

func TestEnvLibrary_ParseErrorNamesGoField(t *testing.T) {
	var tgt Target
	err := env.ParseWithOptions(&tgt, env.Options{
		Environment: map[string]string{"TARGET_0_SERIES_CONCURRENCY": "abc"},
		Prefix:      "TARGET_0_",
	})
	var parseErr env.ParseError
	assert.True(t, errors.As(err, &parseErr), "got %T", err)
	assert.Equal(t, parseErr.Name, "SeriesConcurrency")
	assert.Contains(t, err.Error(), "abc", "the library echoes the value")
	assert.NotNil(t, tgt.SeriesConcurrency, "the library leaves the pointer allocated")
}

func TestLoad_HappyPath(t *testing.T) {
	keyFile := writeKeyFile(t, "fileKey0123456789abcdef\n")
	cfg, err := Load(environ(
		block(0, "sonarr-hd", "sonarr", "http://sonarr-hd:8989", "API_KEY=key0", "SERIES_CONCURRENCY=4", "DISABLE_EPISODE_METRICS=true"),
		block(1, "sonarr-4k", "sonarr", "http://sonarr-4k:8989", "API_KEY_FILE="+keyFile),
		block(2, "radarr", "radarr", "http://radarr:7878", "FORM_AUTH=true", "AUTH_USERNAME=admin", "AUTH_PASSWORD=pw", "SCRAPE_TIMEOUT=30s"),
		block(3, "prowlarr", "prowlarr", "http://prowlarr:9696", "PROWLARR__BACKFILL=true", "PROWLARR__BACKFILL_SINCE_DATE=2024-01-02"),
		block(4, "bazarr", "bazarr", "http://bazarr:6767", "BAZARR__SERIES_BATCH_SIZE=50", "BAZARR__SERIES_BATCH_CONCURRENCY=2"),
		block(5, "sabnzbd", "sabnzbd", "http://sabnzbd:8080", "REQUEST_TIMEOUT=10s", "DISABLE_SSL_VERIFY=true", "PROXY_FROM_ENV=false"),
		[]string{"MAX_UPSTREAM_REQUESTS=16", "URL=http://ignored", "TARGETS_FOO=bar"},
	))
	assert.NoError(t, err)
	assert.Equal(t, cfg.MaxUpstreamRequests, 16)
	assert.Len(t, cfg.Targets, 6)
	for i, tgt := range cfg.Targets {
		assert.Equal(t, tgt.Index, i)
	}
	assert.DeepEqual(t, cfg.Targets[0], Target{
		Index: 0, Name: "sonarr-hd", App: "sonarr", URL: "http://sonarr-hd:8989", APIKey: "key0",
		SeriesConcurrency: new(4), DisableEpisodeMetrics: new(true),
	})
	assert.DeepEqual(t, cfg.Targets[1], Target{
		Index: 1, Name: "sonarr-4k", App: "sonarr", URL: "http://sonarr-4k:8989", APIKey: "fileKey0123456789abcdef",
	})
	assert.DeepEqual(t, cfg.Targets[2], Target{
		Index: 2, Name: "radarr", App: "radarr", URL: "http://radarr:7878",
		FormAuth: true, AuthUsername: "admin", AuthPassword: "pw", ScrapeTimeout: new(30 * time.Second),
	})
	assert.DeepEqual(t, cfg.Targets[3], Target{
		Index: 3, Name: "prowlarr", App: "prowlarr", URL: "http://prowlarr:9696",
		Prowlarr: ProwlarrOverrides{Backfill: new(true), BackfillSinceDate: new("2024-01-02")},
	})
	assert.DeepEqual(t, cfg.Targets[4], Target{
		Index: 4, Name: "bazarr", App: "bazarr", URL: "http://bazarr:6767",
		Bazarr: BazarrOverrides{SeriesBatchSize: new(50), SeriesBatchConcurrency: new(2)},
	})
	assert.DeepEqual(t, cfg.Targets[5], Target{
		Index: 5, Name: "sabnzbd", App: "sabnzbd", URL: "http://sabnzbd:8080",
		RequestTimeout: new(10 * time.Second), DisableSSLVerify: new(true), ProxyFromEnv: new(false),
	})
}

func TestLoad_DefaultCap(t *testing.T) {
	cfg, err := Load(block(0, "a", "radarr", "http://a:1"))
	assert.NoError(t, err)
	assert.Equal(t, cfg.MaxUpstreamRequests, 64)
}

func TestLoad_APIKeyFile(t *testing.T) {
	keyFile := writeKeyFile(t, "  fromFile0123456789abcd \n")
	cases := []struct {
		name  string
		extra []string
		want  string
	}{
		{"file only", []string{"API_KEY_FILE=" + keyFile}, "fromFile0123456789abcd"},
		{"file wins over inline", []string{"API_KEY=inline", "API_KEY_FILE=" + keyFile}, "fromFile0123456789abcd"},
		{"inline only", []string{"API_KEY=inline"}, "inline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(block(0, "a", "radarr", "http://a:1", tc.extra...))
			assert.NoError(t, err)
			assert.Equal(t, cfg.Targets[0].APIKey, tc.want)
			assert.Equal(t, cfg.Targets[0].APIKeyFromFile, "")
		})
	}
}

func TestLoad_UnsetsSecrets(t *testing.T) {
	clearTargetEnv(t)
	keyFile := writeKeyFile(t, "fileKey0123456789abcdef")
	for _, kv := range environ(
		block(0, "radarr", "radarr", "http://radarr:7878", "API_KEY=inline0", "FORM_AUTH=true", "AUTH_USERNAME=admin", "AUTH_PASSWORD=pw"),
		block(1, "sonarr", "sonarr", "http://sonarr:8989", "API_KEY_FILE="+keyFile),
	) {
		name, value, _ := strings.Cut(kv, "=")
		t.Setenv(name, value)
	}

	cfg, err := Load(os.Environ())
	assert.NoError(t, err)
	assert.Equal(t, cfg.Targets[0].AuthPassword, "pw")
	assert.Equal(t, cfg.Targets[1].APIKey, "fileKey0123456789abcdef")
	assertUnset(t,
		"TARGET_0_API_KEY", "TARGET_0_API_KEY_FILE", "TARGET_0_AUTH_USERNAME", "TARGET_0_AUTH_PASSWORD",
		"TARGET_1_API_KEY", "TARGET_1_API_KEY_FILE", "TARGET_1_AUTH_USERNAME", "TARGET_1_AUTH_PASSWORD",
	)
	assert.Equal(t, os.Getenv("TARGET_0_NAME"), "radarr", "non-secret settings stay")
}

func TestLoad_FaultDoesNotStopLaterTargets(t *testing.T) {
	clearTargetEnv(t)
	for _, kv := range environ(
		block(0, "sonarr", "sonarr", "http://sonarr:8989", "API_KEY=secret0", "SERIES_CONCURRENCY=abc"),
		block(1, "radarr", "radarr", "http://radarr:7878", "API_KEY=secret1", "AUTH_PASSWORD=pw1", "FORM_AUTH=true", "AUTH_USERNAME=u"),
	) {
		name, value, _ := strings.Cut(kv, "=")
		t.Setenv(name, value)
	}

	cfg, err := Load(os.Environ())
	assert.DeepEqual(t, messages(err), []string{"TARGET_0_SERIES_CONCURRENCY: invalid int"})
	assert.Len(t, cfg.Targets, 2)
	assert.Equal(t, cfg.Targets[0].APIKey, "secret0")
	assert.Equal(t, cfg.Targets[1].Name, "radarr")
	assert.Equal(t, cfg.Targets[1].APIKey, "secret1")
	assertUnset(t, "TARGET_0_API_KEY", "TARGET_1_API_KEY", "TARGET_1_AUTH_USERNAME", "TARGET_1_AUTH_PASSWORD")
}

func TestLoad_IgnoresProcessEnvironment(t *testing.T) {
	clearTargetEnv(t)
	t.Setenv("MAX_UPSTREAM_REQUESTS", "lots")
	t.Setenv("TARGET_0_SERIES_CONCURRENCY", "abc")
	t.Setenv("TARGET_1_NAME", "stray")

	cfg, err := Load(block(0, "a", "sonarr", "http://a:1"))
	assert.NoError(t, err)
	assert.Len(t, cfg.Targets, 1)
	assert.Nil(t, cfg.Targets[0].SeriesConcurrency)
	assert.Equal(t, cfg.MaxUpstreamRequests, 64)
}

func TestLoad_EmptyValueInherits(t *testing.T) {
	cfg, err := Load(block(0, "a", "sonarr", "http://a:1", "SERIES_CONCURRENCY=", "SCRAPE_TIMEOUT=", "FORM_AUTH="))
	assert.NoError(t, err)
	assert.Nil(t, cfg.Targets[0].SeriesConcurrency)
	assert.Nil(t, cfg.Targets[0].ScrapeTimeout)
	assert.False(t, cfg.Targets[0].FormAuth)
}

func TestLoad_KeyScan(t *testing.T) {
	gap := func(idx string, missing int) string {
		return fmt.Sprintf("TARGET_%s_*: target indices must be contiguous from 0 (missing TARGET_%d_*)", idx, missing)
	}
	cases := []struct {
		name    string
		environ []string
		want    []string
	}{
		{
			name:    "gap after 0",
			environ: environ(block(0, "a", "radarr", "http://a:1"), block(2, "c", "radarr", "http://c:1")),
			want:    []string{gap("2", 1)},
		},
		{
			name:    "no index 0",
			environ: block(1, "b", "radarr", "http://b:1"),
			want: []string{
				gap("1", 0),
				"serve requires at least one target (TARGET_0_NAME, TARGET_0_APP, TARGET_0_URL, …)",
			},
		},
		{
			name: "gaps in numeric order, once per index",
			environ: environ(
				block(0, "a", "radarr", "http://a:1"),
				block(10, "k", "radarr", "http://k:1"),
				block(2, "c", "radarr", "http://c:1"),
			),
			want: []string{gap("2", 1), gap("10", 1)},
		},
		{
			name:    "huge index",
			environ: environ(block(0, "a", "radarr", "http://a:1"), []string{"TARGET_99999999999999999999_NAME=x"}),
			want:    []string{gap("99999999999999999999", 1)},
		},
		{
			name: "malformed variables",
			environ: environ(block(0, "a", "radarr", "http://a:1"), []string{
				"TARGET_01_NAME=x", "TARGET_x_URL=http://evil", "TARGET__NAME=x", "TARGET_0_=x", "TARGET_1=x",
			}),
			want: []string{
				"TARGET_01_NAME: malformed target variable",
				"TARGET_0_: malformed target variable",
				"TARGET_1: malformed target variable",
				"TARGET__NAME: malformed target variable",
				"TARGET_x_URL: malformed target variable",
			},
		},
		{
			name:    "other prefixes ignored",
			environ: environ(block(0, "a", "radarr", "http://a:1"), []string{"TARGETS_FOO=x", "TARGET=x", "target_0_NAME=x", "MY_TARGET_0_NAME=x"}),
		},
		{
			name: "unknown keys",
			environ: environ(block(0, "a", "prowlarr", "http://a:1"), []string{
				"TARGET_0_URLL=http://typo", "TARGET_0_PROWLARR__BACKFIL=true", "TARGET_0_name=x", "TARGET_0_INDEX=3",
			}),
			want: []string{
				"TARGET_0_INDEX: unknown setting",
				"TARGET_0_PROWLARR__BACKFIL: unknown setting",
				"TARGET_0_URLL: unknown setting",
				"TARGET_0_name: unknown setting",
			},
		},
		{
			name:    "unknown key beyond a gap",
			environ: environ(block(0, "a", "radarr", "http://a:1"), []string{"TARGET_2_URLL=x"}),
			want:    []string{gap("2", 1), "TARGET_2_URLL: unknown setting"},
		},
		{
			name: "duplicate and value-less entries",
			environ: environ(block(0, "a", "radarr", "http://a:1"), []string{
				"TARGET_0_URLL=x", "TARGET_0_URLL=y", "TARGET_0_BOGUS", "TARGET_5_NAME",
			}),
			want: []string{"TARGET_0_URLL: unknown setting"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.environ)
			assert.DeepEqual(t, messages(err), tc.want)
		})
	}
}

func TestLoad_ValidateRules(t *testing.T) {
	cases := []struct {
		name    string
		environ []string
		want    []string
		absent  []string
	}{
		{
			name: "no targets",
			want: []string{"serve requires at least one target (TARGET_0_NAME, TARGET_0_APP, TARGET_0_URL, …)"},
		},
		{
			name:    "bad name is not echoed",
			environ: block(0, "Sonarr-HD", "sonarr", "http://a:1"),
			want:    []string{"target 0: name must match ^[a-z0-9][a-z0-9_-]{0,62}$"},
			absent:  []string{"Sonarr-HD"},
		},
		{
			name:    "url as name is not echoed",
			environ: block(0, "http://user:pw@evil.example/x", "sonarr", "http://a:1"),
			want:    []string{"target 0: name must match ^[a-z0-9][a-z0-9_-]{0,62}$"},
			absent:  []string{"evil", "pw"},
		},
		{
			name:    "missing name",
			environ: []string{"TARGET_0_APP=sonarr", "TARGET_0_URL=http://a:1"},
			want:    []string{"target 0: name must match ^[a-z0-9][a-z0-9_-]{0,62}$"},
		},
		{
			name:    "name too long",
			environ: block(0, strings.Repeat("a", 64), "sonarr", "http://a:1"),
			want:    []string{"target 0: name must match ^[a-z0-9][a-z0-9_-]{0,62}$"},
		},
		{
			name:    "longest name",
			environ: block(0, strings.Repeat("a", 63), "sonarr", "http://a:1"),
		},
		{
			name:    "duplicate names",
			environ: environ(block(0, "a", "sonarr", "http://a:1"), block(1, "b", "sonarr", "http://b:1"), block(2, "a", "radarr", "http://c:1")),
			want:    []string{"target 2/a: duplicate name (also target 0)"},
		},
		{
			name:    "unknown app is not echoed",
			environ: block(0, "a", "plexarr", "http://a:1"),
			want:    []string{"target 0/a: app must be one of: " + appList},
			absent:  []string{"plexarr"},
		},
		{
			name:    "missing app",
			environ: []string{"TARGET_0_NAME=a", "TARGET_0_URL=http://a:1"},
			want:    []string{"target 0/a: app must be one of: " + appList},
		},
		{
			name:    "app is case sensitive",
			environ: block(0, "a", "Sonarr", "http://a:1"),
			want:    []string{"target 0/a: app must be one of: " + appList},
			absent:  []string{"Sonarr"},
		},
		{
			name:    "missing url",
			environ: []string{"TARGET_0_NAME=a", "TARGET_0_APP=sonarr"},
			want:    []string{"target 0/a: url is required"},
		},
		{
			name:    "invalid urls are left to the app validation",
			environ: environ(block(0, "a", "sonarr", "not a url"), block(1, "b", "sonarr", "not a url"), block(2, "c", "sonarr", "http://[::1")),
		},
		{
			name:    "duplicate url",
			environ: environ(block(0, "a", "sonarr", "http://user:secret@sonarr:8989"), block(1, "b", "radarr", "http://user:secret@sonarr:8989")),
			want:    []string{"target 1/b: same url as target 0/a"},
			absent:  []string{"sonarr:8989", "secret"},
		},
		{
			name:    "duplicate url differing in case",
			environ: environ(block(0, "a", "sonarr", "http://sonarr:8989"), block(1, "b", "sonarr", "HTTP://Sonarr:8989")),
			want:    []string{"target 1/b: same url as target 0/a"},
		},
		{
			name:    "duplicate url differing in trailing slash",
			environ: environ(block(0, "a", "sonarr", "http://sonarr:8989/"), block(1, "b", "sonarr", "http://sonarr:8989")),
			want:    []string{"target 1/b: same url as target 0/a"},
		},
		{
			name:    "duplicate url with path and trailing slash",
			environ: environ(block(0, "a", "sonarr", "http://h/sonarr"), block(1, "b", "sonarr", "http://H/sonarr/")),
			want:    []string{"target 1/b: same url as target 0/a"},
		},
		{
			name:    "duplicate url with invalid names",
			environ: environ(block(0, "A", "sonarr", "http://h:1"), block(1, "B", "sonarr", "http://h:1")),
			want: []string{
				"target 0: name must match ^[a-z0-9][a-z0-9_-]{0,62}$",
				"target 1: name must match ^[a-z0-9][a-z0-9_-]{0,62}$",
				"target 1: same url as target 0",
			},
		},
		{
			name: "distinct urls",
			environ: environ(
				block(0, "a", "sonarr", "http://h:1/a"), block(1, "b", "sonarr", "http://h:1/b"),
				block(2, "c", "sonarr", "http://h:2/a"), block(3, "d", "sonarr", "https://h:1/a"),
			),
		},
		{
			name:    "prowlarr keys on another app",
			environ: block(0, "a", "sonarr", "http://a:1", "PROWLARR__BACKFILL=false", "PROWLARR__BACKFILL_SINCE_DATE=2024-01-01"),
			want: []string{
				"target 0/a: PROWLARR__BACKFILL is not valid for app sonarr",
				"target 0/a: PROWLARR__BACKFILL_SINCE_DATE is not valid for app sonarr",
			},
		},
		{
			name:    "bazarr keys on another app",
			environ: block(0, "a", "prowlarr", "http://a:1", "BAZARR__SERIES_BATCH_SIZE=10", "BAZARR__SERIES_BATCH_CONCURRENCY=1"),
			want: []string{
				"target 0/a: BAZARR__SERIES_BATCH_SIZE is not valid for app prowlarr",
				"target 0/a: BAZARR__SERIES_BATCH_CONCURRENCY is not valid for app prowlarr",
			},
		},
		{
			name: "arr-only keys on sabnzbd",
			environ: block(0, "a", "sabnzbd", "http://a:1",
				"FORM_AUTH=true", "AUTH_USERNAME=u", "AUTH_PASSWORD=p", "ENABLE_UNKNOWN_QUEUE_ITEMS=false",
				"DISABLE_QUALITY_METRICS=true", "DISABLE_EPISODE_METRICS=false", "DISABLE_ALBUM_METRICS=true",
				"DISABLE_HISTORY_METRICS=true", "DISABLE_WANTED_METRICS=true", "SERIES_CONCURRENCY=3",
				"PROWLARR__BACKFILL=true", "BAZARR__SERIES_BATCH_SIZE=10"),
			want: []string{
				"target 0/a: FORM_AUTH is not valid for app sabnzbd",
				"target 0/a: AUTH_USERNAME is not valid for app sabnzbd",
				"target 0/a: AUTH_PASSWORD is not valid for app sabnzbd",
				"target 0/a: ENABLE_UNKNOWN_QUEUE_ITEMS is not valid for app sabnzbd",
				"target 0/a: DISABLE_QUALITY_METRICS is not valid for app sabnzbd",
				"target 0/a: DISABLE_EPISODE_METRICS is not valid for app sabnzbd",
				"target 0/a: DISABLE_ALBUM_METRICS is not valid for app sabnzbd",
				"target 0/a: DISABLE_HISTORY_METRICS is not valid for app sabnzbd",
				"target 0/a: DISABLE_WANTED_METRICS is not valid for app sabnzbd",
				"target 0/a: SERIES_CONCURRENCY is not valid for app sabnzbd",
				"target 0/a: PROWLARR__BACKFILL is not valid for app sabnzbd",
				"target 0/a: BAZARR__SERIES_BATCH_SIZE is not valid for app sabnzbd",
			},
		},
		{
			name: "keys valid for their app",
			environ: environ(
				block(0, "s", "sabnzbd", "http://s:1", "FORM_AUTH=false", "DISABLE_SSL_VERIFY=true", "PROXY_FROM_ENV=true", "SCRAPE_TIMEOUT=5s", "REQUEST_TIMEOUT=1s", "API_KEY=k"),
				block(1, "p", "prowlarr", "http://p:1", "PROWLARR__BACKFILL=true", "PROWLARR__BACKFILL_SINCE_DATE=2024-01-01", "FORM_AUTH=true", "AUTH_USERNAME=u", "AUTH_PASSWORD=p"),
				block(2, "b", "bazarr", "http://b:1", "BAZARR__SERIES_BATCH_SIZE=10", "BAZARR__SERIES_BATCH_CONCURRENCY=1"),
				block(3, "l", "lidarr", "http://l:1", "DISABLE_ALBUM_METRICS=true", "SERIES_CONCURRENCY=3", "ENABLE_UNKNOWN_QUEUE_ITEMS=true"),
			),
		},
		{
			name:    "misuse is not checked for an unknown app",
			environ: block(0, "a", "plexarr", "http://a:1", "PROWLARR__BACKFILL=true"),
			want:    []string{"target 0/a: app must be one of: " + appList},
		},
		{
			name:    "zero scrape timeout",
			environ: block(0, "a", "sonarr", "http://a:1", "SCRAPE_TIMEOUT=0s"),
			want:    []string{"target 0/a: SCRAPE_TIMEOUT must be greater than zero"},
		},
		{
			name:    "negative scrape timeout",
			environ: block(0, "a", "sonarr", "http://a:1", "SCRAPE_TIMEOUT=-1s"),
			want:    []string{"target 0/a: SCRAPE_TIMEOUT must be greater than zero"},
		},
		{
			name:    "cap equal to the number of targets",
			environ: environ(block(0, "a", "sonarr", "http://a:1"), block(1, "b", "sonarr", "http://b:1"), []string{"MAX_UPSTREAM_REQUESTS=2"}),
			want:    []string{"MAX_UPSTREAM_REQUESTS must be at least the number of targets + 1 (3)"},
		},
		{
			name:    "cap of targets + 1",
			environ: environ(block(0, "a", "sonarr", "http://a:1"), block(1, "b", "sonarr", "http://b:1"), []string{"MAX_UPSTREAM_REQUESTS=3"}),
		},
		{
			name:    "negative cap",
			environ: environ(block(0, "a", "sonarr", "http://a:1"), []string{"MAX_UPSTREAM_REQUESTS=-5"}),
			want:    []string{"MAX_UPSTREAM_REQUESTS must be at least the number of targets + 1 (2)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.environ)
			assert.DeepEqual(t, messages(err), tc.want)
			for _, s := range tc.absent {
				assert.NotContains(t, err.Error(), s)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	cfg := &Config{MaxUpstreamRequests: 1, Targets: []Target{
		{Index: 0, Name: "a", App: "sonarr", URL: "http://a:1"},
		{Index: 1, Name: "a", App: "radarr", URL: "http://A:1/"},
	}}
	assert.DeepEqual(t, messages(cfg.Validate()), []string{
		"target 1/a: duplicate name (also target 0)",
		"target 1/a: same url as target 0/a",
		"MAX_UPSTREAM_REQUESTS must be at least the number of targets + 1 (3)",
	})

	cfg.MaxUpstreamRequests = 3
	cfg.Targets[1].Name = "b"
	cfg.Targets[1].URL = "http://b:1"
	assert.NoError(t, cfg.Validate())
}

func TestLoad_ParseErrorsRewritten(t *testing.T) {
	cases := []struct {
		app, setting, value, want string
	}{
		{"sonarr", "SERIES_CONCURRENCY", "notanint42", "TARGET_0_SERIES_CONCURRENCY: invalid int"},
		{"sonarr", "SERIES_CONCURRENCY", "99999999999", "TARGET_0_SERIES_CONCURRENCY: invalid int"},
		{"sonarr", "SCRAPE_TIMEOUT", "forever", "TARGET_0_SCRAPE_TIMEOUT: invalid duration"},
		{"sonarr", "REQUEST_TIMEOUT", "soonish", "TARGET_0_REQUEST_TIMEOUT: invalid duration"},
		{"sonarr", "DISABLE_SSL_VERIFY", "maybe", "TARGET_0_DISABLE_SSL_VERIFY: invalid bool"},
		{"sonarr", "PROXY_FROM_ENV", "perhaps", "TARGET_0_PROXY_FROM_ENV: invalid bool"},
		{"radarr", "FORM_AUTH", "maybe", "TARGET_0_FORM_AUTH: invalid bool"},
		{"sonarr", "DISABLE_EPISODE_METRICS", "maybe", "TARGET_0_DISABLE_EPISODE_METRICS: invalid bool"},
		{"prowlarr", "PROWLARR__BACKFILL", "maybe", "TARGET_0_PROWLARR__BACKFILL: invalid bool"},
		{"bazarr", "BAZARR__SERIES_BATCH_SIZE", "huge", "TARGET_0_BAZARR__SERIES_BATCH_SIZE: invalid int"},
		{"bazarr", "BAZARR__SERIES_BATCH_CONCURRENCY", "1.5", "TARGET_0_BAZARR__SERIES_BATCH_CONCURRENCY: invalid int"},
		// The failed field is reset, so no value-based rule fires as well.
		{"sabnzbd", "DISABLE_EPISODE_METRICS", "maybe", "TARGET_0_DISABLE_EPISODE_METRICS: invalid bool"},
		{"sabnzbd", "SERIES_CONCURRENCY", "many", "TARGET_0_SERIES_CONCURRENCY: invalid int"},
		{"radarr", "PROWLARR__BACKFILL", "maybe", "TARGET_0_PROWLARR__BACKFILL: invalid bool"},
		{"sonarr", "BAZARR__SERIES_BATCH_SIZE", "huge", "TARGET_0_BAZARR__SERIES_BATCH_SIZE: invalid int"},
	}
	for _, tc := range cases {
		t.Run(tc.app+"/"+tc.setting+"="+tc.value, func(t *testing.T) {
			cfg, err := Load(block(0, "a", tc.app, "http://a:1", tc.setting+"="+tc.value))
			assert.DeepEqual(t, messages(err), []string{tc.want})
			assert.NotContains(t, err.Error(), tc.value)
			assert.DeepEqual(t, cfg.Targets[0], Target{Name: "a", App: tc.app, URL: "http://a:1"}, "failed field must be reset")
		})
	}
}

func TestLoad_CapParseError(t *testing.T) {
	cfg, err := Load(environ(block(0, "a", "sonarr", "http://a:1"), []string{"MAX_UPSTREAM_REQUESTS=lots"}))
	assert.DeepEqual(t, messages(err), []string{"MAX_UPSTREAM_REQUESTS: invalid int"})
	assert.NotContains(t, err.Error(), "lots")
	assert.Len(t, cfg.Targets, 1)
}

func TestLoad_MissingKeyFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent-key")
	_, err := Load(block(0, "a", "sonarr", "http://a:1", "API_KEY_FILE="+missing))
	assert.DeepEqual(t, messages(err), []string{"TARGET_0_API_KEY_FILE: cannot read file " + missing})
	assert.NotContains(t, err.Error(), "no such file")

	_, err = Load(block(0, "a", "sonarr", "http://a:1", "API_KEY_FILE="+dir, "API_KEY=inlineSecret"))
	assert.DeepEqual(t, messages(err), []string{"TARGET_0_API_KEY_FILE: cannot read file " + dir})
	assert.NotContains(t, err.Error(), "inlineSecret")
}

func TestLoad_AllErrorsJoined(t *testing.T) {
	_, err := Load(environ(
		block(0, "a", "sonarr", "http://a:1", "SERIES_CONCURRENCY=abc", "URLL=x"),
		block(1, "Bad", "radarr", "http://b:1"),
		[]string{"TARGET_01_NAME=x", "TARGET_3_NAME=c"},
	))
	assert.DeepEqual(t, messages(err), []string{
		"TARGET_01_NAME: malformed target variable",
		"TARGET_0_URLL: unknown setting",
		"TARGET_3_*: target indices must be contiguous from 0 (missing TARGET_2_*)",
		"TARGET_0_SERIES_CONCURRENCY: invalid int",
		"target 1: name must match ^[a-z0-9][a-z0-9_-]{0,62}$",
	})
}

func TestRewriteTargetError_Fallback(t *testing.T) {
	for _, err := range []error{
		errors.New("zqsecretqz"),
		env.ParseError{Name: "NoSuchField", Err: errors.New("zqsecretqz")},
		env.NoParserError{Name: "Prowlarr"},
	} {
		var tgt Target
		got := rewriteTargetError(&tgt, "TARGET_3_", 3, err)
		assert.Equal(t, got.Error(), "TARGET_3: invalid configuration")
	}
}

func TestLabel(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"sonarr-hd", "target 2/sonarr-hd"},
		{"a_1", "target 2/a_1"},
		{"Sonarr", "target 2"},
		{"", "target 2"},
		{"http://evil", "target 2"},
		{"-x", "target 2"},
	}
	for _, tc := range cases {
		tgt := Target{Index: 2, Name: tc.name}
		assert.Equal(t, tgt.Label(), tc.want)
	}
}

func TestFieldKeys(t *testing.T) {
	params, err := env.GetFieldParams(&Target{})
	assert.NoError(t, err)
	assert.Equal(t, len(fieldKeys), len(params), "every env field needs exactly one fieldKeys entry")
	assert.Equal(t, len(targetFields), len(params))

	seenKeys := map[string]bool{}
	typ := reflect.TypeFor[Target]()
	for name, f := range fieldKeys {
		assert.Equal(t, f.name, name)
		assert.False(t, seenKeys[f.key], "duplicate key %s", f.key)
		seenKeys[f.key] = true
		assert.True(t, knownKeys[f.key], "%s not in GetFieldParams", f.key)
		assert.Equal(t, typ.FieldByIndex(f.index).Name, name)
	}
	for _, p := range params {
		assert.True(t, seenKeys[p.Key], "%s missing from fieldKeys", p.Key)
	}

	kinds := map[string]string{
		"Name":              "value",
		"FormAuth":          "bool",
		"ScrapeTimeout":     "duration",
		"DisableSSLVerify":  "bool",
		"SeriesConcurrency": "int",
		"Backfill":          "bool",
		"BackfillSinceDate": "value",
		"SeriesBatchSize":   "int",
	}
	for name, kind := range kinds {
		assert.Equal(t, fieldKeys[name].kind, kind, name)
	}
	assert.Equal(t, fieldKeys["Backfill"].key, "PROWLARR__BACKFILL")
	assert.Equal(t, fieldKeys["SeriesBatchConcurrency"].key, "BAZARR__SERIES_BATCH_CONCURRENCY")
}

func TestAppNames(t *testing.T) {
	assert.Equal(t, strings.Join(AppNames, ", "), appList)
}
