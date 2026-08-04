// Package arxiv queries the arXiv Atom API for e-print identities.
//
// arXiv is the only one of the four registries that both holds the full text and
// forbids us from serving it: its Terms of Use permit programmatic access to the
// metadata API while reserving bulk e-print distribution to its own channels. So
// the shape ADR-0012 §2 imposes on the academic lane is not a compromise here,
// it is the only compliant shape — this client asks for identity, and the entry
// struct it decodes into has no field an abstract could land in.
//
// Two arXiv-specific rules matter more than the request format:
//
//   - One request every three seconds, counted across every machine under our
//     control. That is a term of use, not a performance hint, and the penalty is
//     a blocked IP for everyone behind it. The interval lives in this package as
//     a constant so no project manifest can raise it.
//   - <arxiv:doi> is not this record's identity. It is the author's declaration
//     of where the work was later published, and ADR-0012 §3 holds the preprint
//     and the version of record to be two Documents. Adopting it would give a
//     preprint candidate the published version's canonical key, so a researcher
//     promoting the e-print PDF would silently file it as the journal article.
package arxiv

import (
	"context"
	"encoding/xml"
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
	Source = "arxiv"

	// Endpoint is hardcoded, never manifest-configurable, so a stored parameter
	// can never redirect search traffic (the same rule the Doubao client follows).
	Endpoint = "https://export.arxiv.org/api/query"

	// MinRequestInterval is arXiv's published rate limit: one request every three
	// seconds, from a single connection. See the package doc for why it is a
	// constant here rather than a setting.
	MinRequestInterval = 3 * time.Second

	// maxResponseBytes bounds one Atom page. Fifty entries with abstracts run to
	// a few hundred kilobytes; anything past this is not a result page.
	maxResponseBytes = 2 << 20
)

const (
	// errorEntryPrefix marks arXiv's error envelope, which arrives as a normal
	// HTTP 200 Atom feed holding a single entry. The status alone never decides
	// success.
	errorEntryPrefix = "http://arxiv.org/api/errors"

	// absPrefixes are stripped to recover the bare e-print id.
	absPrefixHTTP  = "http://arxiv.org/abs/"
	absPrefixHTTPS = "https://arxiv.org/abs/"

	// openWindowStart and openWindowEnd stand in for an unbounded side of the
	// date filter. arXiv has no open-ended range syntax, so a half-open window
	// is rendered as a closed one that cannot exclude anything real.
	openWindowStart = "190001010000"
	openWindowEnd   = "999912312359"
)

// versionSuffix matches the revision marker on an e-print id: 2401.00001v3, and
// the legacy math/0309136v1. The suffix names a revision, not a work — keeping
// it would make the same preprint a fresh candidate on every author correction,
// and revision-level difference is settled at promote time by content hash.
var versionSuffix = regexp.MustCompile(`v\d+$`)

// Config builds a Client.
type Config struct {
	// HTTPClient is required. The caller owns the outbound network policy, as in
	// every other client in this tree.
	HTTPClient *http.Client
	// MaxAttempts bounds retries of transient failures; 0 means the shared default.
	MaxAttempts int
}

// Client queries the arXiv Atom API. It holds no credential: arXiv's API is
// unauthenticated, so there is nothing here to leak into a log or a manifest.
type Client struct {
	httpClient  *http.Client
	endpoint    string
	pacer       *academic_search.Pacer
	maxAttempts int
	sleep       func(context.Context, time.Duration) error
}

// NewClient validates the configuration and returns a ready client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.HTTPClient == nil {
		return nil, fmt.Errorf("arxiv: an HTTP client is required")
	}
	attempts := cfg.MaxAttempts
	if attempts <= 0 {
		attempts = academic_search.DefaultAttempts
	}
	return &Client{
		httpClient:  cfg.HTTPClient,
		endpoint:    Endpoint,
		pacer:       academic_search.NewPacer(MinRequestInterval),
		maxAttempts: attempts,
		sleep:       academic_search.SleepContext,
	}, nil
}

