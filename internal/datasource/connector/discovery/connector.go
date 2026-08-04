package discovery

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/datasource/connector/querycursor"
	"github.com/Tencent/WeKnora/internal/infrastructure/web_search/volcengine"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// lane names this connector in the shared cursor's log lines. The academic
// connector passes its own.
const lane = "Discovery"

// Compile-time proof that *Connector satisfies the datasource.Connector interface.
var _ datasource.Connector = (*Connector)(nil)

// searchTimeout bounds one vendor call. It is shorter than the page timeout: the
// search API answers from an index, while a landing page is an arbitrary site.
const searchTimeout = 15 * time.Second

// vendorSearcher is the search half of the connector's world.
type vendorSearcher interface {
	Search(ctx context.Context, query string, opts volcengine.Options) (*volcengine.Response, error)
}

// pageFetcher is the original-page half. Keeping the two seams separate is what
// makes it testable that a body never comes from the search side.
type pageFetcher interface {
	Fetch(ctx context.Context, rawURL string) (page, error)
}

// Connector implements datasource.Connector for web search discovery.
//
// The two constructor fields are seams, not configuration: the registry holds one
// shared instance and every method takes its config as an argument, so the
// connector keeps no per-data-source state of any kind. Two projects syncing
// concurrently share nothing but these two factories.
type Connector struct {
	newSearcher func(*Config) (vendorSearcher, error)
	newPages    func() pageFetcher
}

// NewConnector creates a discovery connector wired to the real vendor client and
// the SSRF-safe page fetcher.
func NewConnector() *Connector {
	return &Connector{
		newSearcher: func(cfg *Config) (vendorSearcher, error) {
			return volcengine.NewClient(volcengine.Config{
				HTTPClient:      datasource.NewConnectorHTTPClient(searchTimeout),
				APIKey:          cfg.APIKey,
				EndpointProfile: volcengine.EndpointProfileAPIKey,
			})
		},
		newPages: func() pageFetcher { return newPageClient() },
	}
}

// Type returns the connector type identifier.
func (c *Connector) Type() string { return types.ConnectorTypeDiscovery }

// Validate checks the materialized manifest without calling the vendor.
//
// This diverges from the RSS connector, which fetches every feed to validate it.
// A discovery validation that ran the saved queries would spend vendor quota —
// 500 calls a month on the free tier — every time someone opened the settings
// drawer or re-saved the row. Everything a search would reveal that config cannot
// is reported by the first scheduled sync anyway.
func (c *Connector) Validate(ctx context.Context, config *types.DataSourceConfig) error {
	cfg, err := parseConfig(config)
	if err != nil {
		return err
	}
	logger.Infof(ctx, "[Discovery] validated source %s with %d saved queries",
		cfg.SourceID, len(cfg.Queries))
	return nil
}

