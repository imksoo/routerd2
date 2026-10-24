package storage

import (
	"encoding/binary"
	"errors"
	"testing"
)

func ext4Fixture() []byte {
	b := make([]byte, ext4SuperblockSize)
	binary.LittleEndian.PutUint16(b[0x38:], 0xef53)
	binary.LittleEndian.PutUint16(b[0x3c:], 1)
	binary.LittleEndian.PutUint32(b[0x5c:], 0x4)
	binary.LittleEndian.PutUint32(b[0x60:], 0x40)
	binary.LittleEndian.PutUint32(b[0xe0:], 8)
	copy(b[0x68:], []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	return b
}

func TestExt4SuperblockProfileAndUUID(t *testing.T) {
	uuid, err := inspectSuperblock(ext4Fixture())
	if err != nil || uuid != "01020304-0506-0708-090a-0b0c0d0e0f10" {
		t.Fatalf("UUID = %q, %v", uuid, err)
	}
	tests := map[string]func([]byte) []byte{
		"short":                   func(b []byte) []byte { return b[:1023] },
		"magic":                   func(b []byte) []byte { b[0x38] = 0; return b },
		"unknown error policy":    func(b []byte) []byte { b[0x3c] = 4; return b },
		"ext2 without journal":    func(b []byte) []byte { b[0x5c] = 0; return b },
		"ext3 without extents":    func(b []byte) []byte { b[0x60] = 0; return b },
		"journal device":          func(b []byte) []byte { b[0x60] |= 0x8; return b },
		"missing journal inode":   func(b []byte) []byte { b[0xe0] = 0; return b },
		"external journal device": func(b []byte) []byte { b[0xe4] = 1; return b },
		"external journal UUID":   func(b []byte) []byte { b[0xd0] = 1; return b },
		"custom journal path":     func(b []byte) []byte { copy(b[0x200:], "journal_path=/dev/other"); return b },
		"custom option":           func(b []byte) []byte { copy(b[0x200:], "discard"); return b },
		"default no barrier":      func(b []byte) []byte { binary.LittleEndian.PutUint32(b[0x100:], 0x100); return b },
		"default writeback":       func(b []byte) []byte { binary.LittleEndian.PutUint32(b[0x100:], 0x60); return b },
		"zero UUID":               func(b []byte) []byte { clear(b[0x68:0x78]); return b },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := inspectSuperblock(change(ext4Fixture())); !errors.Is(err, ErrFilesystem) {
				t.Fatalf("expected unsupported filesystem, got %v", err)
			}
		})
	}
}
