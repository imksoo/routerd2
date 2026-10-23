package kmod

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDependencyOrderAndBuiltins(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "modules.dep"), []byte("kernel/virtio_net.ko: kernel/virtio_ring.ko kernel/virtio.ko\nkernel/virtio_ring.ko: kernel/virtio.ko\nkernel/virtio.ko:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "modules.builtin"), []byte("kernel/virtio.ko\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	i, err := ReadIndex(d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := i.Order([]string{"virtio-net", "virtio_ring"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kernel/virtio_ring.ko", "kernel/virtio_net.ko"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

func TestRejectInvalidDependencyGraph(t *testing.T) {
	i := &Index{Files: map[string]string{"a": "a.ko", "b": "b.ko"}, Dependencies: map[string][]string{"a": {"b"}, "b": {"a"}}}
	if _, err := i.Order([]string{"a"}); err == nil {
		t.Fatal("accepted cycle")
	}
	if _, err := i.Order([]string{"missing"}); err == nil {
		t.Fatal("accepted unknown module")
	}
	d := t.TempDir()
	_ = os.WriteFile(filepath.Join(d, "modules.dep"), []byte("../outside.ko:\n"), 0o600)
	if _, err := ReadIndex(d); err == nil {
		t.Fatal("accepted path traversal")
	}
}
