// Package volcengine implements a client for the Doubao (Volcengine) Custom
// web search API.
//
// It is deliberately a standalone package rather than another file in
// web_search: two very different consumers share it. The registered
// WebSearchProvider wraps it for chat-style search, and the ResearchFlow
// discovery connector calls it directly with a typed options struct built from
// a version-controlled project manifest. That shared-client shape is the reason
// the generic interfaces.WebSearchProvider does not have to grow a discovery
// profile (ResearchFlow ADR-0011 §4).
//
// The caller supplies the *http.Client, so this package never decides the
// outbound network policy: the provider passes the SSRF-safe client the rest of
// web_search uses, and the connector passes its own.
package volcengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/logger"
)

const (
	// APIKeyEndpoint is the Custom-edition endpoint for API-key auth. Like every
	// other provider in this tree it is hardcoded, never tenant-configurable,
	// so a stored parameter can never redirect search traffic.
	APIKeyEndpoint = "https://open.feedcoopapi.com/search_api/web_search"

	// EndpointProfileAPIKey and EndpointProfileIAM mirror the endpoint_profile
	// enum in the ResearchFlow discovery manifest. Only the first is implemented.
	EndpointProfileAPIKey = "api_key"
	EndpointProfileIAM    = "iam"

	// SearchTypeWeb is the only search type this client speaks. The summary
	// edition stopped accepting new subscriptions on 2026-06-23 and the image
	// edition returns a different result shape (ADR-0009 §7).
	SearchTypeWeb = "web"

	// MaxCount, DefaultCount and MaxQueryRunes are the documented request bounds.
	MaxCount      = 50
	DefaultCount  = 10
	MaxQueryRunes = 100

	maxResponseBytes = 4 << 20
	defaultAttempts  = 3
	baseRetryDelay   = 500 * time.Millisecond
	maxRetryDelay    = 30 * time.Second
)

