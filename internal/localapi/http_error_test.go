package localapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type errorTransport struct {
	status int
	body   string
}

func (t errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: t.status, Body: io.NopCloser(strings.NewReader(t.body)), Header: make(http.Header)}, nil
}

func TestClientHTTPErrorPreservesStatusWithoutWeakeningBodyBound(t *testing.T) {
	for _, tc := range []struct {
		status        int
		body, message string
	}{{409, `{"error":"projection changed"}`, "HTTP 409: projection changed"}, {413, `not JSON`, "module returned HTTP 413"}} {
		client := &Client{HTTP: &http.Client{Transport: errorTransport{tc.status, tc.body}}}
		err := client.Do(context.Background(), http.MethodGet, "/v1/test", nil, nil)
		var typed *HTTPError
		if !errors.As(err, &typed) || typed.StatusCode != tc.status || err.Error() != tc.message || !IsHTTPStatus(fmt.Errorf("wrapped: %w", err), tc.status) || IsHTTPStatus(err, 200) {
			t.Fatal("status lost or error text changed", err)
		}
	}
	client := &Client{HTTP: &http.Client{Transport: errorTransport{413, strings.Repeat("x", MaxBody+1)}}}
	if err := client.Do(context.Background(), http.MethodGet, "/v1/test", nil, nil); err == nil || IsHTTPStatus(err, 413) || !strings.Contains(err.Error(), "size limit") {
		t.Fatal("status fallback bypassed the response-size bound", err)
	}
}
