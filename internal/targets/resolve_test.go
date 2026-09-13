package targets

import (
	"errors"
	"maps"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	arrconfig "github.com/onedr0p/exportarr/internal/arr/config"
	"github.com/onedr0p/exportarr/internal/assert"
	"github.com/onedr0p/exportarr/internal/config"
	sabconfig "github.com/onedr0p/exportarr/internal/sabnzbd/config"
)

const testKey = "abcdef0123456789abcdef0123456789"

func processConfig() config.Config {
	return config.Config{
		App:            "serve",
		LogLevel:       "info",
		LogFormat:      "console",
		Port:           9707,
		Interface:      "0.0.0.0",
		RequestTimeout: 60 * time.Second,
		ScrapeTimeout:  2 * time.Minute,
	}
}

func processDefaults() arrconfig.ArrConfig {
	var c arrconfig.ArrConfig
	c.ApplyBase(processConfig())
	c.SeriesConcurrency = arrconfig.DefaultSeriesConcurrency
	c.Bazarr = arrconfig.BazarrConfig{SeriesBatchSize: 300, SeriesBatchConcurrency: 10}
	return c
}

func resolvableTarget() Target {
	return Target{Index: 1, Name: "sonarr-hd", App: "sonarr", URL: "http://sonarr-hd:8989", APIKey: testKey}
}

// resolved holds addressable copies of every config a target resolves into.
type resolved struct {
	base config.Config
	arr  arrconfig.ArrConfig
	sab  sabconfig.SabnzbdConfig
}

func resolveAll(t *testing.T, tgt Target) *resolved {
	t.Helper()
	arr, err := tgt.ArrConfig(processDefaults(), processConfig())
	assert.NoError(t, err)
	sab, err := tgt.SabnzbdConfig(processConfig())
	assert.NoError(t, err)
	return &resolved{base: tgt.BaseConfig(processConfig()), arr: *arr, sab: *sab}
}

// field returns the addressable field named by a path like "arr.Prowlarr.Backfill".
func (r *resolved) field(t *testing.T, path string) reflect.Value {
	t.Helper()
	parts := strings.Split(path, ".")
	var v reflect.Value
	switch parts[0] {
	case "base":
		v = reflect.ValueOf(&r.base).Elem()
	case "arr":
		v = reflect.ValueOf(&r.arr).Elem()
	case "sab":
		v = reflect.ValueOf(&r.sab).Elem()
	default:
		t.Fatalf("unknown root in %s", path)
	}
	for _, name := range parts[1:] {
		v = v.FieldByName(name)
		if !v.IsValid() {
			t.Fatalf("no field %s", path)
		}
	}
	return v
}

// pointerFields lists every pointer field of Target, nested ones as "Prowlarr.Backfill".
func pointerFields(typ reflect.Type, prefix string, index []int) map[string][]int {
	out := map[string][]int{}
	for i := range typ.NumField() {
		sf := typ.Field(i)
		path := append(append([]int{}, index...), i)
		switch sf.Type.Kind() {
		case reflect.Pointer:
			out[prefix+sf.Name] = path
		case reflect.Struct:
			maps.Copy(out, pointerFields(sf.Type, prefix+sf.Name+".", path))
		}
	}
	return out
}

func lastSegment(path string) string {
	return path[strings.LastIndex(path, ".")+1:]
}

func nonDefault(t *testing.T, typ reflect.Type) reflect.Value {
	t.Helper()
	switch {
	case typ == reflect.TypeFor[time.Duration]():
		return reflect.ValueOf(37 * time.Second)
	case typ.Kind() == reflect.Bool:
		return reflect.ValueOf(true)
	case typ.Kind() == reflect.Int:
		return reflect.ValueOf(7)
	case typ.Kind() == reflect.String:
		return reflect.ValueOf("2023-04-05")
	}
	t.Fatalf("no non-default value for %s", typ)
	return reflect.Value{}
}

