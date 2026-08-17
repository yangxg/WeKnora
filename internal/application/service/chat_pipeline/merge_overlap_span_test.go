package chatpipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ResearchFlow W1-002.
//
// What ResearchFlow needs from this pipeline is one negative guarantee: a result
// that came through it may not be used to verify a quote against a source
// document, because its StartAt/EndAt need not address its Content. Governed
// retrieval therefore runs `hybrid-search + skip_context_enrichment` (ADR-0010),
// a surface that does not execute this merge at all.
//
// Upstream has since made the trusted path position-aware (b7b85621, e0ea453e,
// 0a3f6b0f): two trusted chunks separated by a position gap are now kept apart
// instead of being joined, and an overlapping pair extends the range it reports.
// The previous version of this test asserted the older, unconditional-join
// behaviour and its own comment named that outcome in advance — "if merging ever
// starts maintaining the range, this test fails, and that is the signal to
// revisit the restriction rather than a regression". This is that revisit.
//
// Both halves below matter to ResearchFlow, in opposite directions:
//   - trusted chunks with a gap stay separate, so each keeps a range that still
//     addresses its own content — an improvement, not a license to cite from here;
//   - an untrusted pair is still joined into a body its range no longer
//     addresses, which is why the restriction stands regardless.
func TestMergeSequentialChunks_TrustedGapStaysSeparate(t *testing.T) {
	document := "覆盖率为百分之六十二。" + "\n\n" + "分级目录见下表。" + "\n\n" + "拒付率逐年下降。"
	runes := []rune(document)
	first := spanOf(t, document, "覆盖率为百分之六十二。")
	second := spanOf(t, document, "分级目录见下表。")
	third := spanOf(t, document, "拒付率逐年下降。")

	chunks := []*types.SearchResult{
		trustedChunk("c1", 1, runes, first, 0.9),
		trustedChunk("c2", 2, runes, second, 0.6),
		trustedChunk("c3", 3, runes, third, 0.5),
	}
	// Every chunk addresses its own content before the merge, which is what
	// makes each one "trusted" for the position-aware path.
	for _, chunk := range chunks {
		require.Equal(t, string(runes[chunk.StartAt:chunk.EndAt]), chunk.Content,
			"chunk %s does not address its own content to begin with", chunk.ID)
	}

	merged := (&PluginMerge{}).mergeSequentialChunks(context.Background(), "kn-1", chunks)

	require.Len(t, merged, 3, "a position gap between trusted chunks must not be joined away")
	for _, result := range merged {
		assert.Equal(t, string(runes[result.StartAt:result.EndAt]), result.Content,
			"result %s stopped addressing its own content", result.ID)
	}
}

// An untrusted pair still takes the text-join path, and the joined body is
// longer than the range reported with it. This is the case that keeps governed
// retrieval off this pipeline.
func TestMergeSequentialChunks_UntrustedJoinLeavesTheRangeBehind(t *testing.T) {
	document := "覆盖率为百分之六十二。" + "\n\n" + "分级目录见下表。"
	runes := []rune(document)
	first := spanOf(t, document, "覆盖率为百分之六十二。")
	second := spanOf(t, document, "分级目录见下表。")

	firstChunk := trustedChunk("c1", 1, runes, first, 0.9)
	// A rewritten body is untrusted by definition: its runes no longer line up
	// with the range the parser recorded.
	firstChunk.ContentRewritten = true
	secondChunk := trustedChunk("c2", 2, runes, second, 0.6)
	secondChunk.ContentRewritten = true

	merged := (&PluginMerge{}).mergeSequentialChunks(
		context.Background(), "kn-1", []*types.SearchResult{firstChunk, secondChunk},
	)

	require.Len(t, merged, 1, "sequential untrusted chunks join into one result")
	result := merged[0]
	for _, part := range []string{"覆盖率为百分之六十二。", "分级目录见下表。"} {
		assert.Contains(t, result.Content, part, "the joined body carries every merged chunk")
	}

	addressed := string(runes[result.StartAt:result.EndAt])
	assert.NotEqual(t, addressed, result.Content,
		"the joined body still equals the text its range addresses")
	assert.False(t, strings.HasSuffix(result.Content, addressed),
		"the joined body still ends at the text its range addresses")
}

// trustedChunk builds a result whose content is exactly the document slice its
// range names, which is what chunkTrusted requires.
func trustedChunk(
	id string, index int, runes []rune, span [2]int, score float64,
) *types.SearchResult {
	return &types.SearchResult{
		ID: id, KnowledgeID: "kn-1", ChunkIndex: index, ChunkType: types.ChunkTypeText,
		Content: string(runes[span[0]:span[1]]), StartAt: span[0], EndAt: span[1], Score: score,
	}
}

// spanOf returns the rune range of one substring of the document.
func spanOf(t *testing.T, document, substring string) [2]int {
	t.Helper()
	byteIndex := strings.Index(document, substring)
	require.GreaterOrEqual(t, byteIndex, 0, "substring %q is not in the document", substring)
	start := len([]rune(document[:byteIndex]))
	return [2]int{start, start + len([]rune(substring))}
}
