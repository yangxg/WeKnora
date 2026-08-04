// Package crossref queries the Crossref REST API for work identities.
//
// Crossref is the friendliest of the four registries on metadata — "almost none
// of the metadata is subject to copyright, and you may use it for any purpose" —
// with one carve-out that decides this client's shape: "Some abstracts contained
// in the metadata may be subject to copyright by publishers or authors." That
// single sentence is what removed the last uniform footing under treating a
// registry abstract as governed text (ADR-0012 §2), so this client asks for
// identity fields by name and the abstract never crosses the wire.
//
// Three Crossref-specific decisions are worth reading before editing:
//
//   - The anonymous public pool is not offered. Crossref tries to contact a
//     misbehaving caller before blocking it, and an anonymous caller has given
//     them nobody to contact — so "run anonymously" is a way to be blocked
//     without warning, and ADR-0012 §5 makes it structurally unavailable rather
//     than merely discouraged. Only PoolPolite and PoolPlus exist.
//   - The pool the registry reports back is checked against the one configured.
//     Crossref's own documentation disagrees with itself about the name of the
//     Plus token header (its prose says Crossref-Plus-API-Token, one curl
//     example on the same page says crossref-api-key — ADR-0012 §9 item 3), and
//     the failure mode of getting it wrong is HTTP 200 from a lower pool: the
//     project's paid rate silently disappears and the only symptom is slowness.
//     Comparing x-api-pool turns that into a refusal.
//   - Filters Crossref cannot express are refused, not dropped. There is no
//     review-article type and no open-access filter here; answering such a
//     profile with an unfiltered reading list would look like an answer.
package crossref

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
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/infrastructure/academic_search"
	"github.com/Tencent/WeKnora/internal/logger"
)

const (
	// Source names this registry in errors and audit counters.
	Source = "crossref"

	// Endpoint is hardcoded, never manifest-configurable, so a stored parameter
	// can never redirect search traffic.
	Endpoint = "https://api.crossref.org/works"

	// PoolPolite and PoolPlus are the two admissible endpoint profiles. See the
	// package doc for why there is no anonymous pool.
	PoolPolite = "polite"
	PoolPlus   = "plus"

	// poolPublic is named only so a manifest that asks for it can be refused
	// with an explanation rather than an "unknown value" error.
	poolPublic = "public"

	// PoliteInterval and PlusInterval sit under the documented ceilings of 10 and
	// 150 requests per second. Under, not on: the ceilings are counted across
	// everything sharing our address, so spending the whole budget in one process
	// leaves nothing for a second one.
	PoliteInterval = 110 * time.Millisecond
	PlusInterval   = 10 * time.Millisecond

	// plusTokenHeader is the documented Metadata Plus header. See the package doc
	// for the documentation inconsistency around it and how it is caught.
	plusTokenHeader = "Crossref-Plus-API-Token"

	maxResponseBytes = 2 << 20
)

// selectFields is the identity projection, requested by name.
//
// This is the strongest of the three defences against a registry abstract: a
// field the server was never asked for is not in the response, so it cannot be
// mishandled by a later reader. The wire struct below also declares no field for
// it, and the parent package's Work has none either.
var selectFields = []string{"DOI", "title", "issued", "author", "container-title", "type"}

// crossrefWorkTypes maps the kernel vocabulary onto Crossref's own terms.
//
// Preprints are the entry that matters: Crossref files them as posted-content,
// and passing the kernel's "preprint" through would filter on a type that does
// not exist — matching nothing, and reading to the researcher as "no preprints
// on this topic". A kernel type absent from this map is refused; see
// unmappedWorkTypeReason.
var crossrefWorkTypes = map[string]string{
	academic_search.WorkTypeJournalArticle:  "journal-article",
	academic_search.WorkTypePreprint:        "posted-content",
	academic_search.WorkTypeBookChapter:     "book-chapter",
	academic_search.WorkTypeConferencePaper: "proceedings-article",
	academic_search.WorkTypeDataset:         "dataset",
}