// overrideDestinations maps each Target pointer field to every resolved field it sets.
var overrideDestinations = map[string][]string{
	"ScrapeTimeout":                 {"base.ScrapeTimeout", "arr.CollectTimeout", "sab.CollectTimeout"},
	"RequestTimeout":                {"base.RequestTimeout", "arr.RequestTimeout", "sab.RequestTimeout"},
	"DisableSSLVerify":              {"base.DisableSSLVerify", "arr.DisableSSLVerify", "sab.DisableSSLVerify"},
	"ProxyFromEnv":                  {"base.ProxyFromEnv", "arr.ProxyFromEnv", "sab.ProxyFromEnv"},
	"EnableUnknownQueueItems":       {"arr.EnableUnknownQueueItems"},
	"DisableQualityMetrics":         {"arr.DisableQualityMetrics"},
	"DisableEpisodeMetrics":         {"arr.DisableEpisodeMetrics"},
	"DisableAlbumMetrics":           {"arr.DisableAlbumMetrics"},
	"DisableHistoryMetrics":         {"arr.DisableHistoryMetrics"},
	"DisableWantedMetrics":          {"arr.DisableWantedMetrics"},
	"SeriesConcurrency":             {"arr.SeriesConcurrency"},
	"Prowlarr.Backfill":             {"arr.Prowlarr.Backfill"},
	"Prowlarr.BackfillSinceDate":    {"arr.Prowlarr.BackfillSinceDate", "arr.Prowlarr.BackfillSinceTime"},
	"Bazarr.SeriesBatchSize":        {"arr.Bazarr.SeriesBatchSize"},
	"Bazarr.SeriesBatchConcurrency": {"arr.Bazarr.SeriesBatchConcurrency"},
}

func TestResolve_OverrideCompleteness(t *testing.T) {
	fields := pointerFields(reflect.TypeFor[Target](), "", nil)
	for path := range overrideDestinations {
		_, ok := fields[path]
		assert.True(t, ok, "overrideDestinations lists %s, which is not a pointer field of Target", path)
	}

	for path, index := range fields {
		t.Run(path, func(t *testing.T) {
			dests, ok := overrideDestinations[path]
			if !ok {
				t.Fatalf("pointer field %s has no entry in overrideDestinations", path)
			}
			tgt := resolvableTarget()
			ptr := reflect.ValueOf(&tgt).Elem().FieldByIndex(index)
			value := nonDefault(t, ptr.Type().Elem())
			ptr.Set(reflect.New(ptr.Type().Elem()))
			ptr.Elem().Set(value)

			got := resolveAll(t, tgt)
			want := resolveAll(t, resolvableTarget())
			for _, dest := range dests {
				g := got.field(t, dest)
				assert.False(t, reflect.DeepEqual(g.Interface(), want.field(t, dest).Interface()), "%s did not change %s", path, dest)
				if lastSegment(dest) == lastSegment(path) {
					assert.DeepEqual(t, g.Interface(), value.Interface(), dest)
				}
				want.field(t, dest).Set(g)
			}
			assert.DeepEqual(t, *got, *want, "an override changed a field outside its destinations")
		})
	}
}

func TestResolve_TargetFields(t *testing.T) {
	tgt := resolvableTarget()
	tgt.FormAuth = true
	tgt.AuthUsername = "user"
	tgt.AuthPassword = "pass"
	r := resolveAll(t, tgt)

	assert.Equal(t, r.base.App, "sonarr")
	assert.Equal(t, r.base.URL, "http://sonarr-hd:8989")
	assert.Equal(t, r.base.APIKey, testKey)
	assert.Equal(t, r.arr.App, "sonarr")
	assert.Equal(t, r.arr.URL, "http://sonarr-hd:8989")
	assert.Equal(t, r.arr.APIKey, testKey)
	assert.Equal(t, r.arr.Target, "sonarr-hd")
	assert.True(t, r.arr.FormAuth)
	assert.Equal(t, r.arr.AuthUsername, "user")
	assert.Equal(t, r.arr.AuthPassword, "pass")
	assert.Equal(t, r.sab.URL, "http://sonarr-hd:8989")
	assert.Equal(t, r.sab.APIKey, testKey)
	assert.Equal(t, r.sab.Target, "sonarr-hd")
}

func TestBaseConfig(t *testing.T) {
	process := processConfig()
	process.URL = "http://stale:1"
	process.APIKey = "stale-key"
	process.APIKeyFromFile = "stale-file-contents"
	tgt := resolvableTarget()

	got := tgt.BaseConfig(process)

	want := processConfig()
	want.App = "sonarr"
	want.URL = "http://sonarr-hd:8989"
	want.APIKey = testKey
	assert.DeepEqual(t, got, want)
	assert.Equal(t, process.APIKeyFromFile, "stale-file-contents", "process config must not be modified")
}

