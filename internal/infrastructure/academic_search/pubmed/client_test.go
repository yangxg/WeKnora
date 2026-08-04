package pubmed

import (
	"bytes"
	"context"
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

	// apiKey is a secret and, like OpenAlex's, it travels as a query parameter.
	apiKey = "SECRET-NCBI-KEY-abc123"

	// toolName and devEmail are the registered identity. They are not secrets —
	// NCBI requires them to be registered offline and uses the address to warn a
	// developer before blocking an IP (ADR-0012 §9 item 5).
	toolName = "ResearchFlow"
	devEmail = "research-flow@example.org"
)

const esearchBody = `{
  "header": {"type": "esearch", "version": "0.3"},
  "esearchresult": {
    "count": "137",
    "retmax": "2",
    "retstart": "0",
    "idlist": ["38000000", "37999999"],
    "translationset": [],
    "querytranslation": "SECRET-TRANSLATED-QUERY[All Fields]"
  }
}`

const esummaryBody = `{
  "header": {"type": "esummary", "version": "0.3"},
  "result": {
    "uids": ["38000000", "37999999"],
    "38000000": {
      "uid": "38000000",
      "pubdate": "2024 Mar 15",
      "epubdate": "2024 Feb 01",
      "source": "JAMA",
      "authors": [
        {"name": "Zhang W", "authtype": "Author"},
        {"name": "Silva A", "authtype": "Author"},
        {"name": "National Health Commission", "authtype": "CollectiveName"}
      ],
      "title": "Deprescribing cascades in\n     tender-driven formularies",
      "fulljournalname": "JAMA : the journal of the American Medical Association",
      "elocationid": "doi: 10.1001/jama.2024.12345",
      "articleids": [
        {"idtype": "pubmed", "value": "38000000"},
        {"idtype": "doi", "value": "10.1001/JAMA.2024.12345"},
        {"idtype": "pmc", "value": "PMC11111"}
      ],
      "pubtype": ["Journal Article", "Review"]
    },
    "37999999": {
      "uid": "37999999",
      "pubdate": "2023",
      "source": "Lancet",
      "authors": [{"name": "Doe J", "authtype": "Author"}],
      "title": "A work with no DOI at all",
      "articleids": [{"idtype": "pubmed", "value": "37999999"}],
      "pubtype": ["Journal Article"]
    }
  }
}`

type capture struct {
	paths   []string
	esearch url.Values
	summary url.Values
}

