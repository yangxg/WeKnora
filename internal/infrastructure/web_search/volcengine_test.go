package web_search

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/infrastructure/web_search/volcengine"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type fakeVolcengineSearcher struct {
	calls    int
	gotQuery string
	gotOpts  volcengine.Options
	resp     *volcengine.Response
	err      error
}

func (f *fakeVolcengineSearcher) Search(
	_ context.Context, query string, opts volcengine.Options,
) (*volcengine.Response, error) {
	f.calls++
	f.gotQuery = query
	f.gotOpts = opts
	return f.resp, f.err
}

func TestValidateVolcengineParameters(t *testing.T) {
	tests := []struct {
		name    string
		params  types.WebSearchProviderParameters
		wantErr bool
	}{
		{name: "api key", params: types.WebSearchProviderParameters{APIKey: "key"}},
		{name: "missing key", params: types.WebSearchProviderParameters{}, wantErr: true},
		{name: "blank key", params: types.WebSearchProviderParameters{APIKey: "   "}, wantErr: true},
		{
			name:    "unusable proxy",
			params:  types.WebSearchProviderParameters{APIKey: "key", ProxyURL: "ftp://proxy.internal"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateVolcengineParameters(tt.params); (err != nil) != tt.wantErr {
				t.Fatalf("ValidateVolcengineParameters() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewVolcengineProviderSatisfiesTheProviderInterface(t *testing.T) {
	provider, err := NewVolcengineProvider(types.WebSearchProviderParameters{APIKey: "key"})
	if err != nil {
		t.Fatalf("NewVolcengineProvider() error = %v", err)
	}
	var _ interfaces.WebSearchProvider = provider
	if provider.Name() != "volcengine" {
		t.Errorf("Name() = %q, want volcengine", provider.Name())
	}

	if _, err := NewVolcengineProvider(types.WebSearchProviderParameters{}); err == nil {
		t.Error("NewVolcengineProvider() accepted parameters without an API key")
	}
}

func TestVolcengineProviderNeverAsksTheVendorForPageContent(t *testing.T) {
	searcher := &fakeVolcengineSearcher{resp: &volcengine.Response{}}
	provider := &VolcengineProvider{client: searcher}

	if _, err := provider.Search(context.Background(), "医保支付方式改革", 7, false); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if searcher.calls != 1 {
		t.Fatalf("client calls = %d, want 1", searcher.calls)
	}
	if searcher.gotQuery != "医保支付方式改革" {
		t.Errorf("query = %q, want it passed through unchanged", searcher.gotQuery)
	}
	if searcher.gotOpts.SearchType != volcengine.SearchTypeWeb {
		t.Errorf("SearchType = %q, want %q", searcher.gotOpts.SearchType, volcengine.SearchTypeWeb)
	}
	if searcher.gotOpts.NeedContent {
		t.Error("provider requested vendor page content; it has no governed use for a second-hand body")
	}
	if !searcher.gotOpts.NeedURL {
		t.Error("provider did not request the landing page URL")
	}
	if searcher.gotOpts.Count != 7 {
		t.Errorf("Count = %d, want the caller's maxResults", searcher.gotOpts.Count)
	}
}

func TestVolcengineProviderProjectsResultsOntoWebSearchResult(t *testing.T) {
	published := time.Date(2026, 7, 16, 8, 30, 0, 0, time.UTC)
	searcher := &fakeVolcengineSearcher{resp: &volcengine.Response{
		RequestID: "req-1",
		Results: []volcengine.Result{
			{
				Title:       "国务院办公厅通知",
				URL:         "https://www.gov.cn/a",
				Snippet:     "the recommended summary",
				Content:     "vendor page body",
				PublishedAt: &published,
			},
			{Title: "no date", URL: "https://www.gov.cn/b", Snippet: "another summary"},
		},
		Dropped: 2,
	}}
	provider := &VolcengineProvider{client: searcher}

	results, err := provider.Search(context.Background(), "医保", 10, true)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}

	first := results[0]
	if first.Title != "国务院办公厅通知" || first.URL != "https://www.gov.cn/a" {
		t.Errorf("title/url projection = %+v", first)
	}
	if first.Snippet != "the recommended summary" {
		t.Errorf("Snippet = %q, want the client's summary", first.Snippet)
	}
	// The provider never requests page content, so nothing may arrive in Content
	// even when the vendor volunteers it: a search-side snapshot is not the
	// document any governed citation can point at.
	if first.Content != "" {
		t.Errorf("Content = %q, want empty", first.Content)
	}
	if first.Source != "volcengine" {
		t.Errorf("Source = %q, want volcengine", first.Source)
	}
	if first.PublishedAt == nil || !first.PublishedAt.Equal(published) {
		t.Errorf("PublishedAt = %v, want %v", first.PublishedAt, published)
	}
	if first.PublishedAt == &published {
		t.Error("PublishedAt aliases the client's time; the projection must copy it")
	}
	if results[1].PublishedAt != nil {
		t.Errorf("second PublishedAt = %v, want nil", results[1].PublishedAt)
	}
}

func TestVolcengineProviderHonorsIncludeDate(t *testing.T) {
	published := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	searcher := &fakeVolcengineSearcher{resp: &volcengine.Response{
		Results: []volcengine.Result{
			{Title: "dated", URL: "https://www.gov.cn/a", PublishedAt: &published},
		},
	}}
	provider := &VolcengineProvider{client: searcher}

	results, err := provider.Search(context.Background(), "医保", 1, false)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].PublishedAt != nil {
		t.Errorf("PublishedAt = %v, want nil when the caller did not ask for dates", results[0].PublishedAt)
	}
}

func TestVolcengineProviderRejectsAnEmptyQueryBeforeCallingTheVendor(t *testing.T) {
	searcher := &fakeVolcengineSearcher{resp: &volcengine.Response{}}
	provider := &VolcengineProvider{client: searcher}

	if _, err := provider.Search(context.Background(), "   ", 5, false); err == nil {
		t.Fatal("Search() accepted a blank query")
	}
	if searcher.calls != 0 {
		t.Errorf("client calls = %d, want 0", searcher.calls)
	}
}

func TestVolcengineProviderPropagatesClientErrors(t *testing.T) {
	wanted := &volcengine.APIError{HTTPStatus: 200, CodeN: 10406, Code: "FreeQuotaExhausted"}
	provider := &VolcengineProvider{client: &fakeVolcengineSearcher{err: wanted}}

	_, err := provider.Search(context.Background(), "医保", 5, false)
	if err == nil {
		t.Fatal("Search() swallowed a client error")
	}
	var apiErr *volcengine.APIError
	if !errors.As(err, &apiErr) || apiErr.CodeN != 10406 {
		t.Errorf("error = %v, want the vendor code to survive", err)
	}
}

func TestEmptyTestResultsError_Volcengine(t *testing.T) {
	err := EmptyTestResultsError(string(types.WebSearchProviderTypeVolcengine), nil)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "volcengine returned 0 results") {
		t.Fatalf("unexpected message: %q", msg)
	}
	if !strings.Contains(msg, "quota") {
		t.Fatalf("volcengine message should hint at the monthly free quota: %q", msg)
	}
}
