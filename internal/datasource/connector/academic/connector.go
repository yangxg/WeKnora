package academic

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/datasource/connector/querycursor"
	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search"
	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search/arxiv"
	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search/crossref"
	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search/openalex"
	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search/pubmed"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	lane          = "AcademicDiscovery"
	searchTimeout = 30 * time.Second
	// toolName is the non-secret software identity Crossref asks to see in the
	// User-Agent. PubMed's registered tool name comes from the manifest instead.
	toolName = "ResearchFlow"
)

// Compile-time proof that *Connector satisfies the datasource contract.
var _ datasource.Connector = (*Connector)(nil)

// registrySearcher is the connector's entire capability surface. There is no
// pageFetcher, URL client or body endpoint: the only bytes this connector emits
// are the card it renders itself (ADR-0012 §6, ADR-0013 §2).
type registrySearcher interface {
	Search(ctx context.Context, query string, opts academic_search.Options) (*academic_search.Response, error)
}

// Connector implements datasource.Connector for academic identity discovery.
//
// The factory is a test seam, not per-source state: the registry holds one shared
// Connector and every method receives its DataSourceConfig.
type Connector struct {
	newSearcher func(*Config) (registrySearcher, error)
}

// NewConnector returns a connector wired to the four real registry clients.
// Constructing one makes no request; Validate deliberately stops before even
// constructing it so a settings-page visit cannot spend a metered search.
func NewConnector() *Connector {
	return &Connector{newSearcher: newRegistrySearcher}
}

func newRegistrySearcher(cfg *Config) (registrySearcher, error) {
	httpClient := datasource.NewConnectorHTTPClient(searchTimeout)
	switch cfg.ProviderType {
	case providerOpenAlex:
		return openalex.NewClient(openalex.Config{HTTPClient: httpClient, APIKey: cfg.APIKey})
	case providerCrossref:
		return crossref.NewClient(crossref.Config{
			HTTPClient: httpClient,
			Pool:       cfg.EndpointProfile,
			Mailto:     cfg.Contact["mailto"],
			ToolName:   toolName,
			PlusToken:  cfg.APIKey,
		})
	case providerPubMed:
		return pubmed.NewClient(pubmed.Config{
			HTTPClient: httpClient,
			APIKey:     cfg.APIKey,
			ToolName:   cfg.Contact["tool"],
			Email:      cfg.Contact["email"],
		})
	case providerArXiv:
		return arxiv.NewClient(arxiv.Config{HTTPClient: httpClient})
	default:
		return nil, fmt.Errorf("%w: provider_type %q is not implemented",
			datasource.ErrInvalidConfig, cfg.ProviderType)
	}
}

// Type returns the connector type ResearchFlow materializes.
func (c *Connector) Type() string { return types.ConnectorTypeAcademic }

// Validate checks materialized policy without calling a registry.
func (c *Connector) Validate(ctx context.Context, config *types.DataSourceConfig) error {
	cfg, err := parseConfig(config)
	if err != nil {
		return err
	}
	logger.Infof(ctx, "[AcademicDiscovery] validated source %s (%s) with %d saved queries",
		cfg.SourceID, cfg.ProviderType, len(cfg.Queries))
	return nil
}

// ListResources returns one flat picker resource per saved query. Only query ids
// appear; the text stays out of an HTTP response whose logging policy this
// connector does not control.
func (c *Connector) ListResources(
	ctx context.Context, config *types.DataSourceConfig, parentID string,
) ([]types.Resource, error) {
	if parentID != "" {
		return []types.Resource{}, nil
	}
	cfg, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	out := make([]types.Resource, 0, len(cfg.Queries))
	for _, query := range cfg.Queries {
		out = append(out, types.Resource{
			ExternalID: query.QueryID,
			Type:       "saved_query",
			Name:       query.QueryID,
		})
	}
	return out, nil
}

// ResolveResourceAncestors has nothing to do: saved queries are flat.
func (c *Connector) ResolveResourceAncestors(
	ctx context.Context, config *types.DataSourceConfig, resourceIDs []string,
) ([]string, error) {
	return []string{}, nil
}

// FetchAll runs every selected saved query and emits every bibliographic card.
func (c *Connector) FetchAll(
	ctx context.Context, config *types.DataSourceConfig, resourceIDs []string,
) ([]types.FetchedItem, error) {
	cfg, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	items, _, err := c.walk(ctx, cfg, resourceIDs, nil, false)
	return items, err
}

// FetchIncremental emits only cards whose identity metadata changed.
//
// A nil cursor after a total outage is load-bearing: the sync service persists
// any non-nil cursor even when fetch returns an error. Returning one after every
// query failed would turn an outage into "nothing new was published".
func (c *Connector) FetchIncremental(
	ctx context.Context, config *types.DataSourceConfig, cursor *types.SyncCursor,
) ([]types.FetchedItem, *types.SyncCursor, error) {
	cfg, err := parseConfig(config)
	if err != nil {
		return nil, nil, err
	}
	prev := querycursor.Decode(ctx, cursor, cfg.ManifestHash, lane)
	items, next, err := c.walk(ctx, cfg, config.ResourceIDs, prev, true)
	if next == nil {
		return items, nil, err
	}
	return items, next.ToSyncCursor(ctx, cfg.ManifestHash, lane), err
}

