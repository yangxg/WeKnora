package academic

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search"
)

func completeWork() academic_search.Work {
	return academic_search.Work{
		DOI:     "10.1001/jama.2024.12345",
		ArXivID: "2401.00001",
		PMID:    "38000000",
		Title:   "Deprescribing cascades in tender-driven formularies",
		Year:    2024,
		Authors: []string{"Wei Zhang", "Ana Silva"},
		Venue:   "JAMA",
		Type:    "journal-article",
	}
}

func TestCandidateReferenceUsesTheStrongestIdentityHTTPResolver(t *testing.T) {
	cases := []struct {
		name string
		work academic_search.Work
		want string
	}{
		{"DOI wins", completeWork(), "https://doi.org/10.1001/jama.2024.12345"},
		{"PMID fallback", academic_search.Work{PMID: "38000000"}, "https://pubmed.ncbi.nlm.nih.gov/38000000/"},
		{"arXiv fallback", academic_search.Work{ArXivID: "2401.00001"}, "https://arxiv.org/abs/2401.00001"},
		{"legacy arXiv fallback", academic_search.Work{ArXivID: "math/0309136"}, "https://arxiv.org/abs/math/0309136"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := candidateReference(tc.work)
			if err != nil {
				t.Fatalf("candidateReference() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("reference = %q, want %q", got, tc.want)
			}
			if !strings.HasPrefix(got, "https://") {
				t.Errorf("reference = %q, want an https URL that passes RF's candidate gate", got)
			}
		})
	}
}

func TestCandidateReferenceRefusesMalformedFallbackIdentities(t *testing.T) {
	cases := []academic_search.Work{
		{},
		{PMID: "abc"},
		{PMID: "38000000/../../etc"},
		{ArXivID: ""},
		{ArXivID: "2401 00001"},
		{ArXivID: "../secret"},
		{ArXivID: "https://evil.example/x"},
	}
	for _, work := range cases {
		if got, err := candidateReference(work); err == nil {
			t.Errorf("work %+v produced %q", work, got)
		}
	}
}

// The card is the mechanism that makes WeKnora's CreateKnowledgeFromURL branch
// unreachable: a non-empty Content takes the from-bytes branch and returns first
// (ADR-0013 §2). So even a sparse work has a non-empty card.
func TestBibliographicCardIsNonEmptyAndExplicitlyNotTheWork(t *testing.T) {
	card := renderCard(academic_search.Work{PMID: "38000000"},
		"https://pubmed.ncbi.nlm.nih.gov/38000000/", providerPubMed)
	if strings.TrimSpace(card) == "" {
		t.Fatal("an identity card rendered blank; WeKnora would download the URL itself")
	}
	if !strings.Contains(card, "Bibliographic identity record") {
		t.Errorf("card does not identify what it is:\n%s", card)
	}
	if !strings.Contains(card, "does not contain the work's text or abstract") {
		t.Errorf("card does not say what it is not:\n%s", card)
	}
	if !strings.Contains(card, "Acquire the text lawfully") || !strings.Contains(card, "promote-file") {
		t.Errorf("card does not tell the researcher how to close the handoff:\n%s", card)
	}
}

func TestBibliographicCardRendersEveryCandidateFieldInAFixedOrder(t *testing.T) {
	work := completeWork()
	ref, err := candidateReference(work)
	if err != nil {
		t.Fatal(err)
	}
	card := renderCard(work, ref, providerOpenAlex)

	ordered := []string{
		"# Deprescribing cascades in tender-driven formularies",
		"Bibliographic identity record",
		"- Registry: OpenAlex",
		"- Reference: https://doi.org/10.1001/jama.2024.12345",
		"- DOI: 10.1001/jama.2024.12345",
		"- arXiv ID: 2401.00001",
		"- PMID: 38000000",
		"- Year: 2024",
		"- Authors: Wei Zhang; Ana Silva",
		"- Venue: JAMA",
		"- Registry type: journal-article",
	}
	last := -1
	for _, fragment := range ordered {
		index := strings.Index(card, fragment)
		if index < 0 {
			t.Errorf("card is missing %q:\n%s", fragment, card)
			continue
		}
		if index <= last {
			t.Errorf("%q appeared out of order in:\n%s", fragment, card)
		}
		last = index
	}
}

// Cursor fingerprints the card, so the same work must render byte-for-byte the
// same on every run. No current time, map iteration or random id may enter it.
func TestBibliographicCardIsByteDeterministic(t *testing.T) {
	work := completeWork()
	ref, _ := candidateReference(work)
	first := renderCard(work, ref, providerOpenAlex)
	for range 100 {
		if got := renderCard(work, ref, providerOpenAlex); got != first {
			t.Fatal("the same work rendered differently across calls")
		}
	}
}

func TestBibliographicCardOmitsAbsentFieldsRatherThanInventingValues(t *testing.T) {
	work := academic_search.Work{PMID: "38000000", Title: ""}
	ref, _ := candidateReference(work)
	card := renderCard(work, ref, providerPubMed)
	if !strings.HasPrefix(card, "# Untitled work\n") {
		t.Errorf("sparse card has no deterministic title fallback:\n%s", card)
	}
	for _, invented := range []string{"Unknown author", "Unknown venue", "Year: 0", "DOI: N/A", "arXiv ID: N/A"} {
		if strings.Contains(card, invented) {
			t.Errorf("card invented %q:\n%s", invented, card)
		}
	}
}

// The clients' Work type has nowhere for a body to land. This second assertion
// guards the last projection step: even if a registry fixture contains body-like
// marker text in a field we do carry, the renderer escapes it into one line and
// never promotes it into a section that looks like the work's content.
func TestBibliographicCardKeepsRegistryStringsOnOneLine(t *testing.T) {
	work := completeWork()
	work.Title = "Title\n\n# Forged body heading"
	work.Authors = []string{"Alice\n\nParagraph"}
	ref, _ := candidateReference(work)
	card := renderCard(work, ref, providerOpenAlex)
	if strings.Contains(card, "\n# Forged body heading") || strings.Contains(card, "\n\nParagraph") {
		t.Errorf("registry string broke out of its card field:\n%s", card)
	}
	if !strings.Contains(card, "Title # Forged body heading") {
		t.Errorf("title was not collapsed to one line:\n%s", card)
	}
}
