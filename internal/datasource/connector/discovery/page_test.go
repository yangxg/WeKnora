package discovery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain whitelists loopback for SSRF so the httptest servers (127.0.0.1) are
// reachable, mirroring the RSS connector's tests. Only 127.0.0.1 and ::1 are
// whitelisted, which leaves every other private range — and 127.0.0.2 — usable
// as a genuine counter-example below.
func TestMain(m *testing.M) {
	_ = os.Setenv("SSRF_WHITELIST", "127.0.0.1,::1")
	os.Exit(m.Run())
}

const samplePage = `<!doctype html><html><head><title>DRG 付费改革试点城市名单</title></head>
<body><article><h1>试点城市名单</h1>
<p>国家医保局公布了第一批 DRG 付费改革试点城市，共三十个。</p>
<p>各试点城市应于年底前完成本地分组方案的备案工作，并报送实施情况。</p>
<p>省级医保部门负责组织评估，评估结果纳入年度考核。</p>
</article></body></html>`

func newTestFetcher(t *testing.T) *pageClient {
	t.Helper()
	return newPageClient()
}

func TestFetchPageRendersTheOriginalPageAsMarkdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(samplePage))
	}))
	defer server.Close()

	page, err := newTestFetcher(t).Fetch(context.Background(), server.URL+"/policy/drg")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !strings.Contains(page.Markdown, "第一批 DRG 付费改革试点城市") {
		t.Errorf("markdown missing the article body:\n%s", page.Markdown)
	}
	if strings.Contains(page.Markdown, "<p>") || strings.Contains(page.Markdown, "<article>") {
		t.Errorf("markdown still carries HTML tags:\n%s", page.Markdown)
	}
	if page.Title == "" {
		t.Error("Title is empty; the page has a title element")
	}
}

// TestFetchPageRefusesEveryURLThatIsNotAFetchableOriginalPage walks the URL gate.
// Verified by inspection of the returned errors: the scheme cases are refused by
// this package, and every literal address is refused by ValidateURLForSSRF before
// a socket is opened — upstream forbids literal IPs outright, which subsumes the
// private, loopback and link-local ranges. A hostname that *resolves* into those
// ranges is caught one layer down by the transport's dialer, and a public URL
// that redirects into them by the redirect check exercised in the next test.
func TestFetchPageRefusesEveryURLThatIsNotAFetchableOriginalPage(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "empty", url: ""},
		{name: "blank", url: "   "},
		{name: "no scheme", url: "www.gov.cn/policy"},
		{name: "no host", url: "https://"},
		{name: "file scheme", url: "file:///etc/passwd"},
		{name: "ftp scheme", url: "ftp://files.example.com/doc.pdf"},
		{name: "gopher scheme", url: "gopher://example.com/1"},
		{name: "javascript scheme", url: "javascript:alert(1)"},
		{name: "data scheme", url: "data:text/html,<h1>x</h1>"},
		{name: "non-whitelisted loopback", url: "http://127.0.0.2/admin"},
		{name: "private 10/8", url: "http://10.0.0.5/internal"},
		{name: "private 172.16/12", url: "http://172.16.0.5/internal"},
		{name: "private 192.168/16", url: "http://192.168.1.1/router"},
		{name: "link-local metadata", url: "http://169.254.169.254/latest/meta-data/"},
		{name: "ipv6 loopback literal", url: "http://[::2]/x"},
	}

	fetcher := newTestFetcher(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := fetcher.Fetch(context.Background(), tt.url); err == nil {
				t.Fatalf("Fetch(%q) was allowed", tt.url)
			}
		})
	}
}

// TestFetchPageRefusesARedirectIntoAPrivateAddress covers the case URL
// validation alone cannot: a public URL that answers with a redirect to an
// internal service. The vendor supplies the URL, so the redirect chain is
// attacker-influenced and has to be checked at every hop.
func TestFetchPageRefusesARedirectIntoAPrivateAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer server.Close()

	if _, err := newTestFetcher(t).Fetch(context.Background(), server.URL+"/redirect"); err == nil {
		t.Fatal("a redirect into the link-local range was followed")
	}
}

func TestFetchPageRefusesNonTextResponses(t *testing.T) {
	for _, contentType := range []string{
		"application/pdf",
		"image/png",
		"application/octet-stream",
		"application/zip",
	} {
		t.Run(contentType, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				_, _ = w.Write([]byte("binary-ish payload"))
			}))
			defer server.Close()

			_, err := newTestFetcher(t).Fetch(context.Background(), server.URL+"/file")
			if err == nil {
				t.Fatalf("Fetch() accepted %s", contentType)
			}
			if !strings.Contains(err.Error(), "content type") {
				t.Errorf("error = %v, want it to name the content type", err)
			}
		})
	}
}

// TestFetchPageRefusesAnOversizedPageRatherThanTruncatingIt is the reason the
// size cap is a refusal and not a LimitReader: a truncated body would still be
// ingested, and its content hash would then describe something that is not the
// page. Losing a candidate is recoverable; a candidate whose body silently
// differs from its URL is not.
func TestFetchPageRefusesAnOversizedPageRatherThanTruncatingIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		body := "<html><body><p>" + strings.Repeat("x", maxPageBytes+1024) + "</p></body></html>"
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	_, err := newTestFetcher(t).Fetch(context.Background(), server.URL+"/huge")
	if err == nil {
		t.Fatal("Fetch() accepted an oversized page")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want it to say the page exceeds the cap", err)
	}
}

func TestFetchPageRefusesNonSuccessStatuses(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("<html><body>error page</body></html>"))
			}))
			defer server.Close()

			if _, err := newTestFetcher(t).Fetch(context.Background(), server.URL+"/x"); err == nil {
				t.Fatalf("Fetch() accepted HTTP %d", status)
			}
		})
	}
}

func TestFetchPageRefusesAPageWithNoReadableBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><title>only chrome</title></head><body></body></html>"))
	}))
	defer server.Close()

	if _, err := newTestFetcher(t).Fetch(context.Background(), server.URL+"/empty"); err == nil {
		t.Fatal("Fetch() returned an empty candidate as a success")
	}
}

func TestFetchPageHonorsAContextDeadline(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(samplePage))
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := newTestFetcher(t).Fetch(ctx, server.URL+"/slow"); err == nil {
		t.Fatal("Fetch() ignored the deadline")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Fetch() took %v; the caller's deadline should bound it", elapsed)
	}
}

// TestFetchPageDoesNotSendTheVendorCredentialAnywhere pins that the landing page
// fetch is unauthenticated. The page belongs to a third party the vendor named;
// attaching our own credential to it would hand a stranger the key.
func TestFetchPageDoesNotSendTheVendorCredentialAnywhere(t *testing.T) {
	var gotAuth, gotCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(samplePage))
	}))
	defer server.Close()

	if _, err := newTestFetcher(t).Fetch(context.Background(), server.URL+"/policy"); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if gotAuth != "" || gotCookie != "" {
		t.Errorf("landing page fetch carried credentials: auth=%q cookie=%q", gotAuth, gotCookie)
	}
}
