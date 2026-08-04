// Package openalex queries the OpenAlex Works API for work identities.
//
// OpenAlex differs from the other three registries in two ways that shape this
// client:
//
//   - It carries the abstract as an inverted index — `abstract_inverted_index`,
//     documented as "the abstract as an inverted index (word positions)" — which
//     reconstructs verbatim with a sort. It is the reason a body-shaped-field-name
//     guard is not enough anywhere in this tree: the field is not called
//     "abstract". Three things keep it out, in order of strength: it is not in
//     `select`, so the server never sends it; the wire struct has no field it
//     could decode into; and Work has none either (ADR-0012 §2).
//   - It is metered by spend, not by rate: $1/day with a key and $0.10/day
//     without one, with search costing roughly ten times a plain list, and
//     content download billed separately (which is one more reason this lane
//     never downloads anything). A key is therefore required here rather than
//     optional — a key-less client is not cheaper, it is a tenth of the budget
//     presenting itself as "nothing was published on this topic" (ADR-0012 §5
//     gap 1).
//
// Verified against the official documentation on 2026-08-03, which closes
// ADR-0012 §9 item 2: the current access model is key-only, sent as the
// `api_key=` query parameter, and there is no polite pool and no `mailto`
// convention. That the key travels in a URL rather than a header is not our
// choice; what follows from it is that no log line here may contain a request URL.
package openalex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search"
	"github.com/Tencent/WeKnora/internal/logger"
)

const (
	// Source names this registry in errors and audit counters.
	Source = "openalex"

	// Endpoint is hardcoded, never manifest-configurable, so a stored parameter
	// can never redirect search traffic.
	Endpoint = "https://api.openalex.org/works"

	// MinRequestInterval is a courtesy constant, not a published limit: OpenAlex
	// now meters by spend rather than rate. It is here because an unpaced loop
	// spends a day's budget in seconds, and because a registry with no rate limit
	// today may publish one tomorrow.
	MinRequestInterval = 100 * time.Millisecond

	maxResponseBytes = 2 << 20
)

// selectFields is the identity projection, requested by name.
//
// `authorships` and `primary_location` are whole objects because OpenAlex's
// select takes top-level fields; they arrive carrying more than we want
// (affiliation strings, landing page URLs) and the wire struct below declares
// only the two leaves this lane uses, so the decoder discards the rest.
var selectFields = []string{
	"id", "doi", "ids", "display_name", "publication_year", "type",
	"authorships", "primary_location",
}

// openAlexWorkTypes maps the kernel vocabulary onto OpenAlex's own terms.
//
// The map is short because OpenAlex's `type` has **no enum** in its OpenAPI
// spec — the schema is a bare string whose description names six "common values"
// (article, book, dataset, preprint, dissertation, book-chapter). Only those we
// can cite are mapped. A filter on a value OpenAlex does not actually use
// matches nothing, and "no reviews on this topic" is a wrong answer wearing the
// clothes of a right one, so an unmappable type is refused instead.
//
// Widening this map is a documentation task, not a code one: cite the value
// first, then add it.
var openAlexWorkTypes = map[string]string{
	academic_search.WorkTypeJournalArticle: "article",
	academic_search.WorkTypePreprint:       "preprint",
	academic_search.WorkTypeBookChapter:    "book-chapter",
	academic_search.WorkTypeDataset:        "dataset",
}

// pmidPattern is what a bare PubMed id looks like. Anything else is refused
// rather than repaired: a mangled fallback identity is worse than none, because
// the candidate would look promotable and match nothing.
var pmidPattern = regexp.MustCompile(`^\d{1,9}$`)

// pmidPrefixes are stripped before matching. OpenAlex stores ids.pmid as a
// string whose observed form is a resolver URL, and the OpenAPI spec declares no
// format — so both forms are accepted rather than betting on one.
var pmidPrefixes = []string{
	"https://pubmed.ncbi.nlm.nih.gov/",
	"http://pubmed.ncbi.nlm.nih.gov/",
	"https://www.ncbi.nlm.nih.gov/pubmed/",
	"pmid:",
}

// Config builds a Client.
type Config struct {
	// HTTPClient is required. The caller owns the outbound network policy.
	HTTPClient *http.Client
	// APIKey is required; see the package doc for why it is not optional.
	APIKey string
	// MaxAttempts bounds retries of transient failures; 0 means the shared default.
	MaxAttempts int
}

// Client queries the OpenAlex Works API.
type Client struct {
	httpClient  *http.Client
	endpoint    string
	apiKey      string
	pacer       *academic_search.Pacer
	maxAttempts int
	sleep       func(context.Context, time.Duration) error
}

