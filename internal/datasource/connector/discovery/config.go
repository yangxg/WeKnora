// Package discovery implements the web search discovery data source connector.
//
// It runs a set of saved queries against a search vendor, then fetches each hit's
// original page through an SSRF-safe HTTP client and emits it as a FetchedItem.
// The result is a stream of *candidates* for a project inbox — never governed
// content, and never the vendor's own copy of a page.
//
// Two rules shape everything here, and both come from ResearchFlow ADR-0009:
//
//   - The vendor is asked for hits, never for bodies. FetchedItem.Content may
//     only ever be the Markdown rendering of a first-party fetch of the original
//     URL. A vendor-side snapshot is not the document a citation can point at,
//     and a content hash taken over it would not match the page.
//   - The query is configuration, not telemetry. It never reaches a log line, a
//     sync log, or an error message, so a search that reveals what a project is
//     working on cannot leak through operational plumbing.
//
// Policy lives in a version-controlled project manifest that ResearchFlow
// materializes onto DataSourceConfig.Settings; the vendor key travels separately
// in Credentials. This package re-asserts the parts of that profile it depends
// on rather than trusting the row, because Settings is untyped and unvalidated
// by construction.
package discovery

import (
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/infrastructure/web_search/volcengine"
	"github.com/Tencent/WeKnora/internal/types"
)

// providerTypeVolcengine is the only vendor this connector speaks. A second one
// would need its own client and its own error taxonomy, so an unknown value is
// refused rather than defaulted.
const providerTypeVolcengine = "volcengine"

// endpointProfileAPIKey is the only implemented auth profile. The manifest also
// admits "iam", whose gateway authenticates with an AK/SK request signature; the
// client refuses it, and refusing here too moves the failure from the first
// scheduled search to the moment the row is validated.
const endpointProfileAPIKey = "api_key"

// SavedQuery is one query from the manifest. QueryID is the stable name a
// candidate's provenance is recorded under; Query is the text sent to the
// vendor, and is never logged.
type SavedQuery struct {
	QueryID string
	Query   string
}

// Config is the typed view of one materialized discovery manifest.
type Config struct {
	ProjectID    string
	SourceID     string
	ManifestHash string
	ProviderType string
	APIKey       string
	Queries      []SavedQuery

	// request profile
	count          int
	timeRange      string
	contentFormats []string
	industry       string
	queryRewrite   bool
	sites          []string
	blockHosts     []string
	authInfoLevel  []int
}

// SearchOptions renders the request profile for the vendor client.
//
// It goes through volcengine.DiscoveryProfile rather than building Options
// directly: that struct has no field for SearchType, NeedContent or NeedURL, so
// the three settings that must not drift are written as constants by code this
// package cannot reach past.
func (c *Config) SearchOptions() volcengine.Options {
	return volcengine.DiscoveryProfile{
		Count:          c.count,
		TimeRange:      c.timeRange,
		ContentFormats: c.contentFormats,
		Industry:       c.industry,
		QueryRewrite:   c.queryRewrite,
		Sites:          c.sites,
		BlockHosts:     c.blockHosts,
		AuthInfoLevel:  c.authInfoLevel,
	}.Options()
}

