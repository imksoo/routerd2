package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/imksoo/routerd2/internal/localapi"
)

func TestShowEventStoreIsBoundedReadOnlyAndDoesNotPrintFailedResponses(t *testing.T) {
	calls := 0
	failed := false
	client := &localapi.Client{HTTP: &http.Client{Transport: &handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/v1/event-store" || r.URL.RawQuery != "" {
			t.Error(r.Method, r.URL)
		}
		if failed {
			localapi.Fail(w, 503, fmt.Errorf("storage unavailable"))
			return
		}
		fmt.Fprint(w, `{"capacity":{"pressure":"warning"}}`)
	})}}}
	var output bytes.Buffer
	if err := showEventStore(context.Background(), client, nil, &output); err != nil || calls != 1 || output.Len() == 0 {
		t.Fatal(err, calls, output.String())
	}
	output.Reset()
	if err := showEventStore(context.Background(), client, []string{"anything"}, &output); err == nil || calls != 1 || output.Len() != 0 {
		t.Fatal("invalid command reached storage", err)
	}
	failed = true
	if err := showEventStore(context.Background(), client, nil, &output); !localapi.IsHTTPStatus(err, 503) || output.Len() != 0 {
		t.Fatal(err, output.String())
	}
	s := &interactiveShell{}
	if got, _, ok := s.complete("show event", len("show event"), '\t'); !ok || got != "show event-store " {
		t.Fatal(got)
	}
}
