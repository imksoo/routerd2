package engineimage

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractCopiesFilesAndModes(t *testing.T) {
	image, err := os.CreateTemp(t.TempDir(), "engine.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	archive := tar.NewWriter(image)
	contents := bytes.Repeat([]byte("engine contents\n"), 65536)
	for _, name := range []string{"usr/sbin/engine", "lib/library.so"} {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0750, Size: int64(len(contents))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Chdir(root)
	if err := Extract(image); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"usr/sbin/engine", "lib/library.so"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || !bytes.Equal(data, contents) {
			t.Fatalf("extracted %s contents: %v", name, err)
		}
		stat, err := os.Stat(name)
		if err != nil || stat.Mode().Perm() != 0750 {
			t.Fatalf("extracted %s mode: %v, %v", name, stat, err)
		}
	}
}

func TestExtractRejectsTruncatedBody(t *testing.T) {
	image, err := os.CreateTemp(t.TempDir(), "engine.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	archive := tar.NewWriter(image)
	if err := archive.WriteHeader(&tar.Header{Name: "usr/sbin/engine", Mode: 0755, Size: 65536}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(make([]byte, 32768)); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if err := Extract(image); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated body: %v", err)
	}
}
