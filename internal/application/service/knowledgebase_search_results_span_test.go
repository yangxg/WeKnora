package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

// A retrieval consumer that re-reads a hit in the source document needs three
// things to survive the DTO untouched: the chunk body, the range that body
// addresses, and the ingest-time metadata that says which document and revision
// the range belongs to. buildSearchResult is the single place all three cross
// into the API shape, so this pins the passthrough rather than the wiring.
func TestBuildSearchResultCarriesTheChunkRangeAndIngestMetadata(t *testing.T) {
	service := &knowledgeBaseService{}
	chunk := &types.Chunk{
		ID:          "chunk-7",
		KnowledgeID: "kn-1",
		ChunkIndex:  3,
		Content:     "| 一级 | 180 | 142 |",
		StartAt:     412,
		EndAt:       431,
		ChunkType:   types.ChunkTypeText,
	}
	knowledge := &types.Knowledge{
		ID: "kn-1",
		Metadata: types.JSON(`{
			"doc_id": "doc-0f3a",
			"revision": "2",
			"content_hash": "9c1f",
			"locator_scheme": "char"
		}`),
	}

	result := service.buildSearchResult(chunk, knowledge, 0.81, types.MatchTypeEmbedding, "142")

	if result.StartAt != chunk.StartAt || result.EndAt != chunk.EndAt {
		t.Errorf("range [%d,%d) reached the caller as [%d,%d)",
			chunk.StartAt, chunk.EndAt, result.StartAt, result.EndAt)
	}
	if result.Content != chunk.Content {
		t.Errorf("content was rewritten:\n  chunk:  %q\n  result: %q", chunk.Content, result.Content)
	}
	for key, want := range map[string]string{
		"doc_id":         "doc-0f3a",
		"revision":       "2",
		"content_hash":   "9c1f",
		"locator_scheme": "char",
	} {
		if got := result.Metadata[key]; got != want {
			t.Errorf("metadata %q reached the caller as %q, want %q", key, got, want)
		}
	}
}

// Summary, FAQ and table-summary chunks are written by the pipeline rather than
// cut from the document, and carry the empty range that says so. The DTO must
// report that range as it stands: a consumer can only refuse what it can see,
// and an empty range silently widened to the chunk body would read as an anchor.
func TestBuildSearchResultReportsTheEmptyRangeOfSynthesizedChunks(t *testing.T) {
	service := &knowledgeBaseService{}
	chunk := &types.Chunk{
		ID:          "chunk-summary",
		KnowledgeID: "kn-1",
		Content:     "# Summary\n一级与二级合计覆盖 263 项。",
		StartAt:     0,
		EndAt:       0,
		ChunkType:   types.ChunkTypeSummary,
	}

	result := service.buildSearchResult(chunk, &types.Knowledge{ID: "kn-1"}, 0.7,
		types.MatchTypeEmbedding, "")

	if result.StartAt != 0 || result.EndAt != 0 {
		t.Errorf("synthesized chunk reported range [%d,%d), want [0,0)",
			result.StartAt, result.EndAt)
	}
	if result.ChunkType != string(types.ChunkTypeSummary) {
		t.Errorf("chunk type reached the caller as %q, want %q",
			result.ChunkType, types.ChunkTypeSummary)
	}
}