func TestArrConfig_Inheritance(t *testing.T) {
	process := processConfig()
	process.DisableSSLVerify = true
	process.ProxyFromEnv = true
	process.RequestTimeout = 45 * time.Second

	defaults := arrconfig.ArrConfig{
		App:                     "serve",
		APIVersion:              "v3",
		EnableUnknownQueueItems: true,
		DisableQualityMetrics:   true,
		DisableEpisodeMetrics:   true,
		DisableAlbumMetrics:     true,
		DisableHistoryMetrics:   true,
		DisableWantedMetrics:    true,
		SeriesConcurrency:       4,
		URL:                     "http://stale:1",
		APIKey:                  "stale-key",
		DisableSSLVerify:        false,
		ProxyFromEnv:            false,
		RequestTimeout:          time.Second,
		CollectTimeout:          time.Second,
		Prowlarr: arrconfig.ProwlarrConfig{
			Backfill:          true,
			BackfillSinceDate: "2024-01-02",
			BackfillSinceTime: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
		},
		Bazarr: arrconfig.BazarrConfig{SeriesBatchSize: 50, SeriesBatchConcurrency: 3},
	}
	controlledByTarget := map[string]bool{
		"FormAuth": true, "AuthUsername": true, "AuthPassword": true, "Target": true,
		"DisableSSLVerify": true, "ProxyFromEnv": true,
	}
	v := reflect.ValueOf(defaults)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		if !controlledByTarget[name] {
			assert.False(t, v.Field(i).IsZero(), "fixture must set %s so inheritance is observable", name)
		}
	}

	tgt := resolvableTarget()
	got, err := tgt.ArrConfig(defaults, process)
	assert.NoError(t, err)

	want := defaults
	want.App = "sonarr"
	want.URL = "http://sonarr-hd:8989"
	want.APIKey = testKey
	want.Target = "sonarr-hd"
	want.CollectTimeout = 115 * time.Second
	want.DisableSSLVerify = true
	want.ProxyFromEnv = true
	want.RequestTimeout = 45 * time.Second
	assert.DeepEqual(t, *got, want)
}

func TestArrConfig_AuthComesFromTarget(t *testing.T) {
	defaults := processDefaults()
	defaults.FormAuth = true
	defaults.AuthUsername = "process-user"
	defaults.AuthPassword = "process-pass"

	tgt := resolvableTarget()
	got, err := tgt.ArrConfig(defaults, processConfig())
	assert.NoError(t, err)
	assert.False(t, got.FormAuth)
	assert.Equal(t, got.AuthUsername, "")
	assert.Equal(t, got.AuthPassword, "")

	tgt.FormAuth = true
	tgt.AuthUsername = "user"
	tgt.AuthPassword = "pass"
	got, err = tgt.ArrConfig(processDefaults(), processConfig())
	assert.NoError(t, err)
	assert.True(t, got.FormAuth)
	assert.Equal(t, got.AuthUsername, "user")
	assert.Equal(t, got.AuthPassword, "pass")
}

func TestResolve_CollectTimeout(t *testing.T) {
	cases := []struct {
		name    string
		target  *time.Duration
		process time.Duration
		scrape  time.Duration
		collect time.Duration
	}{
		{"override 30s", new(30 * time.Second), 2 * time.Minute, 30 * time.Second, 25 * time.Second},
		{"override 6s", new(6 * time.Second), 2 * time.Minute, 6 * time.Second, 3 * time.Second},
		{"process 2m", nil, 2 * time.Minute, 2 * time.Minute, 115 * time.Second},
		{"process 8s", nil, 8 * time.Second, 8 * time.Second, 4 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			process := processConfig()
			process.ScrapeTimeout = tc.process
			tgt := resolvableTarget()
			tgt.ScrapeTimeout = tc.target

			assert.Equal(t, tgt.BaseConfig(process).ScrapeTimeout, tc.scrape)
			arr, err := tgt.ArrConfig(processDefaults(), process)
			assert.NoError(t, err)
			assert.Equal(t, arr.CollectTimeout, tc.collect)
			sab, err := tgt.SabnzbdConfig(process)
			assert.NoError(t, err)
			assert.Equal(t, sab.CollectTimeout, tc.collect)
		})
	}
}

