package academic

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search"
)

const (
	doiResolverPrefix    = "https://doi.org/"
	pubMedResolverPrefix = "https://pubmed.ncbi.nlm.nih.gov/"
	arXivResolverPrefix  = "https://arxiv.org/abs/"
)

var (
	pmidPattern = regexp.MustCompile(`^\d{1,9}$`)
	// Modern ids are YYMM.number; legacy ids are archive/YYMMNNN. The archive
	// may contain a subject suffix (math.GT) or a hyphen (hep-th), and there is
	// exactly one slash. Anything path-like beyond that is refused rather than
	// URL-escaped into a plausible resolver URL.
	modernArXivPattern = regexp.MustCompile(`^\d{4}\.\d{4,5}$`)
	legacyArXivPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.-]*/\d{7}$`)
)

// candidateReference turns the strongest identity into the stable HTTP resolver
// URL carried by FetchedItem.URL and shown on the card.
//
// HTTP is not incidental: ResearchFlow's inbox gate admits only http(s), keeping
// its W2a3 guard against provider-minted file:/// references closed. A DOI
// resolver URL then folds to the doi: canonical key W2b0 added; PMID and arXiv
// fallbacks stay as their registries' own stable record URLs and must be passed
// back unchanged when the acquired text is promoted (ADR-0013 §3).
func candidateReference(work academic_search.Work) (string, error) {
	_, reference, err := candidateIdentityAndReference(work)
	return reference, err
}

// candidateIdentityAndReference derives the pair the connector stores. Keeping
// the two in one function matters: the external id/cursor key and the resolver
// URL must always name the same identity, or an incremental run could skip one
// record while showing another.
func candidateIdentityAndReference(work academic_search.Work) (string, string, error) {
	if doi := academic_search.NormalizeDOI(work.DOI); doi != "" {
		return "doi:" + doi, doiResolverPrefix + doi, nil
	}
	if pmid := strings.TrimSpace(work.PMID); pmid != "" {
		if !pmidPattern.MatchString(pmid) {
			return "", "", fmt.Errorf("academic: PMID is not a bare numeric identifier")
		}
		return "pmid:" + pmid, pubMedResolverPrefix + pmid + "/", nil
	}
	if arXivID := strings.TrimSpace(work.ArXivID); arXivID != "" {
		if !modernArXivPattern.MatchString(arXivID) && !legacyArXivPattern.MatchString(arXivID) {
			return "", "", fmt.Errorf("academic: arXiv id cannot form a canonical abstract URL")
		}
		return "arxiv:" + arXivID, arXivResolverPrefix + arXivID, nil
	}
	return "", "", fmt.Errorf("academic: work has no usable identity")
}

// renderCard renders the identity-only inbox body.
//
// The card's non-emptiness is the mechanism that prevents WeKnora from fetching
// the resolver URL: the sync service takes its content-from-bytes branch and
// returns before CreateKnowledgeFromURL. So this is not an ersatz work body. It
// is a deterministic bibliographic record that explicitly says it contains no
// work text or abstract (ADR-0013 §2).
//
// Field order is code, not map iteration, because the shared cursor fingerprints
// these bytes. The same identity must render byte-for-byte the same on every run
// or every sync would look like it found new candidates.
func renderCard(work academic_search.Work, reference, provider string) string {
	title := oneLine(work.Title)
	if title == "" {
		title = "Untitled work"
	}

	var card strings.Builder
	card.WriteString("# ")
	card.WriteString(title)
	card.WriteString("\n\n")
	card.WriteString("> Bibliographic identity record. This card does not contain the work's text or abstract.\n\n")
	card.WriteString("## Identity\n\n")
	writeField(&card, "Registry", providerDisplayName(provider))
	writeField(&card, "Reference", reference)
	writeField(&card, "DOI", academic_search.NormalizeDOI(work.DOI))
	writeField(&card, "arXiv ID", oneLine(work.ArXivID))
	writeField(&card, "PMID", oneLine(work.PMID))
	if work.Year > 0 {
		writeField(&card, "Year", strconv.Itoa(work.Year))
	}
	if len(work.Authors) > 0 {
		authors := make([]string, 0, len(work.Authors))
		for _, author := range work.Authors {
			if name := oneLine(author); name != "" {
				authors = append(authors, name)
			}
		}
		writeField(&card, "Authors", strings.Join(authors, "; "))
	}
	writeField(&card, "Venue", oneLine(work.Venue))
	writeField(&card, "Registry type", oneLine(work.Type))
	card.WriteString("\n## Next step\n\n")
	card.WriteString("Acquire the text lawfully, save it under `sources/local/`, then run `rf source promote-file` with the Reference URL above.\n")
	return card.String()
}

func writeField(card *strings.Builder, name, value string) {
	value = oneLine(value)
	if value == "" {
		return
	}
	card.WriteString("- ")
	card.WriteString(name)
	card.WriteString(": ")
	card.WriteString(value)
	card.WriteByte('\n')
}

// oneLine prevents a registry string from breaking out of its field into a
// Markdown section that looks like the work's content. The clients already
// collapse most fields; the projection re-asserts it at the final crossing.
func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func providerDisplayName(provider string) string {
	switch provider {
	case providerOpenAlex:
		return "OpenAlex"
	case providerCrossref:
		return "Crossref"
	case providerPubMed:
		return "PubMed"
	case providerArXiv:
		return "arXiv"
	default:
		return oneLine(provider)
	}
}
