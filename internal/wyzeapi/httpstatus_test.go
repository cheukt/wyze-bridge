package wyzeapi

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func fakeResponse(status int, body string) *http.Response {
	req := &http.Request{Method: "POST", URL: &url.URL{Scheme: "https", Host: "api.wyzecam.com", Path: "/user/login"}}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
		Header:     http.Header{},
	}
}

// A 429 with an empty body used to surface as "json decode (status 429):
// unexpected end of JSON input", hiding the rate limit from the caller.
func TestValidateResponseEmptyBodyReportsStatus(t *testing.T) {
	c := &Client{log: zerolog.Nop()}

	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"429 empty body", http.StatusTooManyRequests, "", "rate limited"},
		{"502 html body", http.StatusBadGateway, "<html>bad gateway</html>", "cloud is degraded"},
		{"401 empty body", http.StatusUnauthorized, "", "WYZE_API_ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.validateResponse(fakeResponse(tc.status, tc.body))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "json decode") {
				t.Fatalf("status was masked by a decode error: %v", err)
			}
		})
	}
}

// A genuinely malformed 200 body still reports the decode failure.
func TestValidateResponseMalformedBodyStillDecodeError(t *testing.T) {
	c := &Client{log: zerolog.Nop()}
	_, err := c.validateResponse(fakeResponse(http.StatusOK, "{not json"))
	if err == nil || !strings.Contains(err.Error(), "json decode") {
		t.Fatalf("want a decode error, got %v", err)
	}
}