// poolRank orders the pools so a reported pool can be compared with a configured
// one. Only the ordering matters, not the values.
var poolRank = map[string]int{poolPublic: 1, PoolPolite: 2, PoolPlus: 3}

// mailtoPattern is a deliberately loose check. It is here to catch a manifest
// that wrote a name where an address belongs, not to adjudicate RFC 5322: a
// mailto Crossref cannot use is a polite-pool request that silently is not one.
var mailtoPattern = regexp.MustCompile(`^[^@\s]+@[^@\s.]+\.[^@\s]+$`)

// RateLimits is what the registry says it is currently applying to this caller.
//
// Crossref reports these on every response, and they are worth recording: a
// limit below the pool's documented one is how a throttle announces itself
// before it becomes a block. They are counts, so unlike a query they may be
// logged.
type RateLimits struct {
	Limit       int
	Interval    time.Duration
	Concurrency int
}

// Config builds a Client.
type Config struct {
	// HTTPClient is required. The caller owns the outbound network policy.
	HTTPClient *http.Client
	// Pool is PoolPolite (default) or PoolPlus.
	Pool string
	// Mailto is the project's role address, the non-secret contact identity that
	// ADR-0012 §5 gap 2 places in the version-controlled manifest. It is
	// required for both pools and is the only PII a manifest may carry.
	Mailto string
	// ToolName identifies this caller in the User-Agent. Crossref asks callers
	// to be identifiable; an unidentifiable one is contacted by nobody and
	// blocked without warning.
	ToolName string
	// PlusToken is the Metadata Plus credential. Required for PoolPlus and
	// refused for PoolPolite — a token that will never be sent is a paid
	// subscription silently going unused.
	PlusToken string
	// MaxAttempts bounds retries of transient failures; 0 means the shared default.
	MaxAttempts int
}

// Client queries the Crossref REST API.
type Client struct {
	httpClient *http.Client
	endpoint   string
	pool       string
	// expectedPool is the pool a response must report at or above. Empty
	// disables the check, which only the tests do.
	expectedPool string
	mailto       string
	userAgent    string
	plusToken    string
	pacer        *academic_search.Pacer
	maxAttempts  int
	sleep        func(context.Context, time.Duration) error

	mu     sync.Mutex
	limits RateLimits
}

// NewClient validates the configuration and returns a ready client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.HTTPClient == nil {
		return nil, fmt.Errorf("crossref: an HTTP client is required")
	}

	pool := cfg.Pool
	if pool == "" {
		pool = PoolPolite
	}
	switch pool {
	case PoolPolite, PoolPlus:
	case poolPublic:
		return nil, fmt.Errorf(
			"crossref: the anonymous %q pool is not available; Crossref contacts a caller before "+
				"blocking it and an anonymous caller cannot be contacted, so use %q or %q",
			poolPublic, PoolPolite, PoolPlus,
		)
	default:
		return nil, fmt.Errorf("crossref: unknown pool %q, want %q or %q", pool, PoolPolite, PoolPlus)
	}

	mailto := strings.TrimSpace(cfg.Mailto)
	if mailto == "" {
		return nil, fmt.Errorf("crossref: a role contact address is required to identify this caller")
	}
	if !mailtoPattern.MatchString(mailto) {
		return nil, fmt.Errorf("crossref: the contact identity is not an email address")
	}

	token := strings.TrimSpace(cfg.PlusToken)
	switch {
	case pool == PoolPlus && token == "":
		return nil, fmt.Errorf(
			"crossref: the %q pool requires a Metadata Plus token; without it the request would be "+
				"answered from a lower pool and the only symptom would be slowness", PoolPlus)
	case pool == PoolPolite && token != "":
		// Refused rather than ignored: silently dropping a credential is how a
		// paid subscription ends up unused for months.
		return nil, fmt.Errorf(
			"crossref: a Metadata Plus token was supplied for the %q pool, where it is never sent; "+
				"declare the %q pool or drop the token", PoolPolite, PoolPlus)
	}

	tool := strings.TrimSpace(cfg.ToolName)
	if tool == "" {
		tool = "ResearchFlow"
	}
	attempts := cfg.MaxAttempts
	if attempts <= 0 {
		attempts = academic_search.DefaultAttempts
	}
	interval := PoliteInterval
	if pool == PoolPlus {
		interval = PlusInterval
	}

	return &Client{
		httpClient:   cfg.HTTPClient,
		endpoint:     Endpoint,
		pool:         pool,
		expectedPool: pool,
		mailto:       mailto,
		// Crossref's documented form names the tool and repeats the contact
		// address, so a person reading their logs can act without a lookup.
		userAgent:   fmt.Sprintf("%s (mailto:%s)", tool, mailto),
		plusToken:   token,
		pacer:       academic_search.NewPacer(interval),
		maxAttempts: attempts,
		sleep:       academic_search.SleepContext,
	}, nil
}