// serveEUtils routes on the two script names the client is allowed to call. A
// request to anything else — efetch above all — is recorded and answered 404, so
// the test can assert on the set of endpoints actually used.
func serveEUtils(t *testing.T, esearch, esummary string, status int) (*httptest.Server, *capture) {
	t.Helper()
	seen := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.paths = append(seen.paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "esearch.fcgi"):
			seen.esearch = r.URL.Query()
			w.WriteHeader(status)
			_, _ = w.Write([]byte(esearch))
		case strings.HasSuffix(r.URL.Path, "esummary.fcgi"):
			seen.summary = r.URL.Query()
			w.WriteHeader(status)
			_, _ = w.Write([]byte(esummary))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func testClient(t *testing.T, baseURL string, httpClient *http.Client) *Client {
	t.Helper()
	c, err := NewClient(Config{
		HTTPClient: httpClient, APIKey: apiKey, ToolName: toolName, Email: devEmail,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	c.baseURL = baseURL + "/"
	c.pacer = academic_search.NewPacer(0)
	return c
}

func TestNewClientRejectsUnusableConfigurations(t *testing.T) {
	valid := Config{HTTPClient: http.DefaultClient, APIKey: apiKey, ToolName: toolName, Email: devEmail}
	if _, err := NewClient(valid); err != nil {
		t.Fatalf("a valid configuration was refused: %v", err)
	}

	cases := map[string]Config{
		"no http client": {APIKey: apiKey, ToolName: toolName, Email: devEmail},
		// A key-less client runs at 3 rps and, per NCBI, an unregistered caller
		// that breaches policy gets its IP blocked (ADR-0012 §8).
		"no api key": {HTTPClient: http.DefaultClient, ToolName: toolName, Email: devEmail},
		"no tool":    {HTTPClient: http.DefaultClient, APIKey: apiKey, Email: devEmail},
		// NCBI requires tool to be a string with no internal spaces.
		"tool with a space": {
			HTTPClient: http.DefaultClient, APIKey: apiKey, ToolName: "Research Flow", Email: devEmail},
		"no email":  {HTTPClient: http.DefaultClient, APIKey: apiKey, ToolName: toolName},
		"bad email": {HTTPClient: http.DefaultClient, APIKey: apiKey, ToolName: toolName, Email: "research-flow"},
		"email with a space": {
			HTTPClient: http.DefaultClient, APIKey: apiKey, ToolName: toolName, Email: "a b@example.org"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewClient(cfg); err == nil {
				t.Fatal("NewClient() accepted it")
			}
		})
	}
}

func TestNewClientPacesUnderTheKeyedCeiling(t *testing.T) {
	c, err := NewClient(Config{
		HTTPClient: http.DefaultClient, APIKey: apiKey, ToolName: toolName, Email: devEmail,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if got := c.pacer.Interval(); got != MinRequestInterval {
		t.Fatalf("pacing interval = %v, want %v", got, MinRequestInterval)
	}
	// Ten requests per second is the keyed ceiling; sitting exactly on it leaves
	// nothing for anything else sharing the address.
	if MinRequestInterval < 100*time.Millisecond {
		t.Errorf("MinRequestInterval = %v, which exceeds the documented 10 rps", MinRequestInterval)
	}
}

func TestSearchBuildsTheDocumentedESearchRequest(t *testing.T) {
	srv, seen := serveEUtils(t, esearchBody, esummaryBody, http.StatusOK)
	c := testClient(t, srv.URL, srv.Client())

	if _, err := c.Search(context.Background(), "deprescribing", academic_search.Options{Count: 7}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	q := seen.esearch
	if got := q.Get("db"); got != "pubmed" {
		t.Errorf("db = %q", got)
	}
	if got := q.Get("term"); got != "deprescribing" {
		t.Errorf("term = %q", got)
	}
	if got := q.Get("retmode"); got != "json" {
		t.Errorf("retmode = %q, want json", got)
	}
	if got := q.Get("retmax"); got != "7" {
		t.Errorf("retmax = %q, want 7", got)
	}
	if got := q.Get("sort"); got != "pub_date" {
		t.Errorf("sort = %q, want pub_date; discovery wants what is new", got)
	}
	if got := q.Get("api_key"); got != apiKey {
		t.Errorf("api_key = %q", got)
	}
	if got := q.Get("tool"); got != toolName {
		t.Errorf("tool = %q, want the registered tool name", got)
	}
	if got := q.Get("email"); got != devEmail {
		t.Errorf("email = %q, want the registered address", got)
	}
	// The same identity has to ride on the second call too, or half our traffic
	// is anonymous to NCBI.
	for _, key := range []string{"api_key", "tool", "email"} {
		if seen.summary.Get(key) != q.Get(key) {
			t.Errorf("esummary sent %s = %q, esearch sent %q", key, seen.summary.Get(key), q.Get(key))
		}
	}
}

// mindate and maxdate are documented as a pair: sending one alone is not a
// half-open window, it is an ignored filter. A synthetic far endpoint keeps the
// pair intact without excluding anything real.
func TestSearchRendersTheDateWindowAsAPair(t *testing.T) {
	srv, seen := serveEUtils(t, esearchBody, esummaryBody, http.StatusOK)
	c := testClient(t, srv.URL, srv.Client())

	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name             string
		opts             academic_search.Options
		mindate, maxdate string
	}{
		{"both ends", academic_search.Options{From: from, To: to}, "2024/01/01", "2024/12/31"},
		{"open ended", academic_search.Options{From: from}, "2024/01/01", "9999/12/31"},
		{"open started", academic_search.Options{To: to}, "1900/01/01", "2024/12/31"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Search(context.Background(), "x", tc.opts); err != nil {
				t.Fatalf("Search() error = %v", err)
			}
			if got := seen.esearch.Get("datetype"); got != "pdat" {
				t.Errorf("datetype = %q, want pdat", got)
			}
			if got := seen.esearch.Get("mindate"); got != tc.mindate {
				t.Errorf("mindate = %q, want %q", got, tc.mindate)
			}
			if got := seen.esearch.Get("maxdate"); got != tc.maxdate {
				t.Errorf("maxdate = %q, want %q", got, tc.maxdate)
			}
		})
	}

	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	for _, key := range []string{"mindate", "maxdate", "datetype"} {
		if _, ok := seen.esearch[key]; ok {
			t.Errorf("an unbounded search sent %s = %q", key, seen.esearch.Get(key))
		}
	}
}

func TestSearchAppendsPublicationTypeTags(t *testing.T) {
	srv, seen := serveEUtils(t, esearchBody, esummaryBody, http.StatusOK)
	c := testClient(t, srv.URL, srv.Client())

	_, err := c.Search(context.Background(), "deprescribing", academic_search.Options{
		WorkTypes: []string{academic_search.WorkTypeJournalArticle, academic_search.WorkTypeReview},
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	term := seen.esearch.Get("term")
	if !strings.HasPrefix(term, "deprescribing AND (") {
		t.Errorf("term = %q, want the query followed by a parenthesised type clause", term)
	}
	for _, want := range []string{`"Journal Article"[pt]`, `"Review"[pt]`} {
		if !strings.Contains(term, want) {
			t.Errorf("term %q is missing %q", term, want)
		}
	}
	// OR, not AND: a record is rarely both types, so an AND would return nothing
	// and read as an empty topic.
	if !strings.Contains(term, " OR ") {
		t.Errorf("term %q joins the types with something other than OR", term)
	}
}

// Fail closed on the vocabulary we could not verify. A [pt] value PubMed does not
// use matches nothing, and "no datasets on this topic" is a wrong answer wearing
// the clothes of a right one.
func TestSearchRefusesFiltersPubMedCannotExpress(t *testing.T) {
	srv, seen := serveEUtils(t, esearchBody, esummaryBody, http.StatusOK)
	c := testClient(t, srv.URL, srv.Client())

	for _, workType := range []string{
		academic_search.WorkTypeBookChapter,
		academic_search.WorkTypeConferencePaper,
		academic_search.WorkTypeDataset,
	} {
		if _, err := c.Search(context.Background(), "x",
			academic_search.Options{WorkTypes: []string{workType}}); err == nil {
			t.Errorf("work type %q was accepted", workType)
		}
	}
	// PubMed's free-full-text filters select availability, not licence.
	if _, err := c.Search(context.Background(), "x",
		academic_search.Options{OpenAccess: academic_search.OpenAccessOnly}); err == nil {
		t.Error("an open-access-only profile was accepted")
	}
	if seen.esearch != nil {
		t.Error("a refusable profile still reached the registry")
	}
}

// The endpoint choice *is* the boundary. ESummary returns document summaries with
// no abstract; EFetch is the call that would return one, and this client has no
// code path that reaches it (ADR-0012 §2).
func TestSearchCallsOnlyESearchAndESummary(t *testing.T) {
	srv, seen := serveEUtils(t, esearchBody, esummaryBody, http.StatusOK)
	c := testClient(t, srv.URL, srv.Client())

	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(seen.paths) != 2 {
		t.Fatalf("called %d endpoints (%v), want exactly esearch then esummary", len(seen.paths), seen.paths)
	}
	if !strings.HasSuffix(seen.paths[0], "esearch.fcgi") || !strings.HasSuffix(seen.paths[1], "esummary.fcgi") {
		t.Errorf("endpoints = %v", seen.paths)
	}
	for _, path := range seen.paths {
		if strings.Contains(path, "efetch") {
			t.Errorf("efetch was called; it is the endpoint that returns abstracts")
		}
	}
	// The ids from esearch are what esummary is asked about, comma-joined.
	if got := seen.summary.Get("id"); got != "38000000,37999999" {
		t.Errorf("esummary id = %q, want the ids esearch returned", got)
	}
	if got := seen.summary.Get("retmode"); got != "json" {
		t.Errorf("esummary retmode = %q, want json", got)
	}
}

func TestSearchProjectsIdentityAndNeverTheAbstract(t *testing.T) {
	srv, _ := serveEUtils(t, esearchBody, esummaryBody, http.StatusOK)
	c := testClient(t, srv.URL, srv.Client())

	resp, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(resp.Works) != 2 {
		t.Fatalf("got %d works, want 2", len(resp.Works))
	}
	if resp.Total != 137 {
		t.Errorf("Total = %d, want the count esearch reported", resp.Total)
	}
	// Order follows the uids list, so a reading list stays in relevance order.
	first, second := resp.Works[0], resp.Works[1]

	if first.DOI != "10.1001/jama.2024.12345" {
		t.Errorf("DOI = %q, want the normalized form from articleids", first.DOI)
	}
	if first.PMID != "38000000" {
		t.Errorf("PMID = %q", first.PMID)
	}
	if first.Title != "Deprescribing cascades in tender-driven formularies" {
		t.Errorf("Title = %q", first.Title)
	}
	if first.Year != 2024 {
		t.Errorf("Year = %d, want the year parsed off \"2024 Mar 15\"", first.Year)
	}
	// fulljournalname over source: it is the one a person can act on.
	if first.Venue != "JAMA : the journal of the American Medical Association" {
		t.Errorf("Venue = %q", first.Venue)
	}
	if first.Type != "Journal Article" {
		t.Errorf("Type = %q, want the registry's own first pubtype", first.Type)
	}
	wantAuthors := []string{"Zhang W", "Silva A", "National Health Commission"}
	if len(first.Authors) != len(wantAuthors) {
		t.Fatalf("Authors = %v, want %v", first.Authors, wantAuthors)
	}
	for i, want := range wantAuthors {
		if first.Authors[i] != want {
			t.Errorf("Authors[%d] = %q, want %q", i, first.Authors[i], want)
		}
	}

	// A record with no DOI still has its PMID, so it stays on the reading list.
	if second.DOI != "" {
		t.Errorf("second DOI = %q, want empty", second.DOI)
	}
	if identity, _ := second.Identity(); identity != "pmid:37999999" {
		t.Errorf("second Identity() = %q, want pmid:37999999", identity)
	}
	if second.Year != 2023 {
		t.Errorf("second Year = %d, want the year parsed off a bare \"2023\"", second.Year)
	}
	if resp.Dropped != 0 {
		t.Errorf("Dropped = %d, want 0", resp.Dropped)
	}
}

// The wire structs are the second line of defence. Neither the abstract nor
// esearch's querytranslation — which echoes the saved query back, expanded — has
// a field to decode into.
func TestTheWireStructsCannotHoldAnAbstractOrTheQueryTranslation(t *testing.T) {
	banned := []string{
		"abstract", "summary", "snippet", "content", "body", "fulltext",
		"querytranslation", "translation",
	}
	var walk func(typ reflect.Type, path string, depth int)
	walk = func(typ reflect.Type, path string, depth int) {
		for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
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
					t.Errorf("%s.%s is body-shaped or echoes the query (ADR-0012 §2, ADR-0009 §9)",
						path, field.Name)
				}
			}
			walk(field.Type, path+"."+field.Name, depth+1)
		}
	}
	walk(reflect.TypeOf(esearchEnvelope{}), "esearchEnvelope", 0)
	walk(reflect.TypeOf(esummaryEnvelope{}), "esummaryEnvelope", 0)
	walk(reflect.TypeOf(summaryRecord{}), "summaryRecord", 0)
}

// Nothing matched is an answer, not a failure — and asking esummary about an
// empty id list is a wasted request against a 10 rps budget.
func TestSearchSkipsTheSummaryCallWhenNothingMatched(t *testing.T) {
	empty := `{"esearchresult":{"count":"0","retmax":"0","retstart":"0","idlist":[]}}`
	srv, seen := serveEUtils(t, empty, esummaryBody, http.StatusOK)
	c := testClient(t, srv.URL, srv.Client())

	resp, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(resp.Works) != 0 || resp.Total != 0 {
		t.Errorf("resp = %+v, want an empty page", resp)
	}
	if len(seen.paths) != 1 {
		t.Errorf("called %v, want esearch only", seen.paths)
	}
}

func TestSearchDropsSummaryRecordsThatReportAnError(t *testing.T) {
	broken := `{"result":{"uids":["38000000","37999999"],
      "38000000":{"uid":"38000000","error":"cannot get document summary"},
      "37999999":{"uid":"37999999","pubdate":"2023","source":"Lancet",
        "articleids":[{"idtype":"pubmed","value":"37999999"}],"pubtype":["Journal Article"]}}}`
	srv, _ := serveEUtils(t, esearchBody, broken, http.StatusOK)
	c := testClient(t, srv.URL, srv.Client())

	resp, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(resp.Works) != 1 {
		t.Fatalf("got %d works, want the one that came back whole", len(resp.Works))
	}
	if resp.Dropped != 1 {
		t.Errorf("Dropped = %d, want 1", resp.Dropped)
	}
}

// NCBI reports an exceeded rate inside an HTTP 200, so the status alone never
// decides success. It is retryable — unlike a bad request — because backing off
// is exactly the remedy.
func TestSearchRefusesTheRateLimitEnvelope(t *testing.T) {
	limited := `{"error":"API rate limit exceeded","count":"11"}`
	srv, _ := serveEUtils(t, limited, esummaryBody, http.StatusOK)
	c := testClient(t, srv.URL, srv.Client())
	c.maxAttempts = 1

	_, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err == nil {
		t.Fatal("a rate-limit envelope was accepted as a result page")
	}
	apiErr, ok := err.(*academic_search.APIError)
	if !ok {
		t.Fatalf("error type = %T, want *academic_search.APIError", err)
	}
	if !apiErr.Retryable {
		t.Error("an exceeded rate is exactly the case where backing off helps")
	}
	if apiErr.Code != "RateLimitExceeded" {
		t.Errorf("Code = %q, want RateLimitExceeded", apiErr.Code)
	}
}

func TestSearchRefusesAnESearchErrorEnvelope(t *testing.T) {
	bad := `{"esearchresult":{"ERROR":"Invalid term for ` + secretQuery + `"}}`
	srv, _ := serveEUtils(t, bad, esummaryBody, http.StatusOK)
	c := testClient(t, srv.URL, srv.Client())
	c.maxAttempts = 1

	_, err := c.Search(context.Background(), secretQuery, academic_search.Options{})
	if err == nil {
		t.Fatal("an esearchresult error was accepted")
	}
	if strings.Contains(err.Error(), secretQuery) {
		t.Errorf("the error leaks the query: %v", err)
	}
	apiErr, ok := err.(*academic_search.APIError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if apiErr.Retryable {
		t.Error("a rejected term is terminal")
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
		// A blocked IP is a decision by a person at NCBI; retrying into it is
		// what earned the block.
		{http.StatusForbidden, false},
	}
	for _, tc := range cases {
		srv, _ := serveEUtils(t, "Error: "+secretQuery, esummaryBody, tc.status)
		c := testClient(t, srv.URL, srv.Client())
		c.maxAttempts = 1

		_, err := c.Search(context.Background(), secretQuery, academic_search.Options{})
		if err == nil {
			t.Fatalf("status %d was accepted", tc.status)
		}
		apiErr, ok := err.(*academic_search.APIError)
		if !ok {
			t.Fatalf("status %d gave %T", tc.status, err)
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

func TestSearchRetriesTransientFailuresOnBothCalls(t *testing.T) {
	var esearchHits, esummaryHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "esearch.fcgi") {
			esearchHits++
			if esearchHits < 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(esearchBody))
			return
		}
		esummaryHits++
		if esummaryHits < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(esummaryBody))
	}))
	t.Cleanup(srv.Close)

	c := testClient(t, srv.URL, srv.Client())
	c.sleep = func(context.Context, time.Duration) error { return nil }
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if esearchHits != 2 || esummaryHits != 2 {
		t.Errorf("esearch tried %d times and esummary %d, want 2 each", esearchHits, esummaryHits)
	}
}

