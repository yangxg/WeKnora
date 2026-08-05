// Package pubmed queries the NCBI E-utilities for PubMed record identities.
//
// The endpoint choice is the boundary. E-utilities offers three ways to read a
// record and they differ exactly on the axis ADR-0012 §2 cares about: ESearch
// returns UIDs, ESummary returns document summaries with no abstract, and EFetch
// is the one that "can produce abstracts from Entrez PubMed". This client is
// ESearch followed by ESummary, and it has no code path that reaches EFetch — so
// the abstract is not filtered out downstream, it is never requested.
//
// That also decides the shape: two requests per search rather than one, both
// paced, both carrying the same registered identity. Half our traffic arriving
// anonymous would be worse than none, because NCBI's stated remedy for a caller
// it cannot contact is to block the address.
//
// Three deployment facts are code-relevant (all verified against NBK25497 and
// NBK25499 on 2026-08-03):
//
//   - The rate is 3 requests per second without a key and 10 with one, counted
//     per IP. An API key is required here rather than optional: ADR-0012 §8 lists
//     a key-less PubMed manifest among the counterexamples that must be refused
//     before materialization, because the failure shows up as slowness and then
//     as a blocked address, never as an error a project would notice.
//   - `tool` and `email` must be registered with NCBI **offline**; supplying them
//     in requests is not registration. That is a deployment precondition, not
//     something this code can establish (ADR-0012 §9 item 5).
//   - Large jobs belong on weekends or 21:00–05:00 Eastern. That is a scheduling
//     obligation on whatever drives this client, not a rate this client can hold.
//
// Two vocabulary gaps are deliberately left fail-closed; see publicationTypes.
package pubmed

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
	Source = "pubmed"

	// BaseURL is the documented E-utilities host. NCBI asks that all requests go
	// to this path; it is hardcoded so a stored parameter cannot redirect them.
	BaseURL = "https://eutils.ncbi.nlm.nih.gov/entrez/eutils/"

	// MinRequestInterval sits under the keyed ceiling of 10 requests per second.
	// Under, not on: the ceiling is counted per IP address, across everything
	// sharing it.
	MinRequestInterval = 110 * time.Millisecond

	// database is the only Entrez database this client speaks. The others return
	// different summary shapes.
	database = "pubmed"

	// openWindowStart and openWindowEnd fill in the unbounded side of a date
	// filter. mindate and maxdate are documented as a pair, so a lone endpoint is
	// not a half-open window — it is an ignored filter, which silently widens the
	// search.
	openWindowStart = "1900/01/01"
	openWindowEnd   = "9999/12/31"

	maxResponseBytes = 4 << 20
)

// publicationTypes maps the kernel vocabulary onto PubMed [pt] tags.
//
// It holds four entries, and the two omissions are deliberate. [pt] searches a
// MeSH-controlled vocabulary — NLM's "Publication Characteristics (Publication
// Types) with Scope Notes" — which was unreachable during implementation, so
// ADR-0012 §9 left the map at three and made widening a verification task: cite
// the term, then add it. The list was read on 2026-08-05 (reviewed 2026-07-01)
// and settles all three remaining kernel types:
//
//   - Dataset is in the list ("Works consisting of organized collections of
//     data..."), so it is cited and added;
//   - book-chapter has no counterpart at all (there is Monograph and Book
//     Review, but no "Book Chapter");
//   - conference-paper's nearest terms name the wrong object: Conference
//     Proceedings is the published volume, Meeting Abstract is a single
//     abstract. Neither is a conference paper. (The old Congress term is gone.)
//
// So the last two are refused on a citation now, not on an unreachable page.
// Guessing would cost a project a filter that matches nothing and reads as "no
// conference papers on this topic" — a wrong answer that looks like a right one.
// What the list cannot settle is how many records carry a given type; only an
// authorized request could, and a small true count is still true.
var publicationTypes = map[string]string{
	academic_search.WorkTypeJournalArticle: "Journal Article",
	academic_search.WorkTypeReview:         "Review",
	academic_search.WorkTypePreprint:       "Preprint",
	academic_search.WorkTypeDataset:        "Dataset",
}

