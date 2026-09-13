package config

import (
	"testing"

	"github.com/onedr0p/exportarr/internal/assert"
)

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
