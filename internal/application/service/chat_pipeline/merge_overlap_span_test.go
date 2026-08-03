package chatpipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mergeSequentialChunks joins neighbouring bodies on purpose and leaves
// StartAt/EndAt where they were — the comment on the function says as much,
// because user edits make the parser coordinates unsafe for deciding what to
// discard. The consequence is worth stating as a test rather than as prose: a
// merged result can no longer be read back at its own range, so a consumer that
// verifies quotes against the source document must retrieve chunks from a
// surface that does not run this pipeline.
//
// If merging ever starts maintaining the range, this test fails, and that is
// the signal to revisit the restriction rather than a regression.
func TestMergeSequentialChunks_LeavesTheRangeBehindTheJoinedContent(t *testing.T) {
	document := "覆盖率为百分之六十二。" + "\n\n" + "分级目录见下表。" + "\n\n" + "拒付率逐年下降。"
	runes := []rune(document)
	first := spanOf(t, document, "覆盖率为百分之六十二。")
	second := spanOf(t, document, "分级目录见下表。")
	third := spanOf(t, document, "拒付率逐年下降。")

	chunks := []*types.SearchResult{
		{
			ID: "c1", KnowledgeID: "kn-1", ChunkIndex: 1, ChunkType: types.ChunkTypeText,
			Content: string(runes[first[0]:first[1]]), StartAt: first[0], EndAt: first[1], Score: 0.9,
		},
		{
			ID: "c2", KnowledgeID: "kn-1", ChunkIndex: 2, ChunkType: types.ChunkTypeText,
			Content: string(runes[second[0]:second[1]]), StartAt: second[0], EndAt: second[1], Score: 0.6,
		},
		{
			ID: "c3", KnowledgeID: "kn-1", ChunkIndex: 3, ChunkType: types.ChunkTypeText,
			Content: string(runes[third[0]:third[1]]), StartAt: third[0], EndAt: third[1], Score: 0.5,
		},
	}
	// Every chunk can be read back at its own range before the merge.
	for _, chunk := range chunks {
		require.Equal(t, string(runes[chunk.StartAt:chunk.EndAt]), chunk.Content,
			"chunk %s does not address its own content to begin with", chunk.ID)
	}

	merged := (&PluginMerge{}).mergeSequentialChunks(context.Background(), "kn-1", chunks)

	require.Len(t, merged, 1, "three sequential chunks join into one result")
	result := merged[0]
	assert.Equal(t, first[0], result.StartAt, "the merged range keeps the first chunk's start")
	assert.Equal(t, first[1], result.EndAt, "the merged range keeps the first chunk's end")
	for _, part := range []string{"覆盖率为百分之六十二。", "分级目录见下表。", "拒付率逐年下降。"} {
		assert.Contains(t, result.Content, part, "the merged body carries every joined chunk")
	}

	addressed := string(runes[result.StartAt:result.EndAt])
	assert.NotEqual(t, addressed, result.Content,
		"the merged body still equals the text its range addresses")
	assert.False(t, strings.HasSuffix(result.Content, addressed),
		"the merged body still ends at the text its range addresses")
}

// spanOf returns the rune range of one substring of the document.
func spanOf(t *testing.T, document, substring string) [2]int {
	t.Helper()
	byteIndex := strings.Index(document, substring)
	require.GreaterOrEqual(t, byteIndex, 0, "substring %q is not in the document", substring)
	start := len([]rune(document[:byteIndex]))
	return [2]int{start, start + len([]rune(substring))}
}
