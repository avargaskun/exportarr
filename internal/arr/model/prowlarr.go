package model

import "encoding/json"

// Indexer is the response from prowlarr's indexer endpoint.
type Indexer []struct {
	Name     string        `json:"name"`
	SortName string        `json:"sortName"`
	Enabled  bool          `json:"enable"`
	Fields   IndexerFields `json:"fields"`
}

// IndexerFields keeps only the indexer settings the exporter reads. The other
// settings include tracker credentials, so they are never decoded or kept.
type IndexerFields struct {
	VipExpiration string
}

// UnmarshalJSON implements json.Unmarshaler.
func (f *IndexerFields) UnmarshalJSON(b []byte) error {
	var fields []struct {
		Name  string          `json:"name"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	for _, field := range fields {
		if field.Name == "vipExpiration" {
			// A non-string value means no expiration, as before.
			_ = json.Unmarshal(field.Value, &f.VipExpiration)
		}
	}
	return nil
}

// IndexerStats holds per-indexer query/grab counters.
type IndexerStats struct {
	Name                      string `json:"indexerName"`
	AverageResponseTime       int    `json:"averageResponseTime"`
	NumberOfQueries           int    `json:"numberOfQueries"`
	NumberOfGrabs             int    `json:"numberOfGrabs"`
	NumberOfRssQueries        int    `json:"numberOfRssQueries"`
	NumberOfAuthQueries       int    `json:"numberOfAuthQueries"`
	NumberOfFailedQueries     int    `json:"numberOfFailedQueries"`
	NumberOfFailedGrabs       int    `json:"numberOfFailedGrabs"`
	NumberOfFailedRssQueries  int    `json:"numberOfFailedRssQueries"`
	NumberOfFailedAuthQueries int    `json:"numberOfFailedAuthQueries"`
}

// UserAgentStats holds per-user-agent query/grab counters.
type UserAgentStats struct {
	UserAgent       string `json:"userAgent"`
	NumberOfQueries int    `json:"numberOfQueries"`
	NumberOfGrabs   int    `json:"numberOfGrabs"`
}

// IndexerStatResponse is the response from prowlarr's indexerstats endpoint.
type IndexerStatResponse struct {
	Indexers   []IndexerStats   `json:"indexers"`
	UserAgents []UserAgentStats `json:"userAgents"`
}
