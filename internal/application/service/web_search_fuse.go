package service

import (
	"net/url"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// DefaultRRFK is the classic Reciprocal Rank Fusion constant.
// Vendor scores are not commensurate across providers, so rank fusion is the
// only ordering that does not invent a shared scale.
const DefaultRRFK = 60

// MaxWebSearchAggregateProviders caps fan-out so a misconfigured agent cannot
// burn every registered provider on one query.
const MaxWebSearchAggregateProviders = 4

// rankedHit is one provider's ordered list used as RRF input.
type rankedHit struct {
	ProviderID   string
	ProviderType string
	Results      []*types.WebSearchResult
}

// fuseWebSearchResults merges per-provider hit lists with RRF (k = DefaultRRFK).
//
// Rules:
//   - URL identity is scheme+host(lower)+path (no query/fragment); empty URL rows
//     are dropped rather than inventing a key.
//   - Multi-source hits are not given an extra boost beyond RRF (RRF already
//     rewards agreement); stable sort is (-rrf, first-seen order).
//   - The surviving result keeps the first-seen title/snippet/content; Source is
//     rewritten to a comma-joined sorted set of provider types that contributed.
//   - limit truncates after fusion; limit <= 0 keeps all.
func fuseWebSearchResults(lists []rankedHit, limit int) []*types.WebSearchResult {
	type agg struct {
		result   *types.WebSearchResult
		rrf      float64
		sources  map[string]struct{}
		order    int
	}
	byKey := make(map[string]*agg)
	order := 0

	for _, list := range lists {
		for rank, hit := range list.Results {
			if hit == nil {
				continue
			}
			key := normalizeWebSearchURL(hit.URL)
			if key == "" {
				continue
			}
			entry, ok := byKey[key]
			if !ok {
				// Shallow copy so we can rewrite Source without mutating the
				// provider's own slice elements if a caller reuses them.
				cp := *hit
				if list.ProviderType != "" {
					cp.Source = list.ProviderType
				}
				entry = &agg{
					result:  &cp,
					sources: map[string]struct{}{},
					order:   order,
				}
				order++
				byKey[key] = entry
			}
			entry.rrf += 1.0 / float64(DefaultRRFK+rank+1)
			src := list.ProviderType
			if src == "" {
				src = hit.Source
			}
			if src != "" {
				entry.sources[src] = struct{}{}
			}
		}
	}

	merged := make([]*agg, 0, len(byKey))
	for _, e := range byKey {
		if len(e.sources) > 0 {
			names := make([]string, 0, len(e.sources))
			for s := range e.sources {
				names = append(names, s)
			}
			sort.Strings(names)
			e.result.Source = strings.Join(names, ",")
		}
		merged = append(merged, e)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].rrf != merged[j].rrf {
			return merged[i].rrf > merged[j].rrf
		}
		return merged[i].order < merged[j].order
	})

	out := make([]*types.WebSearchResult, 0, len(merged))
	for _, e := range merged {
		out = append(out, e.result)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// normalizeWebSearchURL collapses a result URL to a fusion key.
// Query and fragment are dropped: the same article reached with tracking
// params must not appear twice.
func normalizeWebSearchURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Fall back to the trimmed string so non-URL but non-empty values still
		// dedupe against themselves rather than vanishing silently.
		return strings.ToLower(raw)
	}
	host := strings.ToLower(u.Hostname())
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	// Strip a trailing slash except for root so /a and /a/ collide.
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		path = strings.TrimSuffix(path, "/")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + host + path
}

// dedupeProviderIDs preserves first-seen order, drops empties, and caps length.
func dedupeProviderIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if len(out) >= MaxWebSearchAggregateProviders {
			break
		}
	}
	return out
}
