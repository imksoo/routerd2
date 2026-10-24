package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/imksoo/routerd2/internal/localapi"
)

func TestShowResourcesRequiresConsistentPagesBeforeOutput(t *testing.T) {
	for _, changed := range []bool{false, true} {
		calls := 0
		transport := &handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			calls++
			if calls == 1 {
				fmt.Fprint(w, `{"revision":7,"projection_generation":12,"observation_id":"same","total":2,"offset":0,"next_offset":1,"resources":[{"id":"a","state":{"active":false,"when":0,"ready":0,"reason":"when_indeterminate"}}]}`)
				return
			}
			if req.URL.Query().Get("offset") != "1" || req.URL.Query().Get("observation_id") != "same" {
				t.Error("unfenced continuation", req.URL)
			}
			if changed {
				localapi.Fail(w, 409, fmt.Errorf("resource observation changed"))
				return
			}
			fmt.Fprint(w, `{"revision":7,"projection_generation":12,"observation_id":"same","total":2,"offset":1,"resources":[{"id":"b","state":{"active":true,"when":2,"ready":2}}]}`)
		})}
		client := &localapi.Client{HTTP: &http.Client{Transport: transport}}
		var output bytes.Buffer
		err := showResources(context.Background(), client, &output)
		if changed {
			if !localapi.IsHTTPStatus(err, 409) || output.Len() != 0 {
				t.Fatal("mixed snapshots leaked partial output", err, output.String())
			}
		} else {
			var result struct{ Resources []struct{ ID string } }
			if err != nil || json.Unmarshal(output.Bytes(), &result) != nil || len(result.Resources) != 2 || result.Resources[1].ID != "b" {
				t.Fatal("lost resource page", err, output.String())
			}
		}
	}
}
