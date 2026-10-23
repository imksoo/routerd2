package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnknownFieldsAndTrailingDocumentsRejected(t *testing.T) {
	for _, s := range []string{`{"schema_version":1,"network":{},"typo":true}`, `{"schema_version":1,"network":{"interfces":[]}}`, `{"schema_version":1,"network":{}} {}`} {
		var d Document
		if err := Decode(strings.NewReader(s), &d); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}

func TestYAMLRoundtripAndAmbiguityRejection(t *testing.T) {
	d, err := Parse([]byte("schema_version: 1\nnetwork:\n  interfaces:\n    - name: eth0\n      addresses: [192.0.2.1/24]\n  forwarding:\n    ipv4: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := YAML(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Parse(b); err != nil {
		t.Fatalf("canonical export is invalid: %s: %v", b, err)
	}
	for _, bad := range []string{"schema_version: 1\nschema_version: 2\nnetwork: {}\n", "schema_version: 1\nnetwork: &x {}\n", "schema_version: 1\nnetwork:\n  interfces: []\n"} {
		if _, err = Parse([]byte(bad)); err == nil {
			t.Fatalf("accepted ambiguous/unknown fields: %s", bad)
		}
	}
}

func TestCommittedStateSurvivesBootstrapChanges(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "bootstrap.json")
	if err := AtomicJSON(p, Document{SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	s, c, err := Open(filepath.Join(d, "state"), p)
	if err != nil {
		t.Fatal(err)
	}
	c.Revision = 7
	if err = s.Save(c); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, got, err := Open(s.Dir, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 7 {
		t.Fatal("bootstrap overrode committed configuration")
	}
	fi, err := os.Stat(filepath.Join(s.Dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected permissions %v", fi.Mode())
	}
}
