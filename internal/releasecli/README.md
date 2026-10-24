# Local signed-release commands

`Run(ctx, args, stdout, stderr) error` implements arguments following
`routerd release`. Successful results are JSON. Errors are returned to the
normal command error handler. The CLI only reads local bundles and a local
public-key trust file; it has no shell, download, signing, boot or health
confirmation command.

Every command requires explicit `--root`, `--trust-file` and
`--target-arch amd64|arm64`. The target OS is Linux.

| Command | Additional arguments | Effect |
| --- | --- | --- |
| `stage` | `--file /absolute/path/release.tar` | Verify and install the signed pair. This is the store creation entry. |
| `verify` | `--digest SHA256` | Re-verify a previously installed pair and return its reference. |
| `status` | None | Return verified selection and attempt state. |
| `initialize` | `--digest SHA256` | Register an installer-approved baseline, only in an empty store. |
| `prepare` | `--digest SHA256 --expected-generation N [--max-attempts 1..5]` | Queue a strictly newer publisher sequence for the next boot. |
| `cancel` | `--expected-generation N` | Cancel a pending release that has not started a boot trial. |
| `rollback` | `--expected-generation N --reason TEXT` | Select the previously confirmed pair. |
| `prune` | `--expected-generation N [--digest SHA256 ...]` | Remove old generations, incomplete transactions and the named unselected pairs. |

Use the full manifest digest returned by `stage`. Use the current generation
returned by `status` for an intended state change. Omitting the generation is an
error, even when its value would be zero. A stale value is rejected rather than
applied to a newer selection. Initializing a baseline is an explicit installer
declaration that the pair is already known good; it is not a health check.

The trust file is a JSON object from full SHA-256 public-key IDs to canonical
padded base64 Ed25519 public keys. The shared release trust loader verifies the
file's ownership, path, size and content. Trust is not read from the bundle or
an environment variable. The release README describes this contract.

```sh
routerd release stage \
  --root /var/lib/routerd/releases \
  --trust-file /etc/routerd/release-keys.json --target-arch amd64 \
  --file /var/lib/routerd/incoming/routerd-release.tar

routerd release status \
  --root /var/lib/routerd/releases \
  --trust-file /etc/routerd/release-keys.json --target-arch amd64

routerd release prepare \
  --root /var/lib/routerd/releases \
  --trust-file /etc/routerd/release-keys.json --target-arch amd64 \
  --digest "<manifest-sha256-from-stage>" --expected-generation 3
```

Preparing or rolling back changes durable selection. It does not replace
running processes or trigger a reboot. Appliance boot integration selects and
executes the pair and confirms health from its actual supervised services.
There is intentionally no public `confirm` shortcut.

Status and state-changing commands other than stage require an existing store
selector; a mistyped status path does not create a store. If writing JSON output
fails after a successful operation, the error explicitly says that the
operation completed and directs the caller to read status before retrying.

Tests exercise the commands against real signed executable fixtures, generation
fencing, anti-downgrade state, cancellation, rollback, pruning, rejected flags,
required local trust and output failure after mutation.

```sh
go test -race ./internal/releasecli
```
