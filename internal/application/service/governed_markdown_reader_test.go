package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// governedMarkdownTenantStub satisfies interfaces.TenantService by embedding it:
// only the one method resolveDocReader takes a value of is defined, and any
// other call panics. That is deliberate — the governed `md` path must not reach
// the tenant service at all, so an accidental dependency shows up as a failure
// here rather than as a silent database hit during ingest.
type governedMarkdownTenantStub struct {
	interfaces.TenantService
}

func (*governedMarkdownTenantStub) GetWeKnoraCloudCredentials(
	context.Context,
) *types.WeKnoraCloudCredentials {
	return nil
}

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
	// A tenant service is supplied only because resolveDocReader now builds
	// docparser.ReaderDeps — including the cloud-credential resolver — before
	// NewReader picks a branch. Nothing here is reached for `md`: the simple
	// format branch returns without consulting any dependency, and this stub
	// would panic if it did, which keeps that fact under test.
	service := &knowledgeService{tenantService: &governedMarkdownTenantStub{}}

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
