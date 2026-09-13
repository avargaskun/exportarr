package model

import (
	"encoding/json"
	"testing"

	"github.com/onedr0p/exportarr/internal/assert"
)

func TestIndexer_DecodesOnlyVipExpiration(t *testing.T) {
	body := `[
		{"name":"vip","enable":true,"fields":[
			{"name":"username","value":"alice"},
			{"name":"password","value":"hunter2"},
			{"name":"cookie","value":{"session":"hunter2"}},
			{"name":"vipExpiration","value":"2030-01-02"}
		]},
		{"name":"free","enable":false,"fields":[{"name":"vipExpiration","value":null}]},
		{"name":"odd","enable":true,"fields":[{"name":"vipExpiration","value":42}]}
	]`
	var indexers Indexer
	assert.NoError(t, json.Unmarshal([]byte(body), &indexers))
	assert.Len(t, indexers, 3)
	assert.DeepEqual(t, indexers[0].Fields, IndexerFields{VipExpiration: "2030-01-02"})
	assert.DeepEqual(t, indexers[1].Fields, IndexerFields{})
	assert.DeepEqual(t, indexers[2].Fields, IndexerFields{})
}
