package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
)

func TestSourceRejectsCaseAliasesForTypedFields(t *testing.T) {
	for _, source := range []string{
		`{"SCHEMA_VERSION":2,"resources":[]}`,
		`{"ſchema_version":2,"resources":[]}`,
		`{"schema_version":2,"resources":[],"Resources":[]}`,
		`{"schema_version":2,"resources":[{"id":"lan","ID":"other","interface":{"name":"eth0"}}]}`,
		`{"schema_version":2,"resources":[{"id":"lan","interface":{"name":"eth0","mtu":1500,"MTU":1400}}]}`,
		"schema_version: 2\nresources:\n  - id: lan\n    ID: other\n    interface:\n      name: eth0\n",
	} {
		if _, err := config.Parse([]byte(source)); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("case alias was not rejected: %s; err=%v", source, err)
		}
	}
}

func TestJournalRejectsCaseAliasesAtEveryTypedBoundary(t *testing.T) {
	for _, record := range []string{
		`{"revision":1,"Revision":2,"config":{"schema_version":2,"resources":[]}}`,
		`{"revision":1,"config":{"schema_version":2,"Resources":[]}}`,
		`{"revision":1,"config":{"schema_version":2,"resources":[{"id":"a","Id":"b","interface":{"name":"eth0"}}]}}`,
		`{"revision":1,"config":{"schema_version":2,"resources":[{"id":"a","interface":{"name":"eth0","MTU":1400}}]}}`,
		`{"revision":2,"last_request_id":"trial","confirmation":{"previous_revision":1,"request_id":"trial","deadline":"2030-01-01T00:00:00Z","previous":{"Schema_version":2,"resources":[]}},"config":{"schema_version":2,"resources":[]}}`,
	} {
		store := &config.Store{Dir: t.TempDir()}
		if err := os.WriteFile(filepath.Join(store.Dir, "config.json"), []byte(record), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("journal case alias was accepted: %s; err=%v", record, err)
		}
	}
}

func TestCommitPayloadRejectsNestedAliasesBeforeDecoding(t *testing.T) {
	good := `{"expected_revision":1,"request_id":"candidate","config":{"schema_version":2,"resources":[{"id":"lan","interface":{"name":"eth0","mtu":1500}}]}}`
	var request control.CommitRequest
	if err := config.Decode(strings.NewReader(good), &request); err != nil || request.ExpectedRevision != 1 || request.Config.Resources[0].ID != "lan" {
		t.Fatal("canonical CLI/HTTP commit payload failed", request, err)
	}
	for _, bad := range []string{
		strings.Replace(good, `"expected_revision":1`, `"EXPECTED_REVISION":1`, 1),
		strings.Replace(good, `"config":`, `"CONFIG":`, 1),
		strings.Replace(good, `"mtu":1500`, `"mtu":1500,"MTU":1400`, 1),
	} {
		request := control.CommitRequest{ExpectedRevision: 99}
		if err := config.Decode(strings.NewReader(bad), &request); err == nil || request.ExpectedRevision != 99 {
			t.Fatal("nested alias reached typed decoding", request, err)
		}
	}
}

type strictItem struct {
	Value int `json:"value"`
}

type strictCustom struct {
	Values map[string]int
}

func (v *strictCustom) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &v.Values)
}

func TestStrictDecodePreservesMapKeysAndCustomJSONTypes(t *testing.T) {
	t.Parallel()
	type payload struct {
		Items  map[string]strictItem `json:"items"`
		List   []strictItem          `json:"list"`
		Custom strictCustom          `json:"custom"`
		Raw    json.RawMessage       `json:"raw"`
	}
	var result payload
	input := `{"items":{"ID":{"value":1},"id":{"value":2}},"list":[{"value":3}],"custom":{"UPPER":4,"upper":5},"raw":{"ID":"a","id":"b"}}`
	if err := config.Decode(strings.NewReader(input), &result); err != nil {
		t.Fatal(err)
	}
	if result.Items["ID"].Value != 1 || result.Items["id"].Value != 2 || result.Custom.Values["UPPER"] != 4 || result.Custom.Values["upper"] != 5 || !strings.Contains(string(result.Raw), `"ID"`) || !strings.Contains(string(result.Raw), `"id"`) {
		t.Fatal("operator map keys or a custom JSON value were changed", result)
	}
	for _, input := range []string{`{"items":{"ID":{"VALUE":1}}}`, `{"list":[{"VALUE":1}]}`} {
		if err := config.Decode(strings.NewReader(input), &result); err == nil {
			t.Fatal("typed map/list value admitted an alias", input)
		}
	}
	var generic any
	if err := config.Decode(strings.NewReader(`{"ID":1,"id":2}`), &generic); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(generic, map[string]any{"ID": float64(1), "id": float64(2)}) {
		t.Fatal("generic CLI value lost case-sensitive data keys", generic)
	}
	var scalar int
	if err := config.Decode(strings.NewReader("42"), &scalar); err != nil || scalar != 42 {
		t.Fatal("generic CLI scalar decoding failed", scalar, err)
	}
}

type strictEmbedded struct {
	Resource string `json:"resource"`
}

type strictTaggedValue struct {
	Name string `json:"name"`
}

type strictPlainValue struct {
	Ignored string `json:"ignored"`
}

type strictTagged struct {
	Chosen strictTaggedValue `json:"Value"`
}

type strictPlain struct {
	Value strictPlainValue
}

func TestStrictDecodeMatchesJSONEmbeddedFieldSelection(t *testing.T) {
	t.Parallel()
	type payload struct {
		strictEmbedded `json:",inline"`
		strictPlain
		strictTagged
		Nested strictEmbedded `json:"nested"`
		Hidden string         `json:"-"`
	}
	var result payload
	input := `{"resource":"root","Value":{"name":"tagged wins"},"nested":{"resource":"nested"}}`
	if err := config.Decode(strings.NewReader(input), &result); err != nil || result.Resource != "root" || result.Chosen.Name != "tagged wins" || result.Nested.Resource != "nested" {
		t.Fatal("JSON promoted/tagged field selection changed", result, err)
	}
	for _, input := range []string{`{"Resource":"alias"}`, `{"value":{"name":"alias"}}`, `{"Value":{"ignored":"wrong embedded candidate"}}`, `{"nested":{"Resource":"alias"}}`, `{"Hidden":"ignored"}`} {
		if err := config.Decode(strings.NewReader(input), &result); err == nil {
			t.Fatal("unknown, hidden, or noncanonical promoted field accepted", input)
		}
	}
}

type strictLeaf struct {
	Value int `json:"value"`
}
type strictLeft struct{ strictLeaf }
type strictRight struct{ strictLeaf }

func TestStrictDecodeRejectsAmbiguousPromotedFields(t *testing.T) {
	var result struct {
		strictLeft
		strictRight
	}
	if err := config.Decode(strings.NewReader(`{"value":1}`), &result); err == nil {
		t.Fatal("ambiguous promoted field was accepted")
	}
}
