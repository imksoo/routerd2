package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/imksoo/routerd2/internal/localapi"
)

func TestShowDerivedReadsCurrentAPIAndNeverPrintsFailedOrPersistedUp(t *testing.T) {
	calls := 0
	fail := false
	transport := &handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.EscapedPath() != "/v1/derived/derived%2Fwan" {
			t.Error("resource identity was not escaped", r.URL)
		}
		if fail {
			localapi.Fail(w, 503, fmt.Errorf("current evaluator unavailable"))
			return
		}
		fmt.Fprint(w, `{"id":"derived/wan","state":{"phase":"Indeterminate","inputs":"indeterminate","pending_transition":"None"}}`)
	})}
	client := &localapi.Client{HTTP: &http.Client{Transport: transport}}
	var out bytes.Buffer
	if err := showDerived(context.Background(), client, []string{"derived/wan"}, &out); err != nil || calls != 1 || !bytes.Contains(out.Bytes(), []byte("Indeterminate")) {
		t.Fatal(err, out.String(), calls)
	}
	for _, args := range [][]string{nil, {"one", "two"}, {"../wan"}, {"wan?revision=1"}, {"/wan"}} {
		out.Reset()
		before := calls
		if err := showDerived(context.Background(), client, args, &out); err == nil || calls != before || out.Len() != 0 {
			t.Fatal("invalid lookup reached API", args, err)
		}
	}
	fail = true
	out.Reset()
	if err := showDerived(context.Background(), client, []string{"derived/wan"}, &out); !localapi.IsHTTPStatus(err, 503) || out.Len() != 0 {
		t.Fatal("failed current view leaked output", err, out.String())
	}
	s := &interactiveShell{}
	line, _, ok := s.complete("show der", len("show der"), '\t')
	if !ok || line != "show derived " {
		t.Fatal(line, ok)
	}
}
