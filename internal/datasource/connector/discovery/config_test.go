package discovery

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

// discoverySettings mirrors what ResearchFlow materializes onto Settings. It is
// the decoded JSON shape, not a Go struct, because that is what survives the DB
// round trip (pinned in internal/types/discovery_manifest_projection_test.go).
func discoverySettings() map[string]interface{} {
	return map[string]interface{}{
		"project_id":       "medical-policy",
		"source_id":        "src-medical-discovery",
		"manifest_hash":    strings.Repeat("a", 64),
		"provider_type":    "volcengine",
		"endpoint_profile": "api_key",
		"search_type":      "web",
		"need_content":     false,
		"need_url":         true,
		"count":            float64(20),
		"time_range":       "OneMonth",
		"content_formats":  []interface{}{"markdown"},
		"query_rewrite":    false,
		"auth_info_level":  []interface{}{float64(1), float64(2)},
		"industry":         "gov",
		"sites":            []interface{}{"www.gov.cn", "www.nhc.gov.cn"},
		"block_hosts":      []interface{}{"news.aggregator.example"},
		"queries": []interface{}{
			map[string]interface{}{"query_id": "drg-payment-reform", "query": secretQuery},
			map[string]interface{}{"query_id": "dip-pilot-cities", "query": "DIP 试点城市名单"},
		},
	}
}

// socialScienceSettings is the second isolated fixture. Discovery policy is
// per-project by construction, so both fixtures are exercised: a connector that
// leaked state across configs would pass with one.
func socialScienceSettings() map[string]interface{} {
	return map[string]interface{}{
		"project_id":       "social-science-funds",
		"source_id":        "src-funds-discovery",
		"manifest_hash":    strings.Repeat("b", 64),
		"provider_type":    "volcengine",
		"endpoint_profile": "api_key",
		"search_type":      "web",
		"need_content":     false,
		"need_url":         true,
		"count":            float64(5),
		"time_range":       "OneYear",
		"content_formats":  []interface{}{"text"},
		"query_rewrite":    true,
		"auth_info_level":  []interface{}{float64(1)},
		"sites":            []interface{}{"www.nsfc.gov.cn"},
		"queries": []interface{}{
			map[string]interface{}{"query_id": "nsfc-guidelines", "query": "国家社科基金申报指南"},
		},
	}
}

func discoveryConfig(settings map[string]interface{}) *types.DataSourceConfig {
	return &types.DataSourceConfig{
		Type:        types.ConnectorTypeDiscovery,
		Credentials: map[string]interface{}{"api_key": testAPIKey},
		Settings:    settings,
	}
}

const (
	testAPIKey  = "volc-discovery-key-do-not-log"
	secretQuery = "DRG-payment-reform-pilot-city-list"
)

func TestParseConfigReadsTheMaterializedManifest(t *testing.T) {
	cfg, err := parseConfig(discoveryConfig(discoverySettings()))
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}

	if cfg.ProjectID != "medical-policy" || cfg.SourceID != "src-medical-discovery" {
		t.Errorf("identity = %q/%q", cfg.ProjectID, cfg.SourceID)
	}
	if cfg.ManifestHash != strings.Repeat("a", 64) {
		t.Errorf("ManifestHash = %q", cfg.ManifestHash)
	}
	if cfg.APIKey != testAPIKey {
		t.Errorf("APIKey was not read from Credentials")
	}
	if len(cfg.Queries) != 2 {
		t.Fatalf("len(Queries) = %d, want 2", len(cfg.Queries))
	}
	if cfg.Queries[0].QueryID != "drg-payment-reform" || cfg.Queries[0].Query != secretQuery {
		t.Errorf("first query = %+v", cfg.Queries[0])
	}

	opts := cfg.SearchOptions()
	if opts.Count != 20 || opts.TimeRange != "OneMonth" || opts.Industry != "gov" {
		t.Errorf("request options = %+v", opts)
	}
	if len(opts.Sites) != 2 || opts.Sites[0] != "www.gov.cn" {
		t.Errorf("Sites = %v", opts.Sites)
	}
	if len(opts.BlockHosts) != 1 || opts.BlockHosts[0] != "news.aggregator.example" {
		t.Errorf("BlockHosts = %v", opts.BlockHosts)
	}
	if len(opts.AuthInfoLevel) != 2 || opts.AuthInfoLevel[0] != 1 {
		t.Errorf("AuthInfoLevel = %v", opts.AuthInfoLevel)
	}
	if len(opts.ContentFormats) != 1 || opts.ContentFormats[0] != "markdown" {
		t.Errorf("ContentFormats = %v", opts.ContentFormats)
	}
	if opts.QueryRewrite {
		t.Error("QueryRewrite = true, want the manifest value")
	}
	if err := opts.Validate(); err != nil {
		t.Errorf("materialized options rejected by the client: %v", err)
	}
}

