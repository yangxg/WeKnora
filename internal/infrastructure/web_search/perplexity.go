package web_search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	// defaultPerplexitySearchURL is the official chat-completions endpoint.
	// Perplexity surfaces web citations from the sonar family of models.
	// Not configurable by tenants — prevents SSRF.
	defaultPerplexitySearchURL = "https://api.perplexity.ai/chat/completions"
	defaultPerplexityTimeout   = 60 * time.Second
	defaultPerplexityModel     = "sonar"
	defaultPerplexityResults   = 10
	maxPerplexityResults       = 20
	maxPerplexityResponseBytes = 2 << 20
	// Cap the answer snippet projected onto every citation hit. The full
	// answer is a second-hand vendor synthesis — web_fetch is the path for
	// first-party page text.
	maxPerplexitySnippetRunes = 500
)

// PerplexityProvider implements web search via Perplexity chat completions.
//
// Shape note: Perplexity is not a pure SERP. It returns a synthesized answer
// plus a citation URL list. We project each citation URL into a
// WebSearchResult so the multi-source RRF path can treat it like any other
// web hit. Content stays empty (vendor synthesis is not governed body).
type PerplexityProvider struct {
	client  *http.Client
	baseURL string
	apiKey  string
	model   string
}

// NewPerplexityProvider creates a Perplexity provider from persisted parameters.
func NewPerplexityProvider(params types.WebSearchProviderParameters) (interfaces.WebSearchProvider, error) {
	if err := ValidatePerplexityParameters(params); err != nil {
		return nil, err
	}
	client, err := NewSearchHTTPClient(defaultPerplexityTimeout, params.ProxyURL)
	if err != nil {
		return nil, err
	}
	return &PerplexityProvider{
		client:  client,
		baseURL: defaultPerplexitySearchURL,
		apiKey:  strings.TrimSpace(params.APIKey),
		model:   perplexityModel(params.ExtraConfig),
	}, nil
}

// ValidatePerplexityParameters validates credentials and the optional model.
func ValidatePerplexityParameters(params types.WebSearchProviderParameters) error {
	if strings.TrimSpace(params.APIKey) == "" {
		return fmt.Errorf("API key is required for Perplexity provider")
	}
	model := perplexityModel(params.ExtraConfig)
	if model == "" {
		return fmt.Errorf("invalid Perplexity model")
	}
	return ValidateProxyURL(params.ProxyURL)
}

func perplexityModel(extraConfig map[string]string) string {
	if value := strings.TrimSpace(extraConfig["model"]); value != "" {
		return value
	}
	return defaultPerplexityModel
}

// Name returns the provider type id.
func (p *PerplexityProvider) Name() string {
	return "perplexity"
}

// Search asks Perplexity for an answer and projects citations to web hits.
func (p *PerplexityProvider) Search(
	ctx context.Context,
	query string,
	maxResults int,
	includeDate bool,
) ([]*types.WebSearchResult, error) {
	_ = includeDate // Perplexity citations carry no publication date.
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query is empty")
	}
	if maxResults <= 0 {
		maxResults = defaultPerplexityResults
	}
	if maxResults > maxPerplexityResults {
		maxResults = maxPerplexityResults
	}

	requestBody := perplexityChatRequest{
		Model: p.model,
		Messages: []perplexityMessage{
			{Role: "user", Content: query},
		},
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Perplexity request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create Perplexity request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	// Shape-only: model + max, never the query text.
	logger.Infof(ctx, "[WebSearch][Perplexity] model=%s maxResults=%d", p.model, maxResults)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute Perplexity request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := readPerplexityResponseBody(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, perplexityHTTPError(resp.StatusCode, respBody)
	}

	var response perplexityChatResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to unmarshal Perplexity response: %w", err)
	}

	snippet := ""
	if len(response.Choices) > 0 {
		snippet = truncateRunes(strings.TrimSpace(response.Choices[0].Message.Content), maxPerplexitySnippetRunes)
	}

	citations := dedupeCitationURLs(response.Citations)
	if len(citations) == 0 {
		// No URLs → nothing the RRF/web_fetch path can act on. Empty list is
		// honest; inventing a synthetic hit without a URL would be dropped by
		// fuseWebSearchResults anyway.
		logger.Infof(ctx, "[WebSearch][Perplexity] returned 0 results (no citations)")
		return nil, nil
	}

	results := make([]*types.WebSearchResult, 0, len(citations))
	for _, citeURL := range citations {
		results = append(results, &types.WebSearchResult{
			Title:   titleFromURL(citeURL),
			URL:     citeURL,
			Snippet: snippet,
			Source:  "perplexity",
			// Content intentionally empty — vendor synthesis is not the page.
		})
		if len(results) >= maxResults {
			break
		}
	}
	logger.Infof(ctx, "[WebSearch][Perplexity] returned %d results", len(results))
	return results, nil
}

func dedupeCitationURLs(citations []string) []string {
	seen := make(map[string]struct{}, len(citations))
	out := make([]string, 0, len(citations))
	for _, raw := range citations {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

func titleFromURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		if len(raw) > 80 {
			return raw[:80]
		}
		return raw
	}
	host := strings.TrimPrefix(parsed.Host, "www.")
	path := strings.Trim(parsed.Path, "/")
	if path == "" {
		return host
	}
	// Keep title short and non-secret.
	if len(path) > 48 {
		path = path[:48]
	}
	return host + "/" + path
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

func readPerplexityResponseBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxPerplexityResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read Perplexity response: %w", err)
	}
	if len(body) > maxPerplexityResponseBytes {
		return nil, fmt.Errorf("Perplexity response exceeds %d bytes", maxPerplexityResponseBytes)
	}
	return body, nil
}

func perplexityHTTPError(statusCode int, body []byte) error {
	detail := strings.TrimSpace(string(body))
	if len(detail) > 200 {
		detail = detail[:200]
	}
	if detail == "" {
		return fmt.Errorf("Perplexity API returned status %d", statusCode)
	}
	return fmt.Errorf("Perplexity API returned status %d: %s", statusCode, detail)
}

type perplexityChatRequest struct {
	Model    string              `json:"model"`
	Messages []perplexityMessage `json:"messages"`
}

type perplexityMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type perplexityChatResponse struct {
	Citations []string `json:"citations"`
	Choices   []struct {
		Message perplexityMessage `json:"message"`
	} `json:"choices"`
}
