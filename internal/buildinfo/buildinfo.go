// Package buildinfo identifies the complete release, rather than individual workers.
package buildinfo

import (
	"errors"
	"runtime"
	"strings"

	"github.com/imksoo/routerd2/internal/runtimeabi"
)

const IdentityPrefix = "ROUTERD2-RELEASE-V1:"
const IdentitySuffix = ":END"

// Identity is the single linker-set release identity in each native executable. The
// explicit record survives -trimpath and symbol stripping, allowing a signed
// bundle to be checked without executing any incoming ELF.
// An unversioned developer build has no complete record in rodata and cannot
// be installed as a signed release. Keeping this empty also prevents a stale
// default record from surviving linker replacement beside the real identity.
var Identity string

// ReleaseID is derived from that same record used by the offline verifier.
var ReleaseID = mustIdentity(Identity)

func ParseIdentity(record string) (string, error) {
	if !strings.HasPrefix(record, IdentityPrefix) || !strings.HasSuffix(record, IdentitySuffix) {
		return "", errors.New("invalid routerd release identity record")
	}
	id := strings.TrimSuffix(strings.TrimPrefix(record, IdentityPrefix), IdentitySuffix)
	if len(id) == 0 || len(id) > 128 {
		return "", errors.New("routerd release identity must be 1..128 ASCII characters")
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' || c == '+') {
			return "", errors.New("invalid character in routerd release identity")
		}
	}
	if id == "." || id == ".." {
		return "", errors.New("invalid routerd release identity")
	}
	return id, nil
}

func mustIdentity(record string) string {
	if record == "" {
		return "dev"
	}
	id, err := ParseIdentity(record)
	if err != nil {
		panic(err)
	}
	return id
}

type Info struct {
	ReleaseID  string                `json:"release_id"`
	GoVersion  string                `json:"go_version"`
	GOOS       string                `json:"goos"`
	GOARCH     string                `json:"goarch"`
	APIVersion int                   `json:"api_version"`
	RuntimeABI runtimeabi.Descriptor `json:"runtime_abi"`
}

func Current() Info {
	return Info{ReleaseID: ReleaseID, GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, APIVersion: 1, RuntimeABI: runtimeabi.Current()}
}
