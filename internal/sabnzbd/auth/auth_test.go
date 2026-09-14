package auth

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/onedr0p/exportarr/internal/assert"
)

func TestAPIKeyAuth_Auth(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  url.Values
	}{
		{
			name: "no query",
			want: url.Values{"apikey": {"secret-key"}, "output": {"json"}},
		},
		{
			name:  "existing params are preserved",
			query: "mode=queue&limit=1",
			want: url.Values{
				"mode":   {"queue"},
				"limit":  {"1"},
				"apikey": {"secret-key"},
				"output": {"json"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://sabnzbd:8080/api?"+tc.query, nil)
			assert.NoError(t, err)

			assert.NoError(t, APIKeyAuth{APIKey: "secret-key"}.Auth(req))
			assert.DeepEqual(t, req.URL.Query(), tc.want)
			assert.Equal(t, req.URL.Path, "/api")
			assert.Equal(t, req.URL.Host, "sabnzbd:8080")
		})
	}
}
