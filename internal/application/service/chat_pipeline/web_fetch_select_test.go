package chatpipeline

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectWebResultsForFetchPrefersRerank(t *testing.T) {
	rerankA := &types.SearchResult{ID: "https://rerank.example/a", KnowledgeSource: "web_search", Content: "ra"}
	rerankB := &types.SearchResult{ID: "https://rerank.example/b", KnowledgeSource: "web_search", Content: "rb"}
	searchA := &types.SearchResult{ID: "https://search.example/a", KnowledgeSource: "web_search", Content: "sa"}

	chatManage := &types.ChatManage{
		PipelineState: types.PipelineState{
			RerankResult: []*types.SearchResult{rerankA, rerankB},
			SearchResult: []*types.SearchResult{searchA},
		},
	}

	hits, source := selectWebResultsForFetch(chatManage, 2)
	require.Len(t, hits, 2)
	assert.Equal(t, "rerank", source)
	assert.Equal(t, []string{rerankA.ID, rerankB.ID}, searchResultIDs(hits))
	// Pointers alias pipeline state so content writes stick.
	hits[0].Content = "enriched"
	assert.Equal(t, "enriched", chatManage.RerankResult[0].Content)
}

func TestSelectWebResultsForFetchFallsBackToSearchWhenRerankEmpty(t *testing.T) {
	kbHit := &types.SearchResult{ID: "chunk-1", KnowledgeSource: "knowledge", Content: "kb"}
	web1 := &types.SearchResult{ID: "https://example.com/1", KnowledgeSource: "web_search", Content: "s1"}
	web2 := &types.SearchResult{ID: "https://example.com/2", KnowledgeSource: "web_search", Content: "s2"}
	web3 := &types.SearchResult{ID: "https://example.com/3", KnowledgeSource: "web_search", Content: "s3"}

	chatManage := &types.ChatManage{
		PipelineState: types.PipelineState{
			RerankResult: nil, // rerank skipped (empty model id)
			SearchResult: []*types.SearchResult{kbHit, web1, web2, web3},
		},
	}

	hits, source := selectWebResultsForFetch(chatManage, 2)
	require.Len(t, hits, 2)
	assert.Equal(t, "search", source)
	assert.Equal(t, []string{web1.ID, web2.ID}, searchResultIDs(hits))
	// Only web_search rows; KB hit skipped.
	hits[0].Content = "fetched-body"
	assert.Equal(t, "fetched-body", chatManage.SearchResult[1].Content)
}

func TestSelectWebResultsForFetchFallsBackWhenRerankHasNoWeb(t *testing.T) {
	// Rerank produced only KB hits; web still lives on SearchResult.
	chatManage := &types.ChatManage{
		PipelineState: types.PipelineState{
			RerankResult: []*types.SearchResult{
				{ID: "kb-only", KnowledgeSource: "knowledge"},
			},
			SearchResult: []*types.SearchResult{
				{ID: "https://example.com/web", KnowledgeSource: "web_search", Content: "snip"},
			},
		},
	}

	hits, source := selectWebResultsForFetch(chatManage, 3)
	require.Len(t, hits, 1)
	assert.Equal(t, "search", source)
	assert.Equal(t, "https://example.com/web", hits[0].ID)
}

func TestSelectWebResultsForFetchNoWebAnywhere(t *testing.T) {
	chatManage := &types.ChatManage{
		PipelineState: types.PipelineState{
			RerankResult: []*types.SearchResult{{ID: "k1", KnowledgeSource: "knowledge"}},
			SearchResult: []*types.SearchResult{{ID: "k2", KnowledgeSource: "knowledge"}},
		},
	}
	hits, source := selectWebResultsForFetch(chatManage, 3)
	assert.Empty(t, hits)
	assert.Equal(t, "", source)
}

func TestSelectWebResultsForFetchDefaultTopN(t *testing.T) {
	results := make([]*types.SearchResult, 5)
	for i := range results {
		results[i] = &types.SearchResult{
			ID:              "https://example.com/" + string(rune('a'+i)),
			KnowledgeSource: "web_search",
		}
	}
	// Force default via topN<=0 path through helper's own guard.
	chatManage := &types.ChatManage{
		PipelineState: types.PipelineState{SearchResult: results},
	}
	hits, source := selectWebResultsForFetch(chatManage, 0)
	require.Len(t, hits, 3)
	assert.Equal(t, "search", source)
}
