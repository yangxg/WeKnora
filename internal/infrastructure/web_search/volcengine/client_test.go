package volcengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
)

// secretQuery stands in for a real user query. Every test that can possibly
// leak it asserts it does not appear in errors or logs (ADR-0009 §9).
const secretQuery = "DRG-payment-reform-pilot-city-list"

// testClient points a client at a test server. Production always uses the fixed
// endpoint; only same-package tests reach the unexported field.
func testClient(t *testing.T, endpoint string, httpClient *http.Client) *Client {
	t.Helper()
	c, err := NewClient(Config{HTTPClient: httpClient, APIKey: "test-key"})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	c.endpoint = endpoint
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func webEnvelope(items ...map[string]any) map[string]any {
	if items == nil {
		items = []map[string]any{}
	}
	return map[string]any{
		"ResponseMetadata": map[string]any{
			"RequestId": "req-1",
			"Action":    "WebSearch",
			"Version":   "2025-01-01",
		},
		"Result": map[string]any{
			"ResultCount": len(items),
			"WebResults":  items,
			"LogId":       "log-1",
		},
	}
}

func errorEnvelope(codeN int, code, message string) map[string]any {
	return map[string]any{
		"ResponseMetadata": map[string]any{
			"RequestId": "req-err",
			"Error": map[string]any{
				"CodeN":   codeN,
				"Code":    code,
				"Message": message,
			},
		},
	}
}

func serveJSON(t *testing.T, status int, payload any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNewClientRequiresCredentialsAndAnInjectedHTTPClient(t *testing.T) {
	if _, err := NewClient(Config{APIKey: "key"}); err == nil {
		t.Error("NewClient() without an HTTP client succeeded; the caller must supply an SSRF-safe client")
	}
	if _, err := NewClient(Config{HTTPClient: http.DefaultClient}); err == nil {
		t.Error("NewClient() without an API key succeeded")
	}
	if _, err := NewClient(Config{HTTPClient: http.DefaultClient, APIKey: "  "}); err == nil {
		t.Error("NewClient() with a blank API key succeeded")
	}
}

func TestNewClientUsesTheFixedEndpointAndRefusesTheIAMProfile(t *testing.T) {
	c, err := NewClient(Config{HTTPClient: http.DefaultClient, APIKey: "key"})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if c.endpoint != APIKeyEndpoint {
		t.Errorf("endpoint = %q, want the hardcoded %q", c.endpoint, APIKeyEndpoint)
	}

	// The IAM gateway needs AK/SK request signing, which W2a1 does not
	// implement. Refusing is the only safe answer: silently falling back to a
	// Bearer header would send the API key to a gateway that ignores it.
	_, err = NewClient(Config{
		HTTPClient:      http.DefaultClient,
		APIKey:          "key",
		EndpointProfile: EndpointProfileIAM,
	})
	if err == nil {
		t.Fatal("NewClient() accepted the IAM profile; W2a1 implements api_key only")
	}
	if !strings.Contains(err.Error(), EndpointProfileIAM) {
		t.Errorf("error = %v, want it to name the unsupported profile", err)
	}

	if _, err := NewClient(Config{
		HTTPClient:      http.DefaultClient,
		APIKey:          "key",
		EndpointProfile: "sso",
	}); err == nil {
		t.Error("NewClient() accepted an unknown endpoint profile")
	}
}

func TestSearchSendsTheDocumentedRequestShape(t *testing.T) {
	var body map[string]any
	var authorization, contentType, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		authorization = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(webEnvelope())
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, srv.Client())
	_, err := c.Search(context.Background(), secretQuery, Options{
		SearchType:     SearchTypeWeb,
		Count:          20,
		TimeRange:      "OneMonth",
		ContentFormats: []string{"markdown"},
		Industry:       "gov",
		QueryRewrite:   true,
		NeedContent:    false,
		NeedURL:        true,
		Sites:          []string{"www.gov.cn"},
		BlockHosts:     []string{"news.aggregator.example"},
		AuthInfoLevel:  []int{1, 2},
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if authorization != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", authorization)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}

	// The vendor field names are PascalCase; a lowercase tag silently drops the
	// option and the request degrades to an unfiltered search.
	for key, want := range map[string]any{
		"Query":      secretQuery,
		"SearchType": SearchTypeWeb,
		"Count":      float64(20),
		"TimeRange":  "OneMonth",
		"Industry":   "gov",
	} {
		if got := body[key]; got != want {
			t.Errorf("request[%q] = %v, want %v", key, got, want)
		}
	}

	filter, ok := body["Filter"].(map[string]any)
	if !ok {
		t.Fatalf("request Filter = %v, want an object", body["Filter"])
	}
	if filter["NeedContent"] != false {
		t.Errorf("Filter.NeedContent = %v, want false", filter["NeedContent"])
	}
	if filter["NeedUrl"] != true {
		t.Errorf("Filter.NeedUrl = %v, want true", filter["NeedUrl"])
	}
	if !reflect.DeepEqual(filter["Sites"], []any{"www.gov.cn"}) {
		t.Errorf("Filter.Sites = %v", filter["Sites"])
	}
	if !reflect.DeepEqual(filter["BlockHosts"], []any{"news.aggregator.example"}) {
		t.Errorf("Filter.BlockHosts = %v", filter["BlockHosts"])
	}
	if !reflect.DeepEqual(filter["AuthInfoLevel"], []any{float64(1), float64(2)}) {
		t.Errorf("Filter.AuthInfoLevel = %v", filter["AuthInfoLevel"])
	}

	queryControl, ok := body["QueryControl"].(map[string]any)
	if !ok {
		t.Fatalf("request QueryControl = %v, want an object", body["QueryControl"])
	}
	if queryControl["QueryRewrite"] != true {
		t.Errorf("QueryControl.QueryRewrite = %v, want true", queryControl["QueryRewrite"])
	}
	if !reflect.DeepEqual(body["ContentFormats"], []any{"markdown"}) {
		t.Errorf("ContentFormats = %v", body["ContentFormats"])
	}

	// NeedSummary appears in the Postman sample but not in the current field
	// table, so the first version must not send it (ADR-0009 §7).
	if _, present := body["NeedSummary"]; present {
		t.Error("request carries NeedSummary, which is not in the documented field table")
	}
}

func TestSearchPrefersSummaryOverTheShortSnippet(t *testing.T) {
	srv := serveJSON(t, http.StatusOK, webEnvelope(
		map[string]any{
			"Title":   "With summary",
			"Url":     "https://www.gov.cn/a",
			"Summary": "the long recommended summary",
			"Snippet": "the short discouraged snippet",
		},
		map[string]any{
			"Title":   "Snippet only",
			"Url":     "https://www.gov.cn/b",
			"Snippet": "fallback snippet",
		},
	))

	c := testClient(t, srv.URL, srv.Client())
	resp, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(resp.Results))
	}
	if resp.Results[0].Snippet != "the long recommended summary" {
		t.Errorf("first snippet = %q, want the Summary field", resp.Results[0].Snippet)
	}
	if resp.Results[1].Snippet != "fallback snippet" {
		t.Errorf("second snippet = %q, want the Snippet fallback", resp.Results[1].Snippet)
	}
	if resp.RequestID != "req-1" || resp.LogID != "log-1" {
		t.Errorf("request/log id = %q/%q, want req-1/log-1", resp.RequestID, resp.LogID)
	}
}

func TestSearchParsesPublishTimeAndTolerntesGarbage(t *testing.T) {
	srv := serveJSON(t, http.StatusOK, webEnvelope(
		map[string]any{"Title": "dated", "Url": "https://www.gov.cn/a", "PublishTime": "2026-07-16T08:30:00+08:00"},
		map[string]any{"Title": "undated", "Url": "https://www.gov.cn/b", "PublishTime": "yesterday"},
		map[string]any{"Title": "blank", "Url": "https://www.gov.cn/c"},
	))

	c := testClient(t, srv.URL, srv.Client())
	resp, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb})
	if err != nil {
		t.Fatalf("Search() error = %v; an unparseable time must not fail the page", err)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(resp.Results))
	}
	if resp.Results[0].PublishedAt == nil {
		t.Fatal("first result lost its parseable PublishTime")
	}
	if got := resp.Results[0].PublishedAt.UTC().Format(time.RFC3339); got != "2026-07-16T00:30:00Z" {
		t.Errorf("published_at = %s, want the RFC3339 value in UTC", got)
	}
	if resp.Results[1].PublishedAt != nil || resp.Results[2].PublishedAt != nil {
		t.Error("an unparseable or absent PublishTime must leave PublishedAt nil")
	}
}