// TestSearchOptionsCannotRequestVendorPageBodies is the load-bearing assertion of
// this file. The profile triple is fixed for discovery, so it is re-asserted
// positively rather than trusted from the row: a page body from the vendor is a
// second-hand snapshot, and only the SSRF-safe fetch of the original page may
// produce FetchedItem.Content.
func TestSearchOptionsCannotRequestVendorPageBodies(t *testing.T) {
	for name, settings := range map[string]map[string]interface{}{
		"medical_policy":       discoverySettings(),
		"social_science_funds": socialScienceSettings(),
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := parseConfig(discoveryConfig(settings))
			if err != nil {
				t.Fatalf("parseConfig() error = %v", err)
			}
			opts := cfg.SearchOptions()
			if opts.NeedContent {
				t.Error("NeedContent = true; discovery must never ask the vendor for a body")
			}
			if !opts.NeedURL {
				t.Error("NeedURL = false; without the landing page there is nothing to fetch")
			}
			if opts.SearchType != "web" {
				t.Errorf("SearchType = %q, want web", opts.SearchType)
			}
		})
	}
}

func TestParseConfigRefusesAProfileThatWouldWidenTheRequest(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]interface{})
		want   string
	}{
		{
			name:   "vendor body requested",
			mutate: func(s map[string]interface{}) { s["need_content"] = true },
			want:   "need_content",
		},
		{
			name:   "vendor body requested as a string",
			mutate: func(s map[string]interface{}) { s["need_content"] = "false" },
			want:   "need_content",
		},
		{
			name:   "url suppressed",
			mutate: func(s map[string]interface{}) { s["need_url"] = false },
			want:   "need_url",
		},
		{
			name:   "summary edition",
			mutate: func(s map[string]interface{}) { s["search_type"] = "web_summary" },
			want:   "search_type",
		},
		{
			name:   "image edition",
			mutate: func(s map[string]interface{}) { s["search_type"] = "image" },
			want:   "search_type",
		},
		{
			name:   "iam endpoint profile",
			mutate: func(s map[string]interface{}) { s["endpoint_profile"] = "iam" },
			want:   "endpoint_profile",
		},
		{
			name:   "unknown provider",
			mutate: func(s map[string]interface{}) { s["provider_type"] = "bing" },
			want:   "provider_type",
		},
		{
			name:   "out of range count",
			mutate: func(s map[string]interface{}) { s["count"] = float64(500) },
			want:   "count",
		},
		{
			name:   "unsupported time range",
			mutate: func(s map[string]interface{}) { s["time_range"] = "OneDecade" },
			want:   "time range",
		},
		{
			name:   "unsupported industry",
			mutate: func(s map[string]interface{}) { s["industry"] = "medicine" },
			want:   "industry",
		},
		{
			name:   "unsupported authority tier",
			mutate: func(s map[string]interface{}) { s["auth_info_level"] = []interface{}{float64(9)} },
			want:   "auth info level",
		},
		{
			name:   "no saved queries",
			mutate: func(s map[string]interface{}) { s["queries"] = []interface{}{} },
			want:   "queries",
		},
		{
			name: "saved query without an id",
			mutate: func(s map[string]interface{}) {
				s["queries"] = []interface{}{map[string]interface{}{"query": "x"}}
			},
			want: "query_id",
		},
		{
			name: "saved query without text",
			mutate: func(s map[string]interface{}) {
				s["queries"] = []interface{}{map[string]interface{}{"query_id": "q1"}}
			},
			want: "query",
		},
		{
			name: "duplicate query ids",
			mutate: func(s map[string]interface{}) {
				s["queries"] = []interface{}{
					map[string]interface{}{"query_id": "q1", "query": "a"},
					map[string]interface{}{"query_id": "q1", "query": "b"},
				}
			},
			want: "duplicate",
		},
		{
			name:   "missing manifest hash",
			mutate: func(s map[string]interface{}) { delete(s, "manifest_hash") },
			want:   "manifest_hash",
		},
		{
			name:   "missing project identity",
			mutate: func(s map[string]interface{}) { delete(s, "project_id") },
			want:   "project_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := discoverySettings()
			tt.mutate(settings)
			_, err := parseConfig(discoveryConfig(settings))
			if err == nil {
				t.Fatalf("parseConfig() accepted %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to name %q", err, tt.want)
			}
		})
	}
}

