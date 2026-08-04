package academic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	secretQuery = "SECRET-UNPUBLISHED-RESEARCH-TOPIC"
	drgQueryID  = "drg-payment-reform"
	fundQueryID = "social-fund-preprints"
)

type fakeSearcher struct {
	byQuery map[string]*academic_search.Response
	errs    map[string]error
	calls   []searchCall
}

type searchCall struct {
	query string
	opts  academic_search.Options
}

func (f *fakeSearcher) Search(
	_ context.Context, query string, opts academic_search.Options,
) (*academic_search.Response, error) {
	f.calls = append(f.calls, searchCall{query: query, opts: opts})
	if err, ok := f.errs[query]; ok {
		return nil, err
	}
	if response, ok := f.byQuery[query]; ok {
		return response, nil
	}
	return &academic_search.Response{}, nil
}

func testConnector(searcher *fakeSearcher) *Connector {
	return &Connector{newSearcher: func(*Config) (registrySearcher, error) { return searcher, nil }}
}

func academicSettings(provider, profile string) map[string]interface{} {
	settings := baseSettings(provider, profile)
	settings["queries"] = []interface{}{
		map[string]interface{}{"query_id": drgQueryID, "query": secretQuery},
		map[string]interface{}{"query_id": fundQueryID, "query": "public funding preprints"},
	}
	settings["work_types"] = []interface{}{}
	return settings
}

func academicConfig(settings map[string]interface{}) *types.DataSourceConfig {
	return dataSourceConfig(settings, nil)
}

func doiWork() academic_search.Work {
	return academic_search.Work{
		DOI:     "10.1001/jama.2024.12345",
		PMID:    "38000000",
		Title:   "Deprescribing cascades in tender-driven formularies",
		Year:    2024,
		Authors: []string{"Wei Zhang", "Ana Silva"},
		Venue:   "JAMA",
		Type:    "journal-article",
	}
}

func pmidOnlyWork() academic_search.Work {
	return academic_search.Work{
		PMID:    "37999999",
		Title:   "A policy study with no DOI",
		Year:    1998,
		Authors: []string{"Doe J"},
		Venue:   "Health Policy",
		Type:    "Journal Article",
	}
}

func twoQuerySearcher() *fakeSearcher {
	return &fakeSearcher{
		byQuery: map[string]*academic_search.Response{
			secretQuery:                {Works: []academic_search.Work{doiWork()}, Total: 137},
			"public funding preprints": {Works: []academic_search.Work{pmidOnlyWork()}, Total: 23},
		},
		errs: map[string]error{},
	}
}

func arXivConfig() *types.DataSourceConfig {
	return academicConfig(academicSettings(providerArXiv, profileAnonymous))
}

func TestConnectorTypeAndShapeKeepLandingPageFetchingUnrepresentable(t *testing.T) {
	connector := NewConnector()
	if got := connector.Type(); got != types.ConnectorTypeAcademic {
		t.Errorf("Type() = %q, want %q", got, types.ConnectorTypeAcademic)
	}
	var _ datasource.Connector = connector

	// A web connector needs newPages/pageFetcher. This connector may have one seam
	// only: the registry searcher. Adding any field is a design change that has to
	// explain why identity-only candidates now need another capability.
	typ := reflect.TypeOf(Connector{})
	if typ.NumField() != 1 || typ.Field(0).Name != "newSearcher" {
		t.Fatalf("Connector fields changed: %v; a page/URL fetch seam must remain unrepresentable", typ)
	}
}

func TestNewConnectorCanConstructEveryRegistryClientWithoutCallingIt(t *testing.T) {
	cases := []struct {
		provider, profile string
		contact           map[string]interface{}
		credentials       map[string]interface{}
	}{
		{providerOpenAlex, profileAPIKey, map[string]interface{}{}, map[string]interface{}{"api_key": secretKey}},
		{providerCrossref, profilePolite, map[string]interface{}{"mailto": "rf@example.org"}, nil},
		{providerCrossref, profilePlus, map[string]interface{}{"mailto": "rf@example.org"}, map[string]interface{}{"api_key": secretKey}},
		{providerPubMed, profileAPIKey, map[string]interface{}{"tool": "ResearchFlow", "email": "rf@example.org"}, map[string]interface{}{"api_key": secretKey}},
		{providerArXiv, profileAnonymous, map[string]interface{}{}, nil},
	}
	for _, tc := range cases {
		settings := academicSettings(tc.provider, tc.profile)
		settings["contact"] = tc.contact
		cfg, err := parseConfig(dataSourceConfig(settings, tc.credentials))
		if err != nil {
			t.Fatalf("%s/%s parse: %v", tc.provider, tc.profile, err)
		}
		if _, err := NewConnector().newSearcher(cfg); err != nil {
			t.Errorf("%s/%s construction: %v", tc.provider, tc.profile, err)
		}
	}
}

