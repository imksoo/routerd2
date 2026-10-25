// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

// Package releasepublish is the offline build-side publisher. Its signing key
// never enters the routerd runtime or the bundle it produces.
package releasepublish

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/imksoo/routerd2/internal/release"
	"golang.org/x/sys/unix"
)

// Run implements routerd-bundle pack and trust. It never creates a production
// key, downloads inputs, executes incoming files, stages a release, or reboots.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, err := fmt.Fprintln(stdout, `routerd-bundle (offline publisher; not part of the router runtime)
  routerd-bundle pack --private-key-file FILE --layout FILE --release-id ID --sequence N --target-arch amd64|arm64 --output FILE
  routerd-bundle trust --private-key-file FILE

The key must be an owner-only PKCS#8 Ed25519 PEM file. All paths must be clean
absolute paths without symlink components. Output must not already exist.
The trust command prints only the public trust map, for explicit installation.`)
		return err
	}
	command := args[0]
	if command != "pack" && command != "trust" {
		return errors.New("unknown publisher command; run routerd-bundle help")
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(stderr)
	keyPath := f.String("private-key-file", "", "private PKCS#8 Ed25519 PEM file")
	var layoutPath, output, id, arch string
	var sequence uint64
	if command == "pack" {
		f.StringVar(&layoutPath, "layout", "", "explicit runtime artifact sources, entrypoint and role catalog JSON")
		f.StringVar(&output, "output", "", "new signed USTAR bundle")
		f.StringVar(&id, "release-id", "", "embedded release identity")
		f.StringVar(&arch, "target-arch", "", "amd64 or arm64")
		f.Uint64Var(&sequence, "sequence", 0, "strictly increasing publisher counter")
	}
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *keyPath == "" {
		return errors.New("a private key file and no positional arguments are required")
	}
	key, err := readKey(*keyPath)
	if err != nil {
		return fmt.Errorf("read publisher key: %w", err)
	}
	defer clear(key)
	public := key.Public().(ed25519.PublicKey)
	if command == "trust" {
		return json.NewEncoder(stdout).Encode(map[string]string{release.KeyID(public): base64.StdEncoding.EncodeToString(public)})
	}
	if sequence == 0 || id == "" || (arch != "amd64" && arch != "arm64") {
		return errors.New("pack requires a release ID, positive sequence and explicit amd64 or arm64 target")
	}
	layout, err := readLayout(layoutPath)
	if err != nil {
		return fmt.Errorf("runtime layout: %w", err)
	}
	files := map[string]*os.File{}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	options := release.BundleOptions{ReleaseID: id, Sequence: sequence, GOOS: "linux", GOARCH: arch, BootABI: layout.BootABI, WorkerABI: layout.WorkerABI, Entrypoint: layout.Entrypoint, Roles: layout.Roles}
	for _, a := range layout.Artifacts {
		if _, exists := files[a.Name]; exists || a.File == *keyPath {
			return errors.New("duplicate artifact or signing key selected as runtime payload")
		}
		f, err := openInput(a.File, false, release.MaxArtifactBytes)
		if err != nil {
			return fmt.Errorf("artifact %s input: %w", a.Name, err)
		}
		files[a.Name] = f
		options.Artifacts = append(options.Artifacts, release.Artifact{Name: a.Name, Kind: a.Kind, Command: a.Command})
	}
	var manifest release.Manifest
	err = publish(ctx, output, func(dst *os.File) error {
		var err error
		manifest, err = release.WriteBundle(ctx, dst, options, key, files)
		return err
	})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("bundle published at %s but reporting its manifest failed: %w", output, err)
	}
	digest := sha256.Sum256(raw)
	result := struct {
		Path     string           `json:"path"`
		Digest   string           `json:"digest"`
		Manifest release.Manifest `json:"manifest"`
	}{output, hex.EncodeToString(digest[:]), manifest}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("bundle published at %s but writing its result failed: %w", output, err)
	}
	return nil
}

// Layout is publisher input, not a runtime manifest. Local source paths never
// enter the signed runtime, and neither arbitrary argv nor shell hooks exist.
type Layout struct {
	BootABI    int                   `json:"boot_abi"`
	WorkerABI  int                   `json:"worker_abi"`
	Entrypoint string                `json:"entrypoint"`
	Roles      []release.RoleBinding `json:"roles"`
	Artifacts  []ArtifactInput       `json:"artifacts"`
}
type ArtifactInput struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Command string `json:"command,omitempty"`
	File    string `json:"file"`
}

func readLayout(path string) (Layout, error) {
	var layout Layout
	f, err := openInput(path, false, release.MaxManifestBytes)
	if err != nil {
		return layout, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, release.MaxManifestBytes+1))
	d.DisallowUnknownFields()
	if err := d.Decode(&layout); err != nil {
		return layout, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return layout, errors.New("trailing runtime layout data")
	}
	if len(layout.Artifacts) == 0 || len(layout.Artifacts) > release.MaxArtifacts || len(layout.Roles) == 0 || len(layout.Roles) > release.MaxRoles {
		return layout, errors.New("runtime layout artifact or role count exceeds bounds")
	}
	return layout, nil
}

func readKey(path string) (ed25519.PrivateKey, error) {
	f, err := openInput(path, true, 16<<10)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (16<<10)+1))
	defer clear(raw)
	if err != nil || len(raw) > 16<<10 {
		return nil, errors.New("cannot read a bounded publisher key")
	}
	// pem.Decode deliberately searches past malformed blocks. A publisher
	// must not silently skip the intended damaged key and use a later key.
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("-----BEGIN PRIVATE KEY-----")) || bytes.Count(raw, []byte("-----BEGIN ")) != 1 || bytes.Count(raw, []byte("-----END ")) != 1 {
		return nil, errors.New("expected a PKCS#8 PRIVATE KEY PEM block")
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" || len(block.Headers) != 0 || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("expected one unencrypted PKCS#8 PRIVATE KEY PEM block")
	}
	defer clear(block.Bytes)
	value, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid PKCS#8 private key")
	}
	key, ok := value.(ed25519.PrivateKey)
	if !ok || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("publisher key must use Ed25519")
	}
	return key, nil
}

func openInput(path string, private bool, maximum int64) (*os.File, error) {
	parent, name, err := openParent(path)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err == nil {
		if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size <= 0 || stat.Size > maximum {
			err = errors.New("input must be a nonempty bounded regular file")
		} else if stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0 || stat.Mode&0022 != 0 {
			err = errors.New("input must be owned by the publisher or root and not writable by others")
		} else if private && (stat.Uid != uint32(os.Geteuid()) || stat.Mode&0077 != 0 || stat.Nlink != 1) {
			err = errors.New("signing key must be owner-only with exactly one link")
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Walk using directory descriptors. Never let a path symlink redirect a private
// key read or the final no-overwrite publication to an unintended directory.
func openParent(path string) (*os.File, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, "", errors.New("path must be a clean absolute non-root path")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	current := os.NewFile(uintptr(fd), "/")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, part := range parts[:len(parts)-1] {
		fd, err = unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		current.Close()
		if err != nil {
			return nil, "", err
		}
		current = os.NewFile(uintptr(fd), part)
		var stat unix.Stat_t
		if err = unix.Fstat(fd, &stat); err == nil {
			stickyRoot := stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0
			if stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) || stat.Mode&0022 != 0 && !stickyRoot {
				err = errors.New("path component is writable by another user")
			}
		}
		if err != nil {
			current.Close()
			return nil, "", err
		}
	}
	return current, parts[len(parts)-1], nil
}

func publish(ctx context.Context, path string, write func(*os.File) error) error {
	parent, name, err := openParent(path)
	if err != nil {
		return err
	}
	defer parent.Close()
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := ".routerd-bundle-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(int(parent.Fd()), temporary, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), temporary)
	defer f.Close()
	defer unix.Unlinkat(int(parent.Fd()), temporary, 0)
	if err = write(f); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = f.Chmod(0644); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = unix.Renameat2(int(parent.Fd()), temporary, int(parent.Fd()), name, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("publish new bundle without overwriting %s: %w", path, err)
	}
	if err = parent.Sync(); err != nil {
		return fmt.Errorf("bundle published at %s but directory durability is unconfirmed: %w", path, err)
	}
	return nil
}
