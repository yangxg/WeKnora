// Package academic_search holds the vendor-neutral types that the four academic
// registry clients — OpenAlex, Crossref, PubMed and arXiv — project onto.
//
// It sits beside web_search rather than inside it, and the separation is the
// design. The two lanes differ on the only property that matters here: whether
// a candidate may carry a body.
//
// A web search hit is followed by a first-party fetch of the original page, and
// that page becomes the governed document. An academic hit is not. The terms
// under which the registries publish full text forbid the shape web_search uses
// — arXiv forbids storing and serving e-print content from our servers, PMC
// forbids automated retrieval through anything but its own services — and even
// where terms allow it, an abstract standing in for a work would reach every
// downstream reader as "revision 1 of document X" with no field in which to say
// that only an abstract was governed. The locator gate would still pass, so the
// mistake would be silent, which is why it is refused at the type level instead.
//
// So this lane produces identity and nothing else. Acquiring the text is a
// licensed human act, and the acquired file re-enters governance through
// `rf source promote-file` under the same DOI, which is why a normalized DOI is
// the one thing a candidate must not get wrong (ResearchFlow ADR-0012 §2, §3).
//
// This package imports none of its own subpackages. The four clients import it,
// never the reverse, so adding a registry never edits shared types.
package academic_search

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// MaxCount and DefaultCount mirror the bounds the ResearchFlow discovery
	// manifest enforces on [request].count, so a profile that validates there
	// cannot be refused here.
	MaxCount     = 50
	DefaultCount = 25
)

// The kernel work-type vocabulary. Each client maps these onto its own registry's
// terms; a value outside the set is refused rather than passed through, because
// an unmapped type silently widens what a saved query returns.
const (
	WorkTypeJournalArticle  = "journal-article"
	WorkTypePreprint        = "preprint"
	WorkTypeReview          = "review"
	WorkTypeBookChapter     = "book-chapter"
	WorkTypeConferencePaper = "conference-paper"
	WorkTypeDataset         = "dataset"
)

// OpenAccessAny and OpenAccessOnly are the two admissible filter values. There
// is deliberately no "closed" option: a filter that selects *against* open
// access has no research use and would only serve to make a reading list harder
// to act on.
const (
	OpenAccessAny  = "any"
	OpenAccessOnly = "only"
)

var validWorkTypes = map[string]struct{}{
	WorkTypeJournalArticle:  {},
	WorkTypePreprint:        {},
	WorkTypeReview:          {},
	WorkTypeBookChapter:     {},
	WorkTypeConferencePaper: {},
	WorkTypeDataset:         {},
}

var validOpenAccess = map[string]struct{}{
	OpenAccessAny:  {},
	OpenAccessOnly: {},
}

// doiPattern is the same shape ResearchFlow's canonicalize_reference accepts,
// matched against the lowercased DOI. The suffix allowlist is narrower than DOI
// syntax permits on purpose: it covers the punctuation real DOIs carry —
// including the legacy SICI form — and refuses everything else. Refusing an
// exotic-but-valid DOI costs a candidate its strongest identity, which the
// caller notices; accepting one and mangling it would corrupt a deduplication
// key, which nobody notices.
//
// The two sides must agree. A DOI this client emits but ResearchFlow refuses
// would strand a candidate that can never be matched to a promoted file.
var doiPattern = regexp.MustCompile(`^10\.\d{4,9}(?:\.\d+)*/[a-z0-9._;:()<>\[\]/+=,'*-]+$`)

// doiResolverPrefixes are stripped before matching. OpenAlex returns its `doi`
// field as a resolver URL, so this is the common case rather than an edge one.
var doiResolverPrefixes = []string{
	"https://doi.org/", "http://doi.org/",
	"https://dx.doi.org/", "http://dx.doi.org/",
	"https://www.doi.org/", "http://www.doi.org/",
}

// Work is one academic work's identity. It has no field for an abstract, a
// snippet or a body, and that absence is the enforcement of ADR-0012 §2 — not a
// convention a future edit can quietly drop.
type Work struct {
	// DOI is the bare, lowercased DOI: no `doi:` prefix, no resolver host. It
	// may be empty — older PubMed records and a few e-prints have none.
	DOI string
	// ArXivID and PMID are fallback identities when no DOI is registered. Like
	// the DOI they are identifiers, not locations.
	ArXivID string
	PMID    string
	// Title lets a person recognize the work in a reading list.
	Title string
	// Year is the only registry-declared metadata that goes on to carry
	// governance: it becomes published_at when a human promotes the acquired
	// file. Everything below it stops at the candidate layer (ADR-0012 §4).
	Year int
	// Authors, Venue and Type are registry *claims*. They make a reading list
	// usable and they never enter a Document, an Evidence, a manifest or
	// references.json — the kernel has no way to verify any of them, and an
	// unverified author list in an export is worse than an absent one because
	// it looks authoritative and no gate checks it.
	//
	// Type holds the registry's own term rather than a kernel one: mapping it
	// would be an interpretation, and what a candidate can honestly report is
	// what the registry said.
	Authors []string
	Venue   string
	Type    string
}