// NewClient validates the configuration and returns a ready client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.HTTPClient == nil {
		return nil, fmt.Errorf("openalex: an HTTP client is required")
	}
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf(
			"openalex: an API key is required; a key-less caller is metered at a tenth of the budget " +
				"and fails by returning nothing rather than by reporting an error")
	}
	attempts := cfg.MaxAttempts
	if attempts <= 0 {
		attempts = academic_search.DefaultAttempts
	}
	return &Client{
		httpClient:  cfg.HTTPClient,
		endpoint:    Endpoint,
		apiKey:      apiKey,
		pacer:       academic_search.NewPacer(MinRequestInterval),
		maxAttempts: attempts,
		sleep:       academic_search.SleepContext,
	}, nil
}

// Search runs one query and returns one page of work identities.
func (c *Client) Search(
	ctx context.Context, query string, opts academic_search.Options,
) (*academic_search.Response, error) {
	opts, err := opts.Normalized()
	if err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("openalex: query is empty")
	}
	// Before the request, not after: on a metered registry a refusable profile
	// that reaches the wire costs money to be told what a map already knew.
	filters, err := renderFilters(opts)
	if err != nil {
		return nil, err
	}

	requestURL := c.buildURL(query, opts, filters)

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if err := c.pacer.Wait(ctx); err != nil {
			return nil, err
		}
		response, err := c.attempt(ctx, requestURL)
		if err == nil {
			logger.Infof(ctx, "[AcademicSearch][OpenAlex] returned %d works, dropped %d, attempt %d",
				len(response.Works), response.Dropped, attempt)
			return response, nil
		}
		lastErr = err

		apiErr, ok := err.(*academic_search.APIError)
		if !ok || !apiErr.Retryable || attempt == c.maxAttempts {
			logger.Warnf(ctx, "[AcademicSearch][OpenAlex] search failed after %d attempt(s): %v", attempt, err)
			return nil, err
		}
		delay := academic_search.RetryDelay(attempt, apiErr.RetryAfter)
		if sleepErr := c.sleep(ctx, delay); sleepErr != nil {
			logger.Warnf(ctx, "[AcademicSearch][OpenAlex] backoff interrupted after %d attempt(s): %v",
				attempt, err)
			return nil, err
		}
	}
	return nil, lastErr
}

// buildURL renders the documented request. Sorting by publication date
// descending is what makes this discovery rather than a bibliography lookup.
func (c *Client) buildURL(query string, opts academic_search.Options, filters []string) string {
	params := url.Values{}
	params.Set("search", query)
	// per_page with an underscore: the hyphenated spelling is the older one.
	params.Set("per_page", strconv.Itoa(opts.Count))
	params.Set("select", strings.Join(selectFields, ","))
	params.Set("sort", "publication_date:desc")
	params.Set("api_key", c.apiKey)
	if len(filters) > 0 {
		params.Set("filter", strings.Join(filters, ","))
	}
	return c.endpoint + "?" + params.Encode()
}

// renderFilters maps the profile onto OpenAlex filter terms.
func renderFilters(opts academic_search.Options) ([]string, error) {
	filters := make([]string, 0, 4)
	if !opts.From.IsZero() {
		filters = append(filters, "from_publication_date:"+opts.From.UTC().Format("2006-01-02"))
	}
	if !opts.To.IsZero() {
		filters = append(filters, "to_publication_date:"+opts.To.UTC().Format("2006-01-02"))
	}
	if opts.OpenAccess == academic_search.OpenAccessOnly {
		filters = append(filters, "open_access.is_oa:true")
	}
	if len(opts.WorkTypes) > 0 {
		mapped := make([]string, 0, len(opts.WorkTypes))
		for _, workType := range opts.WorkTypes {
			term, ok := openAlexWorkTypes[workType]
			if !ok {
				return nil, fmt.Errorf(
					"openalex: work type %q is not among the values OpenAlex documents, and its `type` "+
						"field has no enum to check against; filtering on it would match nothing "+
						"and read as an empty topic", workType)
			}
			mapped = append(mapped, term)
		}
		// A pipe, not repeated comma-separated terms: commas are AND, and no work
		// has two types at once, so a comma here would return nothing.
		filters = append(filters, "type:"+strings.Join(mapped, "|"))
	}
	return filters, nil
}

