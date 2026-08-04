package openalex

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search"
	"github.com/Tencent/WeKnora/internal/logger"
)

const (
	secretQuery = "off-label-prescribing-in-provincial-tenders"

	// apiKey is a secret, and on OpenAlex it travels as a query parameter — the
	// one placement that lands in an access log at every hop. That makes the
	// log-leak assertion at the bottom of this file the load-bearing one.
	apiKey = "SECRET-OPENALEX-KEY-abc123"
)

// invertedAbstract is the field this client exists to not read.
//
// OpenAlex ships the abstract as an inverted index — word to positions — which
// reconstructs verbatim with a sort. It is not called "abstract", so a
// name-based guard would miss it; what stops it is that no field is selected for
// it and no struct field can hold it.
var invertedAbstract = map[string]any{
	"SECRETWORD": []any{0},
	"INVERTED":   []any{1},
	"ABSTRACT":   []any{2},
}

func workListBody(count int, results ...map[string]any) string {
	if results == nil {
		// An empty page is `"results": []`, not `null` — the distinction is what
		// TestSearchDistinguishesAnEmptyPageFromAMissingOne is about, so the
		// fixture must not blur it.
		results = []map[string]any{}
	}
	body, err := json.Marshal(map[string]any{
		"meta": map[string]any{
			"count": count, "db_response_time_ms": 12, "page": 1, "per_page": len(results),
		},
		"results": results,
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func fullResult() map[string]any {
	return map[string]any{
		"id":  "https://openalex.org/W2741809807",
		"doi": "https://doi.org/10.1001/JAMA.2024.12345",
		"ids": map[string]any{
			"openalex": "https://openalex.org/W2741809807",
			"doi":      "https://doi.org/10.1001/jama.2024.12345",
			"pmid":     "https://pubmed.ncbi.nlm.nih.gov/38000000",
			"mag":      2741809807,
		},
		"display_name":     "Deprescribing cascades in\n     tender-driven formularies",
		"publication_year": 2024,
		"type":             "article",
		"authorships": []any{
			map[string]any{
				"author_position": "first",
				"author":          map[string]any{"display_name": "Wei Zhang", "orcid": "https://orcid.org/0000"},
				// Affiliations are personal data we neither need nor keep. They
				// arrive because `select` is top-level only; the decoder drops them.
				"raw_affiliation_strings": []any{"SECRETWORD Institute of Health Policy"},
			},
			map[string]any{"author": map[string]any{"display_name": "Ana Silva"}},
		},
		"primary_location": map[string]any{
			"source":           map[string]any{"display_name": "JAMA", "issn_l": "0098-7484"},
			"landing_page_url": "https://example.org/landing",
		},
		"open_access":             map[string]any{"is_oa": true, "oa_status": "gold"},
		"abstract_inverted_index": invertedAbstract,
	}
}

type capture struct{ query url.Values }

func serveJSON(t *testing.T, status int, body string) (*httptest.Server, *capture) {
	t.Helper()
	seen := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func testClient(t *testing.T, endpoint string, httpClient *http.Client) *Client {
	t.Helper()
	c, err := NewClient(Config{HTTPClient: httpClient, APIKey: apiKey})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	c.endpoint = endpoint
	c.pacer = academic_search.NewPacer(0)
	return c
}

// A key-less OpenAlex client is not a cheaper client, it is a tenth of the daily
// budget presenting itself as "nothing was published on this topic" (ADR-0012 §5
// gap 1). Refusing at construction is the whole point of that invariant.
func TestNewClientRejectsUnusableConfigurations(t *testing.T) {
	if _, err := NewClient(Config{APIKey: apiKey}); err == nil {
		t.Error("NewClient() accepted a nil HTTP client")
	}
	if _, err := NewClient(Config{HTTPClient: http.DefaultClient}); err == nil {
		t.Error("NewClient() accepted a missing API key; a keyless caller silently runs at $0.10/day")
	}
	if _, err := NewClient(Config{HTTPClient: http.DefaultClient, APIKey: "   "}); err == nil {
		t.Error("NewClient() accepted a blank API key")
	}
}

func TestNewClientPacesAtTheCourtesyInterval(t *testing.T) {
	c, err := NewClient(Config{HTTPClient: http.DefaultClient, APIKey: apiKey})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if got := c.pacer.Interval(); got != MinRequestInterval {
		t.Fatalf("pacing interval = %v, want %v", got, MinRequestInterval)
	}
	if MinRequestInterval <= 0 {
		t.Fatal("OpenAlex meters by spend rather than rate, but an unpaced loop still burns a day's budget in seconds")
	}
}

func TestSearchBuildsTheDocumentedRequest(t *testing.T) {
	srv, seen := serveJSON(t, http.StatusOK, workListBody(137, fullResult()))
	c := testClient(t, srv.URL, srv.Client())

	if _, err := c.Search(context.Background(), "deprescribing", academic_search.Options{Count: 7}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := seen.query.Get("search"); got != "deprescribing" {
		t.Errorf("search = %q", got)
	}
	// per_page, with an underscore: the hyphenated spelling is the older one.
	if got := seen.query.Get("per_page"); got != "7" {
		t.Errorf("per_page = %q, want 7", got)
	}
	if got := seen.query.Get("sort"); got != "publication_date:desc" {
		t.Errorf("sort = %q, want publication_date:desc", got)
	}
	if got := seen.query.Get("api_key"); got != apiKey {
		t.Errorf("api_key = %q, want the configured key", got)
	}
}

// select is the outermost of three defences against the inverted abstract: a
// field never requested is never sent, so it cannot be reconstructed by anyone.
func TestSearchSelectsIdentityFieldsOnly(t *testing.T) {
	srv, seen := serveJSON(t, http.StatusOK, workListBody(1, fullResult()))
	c := testClient(t, srv.URL, srv.Client())

	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	selected := strings.Split(seen.query.Get("select"), ",")
	if len(selected) == 0 || selected[0] == "" {
		t.Fatal("no select parameter was sent; OpenAlex would return every field including the inverted abstract")
	}
	want := map[string]bool{
		"id": true, "doi": true, "ids": true, "display_name": true,
		"publication_year": true, "type": true, "authorships": true, "primary_location": true,
	}
	for _, field := range selected {
		if !want[field] {
			t.Errorf("select asks for %q, which is not an identity field", field)
		}
		lowered := strings.ToLower(field)
		for _, banned := range []string{"abstract", "inverted", "fulltext", "best_oa_location", "locations"} {
			if strings.Contains(lowered, banned) {
				t.Errorf("select asks for %q; the academic lane carries identity only", field)
			}
		}
	}
}

// The second defence, pinned structurally: the wire struct has no field the
// inverted abstract could decode into, so a server that ignores `select` — or a
// future edit that widens it — still cannot land the abstract anywhere.
func TestTheWireStructCannotHoldAnAbstract(t *testing.T) {
	banned := []string{"abstract", "inverted", "summary", "snippet", "content", "body", "fulltext"}
	var walk func(typ reflect.Type, path string, depth int)
	walk = func(typ reflect.Type, path string, depth int) {
		for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || depth > 6 {
			return
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.ToLower(field.Name + " " + field.Tag.Get("json"))
			for _, ban := range banned {
				if strings.Contains(name, ban) {
					t.Errorf("%s.%s is body-shaped (ADR-0012 §2)", path, field.Name)
				}
			}
			walk(field.Type, path+"."+field.Name, depth+1)
		}
	}
	walk(reflect.TypeOf(workList{}), "workList", 0)
}

func TestSearchRendersTheFiltersOpenAlexCanExpress(t *testing.T) {
	srv, seen := serveJSON(t, http.StatusOK, workListBody(1, fullResult()))
	c := testClient(t, srv.URL, srv.Client())

	_, err := c.Search(context.Background(), "x", academic_search.Options{
		From: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC),
		WorkTypes: []string{
			academic_search.WorkTypeJournalArticle,
			academic_search.WorkTypePreprint,
			academic_search.WorkTypeBookChapter,
		},
		OpenAccess: academic_search.OpenAccessOnly,
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	filter := seen.query.Get("filter")
	for _, want := range []string{
		"from_publication_date:2024-01-01",
		"to_publication_date:2024-12-31",
		"open_access.is_oa:true",
		// OR within one field is a pipe, not repeated terms: repeating
		// `type:` with a comma is AND, and no work has two types, so a
		// comma here would return nothing at all.
		"type:article|preprint|book-chapter",
	} {
		if !strings.Contains(filter, want) {
			t.Errorf("filter %q is missing %q", filter, want)
		}
	}

	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if _, ok := seen.query["filter"]; ok {
		t.Errorf("an unfiltered search sent filter=%q", seen.query.Get("filter"))
	}
}

// OpenAlex's `type` has no enum in its OpenAPI spec — only six documented
// "common values". A type we cannot cite documentation for is refused rather than
// sent: a filter on a value OpenAlex does not use matches nothing, and "no
// reviews on this topic" is a wrong answer that looks like a right one.
func TestSearchRefusesWorkTypesOpenAlexDoesNotDocument(t *testing.T) {
	srv, seen := serveJSON(t, http.StatusOK, workListBody(1, fullResult()))
	c := testClient(t, srv.URL, srv.Client())

	for _, workType := range []string{
		academic_search.WorkTypeReview,
		academic_search.WorkTypeConferencePaper,
	} {
		if _, err := c.Search(context.Background(), "x",
			academic_search.Options{WorkTypes: []string{workType}}); err == nil {
			t.Errorf("work type %q was accepted without documentation", workType)
		}
	}
	if seen.query != nil {
		t.Error("a refusable profile still reached the registry and spent budget")
	}
}

func TestSearchProjectsIdentityAndNeverTheAbstract(t *testing.T) {
	srv, _ := serveJSON(t, http.StatusOK, workListBody(137, fullResult()))
	c := testClient(t, srv.URL, srv.Client())

	resp, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(resp.Works) != 1 {
		t.Fatalf("got %d works, want 1", len(resp.Works))
	}
	work := resp.Works[0]

	if work.DOI != "10.1001/jama.2024.12345" {
		t.Errorf("DOI = %q, want the bare lowercased form", work.DOI)
	}
	if work.PMID != "38000000" {
		t.Errorf("PMID = %q, want the bare id stripped of its resolver URL", work.PMID)
	}
	if work.Title != "Deprescribing cascades in tender-driven formularies" {
		t.Errorf("Title = %q", work.Title)
	}
	if work.Year != 2024 {
		t.Errorf("Year = %d, want 2024", work.Year)
	}
	if work.Venue != "JAMA" {
		t.Errorf("Venue = %q, want the primary location's source", work.Venue)
	}
	if work.Type != "article" {
		t.Errorf("Type = %q, want the registry's own term", work.Type)
	}
	if len(work.Authors) != 2 || work.Authors[0] != "Wei Zhang" || work.Authors[1] != "Ana Silva" {
		t.Errorf("Authors = %v", work.Authors)
	}
	// The DOI wins the identity even though a PMID is present.
	if identity, _ := work.Identity(); identity != "doi:10.1001/jama.2024.12345" {
		t.Errorf("Identity() = %q", identity)
	}
	if resp.Total != 137 {
		t.Errorf("Total = %d, want the registry's 137", resp.Total)
	}

	// The third defence, checked end to end: no token of the inverted abstract,
	// and no raw affiliation string, appears anywhere in the projection.
	fields := append([]string{work.DOI, work.PMID, work.ArXivID, work.Title, work.Venue, work.Type}, work.Authors...)
	for _, field := range fields {
		for token := range invertedAbstract {
			if strings.Contains(field, token) {
				t.Errorf("the inverted abstract reached the projection: %q contains %q", field, token)
			}
		}
	}
}

func TestSearchFallsBackToThePMIDWhenThereIsNoDOI(t *testing.T) {
	cases := map[string]any{
		"resolver url":          "https://pubmed.ncbi.nlm.nih.gov/38000000",
		"resolver url slashed":  "https://pubmed.ncbi.nlm.nih.gov/38000000/",
		"bare id":               "38000000",
		"bare id with the noun": "pmid:38000000",
	}
	for name, pmid := range cases {
		t.Run(name, func(t *testing.T) {
			result := fullResult()
			delete(result, "doi")
			result["ids"] = map[string]any{"pmid": pmid}

			srv, _ := serveJSON(t, http.StatusOK, workListBody(1, result))
			c := testClient(t, srv.URL, srv.Client())
			resp, err := c.Search(context.Background(), "x", academic_search.Options{})
			if err != nil {
				t.Fatalf("Search() error = %v", err)
			}
			if len(resp.Works) != 1 {
				t.Fatalf("got %d works, want 1", len(resp.Works))
			}
			if got := resp.Works[0].PMID; got != "38000000" {
				t.Errorf("PMID = %q, want 38000000", got)
			}
			if identity, _ := resp.Works[0].Identity(); identity != "pmid:38000000" {
				t.Errorf("Identity() = %q, want pmid:38000000", identity)
			}
		})
	}
}

func TestSearchDropsRecordsWithoutAnIdentity(t *testing.T) {
	// An OpenAlex work id is a location in OpenAlex, not an identity for the
	// work: a researcher cannot promote a PDF under it, so it does not count.
	idOnly := fullResult()
	delete(idOnly, "doi")
	delete(idOnly, "ids")

	mangled := fullResult()
	mangled["doi"] = "https://doi.org/not-a-doi"
	delete(mangled, "ids")

	notANumber := fullResult()
	delete(notANumber, "doi")
	notANumber["ids"] = map[string]any{"pmid": "https://pubmed.ncbi.nlm.nih.gov/abc"}

	srv, _ := serveJSON(t, http.StatusOK, workListBody(4, fullResult(), idOnly, mangled, notANumber))
	c := testClient(t, srv.URL, srv.Client())

	resp, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(resp.Works) != 1 {
		t.Fatalf("got %d works, want only the identified one", len(resp.Works))
	}
	if resp.Dropped != 3 {
		t.Errorf("Dropped = %d, want 3", resp.Dropped)
	}
}

// An empty page and a missing page are different answers. Reporting a truncated
// envelope as zero results would read as "nothing published on this topic".
func TestSearchDistinguishesAnEmptyPageFromAMissingOne(t *testing.T) {
	empty, _ := serveJSON(t, http.StatusOK, workListBody(0))
	resp, err := testClient(t, empty.URL, empty.Client()).
		Search(context.Background(), "x", academic_search.Options{})
	if err != nil {
		t.Fatalf("an empty page was refused: %v", err)
	}
	if len(resp.Works) != 0 || resp.Total != 0 {
		t.Errorf("empty page projected as %+v", resp)
	}

	for _, body := range []string{`{"meta":{"count":3}}`, `{"results":[]}`, `{}`} {
		srv, _ := serveJSON(t, http.StatusOK, body)
		c := testClient(t, srv.URL, srv.Client())
		c.maxAttempts = 1
		if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err == nil {
			t.Errorf("the truncated envelope %s was accepted", body)
		}
	}
}

func TestSearchClassifiesTransportFailures(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadRequest, false},
		// A budget that ran out is not a transient condition; retrying only makes
		// the next day's first request fail too.
		{http.StatusForbidden, false},
		{http.StatusUnauthorized, false},
	}
	for _, tc := range cases {
		srv, _ := serveJSON(t, tc.status,
			`{"error":"Invalid query parameters error","message":"filter for `+secretQuery+`"}`)
		c := testClient(t, srv.URL, srv.Client())
		c.maxAttempts = 1

		_, err := c.Search(context.Background(), secretQuery, academic_search.Options{})
		if err == nil {
			t.Fatalf("status %d was accepted", tc.status)
		}
		apiErr, ok := err.(*academic_search.APIError)
		if !ok {
			t.Fatalf("status %d gave %T, want *academic_search.APIError", tc.status, err)
		}
		if apiErr.Retryable != tc.retryable {
			t.Errorf("status %d retryable = %v, want %v", tc.status, apiErr.Retryable, tc.retryable)
		}
		if apiErr.Source != Source {
			t.Errorf("Source = %q, want %q", apiErr.Source, Source)
		}
		if strings.Contains(apiErr.Error(), secretQuery) {
			t.Errorf("status %d leaked the query: %v", tc.status, apiErr)
		}
	}
}

func TestSearchRetriesTransientFailuresThenSucceeds(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workListBody(1, fullResult())))
	}))
	t.Cleanup(srv.Close)

	var slept []time.Duration
	c := testClient(t, srv.URL, srv.Client())
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	for _, delay := range slept {
		if delay != 4*time.Second {
			t.Errorf("slept %v, want the registry's requested 4s each time", slept)
		}
	}
}