// walk is the common implementation and the only place candidates are emitted.
//
// It mirrors web discovery's three failure granularities through the shared
// querycursor package, but the per-candidate operation is rendering a card, not
// fetching a page.
func (c *Connector) walk(
	ctx context.Context,
	cfg *Config,
	resourceIDs []string,
	prev *querycursor.Cursor,
	incremental bool,
) ([]types.FetchedItem, *querycursor.Cursor, error) {
	selected, err := selectQueries(cfg, resourceIDs)
	if err != nil {
		return nil, nil, err
	}
	searcher, err := c.newSearcher(cfg)
	if err != nil {
		return nil, nil, err
	}

	next := querycursor.New()
	var out []types.FetchedItem
	var failures []string
	searched := 0

	// Deliberately serial: each client owns a provider-specific pacer and the
	// rates are deployment obligations, not throughput hints. Fan-out would make
	// the per-provider promise false as soon as two saved queries overlap.
	for _, query := range selected {
		response, err := searcher.Search(ctx, query.Query, cfg.Options)
		if err != nil {
			// Registry clients strip vendor messages because they can quote query
			// text. This layer logs query_id only and never wraps query.Query.
			logger.Warnf(ctx, "[AcademicDiscovery] search failed for query %s of source %s: %v",
				query.QueryID, cfg.SourceID, err)
			failures = append(failures, fmt.Sprintf("query %s: %v", query.QueryID, err))
			next.CarryQueryProgress(prev, query.QueryID)
			continue
		}
		searched++
		next.StartQuery(query.QueryID)
		var prior map[string]string
		if incremental {
			prior = querycursor.PriorProgress(prev, query.QueryID)
		}

		var emitted, skipped, dropped int
		for _, work := range response.Works {
			identity, reference, err := candidateIdentityAndReference(work)
			if err != nil {
				// Left unrecorded, so a transiently malformed registry record is
				// retried next run rather than forgotten forever.
				dropped++
				failures = append(failures,
					fmt.Sprintf("query %s: a registry record has no usable reference", query.QueryID))
				continue
			}
			card := renderCard(work, reference, cfg.ProviderType)
			externalID := candidateExternalID(query.QueryID, identity)
			fingerprint := querycursor.Fingerprint(card)
			next.Record(query.QueryID, externalID, fingerprint)
			if incremental && prior[externalID] == fingerprint {
				skipped++
				continue
			}
			out = append(out, candidateItem(cfg, query, work, card, identity, reference, externalID))
			emitted++
		}

		logger.Infof(ctx,
			"[AcademicDiscovery] query %s of source %s (%s): works=%d emitted=%d skipped=%d dropped=%d registry_dropped=%d",
			query.QueryID, cfg.SourceID, cfg.ProviderType, len(response.Works), emitted, skipped, dropped,
			response.Dropped)
	}

	if searched == 0 {
		return nil, nil, fmt.Errorf("%w: every saved query failed: %s",
			datasource.ErrFetchFailed, strings.Join(failures, "; "))
	}
	if len(failures) > 0 {
		return out, next, &datasource.PartialFetchError{Details: failures}
	}
	return out, next, nil
}

func selectQueries(cfg *Config, resourceIDs []string) ([]SavedQuery, error) {
	if len(resourceIDs) == 0 {
		return cfg.Queries, nil
	}
	byID := make(map[string]SavedQuery, len(cfg.Queries))
	for _, query := range cfg.Queries {
		byID[query.QueryID] = query
	}
	out := make([]SavedQuery, 0, len(resourceIDs))
	for _, id := range resourceIDs {
		query, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: %q is not a saved query of source %s",
				datasource.ErrResourceNotFound, id, cfg.SourceID)
		}
		out = append(out, query)
	}
	return out, nil
}

func candidateItem(
	cfg *Config,
	query SavedQuery,
	work academic_search.Work,
	card, identity, reference, externalID string,
) types.FetchedItem {
	title := oneLine(work.Title)
	if title == "" {
		title = "Untitled work"
	}
	return types.FetchedItem{
		ExternalID:       externalID,
		Title:            title,
		Content:          []byte(card),
		ContentType:      "text/markdown",
		FileName:         sanitizeFileName(title) + ".md",
		URL:              reference,
		UpdatedAt:        publicationTime(work.Year),
		IsDeleted:        false,
		SourceResourceID: query.QueryID,
		Metadata: map[string]string{
			"channel":                types.ChannelAcademic,
			"saved_query_id":         query.QueryID,
			"discovery_source_id":    cfg.SourceID,
			"discovery_project_id":   cfg.ProjectID,
			"discovery_provider":     cfg.ProviderType,
			"academic_identity":      identity,
			"academic_reference_url": reference,
		},
	}
}

func candidateExternalID(queryID, identity string) string {
	return queryID + ":" + identity
}

// publicationTime makes the registry-declared year representable in the
// FetchedItem without inventing a month or day later than necessary. It stops at
// the candidate layer; the human confirms published_at at promote time.
func publicationTime(year int) time.Time {
	if year < 1 || year > 9999 {
		return time.Time{}
	}
	return time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
}

// sanitizeFileName mirrors the other document connectors. The file is only the
// inbox card, but a legal stable name is still required by CreateKnowledgeFromFile.
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "untitled-work"
	}
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_",
		"<", "_", ">", "_", "|", "_", "\n", " ", "\r", " ", "\t", " ",
	)
	result := strings.TrimSpace(replacer.Replace(name))
	if result == "" {
		return "untitled-work"
	}
	const maxBytes = 200
	if len(result) > maxBytes {
		result = result[:maxBytes]
		for len(result) > 0 {
			r, size := utf8.DecodeLastRuneInString(result)
			if r != utf8.RuneError || size != 1 {
				break
			}
			result = result[:len(result)-1]
		}
	}
	return result
}
