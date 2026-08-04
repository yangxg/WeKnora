package crossref

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search"
	"github.com/Tencent/WeKnora/internal/logger"
)

const (
	// secretQuery stands in for a saved query. Crossref echoes the search terms
	// back inside message.query, so the response is a leak vector as much as the
	// request is (ADR-0009 §9).
	secretQuery = "off-label-prescribing-in-provincial-tenders"

	// plusToken is a secret. It rides in a header, and headers reach logs more
	// easily than bodies do.
	plusToken = "SECRET-PLUS-TOKEN-abc123"

	// roleMailto is the non-secret contact identity ADR-0012 §5 gap 2 places in
	// the version-controlled manifest. It must be a role address, and it is the
	// only PII the manifest is allowed to carry.
	roleMailto = "research-flow@example.org"
)

// workListBody wraps items in the documented envelope. The abstract is present
// in these fixtures on purpose: `select` should keep it off the wire, and the
// wire struct should have nowhere to put it if a server ignores `select`.
func workListBody(total int, items ...map[string]any) string {
	body, err := json.Marshal(map[string]any{
		"status":          "ok",
		"message-type":    "work-list",
		"message-version": "1.0.0",
		"message": map[string]any{
			"total-results":  total,
			"items-per-page": len(items),
			// Crossref reflects the query back here.
			"query": map[string]any{"start-index": 0, "search-terms": secretQuery},
			"items": items,
		},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func fullItem() map[string]any {
	return map[string]any{
		"DOI":    "10.1001/JAMA.2024.12345",
		"title":  []any{"Deprescribing cascades in\n        tender-driven formularies"},
		"issued": map[string]any{"date-parts": []any{[]any{2024, 3, 15}}},
		"author": []any{
			map[string]any{"given": "Wei", "family": "Zhang", "sequence": "first"},
			map[string]any{"family": "Silva"},
			map[string]any{"name": "National Health Commission"},
		},
		"container-title": []any{"JAMA"},
		"type":            "journal-article",
		"abstract":        "<jats:p>SECRET-ABSTRACT-TEXT that must never reach a Work.</jats:p>",
	}
}

type capture struct {
	query   url.Values
	headers http.Header
}

func serveJSON(t *testing.T, status int, body string) (*httptest.Server, *capture) {
	t.Helper()
	seen := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.query = r.URL.Query()
		seen.headers = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-api-pool", "polite")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func politeConfig(httpClient *http.Client) Config {
	return Config{HTTPClient: httpClient, Pool: PoolPolite, Mailto: roleMailto, ToolName: "ResearchFlow"}
}

// testClient aims a client at a fake server with pacing disabled; the real
// per-pool intervals are asserted separately.
func testClient(t *testing.T, endpoint string, cfg Config) *Client {
	t.Helper()
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	c.endpoint = endpoint
	c.pacer = academic_search.NewPacer(0)
	return c
}

func TestNewClientRejectsUnusableConfigurations(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no http client", Config{Pool: PoolPolite, Mailto: roleMailto}},
		{"no contact identity", Config{HTTPClient: http.DefaultClient, Pool: PoolPolite}},
		{"contact identity is not an address", Config{
			HTTPClient: http.DefaultClient, Pool: PoolPolite, Mailto: "research-flow"}},
		// The anonymous public pool is refused by construction: Crossref tries to
		// contact a misbehaving caller before blocking it, and an anonymous caller
		// gave them nobody to contact (ADR-0012 §5).
		{"anonymous public pool", Config{
			HTTPClient: http.DefaultClient, Pool: "public", Mailto: roleMailto}},
		{"unknown pool", Config{
			HTTPClient: http.DefaultClient, Pool: "enterprise", Mailto: roleMailto}},
		// A Plus subscription with no token would run at polite rates while the
		// project believes it paid for 150 rps.
		{"plus without a token", Config{
			HTTPClient: http.DefaultClient, Pool: PoolPlus, Mailto: roleMailto}},
		// The mirror failure: a token that will never be sent is a paid
		// credential silently going unused.
		{"polite with a token", Config{
			HTTPClient: http.DefaultClient, Pool: PoolPolite, Mailto: roleMailto, PlusToken: plusToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewClient(tc.cfg); err == nil {
				t.Fatal("NewClient() accepted it")
			}
		})
	}
}

func TestNewClientPacesPerPool(t *testing.T) {
	polite, err := NewClient(politeConfig(http.DefaultClient))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if got := polite.pacer.Interval(); got != PoliteInterval {
		t.Errorf("polite interval = %v, want %v", got, PoliteInterval)
	}

	plus, err := NewClient(Config{
		HTTPClient: http.DefaultClient, Pool: PoolPlus, Mailto: roleMailto, PlusToken: plusToken,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if got := plus.pacer.Interval(); got != PlusInterval {
		t.Errorf("plus interval = %v, want %v", got, PlusInterval)
	}
	// Both must stay under the documented ceilings: those are shared fleet-wide,
	// so sitting exactly on them leaves no room for a second process.
	if PoliteInterval < 100*time.Millisecond {
		t.Errorf("polite interval %v exceeds the documented 10 rps", PoliteInterval)
	}
	if PlusInterval < 7*time.Millisecond {
		t.Errorf("plus interval %v exceeds the documented 150 rps", PlusInterval)
	}
	if PlusInterval >= PoliteInterval {
		t.Error("the plus pool should not be paced more slowly than the polite one")
	}
}

func TestSearchBuildsTheDocumentedRequest(t *testing.T) {
	srv, seen := serveJSON(t, http.StatusOK, workListBody(137, fullItem()))
	c := testClient(t, srv.URL, politeConfig(srv.Client()))

	if _, err := c.Search(context.Background(), "deprescribing", academic_search.Options{Count: 7}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if got := seen.query.Get("query.bibliographic"); got != "deprescribing" {
		t.Errorf("query.bibliographic = %q", got)
	}
	if got := seen.query.Get("rows"); got != "7" {
		t.Errorf("rows = %q, want 7", got)
	}
	if got := seen.query.Get("sort"); got != "issued" {
		t.Errorf("sort = %q, want issued; discovery wants what is new", got)
	}
	if got := seen.query.Get("order"); got != "desc" {
		t.Errorf("order = %q, want desc", got)
	}
	if got := seen.query.Get("mailto"); got != roleMailto {
		t.Errorf("mailto = %q, want the polite-pool identity", got)
	}
	// The User-Agent is the other half of the polite identity, and Crossref asks
	// for it to name the tool and repeat the contact address.
	agent := seen.headers.Get("User-Agent")
	if !strings.Contains(agent, "ResearchFlow") || !strings.Contains(agent, roleMailto) {
		t.Errorf("User-Agent = %q, want it to name the tool and the contact", agent)
	}
	if _, ok := seen.headers[http.CanonicalHeaderKey(plusTokenHeader)]; ok {
		t.Error("the polite pool sent a Plus token header")
	}
}

// select is the lever that keeps the abstract off the wire entirely: a field the
// server never sends cannot be mishandled downstream.
func TestSearchSelectsIdentityFieldsOnly(t *testing.T) {
	srv, seen := serveJSON(t, http.StatusOK, workListBody(1, fullItem()))
	c := testClient(t, srv.URL, politeConfig(srv.Client()))

	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	selected := strings.Split(seen.query.Get("select"), ",")
	if len(selected) == 0 || selected[0] == "" {
		t.Fatal("no select parameter was sent; the registry would return every field including the abstract")
	}
	want := map[string]bool{
		"DOI": true, "title": true, "issued": true,
		"author": true, "container-title": true, "type": true,
	}
	for _, field := range selected {
		if !want[field] {
			t.Errorf("select asks for %q, which is not an identity field", field)
		}
		lowered := strings.ToLower(field)
		for _, banned := range []string{"abstract", "reference", "license", "link", "full-text"} {
			if strings.Contains(lowered, banned) {
				t.Errorf("select asks for %q; the academic lane carries identity only", field)
			}
		}
	}
}

func TestSearchSendsThePlusTokenAsAHeader(t *testing.T) {
	srv, seen := serveJSON(t, http.StatusOK, workListBody(1, fullItem()))
	c := testClient(t, srv.URL, Config{
		HTTPClient: srv.Client(), Pool: PoolPlus, Mailto: roleMailto,
		ToolName: "ResearchFlow", PlusToken: plusToken,
	})
	// The fake server reports the pool it was asked for, so the downgrade guard
	// is not what is under test here.
	c.expectedPool = ""

	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := seen.headers.Get(plusTokenHeader); got != "Bearer "+plusToken {
		t.Errorf("%s = %q, want a bearer token", plusTokenHeader, got)
	}
	// The token is a secret; a query parameter lands in access logs at every hop.
	for key, values := range seen.query {
		for _, value := range values {
			if strings.Contains(value, plusToken) {
				t.Errorf("the Plus token reached query parameter %q", key)
			}
		}
	}
}

func TestSearchRendersTheFiltersCrossrefCanExpress(t *testing.T) {
	srv, seen := serveJSON(t, http.StatusOK, workListBody(1, fullItem()))
	c := testClient(t, srv.URL, politeConfig(srv.Client()))

	_, err := c.Search(context.Background(), "x", academic_search.Options{
		From: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC),
		WorkTypes: []string{
			academic_search.WorkTypeJournalArticle,
			academic_search.WorkTypePreprint,
			academic_search.WorkTypeConferencePaper,
		},
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	filter := seen.query.Get("filter")
	for _, want := range []string{
		"from-pub-date:2024-01-01",
		"until-pub-date:2024-12-31",
		"type:journal-article",
		// Crossref files preprints under posted-content; passing the kernel term
		// through would match nothing and read as "no preprints on this topic".
		"type:posted-content",
		"type:proceedings-article",
	} {
		if !strings.Contains(filter, want) {
			t.Errorf("filter %q is missing %q", filter, want)
		}
	}

	// No window and no types means no filter parameter at all, not an empty one.
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if _, ok := seen.query["filter"]; ok {
		t.Errorf("an unfiltered search sent filter=%q", seen.query.Get("filter"))
	}
}

// Refusing beats silently widening. A profile asking for something Crossref
// cannot express would otherwise come back as an unfiltered reading list that
// looks like an answer.
func TestSearchRefusesFiltersCrossrefCannotExpress(t *testing.T) {
	srv, seen := serveJSON(t, http.StatusOK, workListBody(1, fullItem()))
	c := testClient(t, srv.URL, politeConfig(srv.Client()))

	// Crossref has no review-article type; peer-review is a review *of* a work.
	if _, err := c.Search(context.Background(), "x", academic_search.Options{
		WorkTypes: []string{academic_search.WorkTypeReview},
	}); err == nil {
		t.Error("a work type Crossref cannot express was accepted")
	}
	// Crossref records licences, not open-access status. has-license:true would
	// be a different question wearing this one's name.
	if _, err := c.Search(context.Background(), "x", academic_search.Options{
		OpenAccess: academic_search.OpenAccessOnly,
	}); err == nil {
		t.Error("an open-access-only profile was accepted")
	}
	if seen.query != nil {
		t.Error("a refusable profile still reached the registry")
	}
}

func TestSearchProjectsIdentityAndNeverTheAbstract(t *testing.T) {
	srv, _ := serveJSON(t, http.StatusOK, workListBody(137, fullItem()))
	c := testClient(t, srv.URL, politeConfig(srv.Client()))

	resp, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(resp.Works) != 1 {
		t.Fatalf("got %d works, want 1", len(resp.Works))
	}
	work := resp.Works[0]

	// Lowercased and stripped: the DOI is a deduplication key that has to match
	// the one a human types at promote time.
	if work.DOI != "10.1001/jama.2024.12345" {
		t.Errorf("DOI = %q, want the normalized form", work.DOI)
	}
	if work.Title != "Deprescribing cascades in tender-driven formularies" {
		t.Errorf("Title = %q, want the collapsed title", work.Title)
	}
	if work.Year != 2024 {
		t.Errorf("Year = %d, want 2024", work.Year)
	}
	if work.Venue != "JAMA" {
		t.Errorf("Venue = %q", work.Venue)
	}
	if work.Type != "journal-article" {
		t.Errorf("Type = %q, want the registry's own term", work.Type)
	}
	wantAuthors := []string{"Wei Zhang", "Silva", "National Health Commission"}
	if len(work.Authors) != len(wantAuthors) {
		t.Fatalf("Authors = %v, want %v", work.Authors, wantAuthors)
	}
	for i, want := range wantAuthors {
		if work.Authors[i] != want {
			t.Errorf("Authors[%d] = %q, want %q", i, work.Authors[i], want)
		}
	}
	if resp.Total != 137 {
		t.Errorf("Total = %d, want the registry's 137", resp.Total)
	}

	fields := append([]string{work.DOI, work.Title, work.Venue, work.Type, work.ArXivID, work.PMID}, work.Authors...)
	for _, field := range fields {
		if strings.Contains(field, "SECRET-ABSTRACT-TEXT") || strings.Contains(field, "jats") {
			t.Errorf("the abstract reached the projection through %q", field)
		}
	}
}

func TestSearchNormalizesResolverStyleDOIs(t *testing.T) {
	item := fullItem()
	item["DOI"] = "https://doi.org/10.1001/JAMA.2024.12345"
	srv, _ := serveJSON(t, http.StatusOK, workListBody(1, item))
	c := testClient(t, srv.URL, politeConfig(srv.Client()))

	resp, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := resp.Works[0].DOI; got != "10.1001/jama.2024.12345" {
		t.Errorf("DOI = %q, want the bare lowercased form", got)
	}
}

// A Crossref record with no usable DOI cannot be matched against the file a
// researcher later promotes, which is the only thing the candidate exists for.
func TestSearchDropsRecordsWithoutAUsableDOI(t *testing.T) {
	missing := fullItem()
	delete(missing, "DOI")
	mangled := fullItem()
	mangled["DOI"] = "not-a-doi-at-all"

	srv, _ := serveJSON(t, http.StatusOK, workListBody(3, fullItem(), missing, mangled))
	c := testClient(t, srv.URL, politeConfig(srv.Client()))

	resp, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(resp.Works) != 1 {
		t.Fatalf("got %d works, want only the identified one", len(resp.Works))
	}
	if resp.Dropped != 2 {
		t.Errorf("Dropped = %d, want 2", resp.Dropped)
	}
}

// The silent-downgrade failure ADR-0012 §5 gap 1 warns about, made loud. It also
// covers the documentation inconsistency recorded in §9 item 3: if the Plus
// token header is named wrongly, Crossref answers 200 from a lower pool and the
// project's paid rate quietly disappears.
func TestSearchRefusesADowngradedPool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-api-pool", "public")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workListBody(1, fullItem())))
	}))
	t.Cleanup(srv.Close)

	c := testClient(t, srv.URL, Config{
		HTTPClient: srv.Client(), Pool: PoolPlus, Mailto: roleMailto,
		ToolName: "ResearchFlow", PlusToken: plusToken,
	})

	_, err := c.Search(context.Background(), "x", academic_search.Options{})
	if err == nil {
		t.Fatal("a response from a lower pool than the one configured was accepted")
	}
	apiErr, ok := err.(*academic_search.APIError)
	if !ok {
		t.Fatalf("error type = %T, want *academic_search.APIError", err)
	}
	if apiErr.Retryable {
		t.Error("a credential the registry did not recognize will not be recognized on a retry")
	}
	if apiErr.Code != "PoolDowngraded" {
		t.Errorf("Code = %q, want PoolDowngraded", apiErr.Code)
	}
}

// A pool at or above the configured one is fine, and so is a missing header: a
// server that says nothing is not evidence of a downgrade, and refusing on
// silence would break the client the day Crossref drops the header.
func TestSearchAcceptsAPoolAtOrAboveTheConfiguredOne(t *testing.T) {
	for _, reported := range []string{"polite", "plus", ""} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if reported != "" {
				w.Header().Set("x-api-pool", reported)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(workListBody(1, fullItem())))
		}))
		if _, err := testClient(t, srv.URL, politeConfig(srv.Client())).
			Search(context.Background(), "x", academic_search.Options{}); err != nil {
			t.Errorf("pool %q was refused: %v", reported, err)
		}
		srv.Close()
	}
}

