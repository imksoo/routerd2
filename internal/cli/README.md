# Management CLI and bounded observations

`routerd cli` and the built-in SSH server use the same typed management shell.
There is no POSIX shell escape. The shell supports private candidate editing,
revision-checked commits, confirmed commits, operational queries and completion.

## Configuration editing and larger documents

For interactive edits:

```text
configure
set resources[interface/eth0].interface.mtu 1400
diff
validate
commit confirmed 60
confirm
```

`edit running-config.yaml` accepts a complete YAML replacement ending in `.end`;
`.abort` keeps the candidate unchanged. Typed paths and `edit` contexts also have
Tab completion. Resource lists use `id`; nested named lists use `name`. Slash
and dot characters inside resource selectors are part of that identity.
YAML exports preserve the document's values, not its comments
or original whitespace. Interactive input remains limited to 4 KiB per line
and 100 KiB per candidate/paste. Reading or exporting a larger current document
does not require entering candidate mode.

For larger changes, export and edit a file, then apply it through the same
transaction API:

```sh
routerd config export > running-config.yaml
routerd config validate running-config.yaml
routerd config plan running-config.yaml
routerd config apply --file running-config.yaml \
  --expected-revision 42 --request-id change-2026-001 --confirmed 60
routerd config confirm
```

Use the current revision from `routerd config show` for `--expected-revision`.
File input is bounded to 2 MiB; the document's compact JSON representation must
also leave 4 KiB within that budget for the API envelope. The complete confirmed
commit record has a separate budget because it contains both the new document
and the previous document needed for rollback.

The SSH exec command `configure` accepts a `CommitRequest` YAML/JSON document on
stdin, including `expected_revision`, `request_id`, `config` and optional
`confirm_timeout_seconds`. It uses the file/API size budget, not the interactive
paste budget. A transport error after commit may mean the transaction applied.
Retain the exact candidate, expected revision, confirmation timeout and request
ID and retry the same request. The CLI never creates a new transaction
automatically while recovering an acknowledgment.

### Reading a state larger than the response budget

`show config`, `show running-config`, `routerd config show` and
`routerd config export` first request the existing full state. Only an explicit
HTTP 413 invokes the bounded fallback:

1. Read configuration metadata with the current revision and canonical document
   SHA-256/byte count.
2. Read the current document with that exact revision as a fence.
3. Validate the document, digest and byte count before displaying it.

A revision conflict restarts the metadata/document read, with at most three
attempts. Other failures are reported directly. Configuration reads are
consistent with the returned revision; a concurrent commit can of course make
that revision no longer current after the read completes.

A large successful commit can return a metadata acknowledgment. The CLI checks
its request ID, next revision, digest and byte count against the candidate it
submitted, and reconstructs the current document from those verified bytes.
It does not issue a later GET that might return another administrator's change.
Confirmation can use metadata alone and does not need to download current or
previous document bodies.

`confirmation_previous_omitted`, `services_omitted` and `config_omitted` identify
data not present in an output. The CLI removes omitted zero-value placeholders
from its JSON output. A reassembled current document is present as `config`;
an omitted rollback document remains omitted. Service state is available
separately through `show services` and `show services NAME`. A confirmation
acknowledgment may contain metadata only; `show config` retrieves its document.

## Network observations

`show network` returns a bounded summary. Inspect `counts`, `truncated`,
`truncation`, `observation_complete` and `observation_errors` before treating it
as a full observation. Large desired documents are explicitly omitted from
the summary; `show network desired` reconstructs them through bounded chunks.

Detail commands fetch one page, defaulting to 64 items and permitting 1–256:

| Command | Required and optional section filters |
| --- | --- |
| `show network interfaces` | None |
| `show network addresses` | `interface IF` |
| `show network routes` | Optional `table N` |
| `show network route-next-hops` | `route-id ID`; optional `table N` |
| `show network policy-rules` | None |
| `show network wireguard` | None |
| `show network wireguard-peers` | `interface IF` |
| `show network wireguard-allowed-ips` | `interface IF public-key KEY` |

Every detail command also accepts `offset N`, `limit N` and `projection ID`.
The response contains `total`, `offset`, `items`, `projection_id`, and, when more
data is available, `next_offset` and a ready-to-run `next_command`. That command
preserves filters and fixes the projection ID. For example:

```text
show network routes table 254 limit 64
show network addresses interface eth0 limit 64
show network wireguard-peers interface wg0
```

An interface, route, device or peer can itself have too many child entries for
one page. Its item explicitly reports the child count and truncation. Fetch
addresses, route next hops, WireGuard peers or allowed IPs with the corresponding
detail command. `route_id` comes from a route detail item; `public-key` is the
peer's public key. Private keys are not part of these observations.

`show network desired [projection ID]` downloads the complete desired network
projection in 64 KiB chunks, with a total budget of 2 MiB and at most 32 requests.
Every chunk after the first is pinned to the first projection ID. The CLI checks
revision, total size, offsets and progress, validates the reconstructed document,
and writes output only after the complete read succeeds. A projection change or
malformed page produces an error without partial JSON output.

Projection IDs fence the configured/pending intent, **not an atomic kernel
snapshot**. Kernel addresses, lifetimes, counters and WireGuard state can change
between detail pages while the projection ID remains unchanged. Each page has
its own `observed_at`. Start a new observation when the API reports that the
projection changed. Offset paging is an operator inspection interface, not an
event log or a transactionally consistent dump of all kernel state.

`show network help` lists the complete syntax; Tab completes section and filter
names in operational and candidate modes. Unknown, duplicate or inapplicable
filters are rejected before issuing an API request.
