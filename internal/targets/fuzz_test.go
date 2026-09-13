package targets

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// referenceValidName is an independent, byte-by-byte statement of the name grammar.
func referenceValidName(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

func FuzzValidateName(f *testing.F) {
	for _, s := range []string{
		"sonarr-hd", "a", strings.Repeat("a", 63), strings.Repeat("a", 64),
		"Sonarr", "-x", "x/y", "x:y", "x.y", "%2F", "http://evil", "",
		"_x", "x_", "0", "sonarr\n", "\nsonarr", "sonarr hd", "ſonarr", "K",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := validName(s)
		if got != referenceValidName(s) {
			t.Fatalf("validName(%q) = %v, reference says %v", s, got, !got)
		}
		if !got {
			return
		}
		if !namePattern.MatchString(s) {
			t.Errorf("accepted %q does not match %s", s, namePattern)
		}
		if strings.ContainsAny(s, ":/.%") || strings.ContainsFunc(s, unicode.IsUpper) {
			t.Errorf("accepted %q contains a separator, escape or uppercase letter", s)
		}
	})
}

var (
	apiKeyFileVar = regexp.MustCompile(`^TARGET_(0|[1-9][0-9]*)_API_KEY_FILE$`)
	nameVar       = regexp.MustCompile(`^TARGET_(0|[1-9][0-9]*)_NAME$`)
)

func leafErrors(err error) []error {
	if agg, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range agg.Unwrap() {
			out = append(out, leafErrors(e)...)
		}
		return out
	}
	return []error{err}
}

func FuzzLoad(f *testing.F) {
	seeds := []struct {
		idx                    uint8
		key, value, name, app string
	}{
		{0, "API_KEY", "k", "sonarr-hd", "sonarr"},
		{1, "NAME", "second", "first", "radarr"},
		{0, "URL", "", "sonarr-hd", "sonarr"},
		{2, "NAME", "x", "sonarr-hd", "sonarr"},
		{3, "APP", "sonarr", "sonarr-hd", "sonarr"},
		{128, "TARGET_01_NAME", "x", "sonarr-hd", "sonarr"},
		{128, "TARGET_x_URL", "x", "sonarr-hd", "sonarr"},
		{128, "TARGET__NAME", "x", "sonarr-hd", "sonarr"},
		{128, "TARGET_99999999999999999999_NAME", "x", "sonarr-hd", "sonarr"},
		{128, "TARGETS_FOO", "x", "sonarr-hd", "sonarr"},
		{0, "URLL", "typo", "sonarr-hd", "sonarr"},
		{1, "PROWLARR__BACKFIL", "true", "prowl", "prowlarr"},
		{0, "SERIES_CONCURRENCY", "12", "sonarr-hd", "sonarr"},
		{0, "SCRAPE_TIMEOUT", "5s", "sonarr-hd", "sonarr"},
		{0, "FORM_AUTH", "true", "radarr", "radarr"},
		{0, "BAZARR__SERIES_BATCH_SIZE", "50", "bazarr", "bazarr"},
		{128, "MAX_UPSTREAM_REQUESTS", "64", "sonarr-hd", "sonarr"},
		{0, "PROWLARR__BACKFILL_SINCE_DATE", "2024-01-01", "sonarr-hd", "sonarr"},
		{0, "AUTH_PASSWORD", "pw", "sab", "sabnzbd"},
		{1, "NAME", "dup", "zqdupqz", "sonarr"},
		{4, "API_KEY", "k", "sonarr-hd", "sonarr"},
		{9, "NAME", "second", "first", "radarr"},
		{0, "API_KEY", "k", "Bad Name", "sonarr"},
		{0, "API_KEY", "k", "ok", "zqbadqz"},
		{0, "NAME", "", "orig", "nope"},
		{0, "zqsecretqz", "secret", "sonarr-hd", "sonarr"},
		{0, "API_KEY_FILE", "/etc/passwd", "sonarr-hd", "sonarr"},
		{128, "TARGET_0_API_KEY_FILE", "/etc/passwd", "sonarr-hd", "sonarr"},
		{0, "API_KEY_FILE=x", "/etc/passwd", "sonarr-hd", "sonarr"},
	}
	for _, s := range seeds {
		f.Add(s.idx, s.key, s.value, s.name, s.app)
	}
	f.Fuzz(func(t *testing.T, idx uint8, key, value, name, app string) {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return
		}
		// idx bits 0-1 pick the target index, bits 2-3 a small cap, bit 7 makes key the whole variable name.
		variable := fmt.Sprintf("TARGET_%d_%s", idx%4, key)
		if idx >= 128 {
			variable = key
		}
		if apiKeyFileVar.MatchString(variable) {
			return
		}
		wrapped := "zq" + value + "qz"
		environ := []string{"TARGET_0_NAME=" + name, "TARGET_0_APP=" + app, "TARGET_0_URL=http://127.0.0.1:1"}
		if c := idx >> 2 & 3; c > 0 {
			environ = append(environ, fmt.Sprintf("MAX_UPSTREAM_REQUESTS=%d", c))
		}
		environ = append(environ, variable+"="+wrapped)

		cfg, err := Load(environ)
		if cfg == nil {
			t.Fatal("Load returned a nil config")
		}
		if err != nil {
			echoAllowed := strings.Contains(key, wrapped) || strings.Contains(name, wrapped) ||
				(nameVar.MatchString(variable) && validName(wrapped))
			for _, leaf := range leafErrors(err) {
				if strings.Contains(leaf.Error(), wrapped) && !echoAllowed {
					t.Errorf("error echoes the value %q: %s", wrapped, leaf)
				}
			}
			return
		}
		seen := map[string]bool{}
		for _, tg := range cfg.Targets {
			if !validName(tg.Name) || seen[tg.Name] {
				t.Errorf("accepted invalid or duplicate name %q", tg.Name)
			}
			seen[tg.Name] = true
			if !slices.Contains(AppNames, tg.App) {
				t.Errorf("accepted invalid app %q", tg.App)
			}
			if tg.APIKeyFromFile != "" {
				t.Errorf("target %d kept APIKeyFromFile", tg.Index)
			}
		}
		if cfg.MaxUpstreamRequests < len(cfg.Targets)+1 {
			t.Errorf("accepted cap %d for %d targets", cfg.MaxUpstreamRequests, len(cfg.Targets))
		}
	})
}
