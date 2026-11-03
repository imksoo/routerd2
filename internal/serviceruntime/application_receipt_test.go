package serviceruntime

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestProtocolApplicationRequiresAnApplicationReply(t *testing.T) {
	for _, role := range []string{"bgp", "vrrp"} {
		t.Run(role, func(t *testing.T) {
			deadline := time.Now().Add(5 * time.Second)
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			body := `{"accepted":true}`
			c := &effectsClient{identity: serviceconfig.Update{Role: role}, client: &localapi.Client{HTTP: &http.Client{Transport: bfdRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if got, ok := r.Context().Deadline(); !ok || !got.Equal(deadline) {
					t.Fatal("transport replaced the protocol observation deadline", got, deadline)
				}
				if r.URL.Path != "/v1/services/effects" {
					t.Fatal("application used the observation endpoint", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}}}
			if _, err := c.send(ctx, []byte(`{"active":false}`)); err == nil {
				t.Fatal("a plain observation receipt authorized protocol progress", err)
			}
			body = `{"applied":true}`
			if _, err := c.send(ctx, []byte(`{"active":false}`)); err != nil {
				t.Fatal("explicit application reply was not accepted", err)
			}
		})
	}
}
