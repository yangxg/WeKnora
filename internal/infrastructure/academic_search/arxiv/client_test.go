package arxiv

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search"
	"github.com/Tencent/WeKnora/internal/logger"
)

// secretQuery stands in for a saved query. Everything this file asserts about
// leaks is asserted against it: a search reveals what a project is working on,
// so it may not reach a log line or an error message (ADR-0009 §9).
const secretQuery = "off-label-prescribing-in-provincial-tenders"

// feedWith wraps entries in the Atom envelope arXiv returns. The abstract is
// always present in these fixtures on purpose — the point of most assertions
// below is that it has nowhere to land.
func feedWith(entries ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"
      xmlns:opensearch="http://a9.com/-/spec/opensearch/1.1/"
      xmlns:arxiv="http://arxiv.org/schemas/atom">
  <opensearch:totalResults>137</opensearch:totalResults>
  <opensearch:startIndex>0</opensearch:startIndex>
  ` + strings.Join(entries, "\n") + `
</feed>`
}

const fullEntry = `<entry>
    <id>http://arxiv.org/abs/2401.00001v3</id>
    <published>2024-01-02T18:00:00Z</published>
    <updated>2024-03-02T18:00:00Z</updated>
    <title>Deprescribing cascades in tender-driven formularies</title>
    <summary>SECRET-ABSTRACT-TEXT that must never reach a Work.</summary>
    <author><name>Wei Zhang</name></author>
    <author><name>Ana Silva</name></author>
    <arxiv:doi>10.1016/j.jclinepi.2024.99999</arxiv:doi>
    <arxiv:journal_ref>J Clin Epi 170 (2024) 111-120</arxiv:journal_ref>
    <arxiv:primary_category term="stat.AP"/>
    <link href="http://arxiv.org/abs/2401.00001v3" rel="alternate" type="text/html"/>
    <link title="pdf" href="http://arxiv.org/pdf/2401.00001v3" rel="related" type="application/pdf"/>
  </entry>`

// serveXML returns a server that records the request it was given.
func serveXML(t *testing.T, status int, body string) (*httptest.Server, *url.Values) {
	t.Helper()
	seen := &url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.URL.Query()
		w.Header().Set("Content-Type", "application/atom+xml")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

// testClient builds a client aimed at a fake server with pacing disabled. The
// real interval is asserted separately; making every test wait three seconds
// would only prove that time passes.
func testClient(t *testing.T, endpoint string, httpClient *http.Client) *Client {
	t.Helper()
	c, err := NewClient(Config{HTTPClient: httpClient})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	c.endpoint = endpoint
	c.pacer = academic_search.NewPacer(0)
	return c
}

func TestNewClientRequiresAnHTTPClient(t *testing.T) {
	if _, err := NewClient(Config{}); err == nil {
		t.Fatal("NewClient() accepted a nil HTTP client; the caller owns network policy")
	}
}

// The interval is a term of use, not a tuning knob: arXiv counts one request
// every three seconds across every machine under our control (ADR-0012 §5).
func TestClientPacesAtTheDocumentedInterval(t *testing.T) {
	c, err := NewClient(Config{HTTPClient: http.DefaultClient})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if got := c.pacer.Interval(); got != MinRequestInterval {
		t.Fatalf("pacing interval = %v, want %v", got, MinRequestInterval)
	}
	if MinRequestInterval != 3*time.Second {
		t.Fatalf("MinRequestInterval = %v, want the documented 3s", MinRequestInterval)
	}
}

func TestSearchBuildsTheDocumentedRequest(t *testing.T) {
	srv, seen := serveXML(t, http.StatusOK, feedWith(fullEntry))
	c := testClient(t, srv.URL, srv.Client())

	if _, err := c.Search(context.Background(), "deprescribing", academic_search.Options{Count: 7}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if got := seen.Get("search_query"); got != `all:"deprescribing"` {
		t.Errorf("search_query = %q", got)
	}
	if got := seen.Get("max_results"); got != "7" {
		t.Errorf("max_results = %q, want 7", got)
	}
	if got := seen.Get("start"); got != "0" {
		t.Errorf("start = %q, want 0", got)
	}
	if got := seen.Get("sortBy"); got != "submittedDate" {
		t.Errorf("sortBy = %q, want submittedDate; discovery wants what is new", got)
	}
	if got := seen.Get("sortOrder"); got != "descending" {
		t.Errorf("sortOrder = %q, want descending", got)
	}
}

func TestSearchDefaultsAndCapsTheCount(t *testing.T) {
	srv, seen := serveXML(t, http.StatusOK, feedWith(fullEntry))
	c := testClient(t, srv.URL, srv.Client())

	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := seen.Get("max_results"); got != "25" {
		t.Errorf("max_results = %q, want the default 25", got)
	}

	if _, err := c.Search(context.Background(), "x",
		academic_search.Options{Count: academic_search.MaxCount + 1}); err == nil {
		t.Error("a count above the manifest ceiling should be refused, not silently clamped")
	}
}

func TestSearchRendersTheDateWindow(t *testing.T) {
	srv, seen := serveXML(t, http.StatusOK, feedWith(fullEntry))
	c := testClient(t, srv.URL, srv.Client())

	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC)
	if _, err := c.Search(context.Background(), "x", academic_search.Options{From: from, To: to}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	want := `all:"x" AND submittedDate:[202401010000 TO 202412312359]`
	if got := seen.Get("search_query"); got != want {
		t.Errorf("search_query = %q, want %q", got, want)
	}

	if _, err := c.Search(context.Background(), "x", academic_search.Options{From: from}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := seen.Get("search_query"); !strings.Contains(got, "202401010000 TO 999912312359") {
		t.Errorf("an open-ended window rendered as %q", got)
	}

	if _, err := c.Search(context.Background(), "x", academic_search.Options{To: to}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := seen.Get("search_query"); !strings.Contains(got, "190001010000 TO 202412312359") {
		t.Errorf("an open-started window rendered as %q", got)
	}
}

// The registry's abstract is in the fixture. This asserts it is nowhere in the
// projection — not in a field, not in a stray string (ADR-0012 §2).
func TestSearchProjectsIdentityAndNeverTheAbstract(t *testing.T) {
	srv, _ := serveXML(t, http.StatusOK, feedWith(fullEntry))
	c := testClient(t, srv.URL, srv.Client())

	resp, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(resp.Works) != 1 {
		t.Fatalf("got %d works, want 1", len(resp.Works))
	}
	work := resp.Works[0]

	if work.Title != "Deprescribing cascades in tender-driven formularies" {
		t.Errorf("Title = %q", work.Title)
	}
	if work.Year != 2024 {
		t.Errorf("Year = %d, want 2024", work.Year)
	}
	if len(work.Authors) != 2 || work.Authors[0] != "Wei Zhang" || work.Authors[1] != "Ana Silva" {
		t.Errorf("Authors = %v", work.Authors)
	}
	if work.Venue != "J Clin Epi 170 (2024) 111-120" {
		t.Errorf("Venue = %q", work.Venue)
	}
	if work.Type != academic_search.WorkTypePreprint {
		t.Errorf("Type = %q, want preprint; everything on arXiv is one", work.Type)
	}
	if resp.Total != 137 {
		t.Errorf("Total = %d, want the registry's 137", resp.Total)
	}

	for _, field := range []string{work.Title, work.Venue, work.Type, work.DOI, work.ArXivID, work.PMID} {
		if strings.Contains(field, "SECRET-ABSTRACT-TEXT") {
			t.Errorf("the abstract reached the projection through %q", field)
		}
	}
	for _, author := range work.Authors {
		if strings.Contains(author, "SECRET-ABSTRACT-TEXT") {
			t.Errorf("the abstract reached the author list: %q", author)
		}
	}
}

// A version suffix names a revision, not a work. Keeping it would make the same
// preprint a new candidate every time its author uploads a correction; the
// difference between versions is a body difference, and bodies are governed at
// promote time by content hash, not here.
func TestSearchStripsTheVersionSuffixFromTheArXivID(t *testing.T) {
	srv, _ := serveXML(t, http.StatusOK, feedWith(fullEntry))
	c := testClient(t, srv.URL, srv.Client())

	resp, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := resp.Works[0].ArXivID; got != "2401.00001" {
		t.Errorf("ArXivID = %q, want the unversioned 2401.00001", got)
	}
	identity, ok := resp.Works[0].Identity()
	if !ok || identity != "arxiv:2401.00001" {
		t.Errorf("Identity() = %q, %v", identity, ok)
	}
}

// arxiv:doi points at the *published* version, which ADR-0012 §3 holds to be a
// different Document. Adopting it would hand this candidate another work's
// identity, so a researcher promoting the preprint PDF would land on the
// version-of-record's canonical key — exactly the merge decision 2b forbids.
func TestSearchDoesNotAdoptTheJournalDOIAsTheCandidateIdentity(t *testing.T) {
	srv, _ := serveXML(t, http.StatusOK, feedWith(fullEntry))
	c := testClient(t, srv.URL, srv.Client())

	resp, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := resp.Works[0].DOI; got != "" {
		t.Errorf("DOI = %q; the journal DOI names the published version, not this e-print", got)
	}
}

func TestSearchDropsEntriesWithoutAnIdentity(t *testing.T) {
	anonymous := `<entry>
    <id>http://arxiv.org/abs/</id>
    <title>No identifier at all</title>
    <summary>whatever</summary>
  </entry>`
	srv, _ := serveXML(t, http.StatusOK, feedWith(fullEntry, anonymous))
	c := testClient(t, srv.URL, srv.Client())

	resp, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(resp.Works) != 1 {
		t.Fatalf("got %d works, want the identified one only", len(resp.Works))
	}
	if resp.Dropped != 1 {
		t.Errorf("Dropped = %d, want 1", resp.Dropped)
	}
}

// arXiv reports request errors inside an HTTP 200 Atom feed, so the status
// alone never decides success — the same trap the Doubao client has.
func TestSearchRefusesTheErrorFeed(t *testing.T) {
	errorFeed := feedWith(`<entry>
    <id>http://arxiv.org/api/errors#incorrect_id_format_for_` + secretQuery + `</id>
    <title>Error</title>
    <summary>incorrect id format for ` + secretQuery + `</summary>
  </entry>`)
	srv, _ := serveXML(t, http.StatusOK, errorFeed)
	c := testClient(t, srv.URL, srv.Client())

	_, err := c.Search(context.Background(), secretQuery, academic_search.Options{})
	if err == nil {
		t.Fatal("Search() accepted an error feed as a result page")
	}
	if strings.Contains(err.Error(), secretQuery) {
		t.Errorf("the error leaks the query: %v", err)
	}
	apiErr, ok := err.(*academic_search.APIError)
	if !ok {
		t.Fatalf("error type = %T, want *academic_search.APIError", err)
	}
	if apiErr.Retryable {
		t.Error("a malformed request is terminal; retrying it spends the rate budget to fail again")
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
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
	}
	for _, tc := range cases {
		srv, _ := serveXML(t, tc.status, "<html>an error page</html>")
		c := testClient(t, srv.URL, srv.Client())
		c.maxAttempts = 1

		_, err := c.Search(context.Background(), "x", academic_search.Options{})
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
	}
}

func TestSearchRetriesTransientFailuresThenGivesUp(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(feedWith(fullEntry)))
	}))
	t.Cleanup(srv.Close)

	c := testClient(t, srv.URL, srv.Client())
	c.sleep = func(context.Context, time.Duration) error { return nil }
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestSearchRefusesAnOversizedResponse(t *testing.T) {
	srv, _ := serveXML(t, http.StatusOK, strings.Repeat("a", maxResponseBytes+1))
	c := testClient(t, srv.URL, srv.Client())
	c.maxAttempts = 1

	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err == nil {
		t.Fatal("an oversized response was accepted")
	}
}

func TestSearchRefusesMalformedXML(t *testing.T) {
	srv, _ := serveXML(t, http.StatusOK, "<feed><entry>unclosed")
	c := testClient(t, srv.URL, srv.Client())
	c.maxAttempts = 1

	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err == nil {
		t.Fatal("malformed XML was accepted")
	}
}

// Everything on arXiv is a preprint. A profile that wants only journal articles
// wants nothing here, and answering it without a request is both correct and
// one fewer call against a three-second budget.
func TestSearchSkipsTheRegistryWhenNoPreprintIsWanted(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(feedWith(fullEntry)))
	}))
	t.Cleanup(srv.Close)

	c := testClient(t, srv.URL, srv.Client())
	resp, err := c.Search(context.Background(), "x", academic_search.Options{
		WorkTypes: []string{academic_search.WorkTypeJournalArticle, academic_search.WorkTypeReview},
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if called {
		t.Error("the registry was queried for work types it does not hold")
	}
	if len(resp.Works) != 0 {
		t.Errorf("got %d works, want none", len(resp.Works))
	}

	// ...but a profile that includes preprints still searches.
	if _, err := c.Search(context.Background(), "x", academic_search.Options{
		WorkTypes: []string{academic_search.WorkTypePreprint},
	}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if !called {
		t.Error("a preprint-inclusive profile did not reach the registry")
	}
}

func TestSearchRejectsABlankQuery(t *testing.T) {
	srv, _ := serveXML(t, http.StatusOK, feedWith(fullEntry))
	c := testClient(t, srv.URL, srv.Client())

	if _, err := c.Search(context.Background(), "   ", academic_search.Options{}); err == nil {
		t.Fatal("a blank query was accepted")
	}
}

func TestSearchLogsNeitherTheQueryNorTheResponse(t *testing.T) {
	var logs bytes.Buffer
	logger.SetLogLevel(logger.LevelDebug)
	logger.SetOutput(&logs)
	t.Cleanup(logger.ConfigureFromEnv)

	// Success path.
	srv, _ := serveXML(t, http.StatusOK, feedWith(fullEntry))
	c := testClient(t, srv.URL, srv.Client())
	if _, err := c.Search(context.Background(), secretQuery, academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	// Error-feed path, whose vendor text quotes the query back.
	errSrv, _ := serveXML(t, http.StatusOK, feedWith(`<entry>
    <id>http://arxiv.org/api/errors#incorrect_id_format_for_`+secretQuery+`</id>
    <title>Error</title>
    <summary>incorrect id format for `+secretQuery+`</summary>
  </entry>`))
	errClient := testClient(t, errSrv.URL, errSrv.Client())
	errClient.maxAttempts = 1
	if _, err := errClient.Search(context.Background(), secretQuery, academic_search.Options{}); err == nil {
		t.Fatal("the error feed was accepted")
	}

	// Transport-failure path.
	failSrv, _ := serveXML(t, http.StatusBadRequest, "nope")
	failClient := testClient(t, failSrv.URL, failSrv.Client())
	failClient.maxAttempts = 1
	if _, err := failClient.Search(context.Background(), secretQuery, academic_search.Options{}); err == nil {
		t.Fatal("a 400 was accepted")
	}

	// Without this the bans below would hold vacuously if capture ever broke.
	if !strings.Contains(logs.String(), "[AcademicSearch][arXiv]") {
		t.Fatalf("captured no client logs at all:\n%s", logs.String())
	}
	for _, banned := range []string{secretQuery, "SECRET-ABSTRACT-TEXT", "incorrect id format"} {
		if strings.Contains(logs.String(), banned) {
			t.Errorf("logs leak %q:\n%s", banned, logs.String())
		}
	}
}