// parseConfig reads Settings and Credentials into a validated Config.
//
// Every error names the offending setting and nothing else. Parse failures are
// the most likely thing to reach an operator's screen and the sync log, so they
// are held to the same rule as the logs: no query text, no credential.
func parseConfig(config *types.DataSourceConfig) (*Config, error) {
	if config == nil {
		return nil, fmt.Errorf("%w: config is nil", datasource.ErrInvalidConfig)
	}
	settings := config.Settings
	if len(settings) == 0 {
		return nil, fmt.Errorf("%w: settings are empty", datasource.ErrInvalidConfig)
	}

	cfg := &Config{}
	var err error

	if cfg.ProjectID, err = requireString(settings, "project_id"); err != nil {
		return nil, err
	}
	if cfg.SourceID, err = requireString(settings, "source_id"); err != nil {
		return nil, err
	}
	if cfg.ManifestHash, err = requireString(settings, "manifest_hash"); err != nil {
		return nil, err
	}
	if cfg.ProviderType, err = requireString(settings, "provider_type"); err != nil {
		return nil, err
	}
	if cfg.ProviderType != providerTypeVolcengine {
		return nil, fmt.Errorf("%w: provider_type %q is not implemented",
			datasource.ErrInvalidConfig, cfg.ProviderType)
	}

	profile, err := requireString(settings, "endpoint_profile")
	if err != nil {
		return nil, err
	}
	if profile != endpointProfileAPIKey {
		return nil, fmt.Errorf(
			"%w: endpoint_profile %q needs AK/SK request signing, which is not implemented",
			datasource.ErrInvalidConfig, profile)
	}

	// The profile triple is re-asserted positively. Settings is untyped, so a
	// bool that came back as the string "false" would read as truthy in a
	// careless branch — and the one it would flip is need_content.
	searchType, err := requireString(settings, "search_type")
	if err != nil {
		return nil, err
	}
	if searchType != volcengine.SearchTypeWeb {
		return nil, fmt.Errorf("%w: search_type must be %q, got %q",
			datasource.ErrInvalidConfig, volcengine.SearchTypeWeb, searchType)
	}
	needContent, err := requireBool(settings, "need_content")
	if err != nil {
		return nil, err
	}
	if needContent {
		return nil, fmt.Errorf(
			"%w: need_content must be false; a vendor-side body is not the governed document",
			datasource.ErrInvalidConfig)
	}
	needURL, err := requireBool(settings, "need_url")
	if err != nil {
		return nil, err
	}
	if !needURL {
		return nil, fmt.Errorf(
			"%w: need_url must be true; without a landing page there is nothing to fetch",
			datasource.ErrInvalidConfig)
	}

	if cfg.count, err = requireInt(settings, "count"); err != nil {
		return nil, err
	}
	if cfg.count <= 0 || cfg.count > volcengine.MaxCount {
		return nil, fmt.Errorf("%w: count must be within 1..%d",
			datasource.ErrInvalidConfig, volcengine.MaxCount)
	}
	if cfg.timeRange, err = optionalString(settings, "time_range"); err != nil {
		return nil, err
	}
	if cfg.industry, err = optionalString(settings, "industry"); err != nil {
		return nil, err
	}
	if cfg.contentFormats, err = optionalStringSlice(settings, "content_formats"); err != nil {
		return nil, err
	}
	if cfg.sites, err = optionalStringSlice(settings, "sites"); err != nil {
		return nil, err
	}
	if cfg.blockHosts, err = optionalStringSlice(settings, "block_hosts"); err != nil {
		return nil, err
	}
	if cfg.authInfoLevel, err = optionalIntSlice(settings, "auth_info_level"); err != nil {
		return nil, err
	}
	if cfg.queryRewrite, err = optionalBool(settings, "query_rewrite"); err != nil {
		return nil, err
	}

	// Delegating range checks to the client keeps one definition of what the
	// vendor accepts, and means a bad policy fails here rather than as a
	// ParamError whose message quotes the query.
	if err := cfg.SearchOptions().Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", datasource.ErrInvalidConfig, err)
	}

	if cfg.Queries, err = parseQueries(settings); err != nil {
		return nil, err
	}

	if cfg.APIKey, err = credentialString(config.Credentials, "api_key"); err != nil {
		return nil, err
	}

	return cfg, nil
}

func parseQueries(settings map[string]interface{}) ([]SavedQuery, error) {
	raw, ok := settings["queries"]
	if !ok {
		return nil, fmt.Errorf("%w: queries is required", datasource.ErrInvalidConfig)
	}
	list, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("%w: queries must be a list", datasource.ErrInvalidConfig)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%w: queries must not be empty", datasource.ErrInvalidConfig)
	}

	seen := make(map[string]struct{}, len(list))
	out := make([]SavedQuery, 0, len(list))
	for index, entry := range list {
		table, ok := entry.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("%w: queries[%d] must be a table", datasource.ErrInvalidConfig, index)
		}
		queryID, err := requireString(table, "query_id")
		if err != nil {
			return nil, fmt.Errorf("%w: queries[%d]: %v", datasource.ErrInvalidConfig, index, err)
		}
		// The text is required but never echoed: this error names the id only.
		text, ok := table["query"].(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%w: queries[%d] (%s) has no query text",
				datasource.ErrInvalidConfig, index, queryID)
		}
		if _, duplicate := seen[queryID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate query_id %q", datasource.ErrInvalidConfig, queryID)
		}
		seen[queryID] = struct{}{}
		out = append(out, SavedQuery{QueryID: queryID, Query: strings.TrimSpace(text)})
	}
	return out, nil
}

