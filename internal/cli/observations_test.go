package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/imksoo/routerd2/internal/localapi"
)

func TestShowObservationsBoundsAndStorageFailureDoNotPrintPartialHistory(t *testing.T) {
	calls := 0
	fail := false
	transport := &handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/observations" || r.URL.Query().Get("after_cursor") != "7" || r.URL.Query().Get("limit") != "2" {
			t.Error("wrong durable observation query", r.URL)
		}
		if fail {
			localapi.Fail(w, 503, fmt.Errorf("storage unavailable"))
			return
		}
		fmt.Fprint(w, `{"records":[],"next_cursor":7,"more":false}`)
	})}
	client := &localapi.Client{HTTP: &http.Client{Transport: transport}}
	var output bytes.Buffer
	if err := showObservations(context.Background(), client, []string{"limit", "2", "after", "7"}, &output); err != nil || output.Len() == 0 || calls != 1 {
		t.Fatal("bounded observation page", err, output.String(), calls)
	}
	for _, args := range [][]string{{"after"}, {"after", "-1"}, {"after", "18446744073709551615"}, {"limit", "0"}, {"limit", "17"}, {"after", "7", "after", "8"}, {"limit", "1", "limit", "2"}, {"topic", "configuration.committed"}} {
		output.Reset()
		before := calls
		if err := showObservations(context.Background(), client, args, &output); err == nil || output.Len() != 0 || calls != before {
			t.Fatalf("invalid query reached storage or printed output: %v %v", args, err)
		}
	}
	fail = true
	output.Reset()
	if err := showObservations(context.Background(), client, []string{"after", "7", "limit", "2"}, &output); !localapi.IsHTTPStatus(err, 503) || output.Len() != 0 {
		t.Fatalf("storage failure leaked output: %v %s", err, output.String())
	}
}

func TestObservationCompletionDoesNotRepeatUsedFilters(t *testing.T) {
	s := &interactiveShell{}
	for input, want := range map[string]string{
		"show obs":                           "show observations ",
		"show observations a":                "show observations after ",
		"show observations after 7 l":        "show observations after 7 limit ",
		"show observations limit 2 a":        "show observations limit 2 after ",
		"show observations after 7 limit 2 ": "show observations after 7 limit 2 ",
	} {
		got, _, ok := s.complete(input, len(input), '\t')
		if !ok || got != want {
			t.Fatalf("%q: %q, want %q", input, got, want)
		}
	}
}