// attempt performs one request/response cycle.
func (c *Client) attempt(ctx context.Context, requestURL string) (*academic_search.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		// The error is not wrapped: its message would contain the URL, and the
		// URL contains the API key.
		return nil, &academic_search.APIError{Source: Source, Code: "RequestBuildFailed"}
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// url.Error stringifies with the request URL in it, so only the innermost
		// cause is carried.
		return nil, &academic_search.APIError{
			Source: Source, Code: "TransportError", Retryable: true, Err: innermost(err),
		}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &academic_search.APIError{
			Source: Source, HTTPStatus: resp.StatusCode, Code: "ResponseReadFailed",
			Retryable: true, Err: innermost(err),
		}
	}
	if len(raw) > maxResponseBytes {
		return nil, &academic_search.APIError{
			Source: Source, HTTPStatus: resp.StatusCode, Code: "ResponseTooLarge",
		}
	}

	if resp.StatusCode != http.StatusOK {
		// The body is not wrapped: an OpenAlex error message quotes the filter,
		// and the filter came from the saved query.
		return nil, &academic_search.APIError{
			Source:     Source,
			HTTPStatus: resp.StatusCode,
			Code:       http.StatusText(resp.StatusCode),
			Retryable:  academic_search.HTTPStatusRetryable(resp.StatusCode),
			RetryAfter: academic_search.ParseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	var envelope workList
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &academic_search.APIError{
			Source: Source, HTTPStatus: resp.StatusCode, Code: "InvalidResponse",
		}
	}
	// An empty page and a truncated one are different answers, and only the
	// pointers can tell them apart: `"results": []` decodes to a non-nil empty
	// slice, a missing key to nil. Reporting a truncated envelope as zero hits
	// would read to a researcher as "nothing published on this topic".
	if envelope.Meta == nil || envelope.Results == nil {
		return nil, &academic_search.APIError{
			Source: Source, HTTPStatus: resp.StatusCode, Code: "MissingResults",
		}
	}

	return project(envelope), nil
}

// project turns the work list into identities.
func project(envelope workList) *academic_search.Response {
	results := *envelope.Results
	response := &academic_search.Response{
		Works: make([]academic_search.Work, 0, len(results)),
		Total: envelope.Meta.Count,
	}
	for _, record := range results {
		doi := academic_search.NormalizeDOI(record.DOI)
		if doi == "" {
			// ids.doi and the top-level doi are the same value in practice, but
			// only one of them is guaranteed present.
			doi = academic_search.NormalizeDOI(record.IDs.DOI)
		}
		work := academic_search.Work{
			DOI:   doi,
			PMID:  normalizePMID(record.IDs.PMID),
			Title: collapseSpace(record.DisplayName),
			Year:  record.PublicationYear,
			Venue: collapseSpace(record.PrimaryLocation.Source.DisplayName),
			// The registry's own term, unmapped: what a candidate can honestly
			// report is what OpenAlex said.
			Type: strings.TrimSpace(record.Type),
		}
		for _, authorship := range record.Authorships {
			if name := collapseSpace(authorship.Author.DisplayName); name != "" {
				work.Authors = append(work.Authors, name)
			}
		}
		// Note what is not used as an identity: record.ID, the OpenAlex work URL.
		// It names a row in OpenAlex, not the work, so a researcher cannot promote
		// an acquired PDF under it and the hand-off would dead-end.
		if _, ok := work.Identity(); !ok {
			response.Dropped++
			continue
		}
		response.Works = append(response.Works, work)
	}
	return response
}

// normalizePMID returns the bare PubMed id, or "" when the value is not one.
func normalizePMID(value string) string {
	candidate := strings.TrimSpace(value)
	if candidate == "" {
		return ""
	}
	for _, prefix := range pmidPrefixes {
		if rest, ok := strings.CutPrefix(candidate, prefix); ok {
			candidate = rest
			break
		}
	}
	candidate = strings.Trim(candidate, "/")
	if !pmidPattern.MatchString(candidate) {
		return ""
	}
	return candidate
}

// innermost unwraps to the deepest cause. Go's *url.Error stringifies with the
// request URL in it, and on this registry the URL carries the API key — so the
// wrapper is dropped and only the cause it wraps is kept.
func innermost(err error) error {
	for {
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		inner := unwrapped.Unwrap()
		if inner == nil {
			return err
		}
		err = inner
	}
}

// collapseSpace folds line breaks and indentation so a reading-list entry is one
// line.
func collapseSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// workList and the types below are the wire format, and what they omit is the
// point. There is no field for abstract_inverted_index, so the inverted abstract
// is discarded by the decoder even if `select` is ignored or widened. The same
// goes for authorships' raw affiliation strings and primary_location's landing
// page URL: they arrive and they are dropped.
type workList struct {
	Meta *listMeta `json:"meta"`
	// A pointer so a missing key is distinguishable from an empty page.
	Results *[]work `json:"results"`
}

type listMeta struct {
	Count int `json:"count"`
}

type work struct {
	// ID is the OpenAlex work URL. It is decoded so this struct matches the
	// documented shape, and deliberately not used as an identity; see project.
	ID              string       `json:"id"`
	DOI             string       `json:"doi"`
	IDs             workIDs      `json:"ids"`
	DisplayName     string       `json:"display_name"`
	PublicationYear int          `json:"publication_year"`
	Type            string       `json:"type"`
	Authorships     []authorship `json:"authorships"`
	PrimaryLocation location     `json:"primary_location"`
}

type workIDs struct {
	DOI  string `json:"doi"`
	PMID string `json:"pmid"`
}

type authorship struct {
	Author author `json:"author"`
}

type author struct {
	DisplayName string `json:"display_name"`
}

type location struct {
	Source locationSource `json:"source"`
}

type locationSource struct {
	DisplayName string `json:"display_name"`
}
