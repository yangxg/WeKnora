package academic

import (
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	manifestHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	secretKey    = "SECRET-REGISTRY-KEY-DO-NOT-LOG"
)

func baseSettings(provider, profile string) map[string]interface{} {
	return map[string]interface{}{
		"project_id":       "medical-policy",
		"source_id":        "src-medical-academic",
		"manifest_hash":    manifestHash,
		"provider_type":    provider,
		"endpoint_profile": profile,
		"identity_only":    true,
		"count":            25,
		"date_range":       "2024-01-01..2026-08-01",
		"work_types":       []interface{}{"journal-article", "review"},
		"open_access":      "any",
		"contact":          map[string]interface{}{},
		"queries": []interface{}{
			map[string]interface{}{"query_id": "drg-payment-reform", "query": "DRG payment reform"},
			map[string]interface{}{"query_id": "payment-methods", "query": "provider payment methods"},
		},
	}
}

func dataSourceConfig(settings map[string]interface{}, credentials map[string]interface{}) *types.DataSourceConfig {
	return &types.DataSourceConfig{
		Type:        types.ConnectorTypeAcademic,
		Settings:    settings,
		Credentials: credentials,
	}
}

func TestParseConfigAcceptsTheFourMaterializedProviderShapes(t *testing.T) {
	cases := []struct {
		name        string
		provider    string
		profile     string
		contact     map[string]interface{}
		credentials map[string]interface{}
	}{
		{"OpenAlex", providerOpenAlex, profileAPIKey, map[string]interface{}{}, map[string]interface{}{"api_key": secretKey}},
		{"Crossref polite", providerCrossref, profilePolite, map[string]interface{}{"mailto": "research-flow@example.org"}, nil},
		{"Crossref Plus", providerCrossref, profilePlus, map[string]interface{}{"mailto": "research-flow@example.org"}, map[string]interface{}{"api_key": secretKey}},
		{"PubMed", providerPubMed, profileAPIKey, map[string]interface{}{"tool": "ResearchFlow", "email": "research-flow@example.org"}, map[string]interface{}{"api_key": secretKey}},
		{"arXiv", providerArXiv, profileAnonymous, map[string]interface{}{}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := baseSettings(tc.provider, tc.profile)
			settings["contact"] = tc.contact
			cfg, err := parseConfig(dataSourceConfig(settings, tc.credentials))
			if err != nil {
				t.Fatalf("parseConfig() error = %v", err)
			}
			if cfg.ProviderType != tc.provider || cfg.EndpointProfile != tc.profile {
				t.Errorf("provider/profile = %q/%q", cfg.ProviderType, cfg.EndpointProfile)
			}
			if cfg.APIKey != strings.TrimSpace(stringValue(tc.credentials, "api_key")) {
				t.Errorf("APIKey presence does not match materialized credentials")
			}
			if len(cfg.Queries) != 2 {
				t.Errorf("Queries = %v", cfg.Queries)
			}
			wantFrom := "2024-01-01"
			wantTo := "2026-08-01"
			if cfg.Options.From.Format("2006-01-02") != wantFrom || cfg.Options.To.Format("2006-01-02") != wantTo {
				t.Errorf("date window = %s..%s", cfg.Options.From.Format("2006-01-02"), cfg.Options.To.Format("2006-01-02"))
			}
		})
	}
}

func stringValue(values map[string]interface{}, key string) string {
	if value, ok := values[key].(string); ok {
		return value
	}
	return ""
}

// identity_only is not advisory: false or a stringified bool would let the
// connector be used for a row whose body rule it cannot know. Both are refused.
func TestParseConfigRequiresIdentityOnlyAsTheBoolTrue(t *testing.T) {
	for _, value := range []interface{}{false, "true", "false", 1, nil} {
		settings := baseSettings(providerArXiv, profileAnonymous)
		settings["identity_only"] = value
		_, err := parseConfig(dataSourceConfig(settings, nil))
		if err == nil || !errors.Is(err, datasource.ErrInvalidConfig) {
			t.Errorf("identity_only=%#v gave %v, want ErrInvalidConfig", value, err)
		}
	}
}

// The materialized profile is exact. A body/web key is not ignored because
// ignoring it would let a widened manifest look accepted while the connector
// silently runs a different policy.
func TestParseConfigRejectsEveryBodyOrWebLaneSetting(t *testing.T) {
	for _, key := range []string{
		"need_content", "need_url", "search_type", "content_formats",
		"summary", "abstract", "fulltext", "industry", "sites", "block_hosts",
	} {
		t.Run(key, func(t *testing.T) {
			settings := baseSettings(providerArXiv, profileAnonymous)
			settings[key] = false
			_, err := parseConfig(dataSourceConfig(settings, nil))
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("setting %q gave %v, want an error naming the unknown key", key, err)
			}
		})
	}
}

