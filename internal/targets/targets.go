// Package targets parses and validates the TARGET_<n>_* settings of
// `exportarr serve`: one named upstream app per index, with optional
// per-target overrides of the process defaults. Errors never contain a value
// taken from the environment.
package targets

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// AppNames lists the app types a target may use; commands' builder table must match it.
var AppNames = []string{"radarr", "sonarr", "lidarr", "prowlarr", "bazarr", "sabnzbd"}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

var targetVarPattern = regexp.MustCompile(`^TARGET_(0|[1-9][0-9]*)_(.+)$`)

func validName(s string) bool {
	return namePattern.MatchString(s)
}

// Config is the serve-mode configuration: the global upstream cap and the targets.
type Config struct {
	MaxUpstreamRequests int      `env:"MAX_UPSTREAM_REQUESTS" envDefault:"64"`
	Targets             []Target `env:"-"`
}

// Target is one TARGET_<n>_* block. Nil pointers inherit the process value.
type Target struct {
	Index          int    `env:"-"`
	Name           string `env:"NAME"`
	App            string `env:"APP"`
	URL            string `env:"URL"`
	APIKey         string `env:"API_KEY,unset"`
	APIKeyFromFile string `env:"API_KEY_FILE,file,unset"`
	FormAuth       bool   `env:"FORM_AUTH"`
	AuthUsername   string `env:"AUTH_USERNAME,unset"`
	AuthPassword   string `env:"AUTH_PASSWORD,unset"`

	ScrapeTimeout    *time.Duration `env:"SCRAPE_TIMEOUT"`
	RequestTimeout   *time.Duration `env:"REQUEST_TIMEOUT"`
	DisableSSLVerify *bool          `env:"DISABLE_SSL_VERIFY"`
	ProxyFromEnv     *bool          `env:"PROXY_FROM_ENV"`

	EnableUnknownQueueItems *bool `env:"ENABLE_UNKNOWN_QUEUE_ITEMS"`
	DisableQualityMetrics   *bool `env:"DISABLE_QUALITY_METRICS"`
	DisableEpisodeMetrics   *bool `env:"DISABLE_EPISODE_METRICS"`
	DisableAlbumMetrics     *bool `env:"DISABLE_ALBUM_METRICS"`
	DisableHistoryMetrics   *bool `env:"DISABLE_HISTORY_METRICS"`
	DisableWantedMetrics    *bool `env:"DISABLE_WANTED_METRICS"`
	SeriesConcurrency       *int  `env:"SERIES_CONCURRENCY"`

	Prowlarr ProwlarrOverrides `envPrefix:"PROWLARR__"`
	Bazarr   BazarrOverrides   `envPrefix:"BAZARR__"`
}

// ProwlarrOverrides holds the per-target PROWLARR__* settings.
type ProwlarrOverrides struct {
	Backfill          *bool   `env:"BACKFILL"`
	BackfillSinceDate *string `env:"BACKFILL_SINCE_DATE"`
}

// BazarrOverrides holds the per-target BAZARR__* settings.
type BazarrOverrides struct {
	SeriesBatchSize        *int `env:"SERIES_BATCH_SIZE"`
	SeriesBatchConcurrency *int `env:"SERIES_BATCH_CONCURRENCY"`
}

// Label returns "target <index>/<name>" when the name is valid, else "target <index>".
func (t *Target) Label() string {
	if validName(t.Name) {
		return fmt.Sprintf("target %d/%s", t.Index, t.Name)
	}
	return fmt.Sprintf("target %d", t.Index)
}

type fieldInfo struct {
	name  string
	key   string
	index []int
	kind  string
}

var targetFields = collectFields(reflect.TypeFor[Target](), "", nil)

var fieldKeys = func() map[string]fieldInfo {
	m := make(map[string]fieldInfo, len(targetFields))
	for _, f := range targetFields {
		m[f.name] = f
	}
	return m
}()

var knownKeys = func() map[string]bool {
	m := map[string]bool{}
	for _, p := range env.Must(env.GetFieldParams(&Target{})) {
		m[p.Key] = true
	}
	return m
}()