func TestSearchRefusesOversizedAndMalformedResponses(t *testing.T) {
	oversized, _ := serveJSON(t, http.StatusOK, strings.Repeat("a", maxResponseBytes+1))
	c := testClient(t, oversized.URL, oversized.Client())
	c.maxAttempts = 1
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err == nil {
		t.Error("an oversized response was accepted")
	}

	malformed, _ := serveJSON(t, http.StatusOK, `{"meta":{"count":1},"results":[{`)
	c = testClient(t, malformed.URL, malformed.Client())
	c.maxAttempts = 1
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err == nil {
		t.Error("malformed JSON was accepted")
	}
}

func TestSearchValidatesTheQueryAndCount(t *testing.T) {
	srv, seen := serveJSON(t, http.StatusOK, workListBody(1, fullResult()))
	c := testClient(t, srv.URL, srv.Client())

	if _, err := c.Search(context.Background(), "  ", academic_search.Options{}); err == nil {
		t.Error("a blank query was accepted")
	}
	if _, err := c.Search(context.Background(), "x",
		academic_search.Options{Count: academic_search.MaxCount + 1}); err == nil {
		t.Error("a count above the manifest ceiling was accepted")
	}
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := seen.query.Get("per_page"); got != "25" {
		t.Errorf("per_page = %q, want the default 25", got)
	}
}