// Response is one page of results from one registry.
type Response struct {
	// Works are the candidates, in the order the registry returned them.
	Works []Work
	// Total is the registry's own count of matches, when it reports one.
	Total int
	// Dropped counts records refused for carrying no usable identity. A record
	// nobody can name again is refused and counted rather than passed on as a
	// reading-list entry that can never be closed out.
	Dropped int
}

// Identity returns the strongest identity this work carries, as
// `doi:10.x/y`, `arxiv:2401.00001` or `pmid:38000000`.
//
// A work with none is not usable as a candidate: it cannot be matched against
// the file a researcher later acquires and promotes, which is the entire
// purpose of the hand-off (ADR-0012 §2). Callers drop it.
func (w Work) Identity() (string, bool) {
	if doi := strings.TrimSpace(w.DOI); doi != "" {
		return "doi:" + doi, true
	}
	if arxivID := strings.TrimSpace(w.ArXivID); arxivID != "" {
		return "arxiv:" + arxivID, true
	}
	if pmid := strings.TrimSpace(w.PMID); pmid != "" {
		return "pmid:" + pmid, true
	}
	return "", false
}

// NormalizeDOI returns the bare lowercased DOI, or "" when the value is not one.
//
// Percent escapes are refused rather than decoded: decoding invites a
// double-decode ambiguity in a key that must be stable, and a candidate without
// a DOI still has its other identities to fall back on.
func NormalizeDOI(value string) string {
	candidate := strings.ToLower(strings.TrimSpace(value))
	if candidate == "" {
		return ""
	}
	for _, prefix := range doiResolverPrefixes {
		if rest, ok := strings.CutPrefix(candidate, prefix); ok {
			candidate = rest
			break
		}
	}
	if rest, ok := strings.CutPrefix(candidate, "doi:"); ok {
		candidate = strings.TrimSpace(rest)
	}
	if !doiPattern.MatchString(candidate) {
		return ""
	}
	return candidate
}

// Options is the request profile shared by the four clients.
//
// There is no NeedContent, no NeedFullText and no IncludeAbstract field. The
// clients cannot request a body because they have no way to express the request.
type Options struct {
	// Count is the number of works wanted; 0 means DefaultCount.
	Count int
	// From and To bound the publication date. A zero value means unbounded.
	From time.Time
	To   time.Time
	// WorkTypes filters by the kernel vocabulary above; empty means no filter.
	WorkTypes []string
	// OpenAccess is OpenAccessAny (default) or OpenAccessOnly.
	OpenAccess string
}

// Validate reports whether the profile is within the documented ranges.
//
// It is exported so a connector can refuse a bad manifest at materialization
// time. Validating late would spend a request to learn what a struct already
// knows, and on metered registries a request is money.
func (o Options) Validate() error {
	if _, err := o.Normalized(); err != nil {
		return err
	}
	return nil
}

// Normalized returns the profile with defaults filled in, or an error. Each
// client calls it before building a request so the defaults are decided once.
func (o Options) Normalized() (Options, error) {
	if o.Count < 0 {
		return o, fmt.Errorf("academic_search: count %d must not be negative", o.Count)
	}
	if o.Count > MaxCount {
		return o, fmt.Errorf("academic_search: count %d exceeds the maximum of %d", o.Count, MaxCount)
	}
	if o.Count == 0 {
		o.Count = DefaultCount
	}
	for _, workType := range o.WorkTypes {
		if _, ok := validWorkTypes[workType]; !ok {
			return o, fmt.Errorf("academic_search: unsupported work type %q", workType)
		}
	}
	if o.OpenAccess == "" {
		o.OpenAccess = OpenAccessAny
	}
	if _, ok := validOpenAccess[o.OpenAccess]; !ok {
		return o, fmt.Errorf("academic_search: unsupported open access filter %q", o.OpenAccess)
	}
	if !o.From.IsZero() && !o.To.IsZero() && o.To.Before(o.From) {
		return o, fmt.Errorf("academic_search: the date window ends before it starts")
	}
	return o, nil
}
