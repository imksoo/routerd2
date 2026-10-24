package storage

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

const ext4SuperblockSize = 1024

// inspectSuperblock identifies the supported ext4 profile before invoking the
// kernel. This is not an integrity check or a substitute for filesystem repair.
// In particular, external journals could make an apparently single-device
// mount open another, unspecified device, so they are not accepted.
func inspectSuperblock(b []byte) (string, error) {
	fail := func(reason string) (string, error) {
		return "", fmt.Errorf("%w: %s", ErrFilesystem, reason)
	}
	if len(b) != ext4SuperblockSize {
		return fail("truncated superblock")
	}
	if binary.LittleEndian.Uint16(b[0x38:]) != 0xef53 {
		return fail("wrong superblock magic")
	}
	if behavior := binary.LittleEndian.Uint16(b[0x3c:]); behavior < 1 || behavior > 3 {
		return fail("unrecognized on-disk error behavior")
	}
	compat := binary.LittleEndian.Uint32(b[0x5c:])
	incompat := binary.LittleEndian.Uint32(b[0x60:])
	if compat&0x4 == 0 || incompat&0x40 == 0 {
		return fail("an internal journal and extents are required")
	}
	if incompat&0x8 != 0 || binary.LittleEndian.Uint32(b[0xe0:]) == 0 || binary.LittleEndian.Uint32(b[0xe4:]) != 0 || !allZero(b[0xd0:0xe0]) {
		return fail("external journal or journal device is not supported")
	}
	// Superblock mount strings are interpreted by ext4 before user-supplied
	// options. Refuse them instead of maintaining a second, incomplete ext4
	// option parser (including journal_path and journal_dev).
	if !allZero(b[0x200:0x240]) {
		return fail("custom on-disk mount options are not supported")
	}
	// EXT4_DEFM_NOBARRIER: the filesystem must not disable write barriers.
	defaults := binary.LittleEndian.Uint32(b[0x100:])
	if defaults&0x100 != 0 {
		return fail("on-disk defaults disable write barriers")
	}
	// Default options can be omitted from mountinfo. Reject writeback here so
	// its absence from the printed option list can never mean safe ordering.
	if defaults&0x60 == 0x60 {
		return fail("on-disk defaults select unordered data writeback")
	}
	uuid := b[0x68:0x78]
	if allZero(uuid) {
		return fail("filesystem UUID must be nonzero")
	}
	encoded := hex.EncodeToString(uuid)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func allZero(b []byte) bool {
	return bytes.Count(b, []byte{0}) == len(b)
}
