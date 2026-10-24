// SPDX-License-Identifier: BSD-3-Clause

package release

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// This intentionally accepts a small USTAR subset rather than a generic
// extractor. No PAX/GNU metadata, links, directories, devices or sparse files
// can alter a later entry's path, size, or interpretation.
type bundleReader struct {
	ctx context.Context
	r   io.Reader
}

func (b bundleReader) read(p []byte) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	_, err := io.ReadFull(contextReader{b.ctx, b.r}, p)
	return err
}

func (b bundleReader) header(expected string, maximum int64) (int64, error) {
	var h [512]byte
	if err := b.read(h[:]); err != nil {
		return 0, fmt.Errorf("read %s header: %w", expected, err)
	}
	if string(h[257:263]) != "ustar\x00" || string(h[263:265]) != "00" || h[156] != 0 && h[156] != '0' {
		return 0, errors.New("bundle requires plain USTAR regular files only")
	}
	name, err := tarString(h[:100])
	if err != nil || name != expected || !allZero(h[157:257]) || !allZero(h[345:500]) {
		return 0, fmt.Errorf("unexpected bundle entry (expected %s; no links or path prefixes)", expected)
	}
	got, err := tarOctal(h[148:156])
	if err != nil {
		return 0, fmt.Errorf("tar checksum: %w", err)
	}
	var sum int64
	for i, v := range h {
		if i >= 148 && i < 156 {
			sum += int64(' ')
		} else {
			sum += int64(v)
		}
	}
	if sum != got {
		return 0, errors.New("invalid tar header checksum")
	}
	size, err := tarOctal(h[124:136])
	if err != nil || size <= 0 || size > maximum {
		return 0, errors.New("tar entry size is invalid or exceeds its bound")
	}
	return size, nil
}

func tarString(p []byte) (string, error) {
	i := bytes.IndexByte(p, 0)
	if i < 0 {
		return string(p), nil
	}
	if !allZero(p[i:]) {
		return "", errors.New("nonzero bytes after tar name terminator")
	}
	return string(p[:i]), nil
}

func tarOctal(p []byte) (int64, error) {
	s := strings.Trim(string(p), " \x00")
	if s == "" {
		return 0, errors.New("empty tar octal integer")
	}
	for _, c := range s {
		if c < '0' || c > '7' {
			return 0, errors.New("non-octal tar integer")
		}
	}
	return strconv.ParseInt(s, 8, 64)
}

func allZero(p []byte) bool {
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}

func (b bundleReader) padding(size int64) error {
	n := (512 - size%512) % 512
	var pad [512]byte
	if err := b.read(pad[:n]); err != nil {
		return err
	}
	if !allZero(pad[:n]) {
		return errors.New("nonzero tar entry padding")
	}
	return nil
}

func (b bundleReader) metadata(name string, maximum int64) ([]byte, error) {
	n, err := b.header(name, maximum)
	if err != nil {
		return nil, err
	}
	data := make([]byte, n)
	if err := b.read(data); err != nil {
		return nil, err
	}
	if err := b.padding(n); err != nil {
		return nil, err
	}
	return data, nil
}

func (b bundleReader) finish() error {
	var trailer [1024]byte
	if err := b.read(trailer[:]); err != nil {
		return fmt.Errorf("missing complete tar trailer: %w", err)
	}
	if !allZero(trailer[:]) {
		return errors.New("unexpected extra bundle entry or nonzero tar trailer")
	}
	var extra [4096]byte
	total := 0
	for {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		n, err := b.r.Read(extra[:])
		total += n
		if total > MaxTrailerBytes || !allZero(extra[:n]) {
			return errors.New("excessive or nonzero data after tar trailer")
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type BundleOptions struct {
	ReleaseID string
	Sequence  uint64
	GOOS      string
	GOARCH    string
}

// WriteBundle is the offline publication primitive. It reads from pinned local
// files, inspects both executables without executing them, and signs the
// complete manifest. The caller must publish output only after a nil error,
// normally using a temporary file + fsync + rename. No key is saved by this API.
func WriteBundle(ctx context.Context, output io.Writer, options BundleOptions, key ed25519.PrivateKey, routerd, modules *os.File) (Manifest, error) {
	m := Manifest{Version: ManifestVersion, ReleaseID: options.ReleaseID, Sequence: options.Sequence, GOOS: options.GOOS, GOARCH: options.GOARCH}
	if ctx == nil || output == nil {
		return m, errors.New("bundle context and output are required")
	}
	if len(key) != ed25519.PrivateKeySize {
		return m, errors.New("invalid Ed25519 signing key")
	}
	m.KeyID = KeyID(key.Public().(ed25519.PublicKey))
	files := []*os.File{routerd, modules}
	for i, name := range []string{"routerd", "routerd-modules"} {
		f := files[i]
		if f == nil {
			return m, errors.New("both executable files are required")
		}
		stat, err := f.Stat()
		if err != nil {
			return m, err
		}
		if !stat.Mode().IsRegular() || stat.Size() < 64 || stat.Size() > MaxArtifactBytes {
			return m, errors.New("bundle input must be a bounded regular file")
		}
		hash := sha256.New()
		if _, err := io.Copy(hash, contextReader{ctx, io.NewSectionReader(f, 0, stat.Size())}); err != nil {
			return m, err
		}
		if err := InspectELF(f, stat.Size(), name, m.ReleaseID, m.GOOS, m.GOARCH); err != nil {
			return m, fmt.Errorf("%s: %w", name, err)
		}
		m.Artifacts = append(m.Artifacts, Artifact{Name: name, Size: stat.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))})
	}
	raw, signature, err := SignManifest(m, key)
	if err != nil {
		return m, err
	}
	tw := tar.NewWriter(output)
	header := func(name string, size int64, mode int64) error {
		return tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: size, Mode: mode, ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR})
	}
	for _, metadata := range []struct {
		name string
		data []byte
	}{{"manifest.json", raw}, {"manifest.sig", signature}} {
		if err := header(metadata.name, int64(len(metadata.data)), 0600); err != nil {
			return m, err
		}
		if _, err := tw.Write(metadata.data); err != nil {
			return m, err
		}
	}
	for i, a := range m.Artifacts {
		if err := header(a.Name, a.Size, 0500); err != nil {
			return m, err
		}
		hash := sha256.New()
		n, err := io.Copy(io.MultiWriter(tw, hash), contextReader{ctx, io.NewSectionReader(files[i], 0, a.Size)})
		if err != nil {
			return m, err
		}
		if n != a.Size || hex.EncodeToString(hash.Sum(nil)) != a.SHA256 {
			return m, errors.New("bundle input changed during publication")
		}
	}
	if err := ctx.Err(); err != nil {
		return m, err
	}
	if err := tw.Close(); err != nil {
		return m, err
	}
	return m, nil
}