func TestArrConfig_Backfill(t *testing.T) {
	defaults := processDefaults()
	defaults.Prowlarr.BackfillSinceDate = "2024-01-02"
	defaults.Prowlarr.BackfillSinceTime = time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	tgt := Target{Index: 3, Name: "prowl", App: "prowlarr", URL: "http://prowlarr:9696", APIKey: testKey}
	got, err := tgt.ArrConfig(defaults, processConfig())
	assert.NoError(t, err)
	assert.Equal(t, got.Prowlarr.BackfillSinceTime, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), "inherits the process date")

	tgt.Prowlarr.BackfillSinceDate = new("2023-04-05")
	got, err = tgt.ArrConfig(defaults, processConfig())
	assert.NoError(t, err)
	assert.Equal(t, got.Prowlarr.BackfillSinceDate, "2023-04-05")
	assert.Equal(t, got.Prowlarr.BackfillSinceTime, time.Date(2023, 4, 5, 0, 0, 0, 0, time.UTC))

	tgt.Prowlarr.BackfillSinceDate = new("")
	got, err = tgt.ArrConfig(defaults, processConfig())
	assert.NoError(t, err)
	assert.True(t, got.Prowlarr.BackfillSinceTime.IsZero(), "an empty override clears the time")

	tgt.Prowlarr.BackfillSinceDate = new("2023-13-01")
	got, err = tgt.ArrConfig(defaults, processConfig())
	assert.Nil(t, got)
	assert.Error(t, err)
	assert.Equal(t, err.Error(), "target 3/prowl: backfill-since-date must be in the format YYYY-MM-DD")

	bad := processDefaults()
	bad.Prowlarr.BackfillSinceDate = "someday"
	tgt.Prowlarr.BackfillSinceDate = nil
	_, err = tgt.ArrConfig(bad, processConfig())
	assert.Error(t, err)
	assert.Equal(t, err.Error(), "target 3/prowl: backfill-since-date must be in the format YYYY-MM-DD")
	assert.NotContains(t, err.Error(), "someday")
}

func TestSabnzbdConfig(t *testing.T) {
	process := processConfig()
	process.DisableSSLVerify = true
	tgt := Target{Index: 5, Name: "sab", App: "sabnzbd", URL: "http://sab:8080", APIKey: "sabkey", RequestTimeout: new(9 * time.Second)}

	got, err := tgt.SabnzbdConfig(process)
	assert.NoError(t, err)
	assert.DeepEqual(t, *got, sabconfig.SabnzbdConfig{
		URL:              "http://sab:8080",
		APIKey:           "sabkey",
		DisableSSLVerify: true,
		RequestTimeout:   9 * time.Second,
		CollectTimeout:   115 * time.Second,
		Target:           "sab",
	})
}

func TestSabnzbdConfig_ErrorLabeled(t *testing.T) {
	orig := loadSabnzbdConfig
	t.Cleanup(func() { loadSabnzbdConfig = orig })
	loadSabnzbdConfig = func(config.Config) (*sabconfig.SabnzbdConfig, error) {
		return nil, errors.New("broken")
	}

	tgt := Target{Index: 5, Name: "sab", App: "sabnzbd"}
	got, err := tgt.SabnzbdConfig(processConfig())
	assert.Nil(t, got)
	assert.Error(t, err)
	assert.Equal(t, err.Error(), "target 5/sab: broken")
}

func TestResolve_Independent(t *testing.T) {
	defaults := processDefaults()
	a := resolvableTarget()
	b := resolvableTarget()
	b.Index, b.Name, b.URL = 2, "sonarr-4k", "http://sonarr-4k:8989"

	first, err := a.ArrConfig(defaults, processConfig())
	assert.NoError(t, err)
	second, err := b.ArrConfig(defaults, processConfig())
	assert.NoError(t, err)
	want := *second

	first.SeriesConcurrency = 1
	first.Prowlarr.BackfillSinceDate = "2020-01-01"
	first.Bazarr.SeriesBatchSize = 1
	first.Target = "changed"

	assert.DeepEqual(t, *second, want)
	assert.DeepEqual(t, defaults, processDefaults(), "defaults must not be modified")
	assert.Equal(t, second.Target, "sonarr-4k")
}

func serveFlags(t *testing.T, set map[string]string) *pflag.FlagSet {
	t.Helper()
	fs := pflag.NewFlagSet("serve", pflag.ContinueOnError)
	config.RegisterConfigFlags(fs)
	for name, value := range set {
		assert.NoError(t, fs.Set(name, value))
	}
	return fs
}

