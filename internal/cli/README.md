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

### Find saved configurations after losing a reference file

`show snapshots` through SSH or the local management shell, and local
`routerd config snapshots`, read the same `GET /v1/config/snapshots` result.
`snapshots` contains verified references from this configuration store, newest
revision first. Pass the chosen reference to the existing restore command; the
current expected revision and request ID still fence the actual change. No
configuration bodies or credentials are returned in the listing.

`unreadable` reports saved files that could not be verified without hiding other
usable recovery references. Discovery changes neither the saved files nor the
current configuration. An empty new store has an empty list. There is no
additional snapshot catalog, mutable cache or automatic retention policy.

## DNS query history

`show dns-queries NAME` reads the existing resolver history through the control
API in SSH exec and the local/SSH interactive shell, including candidate mode.
The service name accepts either `dns` or `service.dns`. Optional key/value
filters are `client`, `name`, `response`, `since`, `until` and `limit`.
Client addresses, RFC3339 timestamps and limits are validated by the existing
history API. The default limit is 100; the maximum is 1000. `name` retains the
history owner's SQL LIKE pattern semantics. Use an exact name when no wildcard
is wanted. Output preserves `service`, `read_at`, `limit`, `truncated` and the
original records, including their time, client, question, response and duration.
A truncated page is not a complete history. Disabled or unavailable storage is
an error, not an empty successful result. This command adds no collection,
retention, direct database access or configuration change.

### DNS history and candidate-mode operations acceptance (2026-10-30)

Product `4e14466`, release `wsl-dns-cli-4e14466`, passed the existing management
integration on independent WSL Linux at 23:34:02-23:34:29 JST (19.61 seconds in
the isolated fixture, 26.039 seconds including preparation). Independent UDP
and TCP DNS queries produced actual stored rows. SSH history filtering kept
original IDs, timestamps, question/response values and truncation metadata;
the existing API normalized mapped IPv4 client addresses. An invalid client
filter failed without successful output. The local shell read the same history.
Reading DNS history inside candidate mode preserved the subsequent commit.
`configure` followed by `show snapshots` and `abort` exposed the saved reference
without changing the candidate. The existing restore, confirmation expiry,
control restart and logging restart checks also passed.

Two earlier fixture failures were corrected without changing these binaries:
the first supplied a database path instead of letting the product own it; the
second assumed a trailing dot even though the reused logstore normalizes saved
question names. The successful run uses the original API record and DNS name
meaning. No timeout or result condition was relaxed. Repeat through
`make build` and `scripts/daemon-tests.py management --test TestDaemonConfigurationAndRecovery`
using the built product. Detailed temporary logs were removed after integration;
the reusable fixture preserves these checks. This is isolated management/DNS acceptance, not
physical-router, full-homert or two-router qualification.

## Recover signed update storage capacity

SSH exec `update prune` accepts `control.ReleasePruneRequest` on stdin:

```json
{"expected_generation":7,"release_digests":[]}
```

Read the current generation with `update status`. An empty digest list removes
only superseded journal generations and incomplete private transactions. To
remove an unused runtime, explicitly include its full digest returned by
`update stage`; the product never selects old runtimes for automatic eviction.
The existing release store rejects a stale generation and protects the active,
pending and previous runtimes before removing anything. The command returns
the store's `removed_generations` and `removed_releases` lists. It neither
changes the selected runtime/configuration nor resets the rollback watermarks.
As with the store's existing operation, an I/O failure can follow partial cleanup;
it is an error, not a successful result. No new deletion owner or policy is added.

`make check-update-rollback` exercises actual SSH, the control Unix socket,
signed product artifacts and target configuration validation. It fills a bounded
runtime store, refuses stale/protected cleanup, removes an explicitly unused
runtime and stages the next signed runtime without changing current selection.
The fixture arranges initial selection through the store. It does not establish
kernel reboot, automatic trial confirmation or packet forwarding.

On 2026-10-31 00:03:56-00:04:01 JST, product `f93be4c`, release
`wsl-operations-f93be4c`, and fixture `bcd58c6` passed this real SSH/control
integration on independent WSL Linux. The run took 5.453 seconds including
preparation (2.87 seconds in the fixture). Stale generation, active, pending
and previous runtime deletion were refused. Explicit unused-runtime cleanup
made room for the next signed runtime while selection and rollback watermarks
remained unchanged. The same fixture compared real HTTP update views against
confirmed, pending, cancelled, trial and failed-trial states. An initial
fixture-only expectation of an absent separator space was corrected; product
behavior and deadlines were unchanged.

## ファイアウォール履歴の照会

`show firewall-events NAME` で、既存の保存済みパケット記録を照会できます。
SSH接続の単発コマンドと、ローカル・SSH接続の対話操作で利用できます。設定の候補を編集中でも読み取れます。
サービス名は `events` と `service.events` の両方を受け付けます。

```text
show firewall-events events source 192.0.2.10 action drop limit 50
show firewall-events service.events since 2026-10-31T00:00:00Z limit 100
```

絞り込みは `source`、`action`、`since`、`limit` です。
IPアドレス、RFC3339形式の開始日時、取得件数の検査は既存の履歴APIが担当します。
標準では100件、最大1000件です。出力には元の記録と `service`、`read_at`、`limit`、`at_limit` が含まれます。
`at_limit` は指定件数へ達したことを示し、続きの存在や全履歴の取得を保証しません。
記録が利用できない場合はエラーとして返し、空の履歴として扱いません。
このコマンドは収集、保存、期限管理、設定変更を行いません。

2026年10月31日10:59〜11:00 JST、専用版 `firewall-cli-native103` で、既存の独立端末試験が成功しました。
基点は `b28ee41` で、今回のCLI接続と受入変更を加えた版です。実行物の構築を含む所要時間は31.936秒でした。
実際のIPv4・IPv6パケットから保存された記録をSSHで読み、元の全項目、上限到達表示、絞り込み結果を照合しました。
製品再起動後の再接続、無効な入力の失敗、候補編集中の照会、中止後の確定済み設定不変も確認しました。
既存試験の保持期間8秒、日次巡回を1秒へ短縮する条件は維持しています。
本番の保持期間を経過させた試験や、対象機器の受入を示すものではありません。

```sh
python3 scripts/daemon-tests.py firewall --test TestDaemonFirewallPacketLogs --go /usr/bin/go --binary-dir dist/bin --output dist/firewall-cli-acceptance
```