// Search runs one query and returns one page of e-print identities.
//
// Options are validated before anything is sent. That is not only defensive: a
// request arXiv rejects comes back quoting the offending parameter, and the
// saved query must never reach a log line (ADR-0009 §9).
func (c *Client) Search(
	ctx context.Context, query string, opts academic_search.Options,
) (*academic_search.Response, error) {
	opts, err := opts.Normalized()
	if err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("arxiv: query is empty")
	}

	// Everything on arXiv is a preprint. A profile that excludes preprints wants
	// nothing from this registry, and answering it without a request is both
	// correct and one fewer call against a three-second budget.
	if !wantsPreprints(opts.WorkTypes) {
		logger.Infof(ctx, "[AcademicSearch][arXiv] skipped: the profile excludes preprints")
		return &academic_search.Response{}, nil
	}

	requestURL := c.buildURL(query, opts)

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if err := c.pacer.Wait(ctx); err != nil {
			return nil, err
		}
		response, err := c.attempt(ctx, requestURL)
		if err == nil {
			logger.Infof(ctx, "[AcademicSearch][arXiv] returned %d works, dropped %d, attempt %d",
				len(response.Works), response.Dropped, attempt)
			return response, nil
		}
		lastErr = err

		apiErr, ok := err.(*academic_search.APIError)
		if !ok || !apiErr.Retryable || attempt == c.maxAttempts {
			logger.Warnf(ctx, "[AcademicSearch][arXiv] search failed after %d attempt(s): %v", attempt, err)
			return nil, err
		}
		delay := academic_search.RetryDelay(attempt, apiErr.RetryAfter)
		if sleepErr := c.sleep(ctx, delay); sleepErr != nil {
			logger.Warnf(ctx, "[AcademicSearch][arXiv] backoff interrupted after %d attempt(s): %v", attempt, err)
			return nil, err
		}
	}
	return nil, lastErr
}

// buildURL renders the documented request. Sorting by submission date descending
// is what makes this a discovery query rather than a bibliography lookup: an
// incremental run wants what appeared since the last cursor, and arXiv's default
// relevance order would bury it.
func (c *Client) buildURL(query string, opts academic_search.Options) string {
	searchQuery := fmt.Sprintf(`all:%q`, sanitizeQuery(query))
	if window := renderWindow(opts.From, opts.To); window != "" {
		searchQuery += " AND " + window
	}
	params := url.Values{}
	params.Set("search_query", searchQuery)
	params.Set("start", "0")
	params.Set("max_results", strconv.Itoa(opts.Count))
	params.Set("sortBy", "submittedDate")
	params.Set("sortOrder", "descending")
	return c.endpoint + "?" + params.Encode()
}

