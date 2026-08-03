package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/infrastructure/web_search/volcengine"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// fakeSearcher stands in for the vendor client. The wire protocol is covered by
// the volcengine package's own fake-server tests, so what matters here is the
// projection and the failure handling — a second HTTP server would only make
// those slower to read.
type fakeSearcher struct {
	byQuery map[string]*volcengine.Response
	errs    map[string]error
	calls   []string
}

func (f *fakeSearcher) Search(
	_ context.Context, query string, opts volcengine.Options,
) (*volcengine.Response, error) {
	f.calls = append(f.calls, query)
	if err, ok := f.errs[query]; ok {
		return nil, err
	}
	if opts.NeedContent {
		return nil, fmt.Errorf("test guard: connector asked the vendor for page bodies")
	}
	if resp, ok := f.byQuery[query]; ok {
		return resp, nil
	}
	return &volcengine.Response{}, nil
}

// fakePages stands in for the landing page fetches. Keys are URLs; a missing key
// is a fetch failure, which is exactly the single-item failure case.
type fakePages struct {
	pages map[string]page
	errs  map[string]error
	calls []string
}

func (f *fakePages) Fetch(_ context.Context, rawURL string) (page, error) {
	f.calls = append(f.calls, rawURL)
	if err, ok := f.errs[rawURL]; ok {
		return page{}, err
	}
	if p, ok := f.pages[rawURL]; ok {
		return p, nil
	}
	return page{}, fmt.Errorf("landing page fetch failed: no route to %s", rawURL)
}

func testConnector(searcher *fakeSearcher, pages *fakePages) *Connector {
	return &Connector{
		newSearcher: func(*Config) (vendorSearcher, error) { return searcher, nil },
		newPages:    func() pageFetcher { return pages },
	}
}

const (
	drgQueryID = "drg-payment-reform"
	dipQueryID = "dip-pilot-cities"
	dipQuery   = "DIP 试点城市名单"

	drgURL = "https://www.gov.cn/zhengce/drg-pilot.html"
	dipURL = "https://www.nhc.gov.cn/notice/dip-cities.html"
)

func drgHit() volcengine.Result {
	published := time.Date(2026, 7, 16, 8, 30, 0, 0, time.UTC)
	return volcengine.Result{
		Title:       "国家医保局关于 DRG 付费改革试点的通知",
		URL:         drgURL,
		Snippet:     "VENDOR-SUMMARY-MUST-NOT-BE-INGESTED",
		PublishedAt: &published,
	}
}

func dipHit() volcengine.Result {
	return volcengine.Result{
		Title:   "DIP 试点城市名单公布",
		URL:     dipURL,
		Snippet: "VENDOR-SUMMARY-MUST-NOT-BE-INGESTED",
	}
}

func twoQuerySearcher() *fakeSearcher {
	return &fakeSearcher{byQuery: map[string]*volcengine.Response{
		secretQuery: {RequestID: "req-1", Results: []volcengine.Result{drgHit()}},
		dipQuery:    {RequestID: "req-2", Results: []volcengine.Result{dipHit()}},
	}}
}

func twoPageFetcher() *fakePages {
	return &fakePages{
		pages: map[string]page{
			drgURL: {Markdown: "# 试点城市名单\n\n国家医保局公布了第一批试点城市。", Title: "试点城市名单"},
			dipURL: {Markdown: "# DIP 名单\n\n第二批 DIP 试点城市共二十个。", Title: "DIP 名单"},
		},
		errs: map[string]error{},
	}
}

func TestConnectorTypeMatchesTheMaterializedRow(t *testing.T) {
	if got := NewConnector().Type(); got != types.ConnectorTypeDiscovery {
		t.Errorf("Type() = %q, want %q", got, types.ConnectorTypeDiscovery)
	}
	var _ datasource.Connector = NewConnector()
}

