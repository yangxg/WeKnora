package web_search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestValidateSerpAPIParameters(t *testing.T) {
	tests := []struct {
		name    string
		params  types.WebSearchProviderParameters
		wantErr bool
	}{
		{name: "defaults", params: types.WebSearchProviderParameters{APIKey: "key"}},
		{
			name: "google scholar",
			params: types.WebSearchProviderParameters{
				APIKey:      "key",
				ExtraConfig: map[string]string{"engine": "google_scholar"},
			},
		},
		{name: "missing key", params: types.WebSearchProviderParameters{}, wantErr: true},
		{
			name: "invalid engine",
			params: types.WebSearchProviderParameters{
				APIKey:      "key",
				ExtraConfig: map[string]string{"engine": "bing"},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSerpAPIParameters(tt.params)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateSerpAPIParameters() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSerpAPIProviderSearchGoogle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		q := r.URL.Query()
		if q.Get("engine") != "google" {
			t.Errorf("engine = %q, want google", q.Get("engine"))
		}
		if q.Get("api_key") != "test-key" {
			t.Errorf("api_key missing or wrong")
		}
		if q.Get("q") != "hospital policy" {
			t.Errorf("q = %q", q.Get("q"))
		}
		if q.Get("num") != "3" {
			t.Errorf("num = %q, want 3", q.Get("num"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"organic_results": []map[string]any{
				{
					"title":   "Result 1",
					"link":    "https://example.com/1",
					"snippet": "Summary 1",
					"date":    "Jan 2, 2026",
				},
				{
					"title":   "Result 2",
					"link":    "https://example.com/2",
					"snippet": "Summary 2",
				},
				{
					"title": "",
					"link":  "",
				},
			},
		})
	}))
	defer srv.Close()

	provider := &SerpAPIProvider{
		client:  srv.Client(),
		baseURL: srv.URL,
		apiKey:  "test-key",
		engine:  "google",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results, err := provider.Search(ctx, "hospital policy", 3, true)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].URL != "https://example.com/1" || results[0].Snippet != "Summary 1" {
		t.Errorf("first result = %+v", results[0])
	}
	if results[0].Source != "serpapi" {
		t.Errorf("source = %q, want serpapi", results[0].Source)
	}
	if results[0].Content != "" {
		t.Errorf("Content must stay empty (vendor snippet is not governed body)")
	}
	if results[0].PublishedAt == nil || results[0].PublishedAt.Format("2006-01-02") != "2026-01-02" {
		t.Errorf("published_at = %v, want 2026-01-02", results[0].PublishedAt)
	}
}

func TestSerpAPIProviderSearchScholar(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("engine") != "google_scholar" {
			t.Errorf("engine = %q", r.URL.Query().Get("engine"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"organic_results": []map[string]any{
				{
					"title":   "Paper A",
					"link":    "https://scholar.example/a",
					"snippet": "Abstract A",
				},
			},
		})
	}))
	defer srv.Close()

	provider := &SerpAPIProvider{
		client:  srv.Client(),
		baseURL: srv.URL,
		apiKey:  "test-key",
		engine:  "google_scholar",
	}
	results, err := provider.Search(context.Background(), "drg payment", 5, false)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 || results[0].Title != "Paper A" {
		t.Fatalf("results = %+v", results)
	}
}

func TestSerpAPIProviderSearchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Invalid API key."}`))
	}))
	defer srv.Close()

	provider := &SerpAPIProvider{
		client:  srv.Client(),
		baseURL: srv.URL,
		apiKey:  "bad",
		engine:  "google",
	}
	_, err := provider.Search(context.Background(), "test", 1, false)
	if err == nil || !strings.Contains(err.Error(), "Invalid API key") {
		t.Fatalf("Search() error = %v, want Invalid API key", err)
	}
}

func TestSerpAPIProviderSearchDefaults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("num") != "10" {
			t.Errorf("num = %q, want 10", r.URL.Query().Get("num"))
		}
		_, _ = w.Write([]byte(`{"organic_results":[]}`))
	}))
	defer srv.Close()

	provider := &SerpAPIProvider{
		client:  srv.Client(),
		baseURL: srv.URL,
		apiKey:  "test-key",
		engine:  defaultSerpAPIEngine,
	}
	if _, err := provider.Search(context.Background(), "test", 0, false); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
}

func TestParseSerpAPIDate(t *testing.T) {
	for _, value := range []string{"Jan 2, 2026", "January 2, 2026", "2026-01-02"} {
		if _, ok := parseSerpAPIDate(value); !ok {
			t.Errorf("parseSerpAPIDate(%q) failed", value)
		}
	}
	if _, ok := parseSerpAPIDate("2 days ago"); ok {
		t.Error("relative dates must not parse")
	}
}