// ObservedLimits reports the limits the registry last declared.
func (c *Client) ObservedLimits() RateLimits {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limits
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
		return nil, fmt.Errorf("crossref: query is empty")
	}
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
			logger.Infof(ctx, "[AcademicSearch][Crossref] pool=%s returned %d works, dropped %d, attempt %d",
				c.pool, len(response.Works), response.Dropped, attempt)
			return response, nil
		}
		lastErr = err

		apiErr, ok := err.(*academic_search.APIError)
		if !ok || !apiErr.Retryable || attempt == c.maxAttempts {
			logger.Warnf(ctx, "[AcademicSearch][Crossref] search failed after %d attempt(s): %v", attempt, err)
			return nil, err
		}
		delay := academic_search.RetryDelay(attempt, apiErr.RetryAfter)
		if sleepErr := c.sleep(ctx, delay); sleepErr != nil {
			logger.Warnf(ctx, "[AcademicSearch][Crossref] backoff interrupted after %d attempt(s): %v",
				attempt, err)
			return nil, err
		}
	}
	return nil, lastErr
}

// buildURL renders the documented request.
//
// query.bibliographic rather than the free-form query parameter: it scopes the
// match to the citation fields, so a hit is explainable from what the reading
// list shows. Sorting by issue date descending is what makes this discovery
// rather than a bibliography lookup — an incremental run wants what is new, and
// relevance order would bury it.
func (c *Client) buildURL(query string, opts academic_search.Options, filters []string) string {
	params := url.Values{}
	params.Set("query.bibliographic", query)
	params.Set("rows", strconv.Itoa(opts.Count))
	params.Set("select", strings.Join(selectFields, ","))
	params.Set("sort", "issued")
	params.Set("order", "desc")
	// The polite-pool identity goes in the query string as documented; it is not
	// a secret, it is the point.
	params.Set("mailto", c.mailto)
	if len(filters) > 0 {
		params.Set("filter", strings.Join(filters, ","))
	}
	return c.endpoint + "?" + params.Encode()
}

// renderFilters maps the profile onto Crossref filter terms, refusing anything
// Crossref cannot express.
func renderFilters(opts academic_search.Options) ([]string, error) {
	if opts.OpenAccess == academic_search.OpenAccessOnly {
		// Crossref records licences, not access status. has-license:true would be
		// a different question wearing this one's name, and a reading list of
		// paywalled works presented as open access is worse than a refusal.
		return nil, fmt.Errorf(
			"crossref: an open-access-only filter cannot be expressed here; Crossref records licences, " +
				"not access status")
	}
	filters := make([]string, 0, len(opts.WorkTypes)+2)
	if !opts.From.IsZero() {
		filters = append(filters, "from-pub-date:"+opts.From.UTC().Format("2006-01-02"))
	}
	if !opts.To.IsZero() {
		filters = append(filters, "until-pub-date:"+opts.To.UTC().Format("2006-01-02"))
	}
	for _, workType := range opts.WorkTypes {
		mapped, ok := crossrefWorkTypes[workType]
		if !ok {
			return nil, fmt.Errorf("crossref: %s", unmappedWorkTypeReason(workType))
		}
		filters = append(filters, "type:"+mapped)
	}
	return filters, nil
}