func TestSearchDropsResultsWithoutAUsableURLAndCountsThem(t *testing.T) {
	srv := serveJSON(t, http.StatusOK, webEnvelope(
		map[string]any{"Title": "good", "Url": "https://www.gov.cn/a"},
		map[string]any{"Title": "no url"},
		map[string]any{"Title": "file url", "Url": "file:///etc/passwd"},
		map[string]any{"Title": "unparseable", "Url": "ht tp://%zz"},
	))

	c := testClient(t, srv.URL, srv.Client())
	resp, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb})
	if err != nil {
		t.Fatalf("Search() error = %v; a bad record must not fail the page", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(resp.Results))
	}
	if resp.Dropped != 3 {
		t.Errorf("dropped = %d, want 3 counted rejections", resp.Dropped)
	}
}

func TestSearchRejectsABusinessErrorInsideAnHTTP200(t *testing.T) {
	srv := serveJSON(t, http.StatusOK, errorEnvelope(10406, "FreeQuotaExhausted", "monthly quota used up"))

	c := testClient(t, srv.URL, srv.Client())
	resp, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb})
	if err == nil {
		t.Fatalf("Search() succeeded with resp = %+v; HTTP 200 with an envelope error is a failure", resp)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.CodeN != 10406 || apiErr.Code != "FreeQuotaExhausted" {
		t.Errorf("error code = %d/%q, want 10406/FreeQuotaExhausted", apiErr.CodeN, apiErr.Code)
	}
	if apiErr.Retryable {
		t.Error("FreeQuotaExhausted must not be retryable")
	}
	if apiErr.RequestID != "req-err" {
		t.Errorf("request id = %q, want req-err", apiErr.RequestID)
	}
}