// ListResources returns one resource per saved query.
//
// Only the query id appears. The text stays out of the response body: this feeds
// a settings UI over HTTP, and whether that response is logged is not this
// connector's decision to make.
func (c *Connector) ListResources(
	ctx context.Context, config *types.DataSourceConfig, parentID string,
) ([]types.Resource, error) {
	// Saved queries are flat, so a lazy-load request for a child has nothing.
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

// ResolveResourceAncestors has nothing to do: saved queries are a flat list.
func (c *Connector) ResolveResourceAncestors(
	ctx context.Context, config *types.DataSourceConfig, resourceIDs []string,
) ([]string, error) {
	return []string{}, nil
}

// FetchAll runs every selected saved query and ingests each hit's original page.
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

// FetchIncremental runs the saved queries and emits only candidates whose page
// body is new or changed.
//
// A nil cursor is returned whenever nothing was learned, and that is load-bearing
// rather than tidy: the sync service persists a non-nil cursor even when the fetch
// returns an error, so returning one after a total outage would mark every
// candidate as seen and make the outage look like "nothing new was published".
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

// walk is the shared implementation of both fetch paths.
//
// Failure has three granularities, and they are not interchangeable:
//
//   - One candidate fails (URL refused, page unreachable): it is dropped from
//     this run and left out of the new cursor, so the next run retries it. The
//     rest of the page is unaffected.
//   - One query's search fails: that query's prior progress is carried forward
//     untouched — it learned nothing, so forgetting would re-ingest everything it
//     had already emitted — and the run is reported as partial.
//   - Every query's search fails: no cursor is returned at all, so the service
//     cannot mistake a total outage for an empty result set.
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
	pages := c.newPages()
	options := cfg.SearchOptions()

	newCursor := querycursor.New()
	var out []types.FetchedItem
	var failures []string
	searched := 0

	for _, query := range selected {
		response, err := searcher.Search(ctx, query.Query, options)
		if err != nil {
			// Never %v the query, and never the response body: the vendor's own
			// error message quotes the offending input, which is why the client
			// strips it before it can reach here.
			logger.Warnf(ctx, "[Discovery] search failed for query %s of source %s: %v",
				query.QueryID, cfg.SourceID, err)
			failures = append(failures, fmt.Sprintf("query %s: %v", query.QueryID, err))
			newCursor.CarryQueryProgress(prev, query.QueryID)
			continue
		}
		searched++

		newCursor.StartQuery(query.QueryID)
		var priorItems map[string]string
		if incremental {
			priorItems = querycursor.PriorProgress(prev, query.QueryID)
		}

		var emitted, skipped, dropped int
		for _, hit := range response.Results {
			landingURL := strings.TrimSpace(hit.URL)
			if landingURL == "" {
				// The profile asks for URLs, so a hit without one is a vendor-side
				// defect. It is reported rather than silently discarded.
				dropped++
				failures = append(failures,
					fmt.Sprintf("query %s: a hit arrived without a landing page URL", query.QueryID))
				continue
			}

			externalID := candidateExternalID(query.QueryID, landingURL)
			fetched, err := pages.Fetch(ctx, landingURL)
			if err != nil {
				// Left out of newCursor on purpose: recording it would mark an
				// unread page as ingested and it would never be retried.
				logger.Warnf(ctx, "[Discovery] landing page unavailable for query %s: %v",
					query.QueryID, err)
				failures = append(failures, fmt.Sprintf("query %s: %v", query.QueryID, err))
				dropped++
				continue
			}

			fingerprint := querycursor.Fingerprint(fetched.Markdown)
			newCursor.Record(query.QueryID, externalID, fingerprint)
			if incremental && priorItems[externalID] == fingerprint {
				skipped++
				continue
			}
			out = append(out, candidateItem(cfg, query, hit, fetched, externalID, landingURL))
			emitted++
		}

		// response.Dropped counts hits the client itself refused for lacking a
		// usable URL. Reporting it keeps a short page from reading as a complete one.
		logger.Infof(ctx,
			"[Discovery] query %s of source %s: hits=%d emitted=%d skipped=%d dropped=%d vendor_dropped=%d",
			query.QueryID, cfg.SourceID, len(response.Results), emitted, skipped, dropped, response.Dropped)
	}

	if searched == 0 {
		// Not a PartialFetchError: the service downgrades that to a warning and
		// persists the cursor, which is precisely what must not happen here.
		return nil, nil, fmt.Errorf("%w: every saved query failed: %s",
			datasource.ErrFetchFailed, strings.Join(failures, "; "))
	}
	if len(failures) > 0 {
		return out, newCursor, &datasource.PartialFetchError{Details: failures}
	}
	return out, newCursor, nil
}

// selectQueries resolves the picker's selection against the manifest. An empty
// selection means every query; an unknown id is refused rather than ignored,
// because silently syncing a subset looks identical to syncing everything.
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

// candidateItem projects one hit plus its fetched page onto a FetchedItem.
//
// The split is the whole point: the vendor supplies identity (title, URL, publish
// time) and the page supplies the body. Nothing the vendor said about the content
// — snippet, summary, authority tier, relevance score — is carried, because a
// downstream reader could not tell it apart from the page's own text.
func candidateItem(
	cfg *Config,
	query SavedQuery,
	hit volcengine.Result,
	fetched page,
	externalID, landingURL string,
) types.FetchedItem {
	title := strings.TrimSpace(hit.Title)
	if title == "" {
		title = strings.TrimSpace(fetched.Title)
	}
	if title == "" {
		title = "untitled"
	}

	updatedAt := time.Now().UTC()
	if hit.PublishedAt != nil && !hit.PublishedAt.IsZero() {
		updatedAt = *hit.PublishedAt
	}

	return types.FetchedItem{
		ExternalID:  externalID,
		Title:       title,
		Content:     []byte(fetched.Markdown),
		ContentType: "text/markdown",
		FileName:    sanitizeFileName(title) + ".md",
		URL:         landingURL,
		UpdatedAt:   updatedAt,
		// A hit dropping out of the vendor's ranking says nothing about the page
		// behind it, so discovery never reports deletions.
		IsDeleted:        false,
		SourceResourceID: query.QueryID,
		Metadata: map[string]string{
			"channel": types.ChannelDiscovery,
			// saved_query_id, not the query text: this is the provenance a
			// candidate carries into review, and it must not spell out what was
			// searched for.
			"saved_query_id":       query.QueryID,
			"discovery_source_id":  cfg.SourceID,
			"discovery_project_id": cfg.ProjectID,
			"discovery_provider":   cfg.ProviderType,
			"landing_url":          landingURL,
		},
	}
}

// candidateExternalID scopes a candidate to the query that found it.
//
// Two saved queries finding the same page therefore produce two candidates, each
// with its own provenance, rather than one whose origin depends on which query ran
// last. The duplicate is not a problem downstream: promotion deduplicates by
// canonical URL and content hash, so the second one is refused there — by the gate
// that exists for it — instead of being papered over here.
func candidateExternalID(queryID, landingURL string) string {
	return queryID + ":" + landingURL
}

// sanitizeFileName strips characters invalid in filenames and truncates at a
// UTF-8 boundary (mirrors the RSS and Yuque connectors).
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "untitled"
	}
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
		"\n", " ", "\r", " ", "\t", " ",
	)
	result := strings.TrimSpace(replacer.Replace(name))
	if result == "" {
		return "untitled"
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
