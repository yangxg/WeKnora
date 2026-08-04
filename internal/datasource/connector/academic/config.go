// Package academic implements the identity-only academic discovery connector.
//
// It is a sibling of connector/discovery, not a mode of it. The web connector
// turns a search hit into the original page; this one turns a registry record
// into a bibliographic card and has no page-fetching seam at all. That separation
// keeps the body rule structural: there is no branch in this package that could
// decide to fetch a work's text (ResearchFlow ADR-0012 §2/§6, ADR-0013 §2).
//
// Policy lives in ResearchFlow's version-controlled manifest and is materialized
// onto DataSourceConfig.Settings. Secrets travel separately in Credentials. This
// package re-asserts both halves because Settings is untyped and Credentials can
// be edited through a different endpoint: a row that silently falls to an
// unauthenticated tier looks like "nothing was published", not like a bad config.
package academic

import (
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	providerOpenAlex = "openalex"
	providerCrossref = "crossref"
	providerPubMed   = "pubmed"
	providerArXiv    = "arxiv"

	profileAPIKey    = "api_key"
	profilePolite    = "polite"
	profilePlus      = "plus"
	profileAnonymous = "anonymous"
)

var allowedSettings = map[string]struct{}{
	"project_id":       {},
	"source_id":        {},
	"manifest_hash":    {},
	"provider_type":    {},
	"endpoint_profile": {},
	"identity_only":    {},
	"count":            {},
	"date_range":       {},
	"work_types":       {},
	"open_access":      {},
	"contact":          {},
	"queries":          {},
}

// SavedQuery is one query from the manifest. QueryID is the stable name carried
// into provenance; Query is the text sent to the registry and is never logged.
type SavedQuery struct {
	QueryID string
	Query   string
}

// Config is the typed view of one materialized academic manifest.
type Config struct {
	ProjectID       string
	SourceID        string
	ManifestHash    string
	ProviderType    string
	EndpointProfile string
	APIKey          string
	Contact         map[string]string
	Options         academic_search.Options
	Queries         []SavedQuery
}

// parseConfig reads Settings and Credentials into a validated Config.
//
// Every error names the setting, query id or provider and nothing else. Parse
// failures reach the sync log, so neither the saved query text nor a key may be
// embedded in one.
func parseConfig(config *types.DataSourceConfig) (*Config, error) {
	if config == nil {
		return nil, fmt.Errorf("%w: config is nil", datasource.ErrInvalidConfig)
	}
	if config.Type != "" && config.Type != types.ConnectorTypeAcademic {
		return nil, fmt.Errorf("%w: connector type must be %q", datasource.ErrInvalidConfig, types.ConnectorTypeAcademic)
	}
	settings := config.Settings
	if len(settings) == 0 {
		return nil, fmt.Errorf("%w: settings are empty", datasource.ErrInvalidConfig)
	}
	for key := range settings {
		if _, ok := allowedSettings[key]; !ok {
			return nil, fmt.Errorf("%w: setting %q is not valid for the academic lane",
				datasource.ErrInvalidConfig, key)
		}
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
	if cfg.EndpointProfile, err = requireString(settings, "endpoint_profile"); err != nil {
		return nil, err
	}

	identityOnly, err := requireBool(settings, "identity_only")
	if err != nil {
		return nil, err
	}
	if !identityOnly {
		return nil, fmt.Errorf("%w: identity_only must be true for the academic lane",
			datasource.ErrInvalidConfig)
	}

	count, err := requireInt(settings, "count")
	if err != nil {
		return nil, err
	}
	from, to, err := parseDateRange(settings)
	if err != nil {
		return nil, err
	}
	workTypes, err := requireStringSlice(settings, "work_types")
	if err != nil {
		return nil, err
	}
	openAccess, err := requireString(settings, "open_access")
	if err != nil {
		return nil, err
	}
	cfg.Options = academic_search.Options{
		Count: count, From: from, To: to, WorkTypes: workTypes, OpenAccess: openAccess,
	}
	if err := cfg.Options.Validate(); err != nil {
		return nil, fmt.Errorf("%w: request profile: %v", datasource.ErrInvalidConfig, err)
	}

	if cfg.Contact, err = parseContact(settings); err != nil {
		return nil, err
	}
	if cfg.Queries, err = parseQueries(settings); err != nil {
		return nil, err
	}
	if err := cfg.validateProvider(config.Credentials); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validateProvider(credentials map[string]interface{}) error {
	expectedContact := map[string]struct{}{}
	needsKey := false
	switch c.ProviderType {
	case providerOpenAlex:
		if c.EndpointProfile != profileAPIKey {
			return invalidProfile(c.ProviderType, c.EndpointProfile, profileAPIKey)
		}
		needsKey = true
	case providerCrossref:
		expectedContact["mailto"] = struct{}{}
		switch c.EndpointProfile {
		case profilePolite:
		case profilePlus:
			needsKey = true
		default:
			return invalidProfile(c.ProviderType, c.EndpointProfile, profilePolite+" or "+profilePlus)
		}
	case providerPubMed:
		if c.EndpointProfile != profileAPIKey {
			return invalidProfile(c.ProviderType, c.EndpointProfile, profileAPIKey)
		}
		expectedContact["tool"] = struct{}{}
		expectedContact["email"] = struct{}{}
		needsKey = true
	case providerArXiv:
		if c.EndpointProfile != profileAnonymous {
			return invalidProfile(c.ProviderType, c.EndpointProfile, profileAnonymous)
		}
	default:
		return fmt.Errorf("%w: provider_type %q is not implemented",
			datasource.ErrInvalidConfig, c.ProviderType)
	}

	if err := exactContactKeys(c.Contact, expectedContact, c.ProviderType); err != nil {
		return err
	}
	if needsKey {
		key, err := exactAPIKey(credentials)
		if err != nil {
			return err
		}
		c.APIKey = key
		return nil
	}
	if len(credentials) != 0 {
		return fmt.Errorf("%w: %s/%s must not carry credentials",
			datasource.ErrInvalidCredentials, c.ProviderType, c.EndpointProfile)
	}
	return nil
}

func invalidProfile(provider, got, want string) error {
	return fmt.Errorf("%w: endpoint_profile %q is invalid for %s; want %s",
		datasource.ErrInvalidConfig, got, provider, want)
}

func exactAPIKey(credentials map[string]interface{}) (string, error) {
	if len(credentials) != 1 {
		return "", fmt.Errorf("%w: exactly one api_key is required", datasource.ErrInvalidCredentials)
	}
	raw, ok := credentials["api_key"]
	if !ok {
		return "", fmt.Errorf("%w: api_key is required", datasource.ErrInvalidCredentials)
	}
	key, ok := raw.(string)
	if !ok || strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("%w: api_key must be a non-empty string", datasource.ErrInvalidCredentials)
	}
	return strings.TrimSpace(key), nil
}

func exactContactKeys(contact map[string]string, expected map[string]struct{}, provider string) error {
	if len(contact) != len(expected) {
		return fmt.Errorf("%w: contact keys do not match provider %s", datasource.ErrInvalidConfig, provider)
	}
	for key := range expected {
		if strings.TrimSpace(contact[key]) == "" {
			return fmt.Errorf("%w: contact.%s is required for %s",
				datasource.ErrInvalidConfig, key, provider)
		}
	}
	for key := range contact {
		if _, ok := expected[key]; !ok {
			return fmt.Errorf("%w: contact.%s is not valid for %s",
				datasource.ErrInvalidConfig, key, provider)
		}
	}
	return nil
}

func parseContact(settings map[string]interface{}) (map[string]string, error) {
	raw, ok := settings["contact"]
	if !ok {
		return nil, fmt.Errorf("%w: contact is required", datasource.ErrInvalidConfig)
	}
	table, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%w: contact must be a table", datasource.ErrInvalidConfig)
	}
	out := make(map[string]string, len(table))
	for key, value := range table {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%w: contact.%s must be a non-empty string",
				datasource.ErrInvalidConfig, key)
		}
		out[key] = strings.TrimSpace(text)
	}
	return out, nil
}