// unmappedWorkTypeReason explains a refusal rather than reporting a lookup miss,
// because the fix is always to edit the manifest and the reader needs to know
// which way.
func unmappedWorkTypeReason(workType string) string {
	if workType == academic_search.WorkTypeReview {
		return fmt.Sprintf(
			"work type %q has no Crossref equivalent (its peer-review type is a review *of* a work, "+
				"not a review article); drop it from the Crossref filter", workType)
	}
	return fmt.Sprintf("work type %q has no Crossref equivalent; drop it from the Crossref filter", workType)
}

// attempt performs one request/response cycle.
func (c *Client) attempt(ctx context.Context, requestURL string) (*academic_search.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, &academic_search.APIError{Source: Source, Code: "RequestBuildFailed", Err: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if c.plusToken != "" {
		// A header, not a query parameter: a secret in a URL lands in an access
		// log at every hop between here and the registry.
		req.Header.Set(plusTokenHeader, "Bearer "+c.plusToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &academic_search.APIError{
			Source: Source, Code: "TransportError", Retryable: true, Err: err,
		}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &academic_search.APIError{
			Source: Source, HTTPStatus: resp.StatusCode, Code: "ResponseReadFailed",
			Retryable: true, Err: err,
		}
	}
	// Recorded before the status is judged: a 429's headers are the most
	// informative ones the registry ever sends.
	c.recordLimits(resp.Header)

	if len(raw) > maxResponseBytes {
		return nil, &academic_search.APIError{
			Source: Source, HTTPStatus: resp.StatusCode, Code: "ResponseTooLarge",
		}
	}

	if resp.StatusCode != http.StatusOK {
		// The body is not wrapped: a Crossref error body is plain text quoting
		// the request, and the request contains the saved query.
		return nil, &academic_search.APIError{
			Source:     Source,
			HTTPStatus: resp.StatusCode,
			Code:       http.StatusText(resp.StatusCode),
			Retryable:  academic_search.HTTPStatusRetryable(resp.StatusCode),
			RetryAfter: academic_search.ParseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	if err := c.checkPool(resp.Header.Get("x-api-pool")); err != nil {
		return nil, err
	}

	var envelope workList
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// The decoder's message quotes the offending token, which is response text.
		return nil, &academic_search.APIError{
			Source: Source, HTTPStatus: resp.StatusCode, Code: "InvalidResponse",
		}
	}
	if envelope.Status != "ok" {
		return nil, &academic_search.APIError{
			Source: Source, HTTPStatus: resp.StatusCode, Code: "EnvelopeNotOK",
		}
	}
	if envelope.Message == nil {
		// Neither an error nor a result: refuse rather than report zero hits,
		// which a caller would read as "nothing published on this topic".
		return nil, &academic_search.APIError{
			Source: Source, HTTPStatus: resp.StatusCode, Code: "MissingMessage",
		}
	}

	return project(envelope.Message), nil
}

// checkPool refuses a response served from a lower pool than the one configured.
//
// A missing header is not evidence of anything, so it passes: refusing on
// silence would break this client the day Crossref stops sending it. A header
// naming a lower pool is different — it is the registry saying it did not
// recognize the identity or credential we sent, which no retry will change.
func (c *Client) checkPool(reported string) error {
	reported = strings.TrimSpace(strings.ToLower(reported))
	if c.expectedPool == "" || reported == "" {
		return nil
	}
	got, known := poolRank[reported]
	want := poolRank[c.expectedPool]
	if !known || got >= want {
		return nil
	}
	return &academic_search.APIError{
		Source: Source,
		Code:   "PoolDowngraded",
		Err: fmt.Errorf("configured for the %q pool but the registry answered from %q",
			c.expectedPool, reported),
	}
}

// recordLimits stores the limits the registry declared. Values it does not send,
// or sends unparseably, leave the previous reading in place rather than zeroing
// it: a missing header is not a limit of zero.
func (c *Client) recordLimits(header http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if value, err := strconv.Atoi(strings.TrimSpace(header.Get("x-rate-limit-limit"))); err == nil {
		c.limits.Limit = value
	}
	if value, err := time.ParseDuration(strings.TrimSpace(header.Get("x-rate-limit-interval"))); err == nil {
		c.limits.Interval = value
	}
	if value, err := strconv.Atoi(strings.TrimSpace(header.Get("x-concurrency-limit"))); err == nil {
		c.limits.Concurrency = value
	}
}

// project turns the work list into identities.
func project(message *workListMessage) *academic_search.Response {
	response := &academic_search.Response{
		Works: make([]academic_search.Work, 0, len(message.Items)),
		Total: message.TotalResults,
	}
	for _, record := range message.Items {
		work := academic_search.Work{
			DOI:   academic_search.NormalizeDOI(record.DOI),
			Title: firstNonEmpty(record.Title),
			Year:  record.Issued.year(),
			Venue: firstNonEmpty(record.ContainerTitle),
			// The registry's own term, unmapped: what a candidate can honestly
			// report is what Crossref said.
			Type: strings.TrimSpace(record.Type),
		}
		for _, contributor := range record.Author {
			if name := contributor.displayName(); name != "" {
				work.Authors = append(work.Authors, name)
			}
		}
		// A record with no usable DOI cannot be matched against the file a
		// researcher later promotes, which is the only thing it exists for. A
		// mangled DOI is refused by NormalizeDOI and lands here too.
		if _, ok := work.Identity(); !ok {
			response.Dropped++
			continue
		}
		response.Works = append(response.Works, work)
	}
	return response
}

func firstNonEmpty(values []string) string {
	for _, value := range values {
		if collapsed := collapseSpace(value); collapsed != "" {
			return collapsed
		}
	}
	return ""
}

// collapseSpace folds the line breaks and indentation publishers deposit in
// titles, so a reading list entry is one line and a title comparison is stable.
func collapseSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// workList and the types below are the wire format, and what they omit is the
// point. There is no Abstract field, so the abstract is discarded by the decoder
// even if a server ignores `select`. There is likewise no field for
// message.query, where Crossref echoes the search terms back — the saved query
// does not survive decoding either.
type workList struct {
	Status  string           `json:"status"`
	Message *workListMessage `json:"message"`
}

type workListMessage struct {
	TotalResults int    `json:"total-results"`
	Items        []work `json:"items"`
}

type work struct {
	DOI            string        `json:"DOI"`
	Title          []string      `json:"title"`
	Issued         issuedDate    `json:"issued"`
	Author         []contributor `json:"author"`
	ContainerTitle []string      `json:"container-title"`
	Type           string        `json:"type"`
}

// issuedDate is Crossref's date-parts form: [[2024, 3, 15]], where trailing
// parts may be absent and a part may be null.
type issuedDate struct {
	DateParts [][]*int `json:"date-parts"`
}

func (d issuedDate) year() int {
	if len(d.DateParts) == 0 || len(d.DateParts[0]) == 0 {
		return 0
	}
	if year := d.DateParts[0][0]; year != nil {
		return *year
	}
	return 0
}

type contributor struct {
	Given  string `json:"given"`
	Family string `json:"family"`
	// Name carries organizational authors, which have no given/family split.
	Name string `json:"name"`
}

func (c contributor) displayName() string {
	if name := collapseSpace(c.Name); name != "" {
		return name
	}
	parts := make([]string, 0, 2)
	if given := collapseSpace(c.Given); given != "" {
		parts = append(parts, given)
	}
	if family := collapseSpace(c.Family); family != "" {
		parts = append(parts, family)
	}
	return strings.Join(parts, " ")
}