func TestCheckProcessConfig(t *testing.T) {
	const (
		probeURL  = "http://zqurlqz:8989"
		probeKey  = "zqkeyqz"
		probeUser = "zquserqz"
		probePass = "zqpassqz"
	)
	perTarget := ": URLs and credentials are set per target "
	keyMsg := perTarget + "(TARGET_<n>_API_KEY or TARGET_<n>_API_KEY_FILE)"

	cases := []struct {
		name  string
		base  config.Config
		arr   arrconfig.ArrConfig
		flags map[string]string
		want  []string
	}{
		{name: "clean", flags: map[string]string{"port": "9000", "scrape-timeout": "30s"}},
		{
			name: "URL env",
			base: config.Config{URL: probeURL},
			want: []string{"URL is not supported by serve" + perTarget + "(TARGET_<n>_URL)"},
		},
		{
			name:  "url flag",
			base:  config.Config{URL: probeURL},
			flags: map[string]string{"url": probeURL},
			want:  []string{"--url is not supported by serve" + perTarget + "(TARGET_<n>_URL)"},
		},
		{
			name: "inline API_KEY",
			base: config.Config{APIKey: probeKey},
			want: []string{"API_KEY is not supported by serve" + keyMsg},
		},
		{
			name: "API_KEY_FILE",
			base: config.Config{APIKey: probeKey, APIKeyFromFile: probeKey + "\n"},
			want: []string{"API_KEY_FILE is not supported by serve" + keyMsg},
		},
		{
			name: "blank API_KEY_FILE",
			base: config.Config{APIKeyFromFile: "\n"},
			want: []string{"API_KEY_FILE is not supported by serve" + keyMsg},
		},
		{
			name:  "api-key flag",
			base:  config.Config{APIKey: probeKey},
			flags: map[string]string{"api-key": probeKey},
			want:  []string{"--api-key is not supported by serve" + keyMsg},
		},
		{
			name: "FORM_AUTH",
			arr:  arrconfig.ArrConfig{FormAuth: true},
			want: []string{"FORM_AUTH is not supported by serve" + perTarget + "(TARGET_<n>_FORM_AUTH)"},
		},
		{
			name: "AUTH_USERNAME",
			arr:  arrconfig.ArrConfig{AuthUsername: probeUser},
			want: []string{"AUTH_USERNAME is not supported by serve" + perTarget + "(TARGET_<n>_AUTH_USERNAME)"},
		},
		{
			name: "AUTH_PASSWORD",
			arr:  arrconfig.ArrConfig{AuthPassword: probePass},
			want: []string{"AUTH_PASSWORD is not supported by serve" + perTarget + "(TARGET_<n>_AUTH_PASSWORD)"},
		},
		{
			name: "everything",
			base: config.Config{URL: probeURL, APIKey: probeKey},
			arr:  arrconfig.ArrConfig{FormAuth: true, AuthUsername: probeUser, AuthPassword: probePass},
			want: []string{
				"URL is not supported by serve" + perTarget + "(TARGET_<n>_URL)",
				"API_KEY is not supported by serve" + keyMsg,
				"FORM_AUTH is not supported by serve" + perTarget + "(TARGET_<n>_FORM_AUTH)",
				"AUTH_USERNAME is not supported by serve" + perTarget + "(TARGET_<n>_AUTH_USERNAME)",
				"AUTH_PASSWORD is not supported by serve" + perTarget + "(TARGET_<n>_AUTH_PASSWORD)",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckProcessConfig(&tc.base, &tc.arr, serveFlags(t, tc.flags))
			if tc.want == nil {
				assert.NoError(t, err)
				return
			}
			assert.Error(t, err)
			assert.DeepEqual(t, messages(err), tc.want)
			for _, secret := range []string{"zqurlqz", probeKey, probeUser, probePass} {
				assert.NotContains(t, err.Error(), secret)
			}
		})
	}
}

func TestCheckProcessConfig_FromLoadedConfig(t *testing.T) {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "URL", "API_KEY", "API_KEY_FILE", "FORM_AUTH", "AUTH_USERNAME", "AUTH_PASSWORD":
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
	t.Setenv("URL", "http://zqurlqz:8989")
	t.Setenv("API_KEY_FILE", writeKeyFile(t, "zqkeyqz\n"))
	t.Setenv("FORM_AUTH", "true")
	t.Setenv("AUTH_USERNAME", "zquserqz")
	t.Setenv("AUTH_PASSWORD", "zqpassqz")

	flags := serveFlags(t, nil)
	base, err := config.LoadConfig(flags)
	assert.NoError(t, err)
	assert.Equal(t, base.APIKey, "zqkeyqz")
	arrDefaults, err := arrconfig.LoadArrConfig(*base, flags)
	assert.NoError(t, err)

	err = CheckProcessConfig(base, arrDefaults, flags)
	assert.Error(t, err)
	got := messages(err)
	assert.Len(t, got, 5)
	for i, setting := range []string{"URL ", "API_KEY_FILE ", "FORM_AUTH ", "AUTH_USERNAME ", "AUTH_PASSWORD "} {
		assert.True(t, strings.HasPrefix(got[i], setting), "message %d = %q, want prefix %q", i, got[i], setting)
	}
	for _, secret := range []string{"zqurlqz", "zqkeyqz", "zquserqz", "zqpassqz"} {
		assert.NotContains(t, err.Error(), secret)
	}
}