// The registry publishes the limits it is currently applying to this caller.
// They are worth recording — a limit lower than the pool's documented one is how
// a throttle shows up before it becomes a block — but they are counts, not
// content, so they may be logged.
func TestSearchRecordsTheObservedRateLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-api-pool", "polite")
		w.Header().Set("x-rate-limit-limit", "10")
		w.Header().Set("x-rate-limit-interval", "1s")
		w.Header().Set("x-concurrency-limit", "3")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workListBody(1, fullItem())))
	}))
	t.Cleanup(srv.Close)

	c := testClient(t, srv.URL, politeConfig(srv.Client()))
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	limits := c.ObservedLimits()
	if limits.Limit != 10 || limits.Interval != time.Second || limits.Concurrency != 3 {
		t.Errorf("ObservedLimits() = %+v, want 10 per 1s with concurrency 3", limits)
	}
}

func TestSearchRefusesANonOKEnvelope(t *testing.T) {
	srv, _ := serveJSON(t, http.StatusOK, `{"status":"error","message-type":"work-list","message":{}}`)
	c := testClient(t, srv.URL, politeConfig(srv.Client()))
	c.maxAttempts = 1

	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err == nil {
		t.Fatal("an envelope reporting status!=ok was accepted")
	}
}

func TestSearchClassifiesTransportFailures(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusBadGateway, true},
		{http.StatusBadRequest, false},
		// A Crossref 403 is a person deciding this caller misbehaved. Retrying
		// into it is the behaviour that earned it.
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
	}
	for _, tc := range cases {
		srv, _ := serveJSON(t, tc.status, "Resource not found: "+secretQuery)
		c := testClient(t, srv.URL, politeConfig(srv.Client()))
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
		// Crossref error bodies are plain text quoting the request.
		if strings.Contains(apiErr.Error(), secretQuery) {
			t.Errorf("status %d leaked the query: %v", tc.status, apiErr)
		}
	}
}

