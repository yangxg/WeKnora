package academic_search

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// APIError reports a failed call to an academic registry.
//
// Like the Doubao client's error type it carries no vendor message. The reason
// is the same and it is not hypothetical: three of the four registries echo the
// offending request in their error bodies — a malformed Crossref filter comes
// back quoting the filter, an E-utilities term error quotes the term — and the
// saved query must never reach a log line (ADR-0009 §5, §9). What is left is
// enough to act on: which registry, the transport status, a symbolic code, and
// the request id to quote in a support ticket.
type APIError struct {
	// Source names the registry: openalex, crossref, pubmed or arxiv.
	Source string
	// HTTPStatus is the transport status, or 0 when the call never completed.
	HTTPStatus int
	// Code is a symbolic identifier for the failure, never free-form text
	// copied from a response body.
	Code string
	// RequestID is the registry's own correlation id when it sends one.
	RequestID string
	// RetryAfter is the delay the registry asked for, when it asked.
	RetryAfter time.Duration
	// Retryable reports whether another attempt could plausibly succeed.
	Retryable bool
	// Err is the underlying transport error, if any. Response bodies are never
	// wrapped here.
	Err error
}

func (e *APIError) Error() string {
	parts := make([]string, 0, 5)
	if e.Source != "" {
		parts = append(parts, e.Source+" search failed")
	} else {
		parts = append(parts, "academic search failed")
	}
	if e.HTTPStatus != 0 {
		parts = append(parts, fmt.Sprintf("http=%d", e.HTTPStatus))
	}
	if e.Code != "" {
		parts = append(parts, "code="+e.Code)
	}
	if e.RequestID != "" {
		parts = append(parts, "request_id="+e.RequestID)
	}
	message := strings.Join(parts, " ")
	if e.Err != nil {
		message += ": " + e.Err.Error()
	}
	return message
}

func (e *APIError) Unwrap() error { return e.Err }

// httpStatusRetryable reports whether a transport status is worth another try.
//
// 429 and 5xx are transient. 403 is deliberately *not*: on Crossref a 403 is a
// manual block by a human who decided this caller was misbehaving, and retrying
// into it is the behaviour that earned the block.
func httpStatusRetryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}