func TestSearchRejectsAMissingResultRatherThanReportingEmptySuccess(t *testing.T) {
	srv := serveJSON(t, http.StatusOK, map[string]any{
		"ResponseMetadata": map[string]any{"RequestId": "req-2"},
	})

	c := testClient(t, srv.URL, srv.Client())
	if _, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb}); err == nil {
		t.Fatal("Search() reported success for a response with neither Result nor Error")
	}

	// An explicitly empty Result is a legitimate zero-hit search.
	empty := serveJSON(t, http.StatusOK, webEnvelope())
	c = testClient(t, empty.URL, empty.Client())
	resp, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb})
	if err != nil {
		t.Fatalf("Search() error = %v; an empty WebResults list is a valid answer", err)
	}
	if len(resp.Results) != 0 {
		t.Errorf("len(results) = %d, want 0", len(resp.Results))
	}
}

func TestSearchErrorsCarryNeitherTheQueryNorTheVendorMessage(t *testing.T) {
	// ParamError echoes the offending input, so propagating Message would put
	// the query into every log line that prints the error.
	srv := serveJSON(t, http.StatusOK, errorEnvelope(10400, "ParamError", "invalid Query: "+secretQuery))

	c := testClient(t, srv.URL, srv.Client())
	_, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb})
	if err == nil {
		t.Fatal("Search() succeeded on a ParamError")
	}
	if strings.Contains(err.Error(), secretQuery) {
		t.Errorf("error leaks the query: %v", err)
	}
	if strings.Contains(err.Error(), "invalid Query") {
		t.Errorf("error leaks the vendor message: %v", err)
	}
	if !strings.Contains(err.Error(), "ParamError") {
		t.Errorf("error = %v, want the symbolic vendor code", err)
	}
}

func TestSearchRejectsInvalidJSONWithoutEchoingTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ResponseMetadata": tru` + secretQuery))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, srv.Client())
	_, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb})
	if err == nil {
		t.Fatal("Search() succeeded on malformed JSON")
	}
	if strings.Contains(err.Error(), secretQuery) {
		t.Errorf("error echoes the response body: %v", err)
	}
}

func TestSearchRejectsOversizedResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"Result":{"WebResults":[`))
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for written := 0; written <= maxResponseBytes; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, srv.Client())
	if _, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb}); err == nil {
		t.Fatal("Search() accepted a response past the size ceiling")
	}
}

func TestSearchDoesNotRetryTerminalVendorCodes(t *testing.T) {
	for _, tc := range []struct {
		codeN int
		code  string
	}{
		{10400, "ParamError"},
		{10401, "InvalidTopToken"},
		{10402, "InvalidSearchType"},
		{10403, "InvalidAccountId"},
		{10406, "FreeQuotaExhausted"},
		{10409, "SearchPackageModeUnsupported"},
		{10410, "SearchPackageUnavailable"},
		{10412, "SearchPackageQuotaExhausted"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&calls, 1)
				_ = json.NewEncoder(w).Encode(errorEnvelope(tc.codeN, tc.code, "boom"))
			}))
			defer srv.Close()

			c := testClient(t, srv.URL, srv.Client())
			if _, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb}); err == nil {
				t.Fatalf("Search() succeeded on %s", tc.code)
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Errorf("attempts = %d, want 1; %s burns quota on every retry", got, tc.code)
			}
		})
	}
}

func TestSearchDoesNotRetryUnknownVendorCodes(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(errorEnvelope(19999, "SomethingNew", "boom"))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, srv.Client())
	if _, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb}); err == nil {
		t.Fatal("Search() succeeded on an unknown vendor code")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("attempts = %d, want 1; an unclassified error defaults to terminal", got)
	}
}

func TestSearchRetriesTransientFailuresAndHonorsRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			_ = json.NewEncoder(w).Encode(errorEnvelope(700429, "FreeRateLimitExceeded", "slow down"))
		case 2:
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			_ = json.NewEncoder(w).Encode(webEnvelope(map[string]any{
				"Title": "ok", "Url": "https://www.gov.cn/a",
			}))
		}
	}))
	defer srv.Close()

	var delays []time.Duration
	c := testClient(t, srv.URL, srv.Client())
	c.maxAttempts = 3
	c.sleep = func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}

	resp, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb})
	if err != nil {
		t.Fatalf("Search() error = %v, want the third attempt to succeed", err)
	}
	if len(resp.Results) != 1 {
		t.Errorf("len(results) = %d, want 1", len(resp.Results))
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if len(delays) != 2 {
		t.Fatalf("sleeps = %v, want 2", delays)
	}
	if delays[1] != 2*time.Second {
		t.Errorf("second delay = %v, want the 2s Retry-After header", delays[1])
	}
}

func TestSearchStopsAtMaxAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, srv.Client())
	c.maxAttempts = 3
	if _, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb}); err == nil {
		t.Fatal("Search() succeeded against a permanently failing server")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("attempts = %d, want the bounded 3", got)
	}
}

func TestSearchDoesNotRetryTerminalHTTPStatuses(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, srv.Client())
	if _, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb}); err == nil {
		t.Fatal("Search() succeeded on HTTP 401")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("attempts = %d, want 1; a rejected key never becomes valid by retrying", got)
	}
}

func TestSearchAbandonsRetriesWhenTheContextIsDone(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := testClient(t, srv.URL, srv.Client())
	c.maxAttempts = 5
	c.sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}

	if _, err := c.Search(ctx, secretQuery, Options{SearchType: SearchTypeWeb}); err == nil {
		t.Fatal("Search() succeeded despite a cancelled context")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("attempts = %d, want 1; a cancelled context stops the backoff", got)
	}
}

func TestSearchRejectsSearchTypesOtherThanWeb(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(webEnvelope())
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, srv.Client())
	for _, searchType := range []string{"web_summary", "image", "video"} {
		if _, err := c.Search(context.Background(), secretQuery, Options{SearchType: searchType}); err == nil {
			t.Errorf("Search() accepted SearchType %q; W2a implements web only", searchType)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("requests = %d, want 0; an unsupported search type never reaches the vendor", got)
	}

	// An unset search type defaults to the only supported one.
	if _, err := c.Search(context.Background(), secretQuery, Options{}); err != nil {
		t.Errorf("Search() with an unset SearchType error = %v", err)
	}
}

func TestSearchRejectsOutOfRangeEnumsBeforeSendingThem(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(webEnvelope())
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, srv.Client())
	for name, opts := range map[string]Options{
		"time range":      {TimeRange: "OneDecade"},
		"content format":  {ContentFormats: []string{"pdf"}},
		"industry":        {Industry: "medicine"},
		"auth info level": {AuthInfoLevel: []int{9}},
	} {
		if _, err := c.Search(context.Background(), secretQuery, opts); err == nil {
			t.Errorf("Search() accepted an out-of-range %s", name)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("requests = %d, want 0; local validation keeps the query out of a vendor ParamError", got)
	}

	// An explicit date range is a documented TimeRange value.
	if _, err := c.Search(context.Background(), secretQuery, Options{TimeRange: "2026-01-01..2026-06-30"}); err != nil {
		t.Errorf("Search() rejected a documented date range: %v", err)
	}
}

func TestSearchClampsCountAndTruncatesLongQueries(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(webEnvelope())
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, srv.Client())
	long := strings.Repeat("医", MaxQueryRunes+20)
	if _, err := c.Search(context.Background(), long, Options{Count: MaxCount + 10}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if body["Count"] != float64(MaxCount) {
		t.Errorf("Count = %v, want the documented ceiling %d", body["Count"], MaxCount)
	}
	if got := len([]rune(body["Query"].(string))); got != MaxQueryRunes {
		t.Errorf("query runes = %d, want %d", got, MaxQueryRunes)
	}

	if _, err := c.Search(context.Background(), long, Options{Count: 0}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if body["Count"] != float64(DefaultCount) {
		t.Errorf("Count = %v, want the default %d", body["Count"], DefaultCount)
	}

	if _, err := c.Search(context.Background(), "   ", Options{}); err == nil {
		t.Error("Search() accepted a blank query")
	}
}

func TestSearchLogsNeitherTheQueryNorTheResponseBody(t *testing.T) {
	var logs bytes.Buffer
	logger.SetLogLevel(logger.LevelDebug)
	logger.SetOutput(&logs)
	t.Cleanup(logger.ConfigureFromEnv)

	body := "state-council-notice-body-text"
	srv := serveJSON(t, http.StatusOK, webEnvelope(map[string]any{
		"Title":   "notice",
		"Url":     "https://www.gov.cn/secret-landing-page",
		"Summary": body,
	}))

	c := testClient(t, srv.URL, srv.Client())
	// The marker sits in the first runes so it survives truncation: banning only
	// the full query would pass even if the truncated one were logged.
	long := "long-secret-topic-" + strings.Repeat("医", MaxQueryRunes)
	truncated := string([]rune(long)[:MaxQueryRunes])
	if _, err := c.Search(context.Background(), long, Options{SearchType: SearchTypeWeb}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	failing := serveJSON(t, http.StatusOK, errorEnvelope(10400, "ParamError", "invalid Query: "+secretQuery))
	c = testClient(t, failing.URL, failing.Client())
	if _, err := c.Search(context.Background(), secretQuery, Options{SearchType: SearchTypeWeb}); err == nil {
		t.Fatal("Search() succeeded on a ParamError")
	}

	// Without this the assertions below would hold vacuously if capture broke.
	if !strings.Contains(logs.String(), "[WebSearch][Volcengine]") {
		t.Fatalf("captured no provider logs at all:\n%s", logs.String())
	}
	for _, banned := range []string{secretQuery, long, truncated, body, "secret-landing-page", "test-key"} {
		if strings.Contains(logs.String(), banned) {
			t.Errorf("logs leak %q:\n%s", banned, logs.String())
		}
	}
}

func TestDiscoveryProfileCannotAskTheVendorForPageContent(t *testing.T) {
	// The discovery profile is fixed by construction: DiscoveryProfile has no
	// field for the three settings that would turn discovery into vendor-sourced
	// body text (ADR-0011 §5). Adding one here is the failure this pins.
	profileType := reflect.TypeOf(DiscoveryProfile{})
	for i := 0; i < profileType.NumField(); i++ {
		switch name := profileType.Field(i).Name; name {
		case "SearchType", "NeedContent", "NeedURL":
			t.Errorf("DiscoveryProfile exposes %s; the discovery profile fixes it", name)
		}
	}

	opts := DiscoveryProfile{Count: 20, TimeRange: "OneMonth", Industry: "gov"}.Options()
	if opts.SearchType != SearchTypeWeb {
		t.Errorf("SearchType = %q, want %q", opts.SearchType, SearchTypeWeb)
	}
	if opts.NeedContent {
		t.Error("discovery asked the vendor for page content; the connector must fetch the original page itself")
	}
	if !opts.NeedURL {
		t.Error("discovery must request the URL; it is the only thing the connector can act on")
	}
	if opts.Count != 20 || opts.TimeRange != "OneMonth" || opts.Industry != "gov" {
		t.Errorf("manifest-supplied options were dropped: %+v", opts)
	}
}
