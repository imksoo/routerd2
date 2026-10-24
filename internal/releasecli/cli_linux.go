// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

// Package releasecli exposes local signed-release management. Boot selection,
// health confirmation, execution and reboot are deliberately not CLI actions.
package releasecli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/imksoo/routerd2/internal/release"
)

const usage = `Usage: routerd release COMMAND [flags]

Commands:
  status       Show the verified durable release state
  stage        Verify a local signed tar and install its immutable executable pair
  verify       Re-verify an installed pair by --digest
  initialize   Register an installed baseline in an empty store
  prepare      Queue an installed pair for the next boot
  cancel       Cancel an untried pending release
  rollback     Select the previously confirmed release
  prune        Remove old generations, incomplete transactions, and named unused pairs

Every command requires --root, --trust-file and --target-arch.
prepare, cancel, rollback and prune also require --expected-generation.
stage requires --file; verify, initialize and prepare require --digest.
rollback requires --reason. prune accepts repeated --digest flags.

Preparing or rolling back changes only the durable selection. This interface
does not execute a payload, confirm health, change the running processes, or reboot.
`

type digestList []string

func (d *digestList) String() string         { return strings.Join(*d, ",") }
func (d *digestList) Set(value string) error { *d = append(*d, value); return nil }

// Run accepts arguments after "routerd release". Successful machine-readable
// results are JSON on stdout; errors are returned to the command's normal error
// handler. There are no trust defaults, environment substitutions, shell
// commands, downloads, signing keys, or interactive approval shortcuts.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if ctx == nil || stdout == nil {
		return errors.New("release command requires context and output")
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(stdout, usage)
		return err
	}
	command := args[0]
	switch command {
	case "status", "stage", "verify", "initialize", "prepare", "cancel", "rollback", "prune":
	default:
		return fmt.Errorf("unknown release command %q", command)
	}
	flags := flag.NewFlagSet("routerd release "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintf(stderr, "Usage: routerd release %s [flags]\n", command); flags.PrintDefaults() }
	root := flags.String("root", "", "absolute private release store directory")
	trust := flags.String("trust-file", "", "absolute local JSON Ed25519 public-key trust file")
	arch := flags.String("target-arch", "", "explicit linux target: amd64 or arm64")
	var digest, path, reason string
	var generation uint64
	attempts := 1
	var digests digestList
	switch command {
	case "stage":
		flags.StringVar(&path, "file", "", "local uncompressed signed USTAR bundle")
	case "verify", "initialize", "prepare":
		flags.StringVar(&digest, "digest", "", "full SHA256 manifest digest from stage")
	case "prune":
		flags.Var(&digests, "digest", "unused release digest to remove; repeat for multiple releases")
	}
	if command == "prepare" {
		flags.IntVar(&attempts, "max-attempts", 1, "different kernel boots allowed before fallback (1..5)")
	}
	if command == "rollback" {
		flags.StringVar(&reason, "reason", "", "reason for selecting the previous confirmed release")
	}
	needsGeneration := command == "prepare" || command == "cancel" || command == "rollback" || command == "prune"
	if needsGeneration {
		flags.Uint64Var(&generation, "expected-generation", 0, "required generation from a reviewed status response; zero is valid")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional release arguments")
	}
	if *root == "" || *trust == "" || *arch == "" {
		return errors.New("--root, --trust-file and --target-arch are required")
	}
	if !filepath.IsAbs(*root) || filepath.Clean(*root) != *root || *root == "/" {
		return errors.New("--root must be a clean absolute non-root path")
	}
	if *arch != "amd64" && *arch != "arm64" {
		return errors.New("--target-arch must be amd64 or arm64")
	}
	if command == "stage" && path == "" {
		return errors.New("stage requires --file")
	}
	if (command == "verify" || command == "initialize" || command == "prepare") && digest == "" {
		return errors.New(command + " requires --digest")
	}
	if command == "rollback" && strings.TrimSpace(reason) == "" {
		return errors.New("rollback requires --reason")
	}
	if needsGeneration {
		provided := false
		flags.Visit(func(f *flag.Flag) { provided = provided || f.Name == "expected-generation" })
		if !provided {
			return errors.New("--expected-generation must be explicitly provided")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	keys, err := release.LoadTrustFile(*trust)
	if err != nil {
		return fmt.Errorf("load release trust: %w", err)
	}
	if command != "stage" {
		// Observation and state-changing commands do not create a new store
		// because an operator mistyped its root. stage is the creation entry.
		if _, err := os.Lstat(filepath.Join(*root, "current")); err != nil {
			return fmt.Errorf("release store is not initialized by stage: %w", err)
		}
	}
	store, err := release.Open(release.Options{Root: *root, TrustedKeys: keys, TargetOS: "linux", TargetArch: *arch})
	if err != nil {
		return err
	}
	defer store.Close()
	var result any
	switch command {
	case "status":
		result, err = store.Status(ctx)
	case "stage":
		result, err = store.StageFile(ctx, path)
	case "verify":
		result, err = store.Lookup(ctx, digest)
	case "initialize", "prepare":
		var ref release.Ref
		ref, err = store.Lookup(ctx, digest)
		if err == nil {
			if command == "initialize" {
				result, err = store.Initialize(ctx, ref)
			} else {
				result, err = store.Prepare(ctx, ref, generation, release.PrepareOptions{MaxAttempts: attempts})
			}
		}
	case "cancel":
		result, err = store.CancelPending(ctx, generation)
	case "rollback":
		result, err = store.Rollback(ctx, generation, reason)
	case "prune":
		result, err = store.Prune(ctx, generation, digests)
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(result); err != nil {
		return fmt.Errorf("release operation completed, but writing its result failed; read status before retrying: %w", err)
	}
	return nil
}
