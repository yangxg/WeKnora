package web_search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	// defaultSerpAPISearchURL is the official SerpAPI endpoint.
	// Not configurable by tenants — prevents SSRF.
	defaultSerpAPISearchURL = "https://serpapi.com/search.json"
	defaultSerpAPITimeout   = 20 * time.Second
	defaultSerpAPIResults   = 10
	maxSerpAPIResults       = 100
	maxSerpAPIResponseBytes = 4 << 20
	defaultSerpAPIEngine    = "google"
)

// Engines allowed via ExtraConfig["engine"]. Each tenant provider instance
// pins one engine so a single multi-source agent can bind both Google web
// and Google Scholar as separate provider_ids.
var validSerpAPIEngines = map[string]struct{}{
	"google":         {},
	"google_scholar": {},
}

// SerpAPIProvider implements web search via SerpAPI (Google / Google Scholar).
type SerpAPIProvider struct {
	client  *http.Client
	baseURL string
	apiKey  string
	engine  string
}

// NewSerpAPIProvider creates a SerpAPI provider from persisted parameters.
func NewSerpAPIProvider(params types.WebSearchProviderParameters) (interfaces.WebSearchProvider, error) {
	if err := ValidateSerpAPIParameters(params); err != nil {
		return nil, err
	}
	client, err := NewSearchHTTPClient(defaultSerpAPITimeout, params.ProxyURL)
	if err != nil {
		return nil, err
	}
	return &SerpAPIProvider{
		client:  client,
		baseURL: defaultSerpAPISearchURL,
		apiKey:  strings.TrimSpace(params.APIKey),
		engine:  serpAPIEngine(params.ExtraConfig),
	}, nil
}

// ValidateSerpAPIParameters validates credentials and the engine option.
func ValidateSerpAPIParameters(params types.WebSearchProviderParameters) error {
	if strings.TrimSpace(params.APIKey) == "" {
		return fmt.Errorf("API key is required for SerpAPI provider")
	}
	engine := serpAPIEngine(params.ExtraConfig)
	if _, ok := validSerpAPIEngines[engine]; !ok {
		return fmt.Errorf("invalid SerpAPI engine: %s", engine)
	}
	return ValidateProxyURL(params.ProxyURL)
}

func serpAPIEngine(extraConfig map[string]string) string {
	if value := strings.TrimSpace(extraConfig["engine"]); value != "" {
		return value
	}
	return defaultSerpAPIEngine
}

// Name returns the provider type id.
func (p *SerpAPIProvider) Name() string {
	return "serpapi"
}

// Search performs a SerpAPI search for the pinned engine.
func (p *SerpAPIProvider) Search(
	ctx context.Context,
	query string,
	maxResults int,
	includeDate bool,
) ([]*types.WebSearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query is empty")
	}
	if maxResults <= 0 {
		maxResults = defaultSerpAPIResults
	}
	if maxResults > maxSerpAPIResults {
		maxResults = maxSerpAPIResults
	}

	params := url.Values{}
	params.Set("engine", p.engine)
	params.Set("q", query)
	params.Set("api_key", p.apiKey)
	params.Set("num", fmt.Sprintf("%d", maxResults))

	reqURL := p.baseURL + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create SerpAPI request: %w", err)
	}

	// Shape-only log: engine + count, never the query text (ADR-0009 §9).
	logger.Infof(ctx, "[WebSearch][SerpAPI] engine=%s maxResults=%d", p.engine, maxResults)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, serpAPITransportError(err)
	}
	defer resp.Body.Close()

	body, err := readSerpAPIResponseBody(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, serpAPIHTTPError(resp.StatusCode, body)
	}

	var response serpAPISearchResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to unmarshal SerpAPI response: %w", err)
	}
	if response.Error != "" {
		// Vendor error text can echo the query or request parameters.
		return nil, fmt.Errorf("SerpAPI returned an error")
	}

	results := make([]*types.WebSearchResult, 0, len(response.OrganicResults))
	for _, item := range response.OrganicResults {
		title := strings.TrimSpace(item.Title)
		link := strings.TrimSpace(item.Link)
		if title == "" && link == "" {
			continue
		}
		result := &types.WebSearchResult{
			Title:   title,
			URL:     link,
			Snippet: strings.TrimSpace(item.Snippet),
			// Source is the provider type; engine is recoverable from the
			// tenant provider entity, not from each hit.
			Source: "serpapi",
		}
		if includeDate {
			if publishedAt, ok := parseSerpAPIDate(item.Date); ok {
				result.PublishedAt = &publishedAt
			}
		}
		results = append(results, result)
		if len(results) >= maxResults {
			break
		}
	}
	logger.Infof(ctx, "[WebSearch][SerpAPI] engine=%s returned %d results", p.engine, len(results))
	return results, nil
}

func readSerpAPIResponseBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxSerpAPIResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read SerpAPI response: %w", err)
	}
	if len(body) > maxSerpAPIResponseBytes {
		return nil, fmt.Errorf("SerpAPI response exceeds %d bytes", maxSerpAPIResponseBytes)
	}
	return body, nil
}

func serpAPITransportError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("failed to execute SerpAPI request: %w", context.DeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("failed to execute SerpAPI request: %w", context.Canceled)
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return fmt.Errorf("failed to execute SerpAPI request: network timeout")
		}
		return fmt.Errorf("failed to execute SerpAPI request: network error")
	}
	return fmt.Errorf("failed to execute SerpAPI request: transport error")
}

func serpAPIHTTPError(statusCode int, _ []byte) error {
	// Response bodies are vendor-controlled and may echo the query or API key.
	return fmt.Errorf("SerpAPI returned status %d", statusCode)
}

func parseSerpAPIDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	// SerpAPI google organic often returns "Jan 2, 2006" or "2 days ago".
	// Only accept absolute dates we can parse deterministically.
	for _, layout := range []string{
		"Jan 2, 2006",
		"January 2, 2006",
		"2006-01-02",
		time.RFC3339,
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

type serpAPISearchResponse struct {
	Error          string               `json:"error"`
	OrganicResults []serpAPIOrganicItem `json:"organic_results"`
}

type serpAPIOrganicItem struct {
	Title   string `json:"title"`
	Link    string `json:"link"`
	Snippet string `json:"snippet"`
	Date    string `json:"date"`
}