func TestSearchLogsNeitherTheQueryNorTheKey(t *testing.T) {
	var logs bytes.Buffer
	logger.SetLogLevel(logger.LevelDebug)
	logger.SetOutput(&logs)
	t.Cleanup(logger.ConfigureFromEnv)

	ok, _ := serveJSON(t, http.StatusOK, workListBody(1, fullResult()))
	if _, err := testClient(t, ok.URL, ok.Client()).
		Search(context.Background(), secretQuery, academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	failing, _ := serveJSON(t, http.StatusBadRequest,
		`{"error":"Invalid query","message":"bad filter for `+secretQuery+`"}`)
	failClient := testClient(t, failing.URL, failing.Client())
	failClient.maxAttempts = 1
	if _, err := failClient.Search(context.Background(), secretQuery, academic_search.Options{}); err == nil {
		t.Fatal("a 400 was accepted")
	}

	if !strings.Contains(logs.String(), "[AcademicSearch][OpenAlex]") {
		t.Fatalf("captured no client logs at all:\n%s", logs.String())
	}
	// The key is the one that matters here: it rides in the URL, so any log line
	// that echoes a request URL would expose it.
	for _, banned := range []string{secretQuery, apiKey, "SECRETWORD", "Invalid query", "bad filter"} {
		if strings.Contains(logs.String(), banned) {
			t.Errorf("logs leak %q:\n%s", banned, logs.String())
		}
	}
}
