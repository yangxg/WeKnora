package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/types"
)

// ResearchFlow W3-001.
//
// A governed revision is uploaded as `<document_id>-r<revision>.md` with no
// parser engine named, so every ResearchFlow ingest resolves through the default
// branch below. That resolution is the whole reason ResearchFlow's char and cell
// locators can address the bytes it stored: SimpleFormatReader returns Markdown
// verbatim, so no offset in the governed copy differs from the local one.
//
// This is a certification test, not a feature. If upstream ever routes `md`
// through docreader — which rewrites images, normalizes tables and reflows text
// — every stored offset silently stops pointing at what it addressed. That must
// break here, loudly, rather than at a citation months later.
func TestGovernedMarkdownResolvesToTheVerbatimGoReader(t *testing.T) {
	// The zero value suffices: this branch returns before touching any
	// dependency, which is itself part of what makes the path predictable.
	service := &knowledgeService{}

	for _, fileType := range []string{"md", "markdown", ".MD"} {
		t.Run(fileType, func(t *testing.T) {
			// engine "" is what ResolveParserEngine returns when a knowledge base
			// declares no rule for this type — ResearchFlow declares none.
			reader := service.resolveDocReader(context.Background(), "", fileType, false, nil)
			if _, ok := reader.(*docparser.SimpleFormatReader); !ok {
				t.Fatalf("governed markdown resolved to %T, want *docparser.SimpleFormatReader", reader)
			}
		})
	}
}

func TestSimpleFormatReaderReturnsGovernedMarkdownByteForByte(t *testing.T) {
	// Trailing spaces, CRLF, a table and a wide-character heading: everything a
	// normalizing parser would be tempted to clean up, and every cleanup would
	// move an offset ResearchFlow already recorded.
	governed := "# 参保情况  \r\n\r\n| tier | covered |\r\n| --- | ---: |\r\n| A | 142 |\r\n"

	result, err := (&docparser.SimpleFormatReader{}).Read(context.Background(), &types.ReadRequest{
		FileName:    "doc-0123456789abcdef-r1.md",
		FileType:    "md",
		FileContent: []byte(governed),
	})
	if err != nil {
		t.Fatalf("reading governed markdown failed: %v", err)
	}
	if result.MarkdownContent != governed {
		t.Fatalf("governed markdown was rewritten:\n got %q\nwant %q", result.MarkdownContent, governed)
	}
}