func TestSearchHonoursRetryAfterOn429(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workListBody(1, fullItem())))
	}))
	t.Cleanup(srv.Close)

	var slept []time.Duration
	c := testClient(t, srv.URL, politeConfig(srv.Client()))
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("slept %v, want the registry's requested 7s", slept)
	}
}

func TestSearchRefusesOversizedAndMalformedResponses(t *testing.T) {
	oversized, _ := serveJSON(t, http.StatusOK, strings.Repeat("a", maxResponseBytes+1))
	c := testClient(t, oversized.URL, politeConfig(oversized.Client()))
	c.maxAttempts = 1
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err == nil {
		t.Error("an oversized response was accepted")
	}

	malformed, _ := serveJSON(t, http.StatusOK, `{"status":"ok","message":{`)
	c = testClient(t, malformed.URL, politeConfig(malformed.Client()))
	c.maxAttempts = 1
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err == nil {
		t.Error("malformed JSON was accepted")
	}
}

func TestSearchValidatesTheQueryAndCount(t *testing.T) {
	srv, seen := serveJSON(t, http.StatusOK, workListBody(1, fullItem()))
	c := testClient(t, srv.URL, politeConfig(srv.Client()))

	if _, err := c.Search(context.Background(), "   ", academic_search.Options{}); err == nil {
		t.Error("a blank query was accepted")
	}
	if _, err := c.Search(context.Background(), "x",
		academic_search.Options{Count: academic_search.MaxCount + 1}); err == nil {
		t.Error("a count above the manifest ceiling was accepted")
	}
	if _, err := c.Search(context.Background(), "x", academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := seen.query.Get("rows"); got != "25" {
		t.Errorf("rows = %q, want the default 25", got)
	}
}

func TestSearchLogsNeitherTheQueryNorTheCredentials(t *testing.T) {
	var logs bytes.Buffer
	logger.SetLogLevel(logger.LevelDebug)
	logger.SetOutput(&logs)
	t.Cleanup(logger.ConfigureFromEnv)

	ok, _ := serveJSON(t, http.StatusOK, workListBody(1, fullItem()))
	c := testClient(t, ok.URL, Config{
		HTTPClient: ok.Client(), Pool: PoolPlus, Mailto: roleMailto,
		ToolName: "ResearchFlow", PlusToken: plusToken,
	})
	c.expectedPool = ""
	if _, err := c.Search(context.Background(), secretQuery, academic_search.Options{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	failing, _ := serveJSON(t, http.StatusBadRequest, "Invalid filter for query: "+secretQuery)
	failClient := testClient(t, failing.URL, politeConfig(failing.Client()))
	failClient.maxAttempts = 1
	if _, err := failClient.Search(context.Background(), secretQuery, academic_search.Options{}); err == nil {
		t.Fatal("a 400 was accepted")
	}

	if !strings.Contains(logs.String(), "[AcademicSearch][Crossref]") {
		t.Fatalf("captured no client logs at all:\n%s", logs.String())
	}
	for _, banned := range []string{secretQuery, plusToken, "SECRET-ABSTRACT-TEXT", "Invalid filter"} {
		if strings.Contains(logs.String(), banned) {
			t.Errorf("logs leak %q:\n%s", banned, logs.String())
		}
	}
}