func requireString(values map[string]interface{}, key string) (string, error) {
	raw, ok := values[key]
	if !ok {
		return "", fmt.Errorf("%w: %s is required", datasource.ErrInvalidConfig, key)
	}
	text, ok := raw.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%w: %s must be a non-empty string", datasource.ErrInvalidConfig, key)
	}
	return strings.TrimSpace(text), nil
}

func optionalString(values map[string]interface{}, key string) (string, error) {
	raw, ok := values[key]
	if !ok || raw == nil {
		return "", nil
	}
	text, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%w: %s must be a string", datasource.ErrInvalidConfig, key)
	}
	return strings.TrimSpace(text), nil
}

// requireBool refuses a stringified bool rather than coercing it.
func requireBool(values map[string]interface{}, key string) (bool, error) {
	raw, ok := values[key]
	if !ok {
		return false, fmt.Errorf("%w: %s is required", datasource.ErrInvalidConfig, key)
	}
	value, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("%w: %s must be a bool, not %T", datasource.ErrInvalidConfig, key, raw)
	}
	return value, nil
}

func optionalBool(values map[string]interface{}, key string) (bool, error) {
	if _, ok := values[key]; !ok {
		return false, nil
	}
	return requireBool(values, key)
}

func requireInt(values map[string]interface{}, key string) (int, error) {
	raw, ok := values[key]
	if !ok {
		return 0, fmt.Errorf("%w: %s is required", datasource.ErrInvalidConfig, key)
	}
	value, ok := asInt(raw)
	if !ok {
		return 0, fmt.Errorf("%w: %s must be an integer, not %T", datasource.ErrInvalidConfig, key, raw)
	}
	return value, nil
}

func optionalStringSlice(values map[string]interface{}, key string) ([]string, error) {
	raw, ok := values[key]
	if !ok || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("%w: %s must be a list of strings", datasource.ErrInvalidConfig, key)
	}
	out := make([]string, 0, len(list))
	for _, entry := range list {
		text, ok := entry.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%w: %s must contain non-empty strings", datasource.ErrInvalidConfig, key)
		}
		out = append(out, strings.TrimSpace(text))
	}
	return out, nil
}

func optionalIntSlice(values map[string]interface{}, key string) ([]int, error) {
	raw, ok := values[key]
	if !ok || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("%w: %s must be a list of integers", datasource.ErrInvalidConfig, key)
	}
	out := make([]int, 0, len(list))
	for _, entry := range list {
		value, ok := asInt(entry)
		if !ok {
			return nil, fmt.Errorf("%w: %s must contain integers, not %T",
				datasource.ErrInvalidConfig, key, entry)
		}
		out = append(out, value)
	}
	return out, nil
}

// asInt accepts the numeric shapes a JSON round trip can produce: an integer
// written by Go comes back as float64 through the database and as int from a
// hand-built fixture.
func asInt(raw interface{}) (int, bool) {
	switch value := raw.(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		if value != float64(int(value)) {
			return 0, false
		}
		return int(value), true
	default:
		return 0, false
	}
}

func credentialString(credentials map[string]interface{}, key string) (string, error) {
	raw, ok := credentials[key]
	if !ok {
		return "", fmt.Errorf("%w: %s is required", datasource.ErrInvalidCredentials, key)
	}
	text, ok := raw.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%w: %s must be a non-empty string", datasource.ErrInvalidCredentials, key)
	}
	return strings.TrimSpace(text), nil
}
