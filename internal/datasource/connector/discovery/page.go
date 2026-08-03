package discovery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	htmltomd "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/utils"
)

const (
	// pageTimeout bounds one landing page fetch.
	pageTimeout = 20 * time.Second

	// maxPageBytes caps a landing page body. Exceeding it is an error rather
	// than a truncation: a truncated body would still be ingested, and its
	// content hash would then describe something that is not the page.
	maxPageBytes = 5 * 1024 * 1024

	// pageUserAgent identifies the fetch. Discovery reads pages a search vendor
	// named, so it announces itself rather than impersonating a browser.
	pageUserAgent = "Mozilla/5.0 (compatible; WeKnora-Discovery/1.0; +https://weknora.weixin.qq.com)"
)

// textualContentTypes are the media types a candidate body may arrive as. A PDF
// or an image is a real document but not one this connector can render into
// Markdown, so it is refused rather than ingested as bytes with a misleading
// text/markdown content type.
var textualContentTypes = map[string]struct{}{
	"text/html":             {},
	"application/xhtml+xml": {},
	"text/plain":            {},
	"text/markdown":         {},
	"":                      {}, // absent header: sniffing is left to readability
}

// page is one successfully fetched landing page.
type page struct {
	// Markdown is the readability-extracted body. It is the only permitted
	// source of FetchedItem.Content.
	Markdown string
	// Title is the page's own title, which may be better than the vendor's.
	Title string
}

// pageClient fetches landing pages through the SSRF-safe transport.
//
// It is a distinct type from the vendor client on purpose: the two talk to
// different parties under different rules. The vendor call is authenticated and
// goes to one fixed endpoint; a landing page is an arbitrary third-party URL a
// vendor chose, so it is fetched unauthenticated, size-bounded, and re-validated
// at every redirect hop.
type pageClient struct {
	httpClient *http.Client
}

func newPageClient() *pageClient {
	return &pageClient{httpClient: datasource.NewConnectorHTTPClient(pageTimeout)}
}

// Fetch retrieves rawURL and returns its Markdown rendering.
//
// Errors name the URL, never the query that produced it.
func (c *pageClient) Fetch(ctx context.Context, rawURL string) (page, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return page{}, fmt.Errorf("landing page URL is empty")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return page{}, fmt.Errorf("invalid landing page URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return page{}, fmt.Errorf("landing page URL %q is not http(s)", rawURL)
	}
	if parsed.Host == "" {
		return page{}, fmt.Errorf("landing page URL %q has no host", rawURL)
	}
	// ValidateURLForSSRF covers the private, loopback and link-local ranges plus
	// restricted hostnames; the client's dialer re-checks the resolved IPs, and
	// its CheckRedirect re-checks every hop.
	if err := utils.ValidateURLForSSRF(rawURL); err != nil {
		return page{}, fmt.Errorf("landing page URL rejected: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, pageTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return page{}, err
	}
	// No Authorization, no Cookie: the vendor key belongs to the search API, and
	// the landing page belongs to a stranger.
	req.Header.Set("User-Agent", pageUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return page{}, fmt.Errorf("landing page fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return page{}, fmt.Errorf("landing page returned HTTP %d", resp.StatusCode)
	}
	if err := checkTextualContentType(resp.Header.Get("Content-Type")); err != nil {
		return page{}, err
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	if err != nil {
		return page{}, fmt.Errorf("reading landing page failed: %w", err)
	}
	if len(body) > maxPageBytes {
		return page{}, fmt.Errorf("landing page exceeds %d bytes", maxPageBytes)
	}

	return renderPage(body, rawURL)
}

func checkTextualContentType(header string) error {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return fmt.Errorf("landing page content type %q is unparseable", header)
	}
	if _, ok := textualContentTypes[strings.ToLower(mediaType)]; !ok {
		return fmt.Errorf("landing page content type %q is not textual", mediaType)
	}
	return nil
}

// renderPage extracts the main content and converts it to Markdown.
//
// An empty result is an error. There is deliberately no fallback to the vendor's
// snippet or summary: a candidate whose body did not come from the page itself
// would be indistinguishable downstream from one that did.
func renderPage(body []byte, rawURL string) (page, error) {
	pageURL, _ := url.Parse(rawURL)
	article, err := readability.FromReader(bytes.NewReader(body), pageURL)
	if err != nil {
		return page{}, fmt.Errorf("landing page could not be parsed: %w", err)
	}
	if article.Node == nil {
		return page{}, fmt.Errorf("landing page has no readable content")
	}

	var buf bytes.Buffer
	if err := article.RenderHTML(&buf); err != nil {
		return page{}, fmt.Errorf("rendering landing page failed: %w", err)
	}

	markdown, err := htmltomd.ConvertString(buf.String())
	if err != nil {
		return page{}, fmt.Errorf("converting landing page to markdown failed: %w", err)
	}
	markdown = strings.TrimSpace(markdown)
	if markdown == "" {
		return page{}, fmt.Errorf("landing page rendered to an empty body")
	}
	return page{Markdown: markdown, Title: strings.TrimSpace(article.Title())}, nil
}