func collectFields(t reflect.Type, prefix string, index []int) []fieldInfo {
	var out []fieldInfo
	for i := range t.NumField() {
		sf := t.Field(i)
		path := append(slices.Clone(index), i)
		if p, ok := sf.Tag.Lookup("envPrefix"); ok {
			out = append(out, collectFields(sf.Type, prefix+p, path)...)
			continue
		}
		key, _, _ := strings.Cut(sf.Tag.Get("env"), ",")
		if key == "" || key == "-" {
			continue
		}
		out = append(out, fieldInfo{name: sf.Name, key: prefix + key, index: path, kind: kindWord(sf.Type)})
	}
	return out
}

func kindWord(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == reflect.TypeFor[time.Duration]():
		return "duration"
	case t.Kind() == reflect.Int:
		return "int"
	case t.Kind() == reflect.Bool:
		return "bool"
	}
	return "value"
}

// Load parses environ (os.Environ() in production; explicit in tests) and runs every
// structural check. All failures are joined; none contains a value from the environment.
func Load(environ []string) (*Config, error) {
	m := env.ToMap(environ)
	cfg := &Config{}
	var errs []error
	capErr := env.ParseWithOptions(cfg, env.Options{Environment: m})
	if capErr != nil {
		errs = append(errs, errors.New("MAX_UPSTREAM_REQUESTS: invalid int"))
	}
	count, keyErrs := scanKeys(environ)
	errs = append(errs, keyErrs...)
	for i := range count {
		t, parseErrs := parseTarget(m, i)
		errs = append(errs, parseErrs...)
		cfg.Targets = append(cfg.Targets, t)
	}
	errs = append(errs, cfg.validate(capErr == nil))
	return cfg, errors.Join(errs...)
}

// scanKeys returns the number of contiguous target indices from 0 and the
// errors for malformed variables, index gaps and unknown keys.
func scanKeys(environ []string) (int, []error) {
	var malformed []string
	seen := map[string]bool{}
	unknown := map[string][]string{}
	indices := map[string]bool{}
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "TARGET_") || seen[name] {
			continue
		}
		seen[name] = true
		match := targetVarPattern.FindStringSubmatch(name)
		if match == nil {
			malformed = append(malformed, name)
			continue
		}
		indices[match[1]] = true
		if !knownKeys[match[2]] {
			unknown[match[1]] = append(unknown[match[1]], name)
		}
	}

	count := 0
	for indices[strconv.Itoa(count)] {
		count++
	}

	var errs []error
	slices.Sort(malformed)
	for _, name := range malformed {
		errs = append(errs, fmt.Errorf("%s: malformed target variable", name))
	}
	ordered := slices.Collect(maps.Keys(indices))
	slices.SortFunc(ordered, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
	})
	for _, idx := range ordered {
		if n, err := strconv.Atoi(idx); err != nil || n >= count {
			errs = append(errs, fmt.Errorf("TARGET_%s_*: target indices must be contiguous from 0 (missing TARGET_%d_*)", idx, count))
		}
		names := unknown[idx]
		slices.Sort(names)
		for _, name := range names {
			errs = append(errs, fmt.Errorf("%s: unknown setting", name))
		}
	}
	return count, errs
}

func parseTarget(m map[string]string, i int) (Target, []error) {
	prefix := fmt.Sprintf("TARGET_%d_", i)
	own := make(map[string]string, len(targetFields))
	for _, f := range targetFields {
		if v, ok := m[prefix+f.key]; ok {
			own[prefix+f.key] = v
		}
	}

	var t Target
	var errs []error
	if err := env.ParseWithOptions(&t, env.Options{Environment: own, Prefix: prefix}); err != nil {
		parts := []error{err}
		if agg, ok := err.(interface{ Unwrap() []error }); ok {
			parts = agg.Unwrap()
		}
		for _, e := range parts {
			errs = append(errs, rewriteTargetError(&t, prefix, i, e))
		}
	}

	t.Index = i
	if t.APIKeyFromFile != "" {
		t.APIKey = strings.TrimSpace(t.APIKeyFromFile)
	}
	t.APIKeyFromFile = ""
	return t, errs
}