func parseDateRange(settings map[string]interface{}) (time.Time, time.Time, error) {
	value, err := requireString(settings, "date_range")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	fromRaw, toRaw, ok := strings.Cut(value, "..")
	if !ok || fromRaw == "" || toRaw == "" {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"%w: date_range must be YYYY-MM-DD..YYYY-MM-DD", datasource.ErrInvalidConfig)
	}
	from, fromErr := time.Parse("2006-01-02", fromRaw)
	to, toErr := time.Parse("2006-01-02", toRaw)
	if fromErr != nil || toErr != nil {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"%w: date_range must contain real calendar dates", datasource.ErrInvalidConfig)
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"%w: date_range ends before it starts", datasource.ErrInvalidConfig)
	}
	return from, to, nil
}

func parseQueries(settings map[string]interface{}) ([]SavedQuery, error) {
	raw, ok := settings["queries"]
	if !ok {
		return nil, fmt.Errorf("%w: queries is required", datasource.ErrInvalidConfig)
	}
	list, ok := raw.([]interface{})
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("%w: queries must be a non-empty list", datasource.ErrInvalidConfig)
	}
	seen := make(map[string]struct{}, len(list))
	out := make([]SavedQuery, 0, len(list))
	for index, entry := range list {
		table, ok := entry.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("%w: queries[%d] must be a table", datasource.ErrInvalidConfig, index)
		}
		if len(table) != 2 {
			return nil, fmt.Errorf("%w: queries[%d] must contain only query_id and query",
				datasource.ErrInvalidConfig, index)
		}
		queryID, err := requireString(table, "query_id")
		if err != nil {
			return nil, fmt.Errorf("%w: queries[%d]: %v", datasource.ErrInvalidConfig, index, err)
		}
		query, ok := table["query"].(string)
		if !ok || strings.TrimSpace(query) == "" {
			return nil, fmt.Errorf("%w: queries[%d] (%s) has no query text",
				datasource.ErrInvalidConfig, index, queryID)
		}
		if _, duplicate := seen[queryID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate query_id %q", datasource.ErrInvalidConfig, queryID)
		}
		seen[queryID] = struct{}{}
		out = append(out, SavedQuery{QueryID: queryID, Query: strings.TrimSpace(query)})
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

func requireInt(values map[string]interface{}, key string) (int, error) {
	raw, ok := values[key]
	if !ok {
		return 0, fmt.Errorf("%w: %s is required", datasource.ErrInvalidConfig, key)
	}
	switch value := raw.(type) {
	case int:
		return value, nil
	case int64:
		return int(value), nil
	case float64:
		if value == float64(int(value)) {
			return int(value), nil
		}
	}
	return 0, fmt.Errorf("%w: %s must be an integer, not %T", datasource.ErrInvalidConfig, key, raw)
}

func requireStringSlice(values map[string]interface{}, key string) ([]string, error) {
	raw, ok := values[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s is required", datasource.ErrInvalidConfig, key)
	}
	list, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("%w: %s must be a list of strings", datasource.ErrInvalidConfig, key)
	}
	out := make([]string, 0, len(list))
	for _, entry := range list {
		text, ok := entry.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%w: %s must contain non-empty strings",
				datasource.ErrInvalidConfig, key)
		}
		out = append(out, strings.TrimSpace(text))
	}
	return out, nil
}