// TestValidateNeverSpendsQuota is a deliberate divergence from the RSS connector,
// whose Validate fetches every feed. A discovery validation that ran the saved
// queries would spend vendor quota — 500 calls a month on the free tier — every
// time someone opened the settings drawer.
func TestValidateNeverSpendsQuota(t *testing.T) {
	searcher := twoQuerySearcher()
	pages := twoPageFetcher()
	connector := testConnector(searcher, pages)

	if err := connector.Validate(context.Background(), discoveryConfig(discoverySettings())); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(searcher.calls) != 0 || len(pages.calls) != 0 {
		t.Errorf("Validate() made %d searches and %d page fetches, want none",
			len(searcher.calls), len(pages.calls))
	}

	settings := discoverySettings()
	settings["need_content"] = true
	if err := connector.Validate(context.Background(), discoveryConfig(settings)); err == nil {
		t.Error("Validate() accepted a profile that requests vendor bodies")
	}
}

// TestListResourcesNamesQueriesWithoutRevealingThem keeps the query text out of an
// HTTP response body. ListResources feeds a settings UI, and request/response
// logging is not this connector's to control.
func TestListResourcesNamesQueriesWithoutRevealingThem(t *testing.T) {
	connector := testConnector(twoQuerySearcher(), twoPageFetcher())

	resources, err := connector.ListResources(
		context.Background(), discoveryConfig(discoverySettings()), "")
	if err != nil {
		t.Fatalf("ListResources() error = %v", err)
	}
	if len(resources) != 2 {
		t.Fatalf("len(resources) = %d, want one per saved query", len(resources))
	}
	if resources[0].ExternalID != drgQueryID {
		t.Errorf("ExternalID = %q, want the query id", resources[0].ExternalID)
	}
	for _, resource := range resources {
		rendered := fmt.Sprintf("%+v", resource)
		if strings.Contains(rendered, secretQuery) || strings.Contains(rendered, dipQuery) {
			t.Errorf("resource exposes the query text: %s", rendered)
		}
	}

	nested, err := connector.ListResources(
		context.Background(), discoveryConfig(discoverySettings()), drgQueryID)
	if err != nil {
		t.Fatalf("ListResources(child) error = %v", err)
	}
	if len(nested) != 0 {
		t.Errorf("saved queries are flat; got %d children", len(nested))
	}

	ancestors, err := connector.ResolveResourceAncestors(
		context.Background(), discoveryConfig(discoverySettings()), []string{drgQueryID})
	if err != nil || len(ancestors) != 0 {
		t.Errorf("ResolveResourceAncestors() = %v, %v; want empty", ancestors, err)
	}
}