var (
	validTimeRanges     = map[string]struct{}{"OneDay": {}, "OneWeek": {}, "OneMonth": {}, "OneYear": {}}
	validContentFormats = map[string]struct{}{"text": {}, "markdown": {}}
	validIndustries     = map[string]struct{}{"finance": {}, "game": {}, "gov": {}}
	validAuthInfoLevels = map[int]struct{}{1: {}, 2: {}, 3: {}, 4: {}}

	// dateRangePattern is the documented explicit-window form of TimeRange.
	dateRangePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.\.\d{4}-\d{2}-\d{2}$`)
)

// Options is the typed request profile. Every field maps to a documented
// request parameter; nothing here is read from an untyped map, so a caller that
// forgets an option gets the documented default rather than a silent one.
type Options struct {
	// SearchType defaults to SearchTypeWeb and may hold nothing else.
	SearchType string
	// Count is clamped to [1, MaxCount]; 0 means DefaultCount.
	Count int
	// TimeRange is OneDay/OneWeek/OneMonth/OneYear or YYYY-MM-DD..YYYY-MM-DD.
	TimeRange string
	// ContentFormats selects the body encoding; only meaningful with NeedContent.
	ContentFormats []string
	// Industry narrows the corpus: finance, game or gov.
	Industry string
	// QueryRewrite lets the vendor rewrite the query before searching.
	QueryRewrite bool
	// NeedContent asks the vendor for page bodies. Discovery never sets it: a
	// vendor-side snapshot is not the document ResearchFlow governs.
	NeedContent bool
	// NeedURL asks for the landing page URL.
	NeedURL bool
	// Sites and BlockHosts are the request-side allow and deny lists.
	Sites      []string
	BlockHosts []string
	// AuthInfoLevel filters by the vendor's source authority tiers (1..4).
	AuthInfoLevel []int
}

// DiscoveryProfile is the manifest-driven half of Options.
//
// It has no SearchType, NeedContent or NeedURL field on purpose. Those three
// are fixed for discovery, and a struct that cannot express them cannot drift:
// asking the vendor for page content would hand the connector second-hand body
// text where a first-party fetch of the original page is required.
type DiscoveryProfile struct {
	Count          int
	TimeRange      string
	ContentFormats []string
	Industry       string
	QueryRewrite   bool
	Sites          []string
	BlockHosts     []string
	AuthInfoLevel  []int
}

// Options renders the profile with the three fixed fields written as constants.
func (p DiscoveryProfile) Options() Options {
	return Options{
		SearchType:     SearchTypeWeb,
		NeedContent:    false,
		NeedURL:        true,
		Count:          p.Count,
		TimeRange:      p.TimeRange,
		ContentFormats: p.ContentFormats,
		Industry:       p.Industry,
		QueryRewrite:   p.QueryRewrite,
		Sites:          p.Sites,
		BlockHosts:     p.BlockHosts,
		AuthInfoLevel:  p.AuthInfoLevel,
	}
}

// Result is one accepted search hit, already projected off the wire format.
type Result struct {
	Title string
	URL   string
	// Snippet is the vendor's Summary when present, else its short Snippet.
	Snippet string
	// Content is only ever set when the caller asked for page content.
	Content     string
	PublishedAt *time.Time
}

// Response is one page of results.
type Response struct {
	RequestID string
	LogID     string
	Results   []Result
	// Dropped counts hits rejected for lacking a usable http(s) URL. A bad
	// record is refused and counted rather than silently passed on.
	Dropped int
}

// Config builds a Client.
type Config struct {
	// HTTPClient is required. The caller owns the network policy.
	HTTPClient *http.Client
	// APIKey is required; it is sent as a bearer token.
	APIKey string
	// EndpointProfile is api_key (default) or iam. IAM is not implemented.
	EndpointProfile string
	// MaxAttempts bounds retries of transient failures; 0 means defaultAttempts.
	MaxAttempts int
}

// Client calls the Doubao Custom search API.
type Client struct {
	httpClient  *http.Client
	endpoint    string
	apiKey      string
	maxAttempts int
	sleep       func(ctx context.Context, d time.Duration) error
}

// NewClient validates the configuration and returns a ready client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.HTTPClient == nil {
		return nil, fmt.Errorf("volcengine: an HTTP client is required")
	}
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("API key is required for Volcengine provider")
	}
	switch cfg.EndpointProfile {
	case "", EndpointProfileAPIKey:
	case EndpointProfileIAM:
		// The IAM gateway authenticates with an AK/SK request signature. Sending
		// a bearer token there would fail in a way that looks like a bad key, so
		// refuse until the signing path exists.
		return nil, fmt.Errorf(
			"volcengine: endpoint profile %q requires AK/SK request signing, which is not implemented",
			EndpointProfileIAM,
		)
	default:
		return nil, fmt.Errorf("volcengine: unknown endpoint profile %q", cfg.EndpointProfile)
	}
	attempts := cfg.MaxAttempts
	if attempts <= 0 {
		attempts = defaultAttempts
	}
	return &Client{
		httpClient:  cfg.HTTPClient,
		endpoint:    APIKeyEndpoint,
		apiKey:      apiKey,
		maxAttempts: attempts,
		sleep:       sleepContext,
	}, nil
}

// Validate reports whether the options are within the documented ranges.
// The connector can call it before materializing a manifest so a bad policy
// fails at configuration time rather than on the first scheduled search.
func (o Options) Validate() error {
	_, err := o.normalized()
	return err
}

func (o Options) normalized() (Options, error) {
	if o.SearchType == "" {
		o.SearchType = SearchTypeWeb
	}
	if o.SearchType != SearchTypeWeb {
		return o, fmt.Errorf("volcengine: search type %q is not supported, only %q", o.SearchType, SearchTypeWeb)
	}
	if o.Count <= 0 {
		o.Count = DefaultCount
	}
	if o.Count > MaxCount {
		o.Count = MaxCount
	}
	if o.TimeRange != "" {
		if _, ok := validTimeRanges[o.TimeRange]; !ok && !dateRangePattern.MatchString(o.TimeRange) {
			return o, fmt.Errorf("volcengine: unsupported time range %q", o.TimeRange)
		}
	}
	for _, format := range o.ContentFormats {
		if _, ok := validContentFormats[format]; !ok {
			return o, fmt.Errorf("volcengine: unsupported content format %q", format)
		}
	}
	if o.Industry != "" {
		if _, ok := validIndustries[o.Industry]; !ok {
			return o, fmt.Errorf("volcengine: unsupported industry %q", o.Industry)
		}
	}
	for _, level := range o.AuthInfoLevel {
		if _, ok := validAuthInfoLevels[level]; !ok {
			return o, fmt.Errorf("volcengine: unsupported auth info level %d", level)
		}
	}
	return o, nil
}

// Search runs one query and returns one page of results.
//
// Options are validated locally before anything is sent. That is not merely
// defensive: a request the vendor rejects comes back as ParamError with the
// offending query quoted in the message, and that message must never reach a
// log line.
func (c *Client) Search(ctx context.Context, query string, opts Options) (*Response, error) {
	opts, err := opts.normalized()
	if err != nil {
		return nil, err
	}

	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query is empty")
	}
	if utf8.RuneCountInString(query) > MaxQueryRunes {
		query = string([]rune(query)[:MaxQueryRunes])
		logger.Infof(ctx, "[WebSearch][Volcengine] truncated query to %d characters", MaxQueryRunes)
	}

	body, err := json.Marshal(searchRequest{
		Query:          query,
		SearchType:     opts.SearchType,
		Count:          opts.Count,
		TimeRange:      opts.TimeRange,
		ContentFormats: opts.ContentFormats,
		Industry:       opts.Industry,
		Filter: searchFilter{
			NeedContent:   opts.NeedContent,
			NeedURL:       opts.NeedURL,
			Sites:         opts.Sites,
			BlockHosts:    opts.BlockHosts,
			AuthInfoLevel: opts.AuthInfoLevel,
		},
		QueryControl: queryControl{QueryRewrite: opts.QueryRewrite},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Volcengine request: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		response, err := c.attempt(ctx, body, opts)
		if err == nil {
			logger.Infof(ctx, "[WebSearch][Volcengine] returned %d results, dropped %d, attempt %d",
				len(response.Results), response.Dropped, attempt)
			return response, nil
		}
		lastErr = err

		apiErr, ok := err.(*APIError)
		if !ok || !apiErr.Retryable || attempt == c.maxAttempts {
			logger.Warnf(ctx, "[WebSearch][Volcengine] search failed after %d attempt(s): %v", attempt, err)
			return nil, err
		}
		if sleepErr := c.sleep(ctx, retryDelay(attempt, apiErr.RetryAfter)); sleepErr != nil {
			logger.Warnf(ctx, "[WebSearch][Volcengine] backoff interrupted after %d attempt(s): %v", attempt, err)
			return nil, err
		}
	}
	return nil, lastErr
}

// attempt performs one request/response cycle.
func (c *Client) attempt(ctx context.Context, body []byte, opts Options) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &APIError{Code: "RequestBuildFailed", Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport failures are transient often enough to be worth one more
		// try; the context deadline still bounds the whole call.
		return nil, &APIError{Code: "TransportError", Retryable: true, Err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &APIError{HTTPStatus: resp.StatusCode, Code: "ResponseReadFailed", Retryable: true, Err: err}
	}
	if len(raw) > maxResponseBytes {
		return nil, &APIError{HTTPStatus: resp.StatusCode, Code: "ResponseTooLarge"}
	}

	var envelope searchResponse
	decodeErr := json.Unmarshal(raw, &envelope)

	if resp.StatusCode != http.StatusOK {
		apiErr := &APIError{
			HTTPStatus: resp.StatusCode,
			Retryable:  httpStatusRetryable(resp.StatusCode),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Code:       http.StatusText(resp.StatusCode),
		}
		// The body is not wrapped: an error body can quote the request.
		if decodeErr == nil {
			apiErr.RequestID = envelope.ResponseMetadata.RequestID
			if vendorErr := envelope.ResponseMetadata.Error; vendorErr != nil {
				apiErr.CodeN = vendorErr.CodeN
				if vendorErr.Code != "" {
					apiErr.Code = vendorErr.Code
				}
			}
		}
		return nil, apiErr
	}

	if decodeErr != nil {
		return nil, &APIError{HTTPStatus: resp.StatusCode, Code: "InvalidResponse"}
	}

	// A business error rides inside an HTTP 200, so the status alone never
	// decides success.
	if vendorErr := envelope.ResponseMetadata.Error; vendorErr != nil {
		return nil, &APIError{
			HTTPStatus: resp.StatusCode,
			CodeN:      vendorErr.CodeN,
			Code:       vendorErr.Code,
			RequestID:  envelope.ResponseMetadata.RequestID,
			Retryable:  vendorCodeRetryable(vendorErr.CodeN),
		}
	}
	if envelope.Result == nil {
		// Neither an error nor a result: refuse rather than report zero hits,
		// which a caller would read as "nothing published on this topic".
		return nil, &APIError{
			HTTPStatus: resp.StatusCode,
			Code:       "MissingResult",
			RequestID:  envelope.ResponseMetadata.RequestID,
		}
	}

	return projectResults(&envelope, opts), nil
}

// projectResults turns the wire format into the neutral Result shape.
func projectResults(envelope *searchResponse, opts Options) *Response {
	response := &Response{
		RequestID: envelope.ResponseMetadata.RequestID,
		LogID:     envelope.Result.LogID,
		Results:   make([]Result, 0, len(envelope.Result.WebResults)),
	}
	for _, item := range envelope.Result.WebResults {
		if !usableURL(item.URL) {
			response.Dropped++
			continue
		}
		// Summary is the 500~1000 character field the vendor recommends;
		// Snippet is the ~200 character one it warns against for model input.
		snippet := item.Summary
		if snippet == "" {
			snippet = item.Snippet
		}
		result := Result{
			Title:   item.Title,
			URL:     item.URL,
			Snippet: snippet,
		}
		if opts.NeedContent {
			result.Content = item.Content
		}
		if published, ok := parsePublishTime(item.PublishTime); ok {
			result.PublishedAt = &published
		}
		response.Results = append(response.Results, result)
	}
	return response
}

// usableURL keeps only absolute http(s) URLs. Anything else cannot be fetched
// by the connector, so passing it on would only defer the failure.
func usableURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Host != ""
}

// parsePublishTime accepts the documented RFC3339 form. An unparseable value
// costs the result its date, never the page.
func parsePublishTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// parseRetryAfter reads the header in either documented form.
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		if delay := time.Until(deadline); delay > 0 {
			return delay
		}
	}
	return 0
}

// retryDelay honours the vendor's Retry-After and otherwise backs off
// exponentially, capped either way.
func retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	delay := retryAfter
	if delay <= 0 {
		delay = baseRetryDelay << (attempt - 1)
	}
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	return delay
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// searchRequest is the documented request body. The vendor uses PascalCase
// field names; the tags are not stylistic.
type searchRequest struct {
	Query          string       `json:"Query"`
	SearchType     string       `json:"SearchType"`
	Count          int          `json:"Count"`
	TimeRange      string       `json:"TimeRange,omitempty"`
	ContentFormats []string     `json:"ContentFormats,omitempty"`
	Industry       string       `json:"Industry,omitempty"`
	Filter         searchFilter `json:"Filter"`
	QueryControl   queryControl `json:"QueryControl"`
}

type searchFilter struct {
	NeedContent   bool     `json:"NeedContent"`
	NeedURL       bool     `json:"NeedUrl"`
	Sites         []string `json:"Sites,omitempty"`
	BlockHosts    []string `json:"BlockHosts,omitempty"`
	AuthInfoLevel []int    `json:"AuthInfoLevel,omitempty"`
}

type queryControl struct {
	QueryRewrite bool `json:"QueryRewrite"`
}

type searchResponse struct {
	ResponseMetadata responseMetadata `json:"ResponseMetadata"`
	Result           *searchResult    `json:"Result"`
}

type responseMetadata struct {
	RequestID string         `json:"RequestId"`
	Action    string         `json:"Action"`
	Version   string         `json:"Version"`
	Service   string         `json:"Service"`
	Region    string         `json:"Region"`
	Error     *responseError `json:"Error"`
}

type responseError struct {
	CodeN   int    `json:"CodeN"`
	Code    string `json:"Code"`
	Message string `json:"Message"`
}

type searchResult struct {
	ResultCount int       `json:"ResultCount"`
	WebResults  []webItem `json:"WebResults"`
	LogID       string    `json:"LogId"`
}

type webItem struct {
	Title       string `json:"Title"`
	URL         string `json:"Url"`
	Summary     string `json:"Summary"`
	Snippet     string `json:"Snippet"`
	Content     string `json:"Content"`
	PublishTime string `json:"PublishTime"`
}
