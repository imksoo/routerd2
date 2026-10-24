package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnknownFieldsAndTrailingDocumentsRejected(t *testing.T) {
	for _, s := range []string{`{"schema_version":2,"resources":[],"typo":true}`, `{"schema_version":2,"resources":[{"id":"lan","interfce":{}}]}`, `{"schema_version":2,"resources":[]} {}`} {
		var d Document
		if err := Decode(strings.NewReader(s), &d); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}

func TestYAMLRoundtripAndAmbiguityRejection(t *testing.T) {
	d, err := Parse([]byte(`schema_version: 2
resources:
  - id: lan
    interface:
      name: eth0
  - id: lan-address
    depends_on: [lan]
    address:
      interface: eth0
      cidr: 192.0.2.1/24
  - id: forwarding
    forwarding:
      ipv4: true
`))
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
	for _, bad := range []string{"schema_version: 2\nschema_version: 2\nresources: []\n", "schema_version: 2\nresources: &x []\n", "schema_version: 2\nresources:\n  - id: bad\n    interfce: {}\n"} {
		if _, err = Parse([]byte(bad)); err == nil {
			t.Fatalf("accepted ambiguous/unknown fields: %s", bad)
		}
	}
}

func TestCommittedStateSurvivesBootstrapChanges(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "bootstrap.json")
	if err := AtomicJSON(p, Document{SchemaVersion: SchemaVersion}); err != nil {
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

func TestPrototypeDocumentAndJournalAreRejected(t *testing.T) {
	for _, input := range []string{
		`{"schema_version":1,"network":{}}`,
		`{"schema_version":1,"resources":[]}`,
		`{"schema_version":2,"network":{},"services":[]}`,
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Fatal("accepted a discarded prototype document", input)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"revision":1,"config":{"schema_version":1,"network":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(t.TempDir(), "startup.yaml")
	if err := AtomicJSON(seed, Document{SchemaVersion: SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(dir, seed); err == nil {
		t.Fatal("prototype journal was read or silently replaced by startup seed")
	}
}