func TestParseConfigRequiresTheVendorCredential(t *testing.T) {
	cfg := discoveryConfig(discoverySettings())
	cfg.Credentials = nil
	if _, err := parseConfig(cfg); err == nil {
		t.Fatal("parseConfig() accepted a config without a key")
	}

	cfg.Credentials = map[string]interface{}{"api_key": "   "}
	if _, err := parseConfig(cfg); err == nil {
		t.Fatal("parseConfig() accepted a blank key")
	}

	if _, err := parseConfig(nil); err == nil {
		t.Fatal("parseConfig(nil) did not fail")
	}
}

// TestParseConfigErrorsNeverQuoteTheQueryOrTheKey pins that a misconfigured
// manifest cannot be diagnosed at the cost of leaking what was searched for.
// Parse errors are the most likely thing to reach an operator's screen and the
// sync log, so they are held to the same rule as the logs.
func TestParseConfigErrorsNeverQuoteTheQueryOrTheKey(t *testing.T) {
	settings := discoverySettings()
	settings["count"] = float64(500)
	settings["queries"] = []interface{}{
		map[string]interface{}{"query_id": "drg-payment-reform", "query": secretQuery},
	}

	_, err := parseConfig(discoveryConfig(settings))
	if err == nil {
		t.Fatal("expected an error to inspect")
	}
	for _, banned := range []string{secretQuery, testAPIKey} {
		if strings.Contains(err.Error(), banned) {
			t.Errorf("error leaks %q: %v", banned, err)
		}
	}
}

// TestParseConfigKeepsProjectsApart guards the one thing a shared connector
// instance could get wrong: two data sources are two configs, and nothing may
// carry over between them.
func TestParseConfigKeepsProjectsApart(t *testing.T) {
	first, err := parseConfig(discoveryConfig(discoverySettings()))
	if err != nil {
		t.Fatalf("parseConfig(medical) error = %v", err)
	}
	second, err := parseConfig(discoveryConfig(socialScienceSettings()))
	if err != nil {
		t.Fatalf("parseConfig(funds) error = %v", err)
	}

	if first.ProjectID == second.ProjectID || first.ManifestHash == second.ManifestHash {
		t.Fatal("the two fixtures are not actually distinct")
	}
	if first.SearchOptions().Count == second.SearchOptions().Count {
		t.Error("request options did not come from their own config")
	}
	for _, q := range second.Queries {
		if q.Query == secretQuery {
			t.Error("a query crossed from one project config into the other")
		}
	}
}