// rewriteTargetError turns an env library error into a message that names the
// variable but never its value. A field that failed to parse is reset, since
// the library leaves pointer fields allocated at their zero value.
func rewriteTargetError(t *Target, prefix string, i int, err error) error {
	var parseErr env.ParseError
	var fileErr env.LoadFileContentError
	switch {
	case errors.As(err, &parseErr):
		if f, ok := fieldKeys[parseErr.Name]; ok {
			reflect.ValueOf(t).Elem().FieldByIndex(f.index).SetZero()
			return fmt.Errorf("%s%s: invalid %s", prefix, f.key, f.kind)
		}
	case errors.As(err, &fileErr):
		return fmt.Errorf("%s: cannot read file %s", fileErr.Key, fileErr.Filename)
	}
	return fmt.Errorf("TARGET_%d: invalid configuration", i)
}

// Validate checks names, apps, uniqueness, app-specific key misuse and the cap bound.
func (c *Config) Validate() error {
	return c.validate(true)
}

func (c *Config) validate(checkCap bool) error {
	var errs []error
	if len(c.Targets) == 0 {
		errs = append(errs, errors.New("serve requires at least one target (TARGET_0_NAME, TARGET_0_APP, TARGET_0_URL, …)"))
	}
	names := map[string]*Target{}
	urls := map[string]*Target{}
	for i := range c.Targets {
		errs = append(errs, c.Targets[i].validate(names, urls)...)
	}
	if checkCap && c.MaxUpstreamRequests < len(c.Targets)+1 {
		errs = append(errs, fmt.Errorf("MAX_UPSTREAM_REQUESTS must be at least the number of targets + 1 (%d)", len(c.Targets)+1))
	}
	return errors.Join(errs...)
}

func (t *Target) validate(names, urls map[string]*Target) []error {
	var errs []error
	fail := func(msg string) {
		errs = append(errs, errors.New(t.Label()+": "+msg))
	}

	if !validName(t.Name) {
		fail("name must match " + namePattern.String())
	} else if other, dup := names[t.Name]; dup {
		fail(fmt.Sprintf("duplicate name (also target %d)", other.Index))
	} else {
		names[t.Name] = t
	}

	appOK := slices.Contains(AppNames, t.App)
	if !appOK {
		fail("app must be one of: " + strings.Join(AppNames, ", "))
	}

	if t.URL == "" {
		fail("url is required")
	} else if key, ok := urlKey(t.URL); ok {
		if other, dup := urls[key]; dup {
			fail("same url as " + other.Label())
		} else {
			urls[key] = t
		}
	}

	if appOK {
		v := reflect.ValueOf(t).Elem()
		for _, f := range targetFields {
			if !allowedFor(f.key, t.App) && !v.FieldByIndex(f.index).IsZero() {
				fail(f.key + " is not valid for app " + t.App)
			}
		}
	}

	if t.ScrapeTimeout != nil && *t.ScrapeTimeout <= 0 {
		fail("SCRAPE_TIMEOUT must be greater than zero")
	}
	return errs
}

// urlKey normalizes an absolute URL for duplicate detection; other URLs are
// left to the per-app validation.
func urlKey(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimSuffix(u.EscapedPath(), "/"), true
}

var arrOnlyKeys = map[string]bool{
	"FORM_AUTH":                  true,
	"AUTH_USERNAME":              true,
	"AUTH_PASSWORD":              true,
	"ENABLE_UNKNOWN_QUEUE_ITEMS": true,
	"DISABLE_QUALITY_METRICS":    true,
	"DISABLE_EPISODE_METRICS":    true,
	"DISABLE_ALBUM_METRICS":      true,
	"DISABLE_HISTORY_METRICS":    true,
	"DISABLE_WANTED_METRICS":     true,
	"SERIES_CONCURRENCY":         true,
}

func allowedFor(key, app string) bool {
	switch {
	case strings.HasPrefix(key, "PROWLARR__"):
		return app == "prowlarr"
	case strings.HasPrefix(key, "BAZARR__"):
		return app == "bazarr"
	case arrOnlyKeys[key]:
		return app != "sabnzbd"
	}
	return true
}