func TestSearchRefusesOversizedAndMalformedResponses(t *testing.T) {
	oversized, _ := serveEUtils(t, strings.Repeat("a", maxResponseBytes+1), esummaryBody, http.StatusOK)
	c := testClient(t, oversized.URL, oversized.Client())
	c.maxAttempts = 1
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err == nil {
		t.Error("an oversized response was accepted")
	}

	malformed, _ := serveEUtils(t, `{"esearchresult":{`, esummaryBody, http.StatusOK)
	c = testClient(t, malformed.URL, malformed.Client())
	c.maxAttempts = 1
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err == nil {
		t.Error("malformed esearch JSON was accepted")
	}

	brokenSummary, _ := serveEUtils(t, esearchBody, `{"result":{`, http.StatusOK)
	c = testClient(t, brokenSummary.URL, brokenSummary.Client())
	c.maxAttempts = 1
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err == nil {
		t.Error("malformed esummary JSON was accepted")
	}
}

func TestSearchValidatesTheQueryAndCount(t *testing.T) {
	srv, seen := serveEUtils(t, esearchBody, esummaryBody, http.StatusOK)
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
	if got := seen.esearch.Get("retmax"); got != "25" {
		t.Errorf("retmax = %q, want the default 25", got)
	}
}

func TestSearchLogsNeitherTheQueryNorTheKey(t *testing.T) {
	var logs bytes.Buffer
	logger.SetLogLevel(logger.LevelDebug)
	logger.SetOutput(&logs)
	t.Cleanup(logger.ConfigureFromEnv)

	ok, _ := serveEUtils(t, esearchBody, esummaryBody, http.StatusOK)
	if _, err := testClient(t, ok.URL, ok.Client()).
		Search(context.Background(), secretQuery, academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	bad, _ := serveEUtils(t, `{"esearchresult":{"ERROR":"Invalid term for `+secretQuery+`"}}`,
		esummaryBody, http.StatusOK)
	failClient := testClient(t, bad.URL, bad.Client())
	failClient.maxAttempts = 1
	if _, err := failClient.Search(context.Background(), secretQuery, academic_search.Options{}); err == nil {
		t.Fatal("an error envelope was accepted")
	}

	if !strings.Contains(logs.String(), "[AcademicSearch][PubMed]") {
		t.Fatalf("captured no client logs at all:\n%s", logs.String())
	}
	for _, banned := range []string{
		secretQuery, apiKey, "SECRET-TRANSLATED-QUERY", "Invalid term",
	} {
		if strings.Contains(logs.String(), banned) {
			t.Errorf("logs leak %q:\n%s", banned, logs.String())
		}
	}
}
