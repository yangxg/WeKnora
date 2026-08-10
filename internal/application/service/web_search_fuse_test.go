package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestNormalizeWebSearchURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://Example.COM/a/b?utm=1#x", "https://example.com/a/b"},
		{"https://example.com/a/", "https://example.com/a"},
		{"https://example.com/", "https://example.com/"},
		{"", ""},
		{"  ", ""},
	}
	for _, c := range cases {
		if got := normalizeWebSearchURL(c.in); got != c.want {
			t.Fatalf("normalizeWebSearchURL(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestFuseWebSearchResults_RRFAndDedupe(t *testing.T) {
	lists := []rankedHit{
		{
			ProviderID:   "t1",
			ProviderType: "tavily",
			Results: []*types.WebSearchResult{
				{Title: "A", URL: "https://example.com/a", Source: "tavily"},
				{Title: "B", URL: "https://example.com/b", Source: "tavily"},
			},
		},
		{
			ProviderID:   "v1",
			ProviderType: "volcengine",
			Results: []*types.WebSearchResult{
				// Same as tavily #1 with tracking params → must collapse.
				{Title: "A-volc", URL: "https://example.com/a?ref=1", Source: "volcengine"},
				{Title: "C", URL: "https://example.com/c", Source: "volcengine"},
			},
		},
	}
	got := fuseWebSearchResults(lists, 10)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3 (A shared, B, C)", len(got))
	}
	// A appears in both lists at rank 1 → highest RRF.
	if got[0].URL != "https://example.com/a?ref=1" && normalizeWebSearchURL(got[0].URL) != "https://example.com/a" {
		// First-seen URL is from tavily list.
		if normalizeWebSearchURL(got[0].URL) != "https://example.com/a" {
			t.Fatalf("top URL key = %q want example.com/a", got[0].URL)
		}
	}
	if got[0].Source != "tavily,volcengine" {
		t.Fatalf("shared Source = %q want tavily,volcengine", got[0].Source)
	}
	// Limit truncates after fusion.
	limited := fuseWebSearchResults(lists, 1)
	if len(limited) != 1 {
		t.Fatalf("limit 1 → len=%d", len(limited))
	}
}

func TestFuseWebSearchResults_EmptyAndNoURL(t *testing.T) {
	if got := fuseWebSearchResults(nil, 5); len(got) != 0 {
		t.Fatalf("nil lists → %d", len(got))
	}
	lists := []rankedHit{{
		ProviderType: "tavily",
		Results: []*types.WebSearchResult{
			{Title: "no-url", URL: ""},
			nil,
			{Title: "ok", URL: "https://ok.example/x"},
		},
	}}
	got := fuseWebSearchResults(lists, 0)
	if len(got) != 1 || got[0].Title != "ok" {
		t.Fatalf("drop empty URL: %+v", got)
	}
}

func TestDedupeProviderIDs(t *testing.T) {
	ids := []string{"", " a ", "b", "a", "c", "d", "e", "f"}
	got := dedupeProviderIDs(ids)
	if len(got) != MaxWebSearchAggregateProviders {
		t.Fatalf("len=%d want cap %d: %v", len(got), MaxWebSearchAggregateProviders, got)
	}
	if got[0] != "a" || got[1] != "b" {
		t.Fatalf("order: %v", got)
	}
}
