package service

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestLogDocReaderParseSummaryDoesNotLogExtractedText(t *testing.T) {
	var logs bytes.Buffer
	logger.SetLogLevel(logger.LevelDebug)
	logger.SetOutput(&logs)
	t.Cleanup(logger.ConfigureFromEnv)

	chunks := []types.ParsedChunk{{
		Content: "SECRET-CHUNK-BODY",
		Seq:     7,
		Start:   10,
		End:     27,
		Images: []types.ParsedImage{{
			URL:         "provider://image-1",
			OriginalURL: "provider://image-1",
			Caption:     "SECRET-CAPTION-BODY",
			OCRText:     "SECRET-OCR-BODY",
			Start:       12,
			End:         16,
		}},
	}}

	logDocReaderParseSummary(context.Background(), "knowledge-1", "kb-1", chunks)

	got := logs.String()
	if !strings.Contains(got, "总Chunk数量: 1") {
		t.Fatalf("captured no parse summary logs:\n%s", got)
	}
	for _, body := range []string{"SECRET-CHUNK-BODY", "SECRET-CAPTION-BODY", "SECRET-OCR-BODY"} {
		if strings.Contains(got, body) {
			t.Errorf("parse summary leaked extracted text %q:\n%s", body, got)
		}
	}
	for _, count := range []string{"内容长度=17", "Caption字符数=19", "OCR字符数=15"} {
		if !strings.Contains(got, count) {
			t.Errorf("parse summary lacks %q:\n%s", count, got)
		}
	}
}

func TestRecordTextShapeMetricStoresCountOnly(t *testing.T) {
	out := types.JSONMap{}
	recordTextShapeMetric(out, "ocr", "SECRET-OCR-BODY")
	recordTextShapeMetric(out, "caption", "SECRET-CAPTION-BODY")
	recordTextShapeMetric(out, "chunk", "SECRET-CHUNK-BODY")

	if got := out["ocr_chars"]; got != 15 {
		t.Errorf("ocr_chars = %v, want 15", got)
	}
	if got := out["caption_chars"]; got != 19 {
		t.Errorf("caption_chars = %v, want 19", got)
	}
	if got := out["chunk_chars"]; got != 17 {
		t.Errorf("chunk_chars = %v, want 17", got)
	}
	for key, value := range out {
		if strings.Contains(key, "preview") || strings.Contains(fmt.Sprint(value), "SECRET-") {
			t.Errorf("trace output contains extracted text: %s=%v", key, value)
		}
	}
}
