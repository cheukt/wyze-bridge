package wyzeapi

import (
	"fmt"
	"net/http"
)

// statusError maps an HTTP status onto the error we surface for it, or nil when
// the status is not itself a failure. Wyze reports most errors as HTTP 200 with
// a body code, so this covers only the cases where the cloud (or an edge in
// front of it) answers first — and those bodies are often not JSON at all.
func statusError(statusCode int, body []byte) error {
	switch {
	case statusCode >= 500:
		return fmt.Errorf("Wyze cloud HTTP %d (%s) — cloud is degraded, will retry", statusCode, http.StatusText(statusCode))
	case statusCode == http.StatusTooManyRequests:
		return fmt.Errorf("Wyze cloud HTTP 429 (rate limited) — back off and retry")
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return fmt.Errorf("Wyze cloud HTTP %d (%s) — check WYZE_API_ID / WYZE_API_KEY, account may be locked", statusCode, http.StatusText(statusCode))
	case statusCode >= 400:
		return fmt.Errorf("Wyze cloud HTTP %d (%s): %s", statusCode, http.StatusText(statusCode), string(body))
	}
	return nil
}
