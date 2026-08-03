package volcengine

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// APIError reports a failed Doubao search call.
//
// It deliberately carries no vendor Message. `10400 ParamError` echoes the
// offending input, so propagating the message would put the user's query into
// every log line that prints the error — which ADR-0009 §5 and §9 forbid. What
// remains is enough to act on: the HTTP status, the numeric and symbolic vendor
// codes, and the request id to quote in a support ticket.
type APIError struct {
	// HTTPStatus is the transport status, or 0 when the call never completed.
	HTTPStatus int
	// CodeN and Code are the vendor's numeric and symbolic error identifiers.
	CodeN int
	Code  string
	// RequestID comes from ResponseMetadata.RequestId.
	RequestID string
	// RetryAfter is the vendor's requested delay, when it sent one.
	RetryAfter time.Duration
	// Retryable reports whether another attempt could plausibly succeed.
	Retryable bool
	// Err is the underlying transport error, if any. Response bodies are never
	// wrapped here.
	Err error
}

func (e *APIError) Error() string {
	parts := []string{"volcengine search failed"}
	if e.HTTPStatus != 0 {
		parts = append(parts, fmt.Sprintf("http=%d", e.HTTPStatus))
	}
	if e.CodeN != 0 {
		parts = append(parts, fmt.Sprintf("codeN=%d", e.CodeN))
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

// retryableVendorCodes are the business codes worth another attempt. Everything
// else — including codes this build has never seen — is terminal: the free tier
// is 500 calls a month, so retrying an unclassified failure spends quota to
// learn nothing. The documented terminal codes are 10400 ParamError,
// 10401 InvalidTopToken, 10402 InvalidSearchType, 10403 InvalidAccountId,
// 10406 FreeQuotaExhausted, 10409 SearchPackageModeUnsupported,
// 10410 SearchPackageUnavailable and 10412 SearchPackageQuotaExhausted.
var retryableVendorCodes = map[int]struct{}{
	10500:  {}, // InnerError
	700429: {}, // FreeRateLimitExceeded
}

func vendorCodeRetryable(codeN int) bool {
	_, ok := retryableVendorCodes[codeN]
	return ok
}

// httpStatusRetryable reports whether a transport status is worth retrying.
// 429 and 5xx are transient; every other non-2xx is a configuration or
// credential problem that repeating cannot fix.
func httpStatusRetryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}
