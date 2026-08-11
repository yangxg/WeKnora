package service

import (
	"context"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// logDocReaderParseSummary records parse shape without copying document,
// caption, or OCR text into application logs.
func logDocReaderParseSummary(
	ctx context.Context,
	knowledgeID string,
	knowledgeBaseID string,
	chunks []types.ParsedChunk,
) {
	chunksWithImages := 0
	totalImages := 0
	for _, chunk := range chunks {
		if len(chunk.Images) > 0 {
			chunksWithImages++
			totalImages += len(chunk.Images)
		}
	}

	logger.Infof(ctx, "[DocReader] ========== 解析结果概览 ==========")
	logger.Infof(ctx, "[DocReader] 知识ID: %s, 知识库ID: %s", knowledgeID, knowledgeBaseID)
	logger.Infof(ctx, "[DocReader] 总Chunk数量: %d", len(chunks))
	logger.Infof(ctx, "[DocReader] 包含图片的Chunk数: %d, 总图片数: %d", chunksWithImages, totalImages)

	for chunkIndex, chunk := range chunks {
		logger.Infof(ctx, "[DocReader] Chunk #%d (seq=%d): 内容长度=%d, 图片数=%d, 范围=[%d-%d]",
			chunkIndex, chunk.Seq, len([]rune(chunk.Content)), len(chunk.Images), chunk.Start, chunk.End)
		for imageIndex, image := range chunk.Images {
			logger.Infof(ctx,
				"[DocReader]   图片 #%d: Caption字符数=%d, OCR字符数=%d, 位置=[%d-%d]",
				imageIndex, len([]rune(image.Caption)), len([]rune(image.OCRText)), image.Start, image.End)
		}
	}
	logger.Infof(ctx, "[DocReader] ========== 解析结果概览结束 ==========")
}

// recordTextShapeMetric keeps trace output useful without retaining
// document, OCR, or caption text outside governed storage.
func recordTextShapeMetric(output types.JSONMap, name string, text string) {
	output[name+"_chars"] = len([]rune(text))
}
