package runtimeconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSerializedWorkerCatalogDoesNotMergeFactoryDefaultsOrReadOldModulePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot.json")
	if err := os.WriteFile(path, []byte(`{"platform":"host","workers":{"network":"/native/network"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Workers) != 1 || b.Workers["network"] != "/native/network" {
		t.Fatal("undeclared default worker added", b.Workers)
	}
	for _, raw := range []string{`{"platform":"host","module_path":"/old/modules"}`, `{"platform":"host","workers":{}}`, `{"platform":"host","workers":{"network":"relative"}}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(path); err == nil {
			t.Fatal("invalid or former boot catalog accepted", raw)
		}
	}
}
