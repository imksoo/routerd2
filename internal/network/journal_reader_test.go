//go:build linux

package network

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNetworkReaderRejectsAmbiguousOwnershipJSON(t *testing.T) {
	for _, raw := range []string{
		`{"version":7,"version":7,"status":"applied"}`,
		`{"version":7,"Version":7,"status":"applied"}`,
		`{"version":7,"status":"applied","ſtatus":"applied"}`,
		`{"version":7,"status":"applied","future_owner":true}`,
		`{"version":7,"status":"applied","desired":{"interfaces":[{"name":"lan0","name":"wan0"}]}}`,
		`{"version":7,"status":"applied","desired":{"interfaces":[{"name":"lan0","MTU":1400}]}}`,
		`{"version":7,"status":"unknown"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "network.json")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			k := fake()
			e, err := newEngine(Options{StatePath: path}, k)
			if e != nil {
				defer e.Close()
			}
			if err == nil {
				t.Fatal("ambiguous or unknown ownership record accepted")
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || string(after) != raw || len(k.writes) != 0 {
				t.Fatal("refused record changed file or kernel", readErr)
			}
		})
	}
}

func TestNetworkReaderRejectsUnsafeJournalFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "readable", "parent symlink", "writable parent", "lock symlink", "lock hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "network.json")
			if err := os.WriteFile(path, []byte(`{"version":7,"status":"applied"}`), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink", "hardlink":
				original := path + ".original"
				if err := os.Rename(path, original); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "symlink" {
					err = os.Symlink(original, path)
				} else {
					err = os.Link(original, path)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "readable":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "network.json")
			case "writable parent":
				if err := os.Chmod(dir, 0770); err != nil {
					t.Fatal(err)
				}
			case "lock symlink", "lock hardlink":
				original := filepath.Join(dir, "foreign-lock")
				if err := os.WriteFile(original, nil, 0600); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "lock symlink" {
					err = os.Symlink(original, path+".lock")
				} else {
					err = os.Link(original, path+".lock")
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			k := fake()
			e, err := newEngine(Options{StatePath: path}, k)
			if e != nil {
				defer e.Close()
			}
			if err == nil || len(k.writes) != 0 {
				t.Fatal("unsafe journal authority accepted", err)
			}
		})
	}
}
