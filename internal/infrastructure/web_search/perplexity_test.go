package web_search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestValidatePerplexityParameters(t *testing.T) {
	tests := []struct {
		name    string
		params  types.WebSearchProviderParameters
		wantErr bool
	}{
		{name: "defaults", params: types.WebSearchProviderParameters{APIKey: "key"}},
		{
			name: "custom model",
			params: types.WebSearchProviderParameters{
				APIKey:      "key",
				ExtraConfig: map[string]string{"model": "sonar-pro"},
			},
		},
		{name: "missing key", params: types.WebSearchProviderParameters{}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePerplexityParameters(tt.params)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidatePerplexityParameters() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPerplexityProviderSearch(t *testing.T) {
	longAnswer := strings.Repeat("答", maxPerplexitySnippetRunes+20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		var request perplexityChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode: %v", err)
		}
		if request.Model != "sonar" {
			t.Errorf("model = %q, want sonar", request.Model)
		}
		if len(request.Messages) != 1 || request.Messages[0].Content != "drg policy" {
			t.Errorf("messages = %+v", request.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"citations": []string{
				"https://www.example.com/a/path",
				"https://www.example.com/a/path", // dup
				"https://other.example/b",
				"",
			},
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": longAnswer}},
			},
		})
	}))
	defer srv.Close()

	provider := &PerplexityProvider{
		client:  srv.Client(),
		baseURL: srv.URL,
		apiKey:  "test-key",
		model:   defaultPerplexityModel,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results, err := provider.Search(ctx, "drg policy", 10, true)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2 (deduped)", len(results))
	}
	if results[0].URL != "https://www.example.com/a/path" {
		t.Errorf("url = %q", results[0].URL)
	}
	if results[0].Source != "perplexity" {
		t.Errorf("source = %q", results[0].Source)
	}
	if results[0].Content != "" {
		t.Error("Content must stay empty")
	}
	if results[0].Title == "" || !strings.Contains(results[0].Title, "example.com") {
		t.Errorf("title = %q, want host-based", results[0].Title)
	}
	if len([]rune(results[0].Snippet)) != maxPerplexitySnippetRunes {
		t.Errorf("snippet runes = %d, want %d", len([]rune(results[0].Snippet)), maxPerplexitySnippetRunes)
	}
}

func TestPerplexityProviderSearchNoCitations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"citations": []string{},
			"choices": []map[string]any{
				{"message": map[string]any{"content": "answer only"}},
			},
		})
	}))
	defer srv.Close()

	provider := &PerplexityProvider{
		client:  srv.Client(),
		baseURL: srv.URL,
		apiKey:  "test-key",
		model:   defaultPerplexityModel,
	}
	results, err := provider.Search(context.Background(), "test", 5, false)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("want empty when no citations, got %d", len(results))
	}
}

func TestPerplexityProviderSearchError(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	provider := &PerplexityProvider{
		client:  srv.Client(),
		baseURL: srv.URL,
		apiKey:  "bad",
		model:   defaultPerplexityModel,
	}
	_, err := provider.Search(context.Background(), "test", 1, false)
	if err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("Search() error = %v, want status 401", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 for non-retryable HTTP error", got)
	}
}

func TestPerplexityProviderRetriesTransportEOF(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("response writer does not support hijacking")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			_ = conn.Close()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"citations": []string{"https://example.com/recovered"},
			"choices":   []map[string]any{{"message": map[string]any{"content": "answer"}}},
		})
	}))
	defer srv.Close()

	provider := &PerplexityProvider{
		client:  srv.Client(),
		baseURL: srv.URL,
		apiKey:  "test-key",
		model:   defaultPerplexityModel,
	}
	results, err := provider.Search(context.Background(), "test", 1, false)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if len(results) != 1 || results[0].URL != "https://example.com/recovered" {
		t.Fatalf("results = %+v", results)
	}
}

func TestPerplexityProviderRetriesTruncatedResponse(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Length", "1024")
			_, _ = w.Write([]byte(`{"citations":["https://example.com/truncated"]`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"citations": []string{"https://example.com/complete"},
			"choices":   []map[string]any{{"message": map[string]any{"content": "answer"}}},
		})
	}))
	defer srv.Close()

	provider := &PerplexityProvider{
		client:  srv.Client(),
		baseURL: srv.URL,
		apiKey:  "test-key",
		model:   defaultPerplexityModel,
	}
	results, err := provider.Search(context.Background(), "test", 1, false)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if len(results) != 1 || results[0].URL != "https://example.com/complete" {
		t.Fatalf("results = %+v", results)
	}
}

func TestPerplexityProviderSearchCapsResults(t *testing.T) {
	cites := make([]string, 15)
	for i := range cites {
		cites[i] = "https://example.com/" + string(rune('a'+i))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"citations": cites,
			"choices":   []map[string]any{{"message": map[string]any{"content": "x"}}},
		})
	}))
	defer srv.Close()

	provider := &PerplexityProvider{
		client:  srv.Client(),
		baseURL: srv.URL,
		apiKey:  "test-key",
		model:   defaultPerplexityModel,
	}
	results, err := provider.Search(context.Background(), "test", 3, false)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len = %d, want 3", len(results))
	}
}

func TestTitleFromURL(t *testing.T) {
	if got := titleFromURL("https://www.example.com/path/to"); !strings.HasPrefix(got, "example.com/") {
		t.Errorf("titleFromURL = %q", got)
	}
	if got := titleFromURL("not-a-url"); got != "not-a-url" {
		t.Errorf("fallback = %q", got)
	}
}