// Validate parses local policy and constructs no client, so opening the settings
// drawer does not spend a registry request — important on OpenAlex, where every
// search has a monetary cost.
func TestValidateNeverCallsTheRegistry(t *testing.T) {
	searcher := twoQuerySearcher()
	connector := testConnector(searcher)
	if err := connector.Validate(context.Background(), arXivConfig()); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(searcher.calls) != 0 {
		t.Errorf("Validate made %d registry call(s)", len(searcher.calls))
	}
}

func TestListResourcesExposesIDsAndNeverQueryText(t *testing.T) {
	connector := testConnector(twoQuerySearcher())
	resources, err := connector.ListResources(context.Background(), arXivConfig(), "")
	if err != nil {
		t.Fatalf("ListResources() error = %v", err)
	}
	if len(resources) != 2 || resources[0].ExternalID != drgQueryID || resources[1].ExternalID != fundQueryID {
		t.Errorf("resources = %+v", resources)
	}
	for _, resource := range resources {
		if strings.Contains(fmt.Sprintf("%+v", resource), secretQuery) {
			t.Errorf("resource response leaks the query: %+v", resource)
		}
	}
	children, err := connector.ListResources(context.Background(), arXivConfig(), drgQueryID)
	if err != nil || len(children) != 0 {
		t.Errorf("child listing = %+v, %v", children, err)
	}
	ancestors, err := connector.ResolveResourceAncestors(context.Background(), arXivConfig(), []string{drgQueryID})
	if err != nil || len(ancestors) != 0 {
		t.Errorf("ancestors = %v, %v", ancestors, err)
	}
}