// toolPattern enforces NCBI's rule that tool be "a string with no internal
// spaces". emailPattern is loose on purpose: it catches a manifest that wrote a
// name where an address belongs, and NCBI needs the address to warn a developer
// before blocking an address.
var (
	toolPattern  = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s.]+\.[^@\s]+$`)
	yearPattern  = regexp.MustCompile(`\b(1[5-9]\d{2}|20\d{2}|21\d{2})\b`)
)

// Config builds a Client.
type Config struct {
	// HTTPClient is required. The caller owns the outbound network policy.
	HTTPClient *http.Client
	// APIKey is required; see the package doc for why it is not optional.
	APIKey string
	// ToolName and Email are the identity registered with NCBI. Neither is a
	// secret — they belong in the version-controlled manifest, where a reviewer
	// can see them (ADR-0012 §5 gap 2).
	ToolName string
	Email    string
	// MaxAttempts bounds retries of transient failures; 0 means the shared default.
	MaxAttempts int
}

// Client queries the NCBI E-utilities.
type Client struct {
	httpClient  *http.Client
	baseURL     string
	apiKey      string
	toolName    string
	email       string
	pacer       *academic_search.Pacer
	maxAttempts int
	sleep       func(context.Context, time.Duration) error
}

// NewClient validates the configuration and returns a ready client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.HTTPClient == nil {
		return nil, fmt.Errorf("pubmed: an HTTP client is required")
	}
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf(
			"pubmed: an API key is required; a key-less caller runs at 3 requests per second and, if it " +
				"breaches policy while unregistered, has its address blocked")
	}
	tool := strings.TrimSpace(cfg.ToolName)
	if tool == "" {
		return nil, fmt.Errorf("pubmed: a registered tool name is required")
	}
	if !toolPattern.MatchString(tool) {
		return nil, fmt.Errorf("pubmed: the tool name must be a string with no internal spaces")
	}
	email := strings.TrimSpace(cfg.Email)
	if email == "" {
		return nil, fmt.Errorf(
			"pubmed: a registered developer address is required; NCBI uses it to warn before blocking")
	}
	if !emailPattern.MatchString(email) {
		return nil, fmt.Errorf("pubmed: the registered contact is not an email address")
	}
	attempts := cfg.MaxAttempts
	if attempts <= 0 {
		attempts = academic_search.DefaultAttempts
	}
	return &Client{
		httpClient:  cfg.HTTPClient,
		baseURL:     BaseURL,
		apiKey:      apiKey,
		toolName:    tool,
		email:       email,
		pacer:       academic_search.NewPacer(MinRequestInterval),
		maxAttempts: attempts,
		sleep:       academic_search.SleepContext,
	}, nil
}

// Search runs one query and returns one page of record identities.
//
// It is two requests: ESearch for the UIDs, then one ESummary for all of them.
// One summary call rather than one per record — NCBI's own guidance is to batch,
// and 50 ids is far below the ~200 at which a POST becomes necessary.
func (c *Client) Search(
	ctx context.Context, query string, opts academic_search.Options,
) (*academic_search.Response, error) {
	opts, err := opts.Normalized()
	if err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("pubmed: query is empty")
	}
	// Before the request: a refusable profile that reaches the wire spends rate
	// budget to be told what a map already knew.
	term, err := buildTerm(query, opts)
	if err != nil {
		return nil, err
	}

	ids, total, err := c.esearch(ctx, term, opts)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		// Nothing matched is an answer. Asking ESummary about an empty id list
		// would spend a request to be told the same thing.
		logger.Infof(ctx, "[AcademicSearch][PubMed] returned 0 works of %d matched", total)
		return &academic_search.Response{Total: total}, nil
	}

	response, err := c.esummary(ctx, ids)
	if err != nil {
		return nil, err
	}
	response.Total = total
	logger.Infof(ctx, "[AcademicSearch][PubMed] returned %d works, dropped %d, of %d matched",
		len(response.Works), response.Dropped, total)
	return response, nil
}

// buildTerm renders the Entrez query, appending the publication-type clause.
func buildTerm(query string, opts academic_search.Options) (string, error) {
	if opts.OpenAccess == academic_search.OpenAccessOnly {
		// PubMed's free-full-text filters select availability, not licence. A
		// reading list of merely-readable works presented as open access would
		// mislead the person deciding how to acquire each one.
		return "", fmt.Errorf(
			"pubmed: an open-access-only filter cannot be expressed here; PubMed's filters select " +
				"free availability, not licence")
	}
	if len(opts.WorkTypes) == 0 {
		return query, nil
	}
	clauses := make([]string, 0, len(opts.WorkTypes))
	for _, workType := range opts.WorkTypes {
		term, ok := publicationTypes[workType]
		if !ok {
			return "", fmt.Errorf(
				"pubmed: work type %q is not among the publication types verified for [pt]; filtering on "+
					"an unused type matches nothing and reads as an empty topic", workType)
		}
		clauses = append(clauses, fmt.Sprintf("%q[pt]", term))
	}
	// OR inside the parentheses: a record rarely carries two of these types, so
	// AND would return nothing.
	return fmt.Sprintf("%s AND (%s)", query, strings.Join(clauses, " OR ")), nil
}

// identityParams are the three values every request carries. api_key is a secret
// and E-utilities accepts it only as a query parameter, which is why no log line
// in this package may contain a request URL.
func (c *Client) identityParams() url.Values {
	params := url.Values{}
	params.Set("api_key", c.apiKey)
	params.Set("tool", c.toolName)
	params.Set("email", c.email)
	return params
}

// esearch returns the matching UIDs and the registry's total count.
func (c *Client) esearch(
	ctx context.Context, term string, opts academic_search.Options,
) ([]string, int, error) {
	params := c.identityParams()
	params.Set("db", database)
	params.Set("term", term)
	params.Set("retmode", "json")
	params.Set("retmax", strconv.Itoa(opts.Count))
	params.Set("retstart", "0")
	// Publication date descending: discovery wants what is new, and relevance
	// order — the default — would bury it.
	params.Set("sort", "pub_date")
	if !opts.From.IsZero() || !opts.To.IsZero() {
		params.Set("datetype", "pdat")
		params.Set("mindate", formatDate(opts.From, openWindowStart))
		params.Set("maxdate", formatDate(opts.To, openWindowEnd))
	}

	raw, err := c.fetch(ctx, "esearch.fcgi", params)
	if err != nil {
		return nil, 0, err
	}

	var envelope esearchEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, 0, &academic_search.APIError{Source: Source, Code: "InvalidResponse"}
	}
	if err := envelope.rateLimitError(); err != nil {
		return nil, 0, err
	}
	if envelope.Result == nil {
		return nil, 0, &academic_search.APIError{Source: Source, Code: "MissingResult"}
	}
	if strings.TrimSpace(envelope.Result.Error) != "" {
		// The message is dropped, not wrapped: an Entrez term error quotes the
		// term, and the term is the saved query.
		return nil, 0, &academic_search.APIError{Source: Source, Code: "InvalidTerm"}
	}

	// count arrives as a string. An unparseable one costs the response its total
	// and nothing else.
	total, _ := strconv.Atoi(strings.TrimSpace(envelope.Result.Count))
	return envelope.Result.IDList, total, nil
}

// esummary projects the document summaries for the given UIDs.
func (c *Client) esummary(ctx context.Context, ids []string) (*academic_search.Response, error) {
	params := c.identityParams()
	params.Set("db", database)
	params.Set("id", strings.Join(ids, ","))
	params.Set("retmode", "json")

	raw, err := c.fetch(ctx, "esummary.fcgi", params)
	if err != nil {
		return nil, err
	}

	var envelope esummaryEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &academic_search.APIError{Source: Source, Code: "InvalidResponse"}
	}
	if strings.TrimSpace(envelope.Error) != "" {
		return nil, rateLimitOrGeneric(envelope.Error)
	}
	if envelope.Result == nil {
		return nil, &academic_search.APIError{Source: Source, Code: "MissingResult"}
	}

	// The result object mixes a "uids" array with one key per uid, so the order is
	// read from the array rather than from map iteration — a reading list whose
	// order changed between runs would look like new results every time.
	order := ids
	if rawUIDs, ok := envelope.Result["uids"]; ok {
		var uids []string
		if err := json.Unmarshal(rawUIDs, &uids); err == nil && len(uids) > 0 {
			order = uids
		}
	}

	response := &academic_search.Response{Works: make([]academic_search.Work, 0, len(order))}
	for _, uid := range order {
		rawRecord, ok := envelope.Result[uid]
		if !ok {
			response.Dropped++
			continue
		}
		var record summaryRecord
		if err := json.Unmarshal(rawRecord, &record); err != nil {
			response.Dropped++
			continue
		}
		// NCBI reports a per-record failure inside an otherwise fine response.
		if strings.TrimSpace(record.Error) != "" {
			response.Dropped++
			continue
		}
		work := record.project(uid)
		if _, ok := work.Identity(); !ok {
			response.Dropped++
			continue
		}
		response.Works = append(response.Works, work)
	}
	return response, nil
}

// fetch performs one paced, bounded, retried request and returns the body.
func (c *Client) fetch(ctx context.Context, script string, params url.Values) ([]byte, error) {
	requestURL := c.baseURL + script + "?" + params.Encode()

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if err := c.pacer.Wait(ctx); err != nil {
			return nil, err
		}
		raw, err := c.attempt(ctx, requestURL)
		if err == nil {
			return raw, nil
		}
		lastErr = err

		apiErr, ok := err.(*academic_search.APIError)
		if !ok || !apiErr.Retryable || attempt == c.maxAttempts {
			logger.Warnf(ctx, "[AcademicSearch][PubMed] %s failed after %d attempt(s): %v",
				script, attempt, err)
			return nil, err
		}
		delay := academic_search.RetryDelay(attempt, apiErr.RetryAfter)
		if sleepErr := c.sleep(ctx, delay); sleepErr != nil {
			logger.Warnf(ctx, "[AcademicSearch][PubMed] backoff interrupted after %d attempt(s): %v",
				attempt, err)
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) attempt(ctx context.Context, requestURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		// Not wrapped: the message would carry the URL, and the URL carries the key.
		return nil, &academic_search.APIError{Source: Source, Code: "RequestBuildFailed"}
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
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
		return nil, &academic_search.APIError{
			Source:     Source,
			HTTPStatus: resp.StatusCode,
			Code:       http.StatusText(resp.StatusCode),
			Retryable:  academic_search.HTTPStatusRetryable(resp.StatusCode),
			RetryAfter: academic_search.ParseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	return raw, nil
}

// formatDate renders an E-utilities date, substituting a far endpoint for a zero
// value so mindate and maxdate always travel as a pair.
func formatDate(value time.Time, fallback string) string {
	if value.IsZero() {
		return fallback
	}
	return value.UTC().Format("2006/01/02")
}

// rateLimitOrGeneric classifies a top-level error string.
//
// The rate-limit case is the one worth naming: NCBI reports it inside an HTTP
// 200, so the status alone never decides success, and unlike a bad request it is
// retryable because backing off is precisely the remedy. Only the classification
// is kept — the message itself is registry text.
func rateLimitOrGeneric(message string) error {
	if strings.Contains(strings.ToLower(message), "rate limit") {
		return &academic_search.APIError{
			Source: Source, Code: "RateLimitExceeded", Retryable: true,
		}
	}
	return &academic_search.APIError{Source: Source, Code: "EnvelopeError"}
}

// innermost unwraps to the deepest cause. Go's *url.Error stringifies with the
// request URL in it, and here the URL carries the API key.
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

func collapseSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// esearchEnvelope is the ESearch wire format.
//
// What it omits is the point: there is no field for esearchresult's
// `querytranslation`, which echoes the saved query back in expanded form. The
// decoder drops it, so it cannot reach a log line or an error message even by
// accident (ADR-0009 §9).
type esearchEnvelope struct {
	// Error carries the top-level rate-limit envelope, which arrives with an
	// HTTP 200 and no esearchresult at all.
	Error  string        `json:"error"`
	Result *esearchInner `json:"esearchresult"`
}

func (e esearchEnvelope) rateLimitError() error {
	if strings.TrimSpace(e.Error) == "" {
		return nil
	}
	return rateLimitOrGeneric(e.Error)
}

type esearchInner struct {
	Count  string   `json:"count"`
	IDList []string `json:"idlist"`
	// Error is Entrez's per-query complaint. Only its presence is used.
	Error string `json:"ERROR"`
}

// esummaryEnvelope holds the mixed result object as raw messages, because its
// keys are UIDs rather than a fixed schema.
type esummaryEnvelope struct {
	Error  string                     `json:"error"`
	Result map[string]json.RawMessage `json:"result"`
}

// summaryRecord is one document summary. There is no abstract field because
// ESummary sends no abstract; the struct records that fact rather than relying on
// it.
type summaryRecord struct {
	UID   string `json:"uid"`
	Error string `json:"error"`
	// PubDate is a display string: "2024 Mar 15", "2024 Mar" or "2024".
	PubDate         string          `json:"pubdate"`
	Source          string          `json:"source"`
	FullJournalName string          `json:"fulljournalname"`
	Title           string          `json:"title"`
	Authors         []summaryAuthor `json:"authors"`
	ArticleIDs      []articleID     `json:"articleids"`
	PubType         []string        `json:"pubtype"`
}

type summaryAuthor struct {
	// Name is PubMed's own rendering, "Zhang W". It is a registry claim that
	// stops at the candidate layer (ADR-0012 §4).
	Name string `json:"name"`
}

type articleID struct {
	IDType string `json:"idtype"`
	Value  string `json:"value"`
}

// project turns one summary into an identity.
func (r summaryRecord) project(uid string) academic_search.Work {
	work := academic_search.Work{
		PMID:  strings.TrimSpace(firstNonEmpty(r.UID, uid)),
		Title: collapseSpace(r.Title),
		Year:  parseYear(r.PubDate),
		// fulljournalname over source: it is the one a person can act on when
		// deciding where to acquire the text.
		Venue: collapseSpace(firstNonEmpty(r.FullJournalName, r.Source)),
	}
	for _, id := range r.ArticleIDs {
		if strings.EqualFold(strings.TrimSpace(id.IDType), "doi") {
			work.DOI = academic_search.NormalizeDOI(id.Value)
			break
		}
	}
	for _, author := range r.Authors {
		if name := collapseSpace(author.Name); name != "" {
			work.Authors = append(work.Authors, name)
		}
	}
	if len(r.PubType) > 0 {
		// The registry's own first term, unmapped. PubMed often lists several;
		// choosing among them would be an interpretation.
		work.Type = collapseSpace(r.PubType[0])
	}
	return work
}

// parseYear reads the year out of a display date. A date that will not parse
// costs the record its year, never its identity: the year is metadata the
// promoting human confirms.
func parseYear(value string) int {
	match := yearPattern.FindString(value)
	if match == "" {
		return 0
	}
	year, err := strconv.Atoi(match)
	if err != nil {
		return 0
	}
	return year
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
