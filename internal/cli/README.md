# Management CLI and bounded observations

`routerd cli` and the built-in SSH server use the same typed management shell.
There is no POSIX shell escape. The shell supports private candidate editing,
revision-checked commits, confirmed commits, operational queries and completion.

`show event-store` reads fresh global event-journal capacity without changing
configuration or allocating a consumer. Local command
`routerd events export --output FILE` publishes a new private immutable history
archive; `routerd events verify FILE` checks it offline. See the [storage contract](../../docs/event-storage.md)
for warning thresholds, atomic publication and remaining retention work.

## Control restart

`restart control` requests supervisor-owned replacement: stop DHCPv6 while the
old control/network can acknowledge withdrawals, then the other dynamic
workers, then control. Network, management and logging remain available. New
dynamic workers wait until old producer identities have retired. Inspect
supervisor `lifecycle` and protocol/application state before checking traffic;
202 acknowledges the request, not completed reacquisition. Abrupt control death
still uses cold recovery and cannot promise an acknowledged graceful Release.

## Appliance power operations

`poweroff` and `reboot` in the local/SSH shell, or `routerd poweroff` and
`routerd reboot` locally, request an explicit power operation from appliance
PID 1. The supervisor accepts `POST /v1/power` with `{"action":"poweroff"}` or
`{"action":"reboot"}` over its private Unix socket. It flushes acceptance,
stops release-readiness monitoring, drains/reaps workers in the normal order,
then the entrypoint syncs and calls the Linux power syscall. A reboot request
is a normal shutdown result, not a runtime failure. It does not confirm a trial.
Host and container supervisors reject these operations; the management
connection ends when its worker stops.

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
routerd config snapshot --expected-revision 12
```

Use the current revision from `routerd config show` for `--expected-revision`.
A commit acknowledgment records the revision. Inspect `show config` and
`show services` for application and protocol state, including required address
or prefix acquisition, before checking client traffic after a change.
`config snapshot` captures an immutable confirmed revision without changing the
running configuration. Its store/revision/digest reference is a prerequisite
for explicit startup selection, which is not yet wired into boot. See
[the persistent configuration contract](../../docs/startup-config.md).
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
separately through `show services` and `show services NAME`. In operational
mode, `show resources` reports each resource's active/readiness conditions,
blocking reason and prerequisite together with the operator revision and
projection generation. It follows bounded pages under a single observation
digest and refuses mixed snapshots. In candidate mode, `show resources`
continues to display the candidate's source declarations. A confirmation
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

## Durable observations

`show observations [after CURSOR] [limit 1..16]` reads one bounded page of
durable configuration commit/confirm/rollback history. Defaults are cursor 0
and limit 16. Continue with the returned `next_cursor` while `more` is true.
The cursor survives control-worker replacement within the same journal.
Use the command in either operational or candidate mode; it does not edit the
candidate. Unknown/duplicate filters and invalid bounds fail before an API
request. A failed read prints no partial history. This history is separate from
the lossy `show logs` transport and grants no permission to mutate the kernel.

## Current DerivedEvent status

`show derived RESOURCE_ID` reads one finite current observation through
`GET /v1/derived/{id}`. It works in operational and candidate modes, supports
IDs containing `/`, and never edits the candidate. Invalid IDs/arguments fail
before any request; an unsuccessful read prints no partial output. Unknown or
inactive resources expose no asserted boolean. Historical last-transition
times are not proof of current Up, and there is no journal/file fallback when
the current evaluator is unavailable. See the
[resource contract](../../docs/resource-program.md#derivedevent).

## Saved configuration capture and restore

The SSH exec command `snapshot` takes a `control.SnapshotRequest` as JSON/YAML
on stdin and returns an immutable `config.SnapshotRef`. The exec command
`restore` takes `control.RestoreRequest` with that `snapshot`, the current
`expected_revision`, a stable `request_id` and optional
`confirm_timeout_seconds` (5..3600). `confirm` accepts the restored candidate;
without confirmation it reverts to the configuration that was active before
the restore. These two commands require exec input, not the interactive
candidate editor.

The local commands provide the same flow:

```
routerd config snapshot --expected-revision 7 > saved-reference.json
routerd config restore --snapshot saved-reference.json --expected-revision 9 --request-id recover-lan --confirmed 60
routerd config confirm
```

A saved reference identifies immutable content in the same configuration store;
it is not the configuration file itself. A restore commits saved intent as a
new revision. On timeout, retain and reuse the same request ID and parameters;
the command never generates a new mutation to retry an uncertain response.
Local restore generates an ID only when none is supplied, as `config apply`
does, and includes it in any failure message. Snapshot files and their existing
validation/ownership remain under the configuration store.
