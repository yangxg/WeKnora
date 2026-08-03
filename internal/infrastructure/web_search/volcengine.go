package web_search

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/infrastructure/web_search/volcengine"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const defaultVolcengineTimeout = 15 * time.Second

// volcengineSearcher is the seam this provider tests against. The wire protocol
// lives in the volcengine package and is covered by its own fake-server tests;
// here the interesting behaviour is the projection, so the provider tests use a
// fake searcher instead of a second HTTP server.
type volcengineSearcher interface {
	Search(ctx context.Context, query string, opts volcengine.Options) (*volcengine.Response, error)
}

// VolcengineProvider implements web search using the Doubao (Volcengine) Custom
// search API.
//
// Unlike most providers here it holds no request options of its own: the chat
// search path always wants the same profile, and the discovery path does not go
// through this type at all — it drives the shared client directly from a
// version-controlled manifest (ResearchFlow ADR-0011 §4). Keeping the tunables
// out of the persisted provider parameters is deliberate: a discovery-relevant
// option that could be edited in the WeKnora settings UI would be a policy the
// project repository no longer describes.
type VolcengineProvider struct {
	client volcengineSearcher
}

// NewVolcengineProvider creates a Volcengine web search provider from persisted parameters.
func NewVolcengineProvider(params types.WebSearchProviderParameters) (interfaces.WebSearchProvider, error) {
	if err := ValidateVolcengineParameters(params); err != nil {
		return nil, err
	}
	httpClient, err := NewSearchHTTPClient(defaultVolcengineTimeout, params.ProxyURL)
	if err != nil {
		return nil, err
	}
	client, err := volcengine.NewClient(volcengine.Config{
		HTTPClient: httpClient,
		APIKey:     params.APIKey,
	})
	if err != nil {
		return nil, err
	}
	return &VolcengineProvider{client: client}, nil
}

// ValidateVolcengineParameters validates credentials and the optional proxy.
func ValidateVolcengineParameters(params types.WebSearchProviderParameters) error {
	if strings.TrimSpace(params.APIKey) == "" {
		return fmt.Errorf("API key is required for Volcengine provider")
	}
	return ValidateProxyURL(params.ProxyURL)
}

// Name returns the provider name.
func (p *VolcengineProvider) Name() string {
	return "volcengine"
}

// Search performs a web search using the Doubao Custom search API.
func (p *VolcengineProvider) Search(
	ctx context.Context,
	query string,
	maxResults int,
	includeDate bool,
) ([]*types.WebSearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is empty")
	}
	if maxResults <= 0 {
		maxResults = volcengine.DefaultCount
	}
	if maxResults > volcengine.MaxCount {
		maxResults = volcengine.MaxCount
	}

	response, err := p.client.Search(ctx, query, volcengine.Options{
		SearchType: volcengine.SearchTypeWeb,
		Count:      maxResults,
		NeedURL:    true,
		// NeedContent stays false. The vendor's copy of a page is a second-hand
		// snapshot, so nothing downstream may treat it as the page itself; asking
		// for it would only add cost and a body no caller is allowed to trust.
		NeedContent: false,
	})
	if err != nil {
		return nil, err
	}

	results := make([]*types.WebSearchResult, 0, len(response.Results))
	for _, item := range response.Results {
		result := &types.WebSearchResult{
			Title:   item.Title,
			URL:     item.URL,
			Snippet: item.Snippet,
			Source:  "volcengine",
		}
		if includeDate && item.PublishedAt != nil {
			publishedAt := *item.PublishedAt
			result.PublishedAt = &publishedAt
		}
		results = append(results, result)
		if len(results) >= maxResults {
			break
		}
	}
	logger.Infof(ctx, "[WebSearch][Volcengine] returned %d results, dropped %d", len(results), response.Dropped)
	return results, nil
}