func TestFetchAllProjectsIdentityIntoNonEmptyCards(t *testing.T) {
	searcher := twoQuerySearcher()
	items, err := testConnector(searcher).FetchAll(context.Background(), arXivConfig(), nil)
	if err != nil {
		t.Fatalf("FetchAll() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}

	first := items[0]
	if first.ExternalID != drgQueryID+":doi:10.1001/jama.2024.12345" {
		t.Errorf("ExternalID = %q", first.ExternalID)
	}
	if first.URL != "https://doi.org/10.1001/jama.2024.12345" {
		t.Errorf("URL = %q", first.URL)
	}
	if len(first.Content) == 0 {
		t.Fatal("Content is empty; WeKnora would take CreateKnowledgeFromURL and fetch the resolver")
	}
	if first.ContentType != "text/markdown" || !strings.HasSuffix(first.FileName, ".md") {
		t.Errorf("content type/file name = %q/%q", first.ContentType, first.FileName)
	}
	if first.SourceResourceID != drgQueryID {
		t.Errorf("SourceResourceID = %q", first.SourceResourceID)
	}
	if first.UpdatedAt != time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC) {
		t.Errorf("UpdatedAt = %v", first.UpdatedAt)
	}
	wantMetadata := map[string]string{
		"channel":                types.ChannelAcademic,
		"saved_query_id":         drgQueryID,
		"discovery_source_id":    "src-medical-academic",
		"discovery_project_id":   "medical-policy",
		"discovery_provider":     providerArXiv,
		"academic_identity":      "doi:10.1001/jama.2024.12345",
		"academic_reference_url": first.URL,
	}
	for key, want := range wantMetadata {
		if got := first.Metadata[key]; got != want {
			t.Errorf("Metadata[%q] = %q, want %q", key, got, want)
		}
	}
	for _, forbidden := range []string{"query", "abstract", "summary", "snippet", "landing_url"} {
		if _, ok := first.Metadata[forbidden]; ok {
			t.Errorf("Metadata carries forbidden key %q", forbidden)
		}
	}

	second := items[1]
	if second.URL != "https://pubmed.ncbi.nlm.nih.gov/37999999/" {
		t.Errorf("PMID fallback URL = %q", second.URL)
	}
	if second.ExternalID != fundQueryID+":pmid:37999999" {
		t.Errorf("PMID fallback ExternalID = %q", second.ExternalID)
	}
}

// This pins ADR-0013 §2 at the connector crossing. FetchedItem.URL is non-empty,
// so an empty Content would make the sync service download and parse it itself.
func TestEveryEmittedItemHasContentSoURLFetchingStaysUnreachable(t *testing.T) {
	works := []academic_search.Work{
		doiWork(), pmidOnlyWork(),
		{ArXivID: "2401.00001", Title: "fallback"},
		{DOI: "10.48550/arxiv.2401.00001", ArXivID: "2401.00001"},
	}
	searcher := &fakeSearcher{byQuery: map[string]*academic_search.Response{
		secretQuery: {Works: works}, "public funding preprints": {},
	}, errs: map[string]error{}}
	items, err := testConnector(searcher).FetchAll(context.Background(), arXivConfig(), nil)
	if err != nil {
		t.Fatalf("FetchAll() error = %v", err)
	}
	if len(items) != len(works) {
		t.Fatalf("got %d items, want %d", len(items), len(works))
	}
	for _, item := range items {
		if item.URL == "" || len(item.Content) == 0 {
			t.Errorf("item %q has URL=%q content_bytes=%d; that shape reaches CreateKnowledgeFromURL",
				item.ExternalID, item.URL, len(item.Content))
		}
	}
}

func TestFetchIncrementalSkipsUnchangedCardsAndEmitsChangedMetadata(t *testing.T) {
	ctx := context.Background()
	searcher := twoQuerySearcher()
	connector := testConnector(searcher)

	first, cursor, err := connector.FetchIncremental(ctx, arXivConfig(), nil)
	if err != nil || cursor == nil || len(first) != 2 {
		t.Fatalf("first sync = %d items, cursor=%v, err=%v", len(first), cursor, err)
	}
	second, next, err := connector.FetchIncremental(ctx, arXivConfig(), cursor)
	if err != nil || next == nil || len(second) != 0 {
		t.Fatalf("unchanged sync = %d items, cursor=%v, err=%v", len(second), next, err)
	}

	changed := doiWork()
	changed.Venue = "JAMA Network Open"
	searcher.byQuery[secretQuery] = &academic_search.Response{Works: []academic_search.Work{changed}}
	third, _, err := connector.FetchIncremental(ctx, arXivConfig(), next)
	if err != nil || len(third) != 1 {
		t.Fatalf("changed sync = %d items, err=%v", len(third), err)
	}
	if !strings.Contains(string(third[0].Content), "JAMA Network Open") {
		t.Errorf("changed card =\n%s", third[0].Content)
	}
}

// Candidate failure: malformed identity is left out of the cursor so it retries,
// while valid candidates from the same query still emit.
func TestOneBadCandidateDoesNotPoisonItsQueryAndRetriesNextRun(t *testing.T) {
	bad := academic_search.Work{PMID: "../../secret", Title: "bad"}
	searcher := &fakeSearcher{byQuery: map[string]*academic_search.Response{
		secretQuery:                {Works: []academic_search.Work{doiWork(), bad}},
		"public funding preprints": {},
	}, errs: map[string]error{}}
	connector := testConnector(searcher)

	items, cursor, err := connector.FetchIncremental(context.Background(), arXivConfig(), nil)
	if len(items) != 1 || cursor == nil {
		t.Fatalf("items=%d cursor=%v err=%v", len(items), cursor, err)
	}
	var partial *datasource.PartialFetchError
	if !errors.As(err, &partial) {
		t.Fatalf("error = %T %v, want PartialFetchError", err, err)
	}

	// The malformed record appears again and fails again: recording it would have
	// made this second run silently skip it forever.
	_, _, err = connector.FetchIncremental(context.Background(), arXivConfig(), cursor)
	if !errors.As(err, &partial) {
		t.Fatalf("second error = %T %v, want the bad candidate retried", err, err)
	}
}

// Query failure: that query's old progress is carried forward unchanged, so
// recovery does not re-emit everything it had already produced.
func TestOneFailedQueryCarriesItsPriorProgressAndReportsPartial(t *testing.T) {
	ctx := context.Background()
	searcher := twoQuerySearcher()
	connector := testConnector(searcher)
	_, cursor, err := connector.FetchIncremental(ctx, arXivConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}

	searcher.errs[secretQuery] = errors.New("registry unavailable")
	items, partialCursor, err := connector.FetchIncremental(ctx, arXivConfig(), cursor)
	if len(items) != 0 || partialCursor == nil {
		t.Fatalf("partial sync = %d items, cursor=%v", len(items), partialCursor)
	}
	var partial *datasource.PartialFetchError
	if !errors.As(err, &partial) {
		t.Fatalf("error = %T %v", err, err)
	}

	delete(searcher.errs, secretQuery)
	items, _, err = connector.FetchIncremental(ctx, arXivConfig(), partialCursor)
	if err != nil || len(items) != 0 {
		t.Fatalf("recovery re-emitted %d item(s), err=%v", len(items), err)
	}
}

// Total outage: no cursor at all. The service persists any non-nil cursor even
// with a fetch error; returning one here would mark every candidate as seen.
func TestEveryQueryFailingReturnsNoCursor(t *testing.T) {
	searcher := twoQuerySearcher()
	searcher.errs[secretQuery] = errors.New("down")
	searcher.errs["public funding preprints"] = errors.New("down")

	items, cursor, err := testConnector(searcher).FetchIncremental(context.Background(), arXivConfig(), nil)
	if len(items) != 0 || cursor != nil {
		t.Fatalf("items=%d cursor=%v", len(items), cursor)
	}
	if !errors.Is(err, datasource.ErrFetchFailed) {
		t.Fatalf("error = %v, want ErrFetchFailed", err)
	}
}

func TestManifestHashChangeDiscardsThePriorCursor(t *testing.T) {
	ctx := context.Background()
	searcher := twoQuerySearcher()
	connector := testConnector(searcher)
	_, cursor, err := connector.FetchIncremental(ctx, arXivConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	changed := academicSettings(providerArXiv, profileAnonymous)
	changed["manifest_hash"] = strings.Repeat("b", 64)
	items, _, err := connector.FetchIncremental(ctx, academicConfig(changed), cursor)
	if err != nil || len(items) != 2 {
		t.Fatalf("policy change emitted %d items, err=%v; want a complete rediscovery", len(items), err)
	}
}

func TestResourceSelectionIsExactAndUnknownIDsAreRefused(t *testing.T) {
	searcher := twoQuerySearcher()
	items, err := testConnector(searcher).FetchAll(context.Background(), arXivConfig(), []string{fundQueryID})
	if err != nil || len(items) != 1 || len(searcher.calls) != 1 || searcher.calls[0].query != "public funding preprints" {
		t.Fatalf("selected fetch: items=%d calls=%v err=%v", len(items), searcher.calls, err)
	}

	_, err = testConnector(twoQuerySearcher()).FetchAll(context.Background(), arXivConfig(), []string{"unknown"})
	if !errors.Is(err, datasource.ErrResourceNotFound) {
		t.Fatalf("unknown id error = %v", err)
	}
}

func TestRequestOptionsReachTheClientWithoutWidening(t *testing.T) {
	settings := academicSettings(providerArXiv, profileAnonymous)
	settings["count"] = 7
	settings["date_range"] = "2024-02-01..2025-03-31"
	settings["work_types"] = []interface{}{"preprint"}
	settings["open_access"] = academic_search.OpenAccessOnly
	searcher := twoQuerySearcher()
	_, _ = testConnector(searcher).FetchAll(context.Background(), academicConfig(settings), []string{drgQueryID})
	if len(searcher.calls) != 1 {
		t.Fatalf("calls = %v", searcher.calls)
	}
	opts := searcher.calls[0].opts
	if opts.Count != 7 || opts.From.Format("2006-01-02") != "2024-02-01" ||
		opts.To.Format("2006-01-02") != "2025-03-31" || opts.OpenAccess != academic_search.OpenAccessOnly ||
		len(opts.WorkTypes) != 1 || opts.WorkTypes[0] != "preprint" {
		t.Errorf("Options = %+v", opts)
	}
}

func TestLogsAndErrorsContainNeitherQueryNorCredentials(t *testing.T) {
	var logs bytes.Buffer
	logger.SetLogLevel(logger.LevelDebug)
	logger.SetOutput(&logs)
	t.Cleanup(logger.ConfigureFromEnv)

	searcher := twoQuerySearcher()
	connector := testConnector(searcher)
	if _, err := connector.FetchAll(context.Background(), arXivConfig(), []string{drgQueryID}); err != nil {
		t.Fatal(err)
	}
	searcher.errs[secretQuery] = errors.New("registry unavailable")
	_, err := connector.FetchAll(context.Background(), arXivConfig(), []string{drgQueryID})
	if err == nil {
		t.Fatal("failed registry search was accepted")
	}

	if !strings.Contains(logs.String(), "[AcademicDiscovery]") {
		t.Fatalf("captured no connector logs at all:\n%s", logs.String())
	}
	for _, banned := range []string{secretQuery, secretKey, "SECRET-ABSTRACT", "SECRET-SUMMARY"} {
		if strings.Contains(logs.String(), banned) || strings.Contains(err.Error(), banned) {
			t.Errorf("logs or error leak %q:\nlogs=%s\nerr=%v", banned, logs.String(), err)
		}
	}
}