func TestParseConfigEnforcesCredentialAndContactShapesInBothDirections(t *testing.T) {
	cases := []struct {
		name        string
		provider    string
		profile     string
		contact     map[string]interface{}
		credentials map[string]interface{}
	}{
		{"OpenAlex without key", providerOpenAlex, profileAPIKey, map[string]interface{}{}, nil},
		{"OpenAlex with contact", providerOpenAlex, profileAPIKey, map[string]interface{}{"mailto": "x@example.org"}, map[string]interface{}{"api_key": secretKey}},
		{"Crossref polite with key", providerCrossref, profilePolite, map[string]interface{}{"mailto": "x@example.org"}, map[string]interface{}{"api_key": secretKey}},
		{"Crossref polite without contact", providerCrossref, profilePolite, map[string]interface{}{}, nil},
		{"Crossref plus without key", providerCrossref, profilePlus, map[string]interface{}{"mailto": "x@example.org"}, nil},
		{"PubMed missing tool", providerPubMed, profileAPIKey, map[string]interface{}{"email": "x@example.org"}, map[string]interface{}{"api_key": secretKey}},
		{"PubMed extra contact", providerPubMed, profileAPIKey, map[string]interface{}{"tool": "RF", "email": "x@example.org", "mailto": "x@example.org"}, map[string]interface{}{"api_key": secretKey}},
		{"arXiv with key", providerArXiv, profileAnonymous, map[string]interface{}{}, map[string]interface{}{"api_key": secretKey}},
		{"arXiv with contact", providerArXiv, profileAnonymous, map[string]interface{}{"email": "x@example.org"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := baseSettings(tc.provider, tc.profile)
			settings["contact"] = tc.contact
			_, err := parseConfig(dataSourceConfig(settings, tc.credentials))
			if err == nil {
				t.Fatal("parseConfig() accepted the invalid auth shape")
			}
			if strings.Contains(err.Error(), secretKey) {
				t.Errorf("error leaks the key: %v", err)
			}
		})
	}
}

func TestParseConfigRejectsUnknownProviderOrProfile(t *testing.T) {
	cases := []struct{ provider, profile string }{
		{"semantic-scholar", profileAPIKey},
		{providerOpenAlex, profileAnonymous},
		{providerCrossref, "public"},
		{providerPubMed, profilePolite},
		{providerArXiv, profileAPIKey},
	}
	for _, tc := range cases {
		settings := baseSettings(tc.provider, tc.profile)
		_, err := parseConfig(dataSourceConfig(settings, nil))
		if err == nil {
			t.Errorf("provider/profile %q/%q was accepted", tc.provider, tc.profile)
		}
	}
}

func TestParseConfigMapsTheRequestProfileExactly(t *testing.T) {
	settings := baseSettings(providerOpenAlex, profileAPIKey)
	settings["count"] = float64(7) // the shape a JSON round trip produces
	settings["work_types"] = []interface{}{"preprint", "dataset"}
	settings["open_access"] = academic_search.OpenAccessOnly

	cfg, err := parseConfig(dataSourceConfig(settings, map[string]interface{}{"api_key": secretKey}))
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	if cfg.Options.Count != 7 || cfg.Options.OpenAccess != academic_search.OpenAccessOnly {
		t.Errorf("Options = %+v", cfg.Options)
	}
	if len(cfg.Options.WorkTypes) != 2 || cfg.Options.WorkTypes[0] != "preprint" || cfg.Options.WorkTypes[1] != "dataset" {
		t.Errorf("WorkTypes = %v", cfg.Options.WorkTypes)
	}
}

func TestParseConfigRejectsBadPolicyBeforeARegistryCall(t *testing.T) {
	mutations := []func(map[string]interface{}){
		func(s map[string]interface{}) { s["count"] = 51 },
		func(s map[string]interface{}) { s["count"] = 1.5 },
		func(s map[string]interface{}) { s["date_range"] = "2026-01-01..2024-01-01" },
		func(s map[string]interface{}) { s["date_range"] = "not-a-window" },
		func(s map[string]interface{}) { s["work_types"] = []interface{}{"blog-post"} },
		func(s map[string]interface{}) { s["open_access"] = "sometimes" },
		func(s map[string]interface{}) { s["queries"] = []interface{}{} },
		func(s map[string]interface{}) {
			s["queries"] = []interface{}{
				map[string]interface{}{"query_id": "q", "query": "x"},
				map[string]interface{}{"query_id": "q", "query": "y"},
			}
		},
	}
	for index, mutate := range mutations {
		settings := baseSettings(providerOpenAlex, profileAPIKey)
		mutate(settings)
		_, err := parseConfig(dataSourceConfig(settings, map[string]interface{}{"api_key": secretKey}))
		if err == nil {
			t.Errorf("mutation %d was accepted", index)
		}
	}
}

func TestParseConfigErrorsNeverContainQueryOrKey(t *testing.T) {
	settings := baseSettings(providerOpenAlex, profileAPIKey)
	settings["queries"] = []interface{}{
		map[string]interface{}{"query_id": "secret-query", "query": "SECRET-RESEARCH-TOPIC"},
		map[string]interface{}{"query_id": "secret-query", "query": "SECRET-RESEARCH-TOPIC-2"},
	}
	_, err := parseConfig(dataSourceConfig(settings, map[string]interface{}{"api_key": secretKey}))
	if err == nil {
		t.Fatal("duplicate query_id was accepted")
	}
	for _, banned := range []string{"SECRET-RESEARCH-TOPIC", secretKey} {
		if strings.Contains(err.Error(), banned) {
			t.Errorf("error leaks %q: %v", banned, err)
		}
	}
}