// attempt performs one request/response cycle.
func (c *Client) attempt(ctx context.Context, requestURL string) (*academic_search.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, &academic_search.APIError{Source: Source, Code: "RequestBuildFailed", Err: err}
	}
	req.Header.Set("Accept", "application/atom+xml")

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
	if len(raw) > maxResponseBytes {
		return nil, &academic_search.APIError{
			Source: Source, HTTPStatus: resp.StatusCode, Code: "ResponseTooLarge",
		}
	}

	if resp.StatusCode != http.StatusOK {
		// The body is not wrapped: an arXiv error page echoes the request.
		return nil, &academic_search.APIError{
			Source:     Source,
			HTTPStatus: resp.StatusCode,
			Code:       http.StatusText(resp.StatusCode),
			Retryable:  academic_search.HTTPStatusRetryable(resp.StatusCode),
			RetryAfter: academic_search.ParseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	var feed atomFeed
	if err := xml.Unmarshal(raw, &feed); err != nil {
		// The parse error itself is dropped rather than wrapped: encoding/xml
		// quotes the offending token, and that token is response text.
		return nil, &academic_search.APIError{
			Source: Source, HTTPStatus: resp.StatusCode, Code: "InvalidResponse",
		}
	}

	if isErrorFeed(feed) {
		// A malformed request is terminal. Retrying it would spend the rate
		// budget to fail identically, and the message that would explain it is
		// exactly the text we may not carry.
		return nil, &academic_search.APIError{
			Source: Source, HTTPStatus: resp.StatusCode, Code: "InvalidRequest",
		}
	}

	return project(feed), nil
}

// project turns the Atom feed into identities. It reads only the fields
// atomEntry declares, which is how the abstract is discarded: encoding/xml
// silently drops elements no field claims, so <summary> has nowhere to go.
func project(feed atomFeed) *academic_search.Response {
	response := &academic_search.Response{
		Works: make([]academic_search.Work, 0, len(feed.Entries)),
		Total: feed.Total,
	}
	for _, entry := range feed.Entries {
		work := academic_search.Work{
			ArXivID: eprintID(entry.ID),
			Title:   collapseSpace(entry.Title),
			Year:    publishedYear(entry.Published),
			Venue:   collapseSpace(entry.JournalRef),
			// Every arXiv record is a preprint, so the registry states no type
			// of its own and this is both its claim and the kernel term.
			Type: academic_search.WorkTypePreprint,
		}
		for _, author := range entry.Authors {
			if name := collapseSpace(author.Name); name != "" {
				work.Authors = append(work.Authors, name)
			}
		}
		// Note what is *not* set: work.DOI. entry.JournalDOI names the published
		// version, which is a different Document (ADR-0012 §3 decision 2b).
		if _, ok := work.Identity(); !ok {
			response.Dropped++
			continue
		}
		response.Works = append(response.Works, work)
	}
	return response
}

// eprintID recovers the bare, unversioned e-print id from the entry's Atom id.
func eprintID(raw string) string {
	id := strings.TrimSpace(raw)
	for _, prefix := range []string{absPrefixHTTPS, absPrefixHTTP} {
		if rest, ok := strings.CutPrefix(id, prefix); ok {
			id = rest
			break
		}
	}
	return versionSuffix.ReplaceAllString(id, "")
}

// publishedYear reads the year off the submission timestamp. A record whose date
// will not parse keeps its identity and loses its year: the year is metadata the
// promoting human confirms, the identity is not.
func publishedYear(raw string) int {
	published, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return published.Year()
}

// wantsPreprints reports whether the profile admits preprints. An empty filter
// admits everything.
func wantsPreprints(workTypes []string) bool {
	if len(workTypes) == 0 {
		return true
	}
	for _, workType := range workTypes {
		if workType == academic_search.WorkTypePreprint {
			return true
		}
	}
	return false
}

// renderWindow renders the submittedDate filter, inclusive of both endpoint days.
func renderWindow(from, to time.Time) string {
	if from.IsZero() && to.IsZero() {
		return ""
	}
	lower, upper := openWindowStart, openWindowEnd
	if !from.IsZero() {
		lower = from.UTC().Format("20060102") + "0000"
	}
	if !to.IsZero() {
		upper = to.UTC().Format("20060102") + "2359"
	}
	return fmt.Sprintf("submittedDate:[%s TO %s]", lower, upper)
}

// sanitizeQuery removes the characters that would end the quoted phrase early.
// A query that breaks out of its quotes becomes a different query, and arXiv
// would answer it rather than reject it — a silent wrong answer, which is worse
// than a refused one.
func sanitizeQuery(query string) string {
	return collapseSpace(strings.NewReplacer(`"`, " ", `\`, " ").Replace(query))
}

// collapseSpace folds the line breaks and indentation Atom wraps text in.
func collapseSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// atomFeed and atomEntry are the wire format, and the fields they omit are the
// point. There is no Summary field, so the abstract every entry carries is
// discarded by the decoder rather than by a later filter someone could remove.
type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Total   int         `xml:"totalResults"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID        string       `xml:"id"`
	Title     string       `xml:"title"`
	Published string       `xml:"published"`
	Authors   []atomAuthor `xml:"author"`
	// JournalRef is arXiv's <arxiv:journal_ref>, an author-supplied string. It
	// serves recognition in a reading list and never enters governance.
	JournalRef string `xml:"journal_ref"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

// isErrorFeed detects arXiv's HTTP-200 error envelope.
func isErrorFeed(feed atomFeed) bool {
	if len(feed.Entries) != 1 {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(feed.Entries[0].ID), errorEntryPrefix)
}