// TestFetchAllProjectsHitsOntoCandidatesFromTheOriginalPageOnly is the two-layer
// projection the whole stage exists for: the vendor supplies identity and a URL,
// and the body comes from the page.
func TestFetchAllProjectsHitsOntoCandidatesFromTheOriginalPageOnly(t *testing.T) {
	searcher := twoQuerySearcher()
	pages := twoPageFetcher()
	connector := testConnector(searcher, pages)

	items, err := connector.FetchAll(context.Background(), discoveryConfig(discoverySettings()), nil)
	if err != nil {
		t.Fatalf("FetchAll() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}

	var drg *types.FetchedItem
	for i := range items {
		if items[i].URL == drgURL {
			drg = &items[i]
		}
	}
	if drg == nil {
		t.Fatal("the DRG candidate is missing")
	}

	if got := string(drg.Content); !strings.Contains(got, "国家医保局公布了第一批试点城市") {
		t.Errorf("Content did not come from the landing page: %q", got)
	}
	if bytes.Contains(drg.Content, []byte("VENDOR-SUMMARY")) {
		t.Error("the vendor snippet reached FetchedItem.Content")
	}
	if drg.ContentType != "text/markdown" {
		t.Errorf("ContentType = %q", drg.ContentType)
	}
	if drg.Title != "国家医保局关于 DRG 付费改革试点的通知" {
		t.Errorf("Title = %q, want the vendor title", drg.Title)
	}
	if drg.SourceResourceID != drgQueryID {
		t.Errorf("SourceResourceID = %q, want the saved query id", drg.SourceResourceID)
	}
	if !strings.HasPrefix(drg.ExternalID, drgQueryID+":") || !strings.Contains(drg.ExternalID, drgURL) {
		t.Errorf("ExternalID = %q, want it scoped by saved query", drg.ExternalID)
	}
	if want := time.Date(2026, 7, 16, 8, 30, 0, 0, time.UTC); !drg.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %v, want the vendor publish time", drg.UpdatedAt)
	}
	if drg.IsDeleted {
		t.Error("IsDeleted = true; a hit leaving the ranking says nothing about the page")
	}

	if drg.Metadata["channel"] != types.ChannelDiscovery {
		t.Errorf("channel = %q", drg.Metadata["channel"])
	}
	if drg.Metadata["saved_query_id"] != drgQueryID {
		t.Errorf("saved_query_id = %q", drg.Metadata["saved_query_id"])
	}
	if drg.Metadata["discovery_source_id"] != "src-medical-discovery" ||
		drg.Metadata["discovery_project_id"] != "medical-policy" {
		t.Errorf("provenance metadata = %+v", drg.Metadata)
	}
	if drg.Metadata["landing_url"] != drgURL {
		t.Errorf("landing_url = %q", drg.Metadata["landing_url"])
	}
	for key, value := range drg.Metadata {
		if strings.Contains(value, secretQuery) {
			t.Errorf("metadata[%s] carries the query text", key)
		}
		if strings.Contains(value, "VENDOR-SUMMARY") {
			t.Errorf("metadata[%s] carries the vendor summary", key)
		}
	}
}

// TestFetchAllHonorsTheSelectedQueries covers the resourceIDs argument: a user who
// deselected a query in the picker must not have it run.
func TestFetchAllHonorsTheSelectedQueries(t *testing.T) {
	searcher := twoQuerySearcher()
	connector := testConnector(searcher, twoPageFetcher())

	items, err := connector.FetchAll(
		context.Background(), discoveryConfig(discoverySettings()), []string{dipQueryID})
	if err != nil {
		t.Fatalf("FetchAll() error = %v", err)
	}
	if len(items) != 1 || items[0].SourceResourceID != dipQueryID {
		t.Fatalf("items = %+v, want only the selected query", items)
	}
	if len(searcher.calls) != 1 {
		t.Errorf("searched %d times, want 1", len(searcher.calls))
	}

	if _, err := connector.FetchAll(
		context.Background(), discoveryConfig(discoverySettings()), []string{"not-a-query"},
	); err == nil {
		t.Error("FetchAll() accepted a resource id that is not a saved query")
	}
}

// TestFetchIncrementalSkipsUnchangedCandidates is the point of the cursor: a
// scheduled discovery run should not re-ingest a page it already emitted, and
// should re-emit one whose body changed.
func TestFetchIncrementalSkipsUnchangedCandidates(t *testing.T) {
	searcher := twoQuerySearcher()
	pages := twoPageFetcher()
	connector := testConnector(searcher, pages)
	config := discoveryConfig(discoverySettings())

	first, cursor, err := connector.FetchIncremental(context.Background(), config, nil)
	if err != nil {
		t.Fatalf("first FetchIncremental() error = %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first run emitted %d items, want 2", len(first))
	}
	if cursor == nil {
		t.Fatal("first run returned no cursor")
	}
	if cursor.LastSchemaHash != strings.Repeat("a", 64) {
		t.Errorf("LastSchemaHash = %q, want the manifest hash", cursor.LastSchemaHash)
	}

	second, cursor, err := connector.FetchIncremental(context.Background(), config, cursor)
	if err != nil {
		t.Fatalf("second FetchIncremental() error = %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second run re-emitted %d unchanged candidates", len(second))
	}

	pages.pages[drgURL] = page{Markdown: "# 试点城市名单\n\n名单已更新，新增五个城市。", Title: "试点城市名单"}
	third, _, err := connector.FetchIncremental(context.Background(), config, cursor)
	if err != nil {
		t.Fatalf("third FetchIncremental() error = %v", err)
	}
	if len(third) != 1 || third[0].URL != drgURL {
		t.Fatalf("third run emitted %+v, want only the changed page", third)
	}
}

// TestFetchIncrementalResetsWhenThePolicyChanged pins the manifest_hash contract:
// editing the policy must re-discover rather than silently keep skipping pages
// the old policy had already seen.
func TestFetchIncrementalResetsWhenThePolicyChanged(t *testing.T) {
	connector := testConnector(twoQuerySearcher(), twoPageFetcher())
	config := discoveryConfig(discoverySettings())

	_, cursor, err := connector.FetchIncremental(context.Background(), config, nil)
	if err != nil {
		t.Fatalf("FetchIncremental() error = %v", err)
	}

	stale := &types.SyncCursor{
		LastSyncTime:    cursor.LastSyncTime,
		ConnectorCursor: cursor.ConnectorCursor,
		LastSchemaHash:  strings.Repeat("c", 64),
	}
	items, refreshed, err := connector.FetchIncremental(context.Background(), config, stale)
	if err != nil {
		t.Fatalf("FetchIncremental(stale) error = %v", err)
	}
	if len(items) != 2 {
		t.Errorf("a policy edit emitted %d items, want a full re-discovery", len(items))
	}
	if refreshed.LastSchemaHash != strings.Repeat("a", 64) {
		t.Errorf("LastSchemaHash = %q, want the current manifest hash", refreshed.LastSchemaHash)
	}
}

// TestOneUnreachableCandidateDoesNotCostTheRestOfThePage is the first of the two
// obligations WeKnora's core does not provide.
func TestOneUnreachableCandidateDoesNotCostTheRestOfThePage(t *testing.T) {
	searcher := twoQuerySearcher()
	searcher.byQuery[secretQuery] = &volcengine.Response{
		Results: []volcengine.Result{
			{Title: "unfetchable", URL: "https://www.gov.cn/gone.html"},
			drgHit(),
			{Title: "rejected", URL: "http://10.0.0.5/internal"},
		},
	}
	pages := twoPageFetcher()
	pages.errs["https://www.gov.cn/gone.html"] = fmt.Errorf("landing page returned HTTP 404")
	pages.errs["http://10.0.0.5/internal"] = fmt.Errorf("landing page URL rejected: SSRF validation failed")

	connector := testConnector(searcher, pages)
	items, cursor, err := connector.FetchIncremental(
		context.Background(), discoveryConfig(discoverySettings()), nil)

	var partial *datasource.PartialFetchError
	if !errors.As(err, &partial) {
		t.Fatalf("err = %v, want a PartialFetchError so the sync is reported as partial", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want the two reachable candidates", len(items))
	}
	if cursor == nil {
		t.Fatal("a partial page must still return a cursor")
	}

	// A failed item must not be recorded as done, or it would never be retried.
	rendered := fmt.Sprint(cursor.ConnectorCursor)
	if strings.Contains(rendered, "gone.html") || strings.Contains(rendered, "10.0.0.5") {
		t.Errorf("cursor recorded a failed candidate: %s", rendered)
	}

	// Retrying must pick the failure back up once it becomes reachable.
	pages.errs = map[string]error{}
	pages.pages["https://www.gov.cn/gone.html"] = page{Markdown: "# back\n\n内容已恢复。"}
	pages.pages["http://10.0.0.5/internal"] = page{Markdown: "# internal\n\n不应可达。"}
	retried, _, err := connector.FetchIncremental(
		context.Background(), discoveryConfig(discoverySettings()), cursor)
	if err != nil {
		t.Fatalf("retry error = %v", err)
	}
	if len(retried) != 2 {
		t.Errorf("retry emitted %d items, want the two previously failed ones", len(retried))
	}
}

// TestAWholePageFailureDoesNotAdvanceTheCursor is the second obligation. The
// service persists whatever cursor a connector returns even when the fetch
// errored, so the only way to keep a total outage from being mistaken for
// "nothing new" is to return no cursor at all.
func TestAWholePageFailureDoesNotAdvanceTheCursor(t *testing.T) {
	searcher := &fakeSearcher{errs: map[string]error{
		secretQuery: &volcengine.APIError{HTTPStatus: 500, Code: "InnerError", Retryable: true},
		dipQuery:    &volcengine.APIError{HTTPStatus: 500, Code: "InnerError", Retryable: true},
	}}
	connector := testConnector(searcher, twoPageFetcher())
	config := discoveryConfig(discoverySettings())

	items, cursor, err := connector.FetchIncremental(context.Background(), config, nil)
	if err == nil {
		t.Fatal("a total search failure was reported as success")
	}
	var partial *datasource.PartialFetchError
	if errors.As(err, &partial) {
		t.Error("a total failure must not be downgraded to a partial fetch")
	}
	if len(items) != 0 {
		t.Errorf("len(items) = %d, want 0", len(items))
	}
	if cursor != nil {
		t.Fatal("a nil cursor is the only way to stop the service from advancing it")
	}

	// The same must hold when a prior cursor exists: the caller keeps the old one.
	_, priorCursor, err := connector.FetchIncremental(
		context.Background(), config, &types.SyncCursor{LastSchemaHash: strings.Repeat("a", 64)})
	if err == nil || priorCursor != nil {
		t.Errorf("cursor = %v, err = %v; want no cursor and an error", priorCursor, err)
	}
}

// TestOneFailedQueryKeepsTheOtherQuerysProgress separates the two failure
// granularities: one query failing is partial, and the failed query's prior
// state must survive so its candidates are not re-ingested wholesale later.
func TestOneFailedQueryKeepsTheOtherQuerysProgress(t *testing.T) {
	connector := testConnector(twoQuerySearcher(), twoPageFetcher())
	config := discoveryConfig(discoverySettings())

	_, cursor, err := connector.FetchIncremental(context.Background(), config, nil)
	if err != nil {
		t.Fatalf("warm-up error = %v", err)
	}

	broken := twoQuerySearcher()
	broken.errs = map[string]error{dipQuery: &volcengine.APIError{CodeN: 10500, Code: "InnerError"}}
	connector = testConnector(broken, twoPageFetcher())

	items, next, err := connector.FetchIncremental(context.Background(), config, cursor)
	var partial *datasource.PartialFetchError
	if !errors.As(err, &partial) {
		t.Fatalf("err = %v, want a partial fetch", err)
	}
	if len(items) != 0 {
		t.Errorf("len(items) = %d; the surviving query had nothing new", len(items))
	}
	if next == nil {
		t.Fatal("a partial page must return a cursor")
	}
	if !strings.Contains(fmt.Sprint(next.ConnectorCursor), dipURL) {
		t.Error("the failed query's prior progress was dropped; its pages would re-ingest")
	}
}

// TestHitsWithoutAUsableURLAreDroppedNotIngested guards the case where the vendor
// returns a result the profile asked to have a URL for, and it does not.
func TestHitsWithoutAUsableURLAreDroppedNotIngested(t *testing.T) {
	searcher := twoQuerySearcher()
	searcher.byQuery[secretQuery] = &volcengine.Response{
		Results: []volcengine.Result{{Title: "no url", URL: "  "}, drgHit()},
		Dropped: 3,
	}
	pages := twoPageFetcher()
	connector := testConnector(searcher, pages)

	items, _, err := connector.FetchIncremental(
		context.Background(), discoveryConfig(discoverySettings()), nil)
	var partial *datasource.PartialFetchError
	if err != nil && !errors.As(err, &partial) {
		t.Fatalf("err = %v", err)
	}
	for _, item := range items {
		if strings.TrimSpace(item.URL) == "" {
			t.Error("a candidate without a URL was emitted")
		}
	}
	for _, fetched := range pages.calls {
		if strings.TrimSpace(fetched) == "" {
			t.Error("the fetcher was called with an empty URL")
		}
	}
}

// TestBothFixturesStayIsolated runs the two project fixtures through one connector
// instance. The connector is a singleton in the registry, so any state it kept
// between configs would cross projects.
func TestBothFixturesStayIsolated(t *testing.T) {
	searcher := &fakeSearcher{byQuery: map[string]*volcengine.Response{
		secretQuery: {Results: []volcengine.Result{drgHit()}},
		dipQuery:    {Results: []volcengine.Result{dipHit()}},
		"国家社科基金申报指南": {Results: []volcengine.Result{{
			Title: "国家社科基金申报指南", URL: "https://www.nsfc.gov.cn/guide.html",
		}}},
	}}
	pages := twoPageFetcher()
	pages.pages["https://www.nsfc.gov.cn/guide.html"] = page{Markdown: "# 申报指南\n\n本年度申报要求如下。"}
	connector := testConnector(searcher, pages)

	medical, medicalCursor, err := connector.FetchIncremental(
		context.Background(), discoveryConfig(discoverySettings()), nil)
	if err != nil {
		t.Fatalf("medical error = %v", err)
	}
	funds, fundsCursor, err := connector.FetchIncremental(
		context.Background(), discoveryConfig(socialScienceSettings()), nil)
	if err != nil {
		t.Fatalf("funds error = %v", err)
	}

	if len(medical) != 2 || len(funds) != 1 {
		t.Fatalf("medical=%d funds=%d", len(medical), len(funds))
	}
	for _, item := range funds {
		if item.Metadata["discovery_project_id"] != "social-science-funds" {
			t.Errorf("funds candidate claims project %q", item.Metadata["discovery_project_id"])
		}
		if strings.HasPrefix(item.ExternalID, drgQueryID) {
			t.Error("a medical query id leaked into the funds project")
		}
	}
	if medicalCursor.LastSchemaHash == fundsCursor.LastSchemaHash {
		t.Error("the two projects share a cursor hash")
	}

	// Feeding one project's cursor to the other must not silence it: the hashes
	// differ, so the mismatch forces a re-discovery rather than a false skip.
	crossed, _, err := connector.FetchIncremental(
		context.Background(), discoveryConfig(socialScienceSettings()), medicalCursor)
	if err != nil {
		t.Fatalf("crossed error = %v", err)
	}
	if len(crossed) != 1 {
		t.Errorf("a foreign cursor suppressed %d candidates", 1-len(crossed))
	}
}

// TestTheConnectorLogsNeitherTheQueryNorTheVendorResponse holds the connector to
// ADR-0009 §9. Verified non-vacuous by mutation: adding the query to the
// per-query log line makes this fail.
func TestTheConnectorLogsNeitherTheQueryNorTheVendorResponse(t *testing.T) {
	var logs bytes.Buffer
	logger.SetOutput(&logs)
	t.Cleanup(func() { logger.ConfigureFromEnv() })

	// Every branch that logs has to run, or the mutation that proves this test
	// non-vacuous can land somewhere unreachable: a successful search, a failed
	// search, a successful page fetch and a failed page fetch.
	searcher := twoQuerySearcher()
	searcher.byQuery[secretQuery] = &volcengine.Response{
		Results: []volcengine.Result{drgHit(), {Title: "gone", URL: "https://www.gov.cn/gone.html"}},
	}
	searcher.errs = map[string]error{
		dipQuery: &volcengine.APIError{HTTPStatus: 200, CodeN: 10400, Code: "ParamError"},
	}
	pages := twoPageFetcher()
	pages.errs["https://www.gov.cn/gone.html"] = fmt.Errorf("landing page returned HTTP 404")
	connector := testConnector(searcher, pages)

	_, _, err := connector.FetchIncremental(
		context.Background(), discoveryConfig(discoverySettings()), nil)
	var partial *datasource.PartialFetchError
	if !errors.As(err, &partial) {
		t.Fatalf("FetchIncremental() error = %v, want a partial fetch", err)
	}
	// The partial-fetch details reach the sync log, so they are held to the same
	// rule as the log lines themselves.
	for _, detail := range partial.Details {
		for _, banned := range []string{secretQuery, dipQuery, "VENDOR-SUMMARY"} {
			if strings.Contains(detail, banned) {
				t.Errorf("partial fetch detail leaks %q: %s", banned, detail)
			}
		}
	}

	captured := logs.String()
	if !strings.Contains(captured, "[Discovery]") {
		t.Fatalf("captured no connector logs at all:\n%s", captured)
	}
	for _, banned := range []string{secretQuery, dipQuery, "VENDOR-SUMMARY", testAPIKey} {
		if strings.Contains(captured, banned) {
			t.Errorf("logs leak %q:\n%s", banned, captured)
		}
	}
}
