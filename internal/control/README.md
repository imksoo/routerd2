# Bounded local management APIs

The control worker serves HTTP/JSON over its private Unix socket. The network
worker has a separate socket and owns native kernel effects. The general local
API request/response limit remains **2 MiB**. A successful mutation must remain
acknowledgeable even when its full observation is larger than that limit.

## Configuration and health

| Endpoint | Response |
| --- | --- |
| `GET /v1/config` | The existing complete `State` for small configurations. An oversized state returns **413**, with the metadata/document alternatives named in the error. |
| `GET /v1/config/metadata` | Revision, request identity, health, confirmation metadata, document digest/size and explicit omission flags. |
| `GET /v1/config/document?revision=N` | Exactly one compact current `config.Document`. `N` is mandatory and positive. A different current revision returns **409**. |
| `POST /v1/config/commit` | Existing complete `State` when it fits; otherwise a successful **200** metadata acknowledgement for the committed candidate. |
| `POST /v1/config/confirm` | The same bounded success rule. The returned current confirmation is absent after success. |
| `GET /v1/health` | Lightweight revision/health/epoch/sequence, confirmation identity and service counts. Healthy is **200**; unhealthy is **503**. |

Metadata uses `config_omitted`, `confirmation_previous_omitted` and
`services_omitted`. Missing document bodies must never be interpreted as an
empty candidate or an empty rollback configuration. `services` in metadata is
a health summary; the service endpoints remain the source for instance detail.
Long diagnostic text has a `last_error_truncated` flag.

`config_digest` is the hexadecimal SHA-256 of `json.Marshal(config.Document)`;
`config_bytes` is the exact length of that compact JSON. This digest is distinct
from `last_request_hash`, which also covers commit options. A reader obtains
metadata, fetches its revision-fenced document, and checks the size and digest.
It retries a revision conflict a bounded number of times instead of mixing
metadata and a different candidate. A successful large commit acknowledgement
can be checked against the caller's candidate without fetching the document
again. The CLI implements these checks and preserves omission information for
rollback/service detail it has not fetched.

Health does not construct the complete control State, encode protocol snapshots,
or copy route issue details. Its field names remain compatible with the boot
watcher: `revision`, `healthy`, `last_error`, `producer_epoch`, `sequence`,
`confirmation`, and `services.{revision,phase,route_issue_count}`. Service health
also carries `instance_count`. The complete record's two documents and service
observations therefore cannot overflow a health response.

## Network summaries

`GET /v1/services/resources` exposes the coordinator's selected resource
projection in ID order, 128 resources per page. Each item has `id` and
`state.{active,ready,when,reason,dependency}`; optional reason/dependency explain
false or indeterminate guards, readiness and prerequisites. Truth values are
0 (indeterminate), 1 (false), 2 (true). These are policy conditions, not an
independent kernel acknowledgement. The page includes runtime `phase`, bounded
`last_error` with `last_error_truncated`, operator `revision`,
`projection_generation`, `total`, `offset`, `observation_id` and optional
`next_offset`. Empty/static programs are supported.

Continuations require `offset=N&observation_id=DIGEST`. The digest covers the
whole sorted state and phase, including readiness changes that leave the
network generation unchanged. A changed observation returns 409; invalid,
unknown or duplicate query parameters return 400. No source payloads,
provenance or protocol snapshots are exposed. `show resources` assembles the
pages before writing output and rejects mixed observations.

The network worker's `GET /v1/snapshot` and control's `GET /v1/network` return
`network.ObservationView`. Small complete observations retain the existing
Snapshot fields. Every response additionally supplies:

- `observed_at`: the observation completion time;
- `projection_id`: a digest of committed/attempted intent, revisions and apply
  status;
- `observation_complete`: false if a kernel read failed or any section is a
  bounded preview;
- `counts`: complete counts for the successfully observed, scoped collections;
- `truncated` and `truncation`: explicit indications of omitted or previewed
  desired state, interfaces, addresses, routes, next hops, policy rules,
  WireGuard devices/peers/allowed IPs, or diagnostics.

Encoded network observations are capped at **256 KiB**, including JSON escaping.
Large summaries show previews and exact observed counts; they do not claim
their collections are complete. Observation errors describe failed reads, so a
count from an incomplete observation does not prove the kernel has no further
entries. Legacy internal `NetworkClient.Snapshot` rejects an incomplete view
instead of silently returning its preview as a complete snapshot.

Interface and default route-table scope follows the current/attempted desired
state and ownership journal. An explicit route table or address interface detail
query can inspect a different scope. Observation never adopts or mutates it.
Route ownership candidates are indexed by normalized destination, table and
metric, then checked with the same exact ownership predicate used by the
effector. This avoids scanning the entire managed FIB for every observed route.

## Network detail pages

The control endpoint is `GET /v1/network/{section}`. Its network-worker equivalent
is `GET /v1/observations/{section}`.

| Section | Required filters | Optional section filters | Item details |
| --- | --- | --- | --- |
| `interfaces` | — | — | Interface state with `address_count`, an address preview and `addresses_truncated`. |
| `addresses` | `interface` | — | Complete individual address states, ownership, flags and lifetimes. |
| `routes` | — | `table` | Route state with stable `route_id`, `next_hop_count`, a next-hop preview and `next_hops_truncated`. |
| `route-next-hops` | `route_id` | `table` | Individual next hops of the identified route. |
| `policy-rules` | — | — | Individual policy rule states and ownership. |
| `wireguard` | — | — | Device state and `peer_count`; nonempty peer collections are explicitly omitted with `peers_truncated`. |
| `wireguard-peers` | `interface` | — | Public peer state with `allowed_ip_count`, an allowed-IP preview and `allowed_ips_truncated`. |
| `wireguard-allowed-ips` | `interface`, `public_key` | — | Individual prefixes for one public peer key. |

Every section accepts `offset` (default 0), `limit` (default 64, maximum 256),
and optional `projection_id`. `table=0` or no table selects the referenced table
scope of the summary. Each page contains `section`, `total`, `offset`, `items`,
optional `next_offset`, and the revision/status/observation header. Clients use
the returned next offset; the byte cap can produce fewer items than the requested
limit. One unrepresentable individual item returns **413** instead of silently
dropping it. Unknown, duplicate, empty or unrelated query parameters return
**400**. A supplied projection ID mismatch returns **409**; a missing requested
peer/route returns **404**; a kernel observation failure returns **503**.

The full desired projection uses `GET /v1/network/desired` (worker:
`GET /v1/observations/desired`). Here `offset` and `limit` are byte counts, with a
default/maximum chunk size of **64 KiB**. A `DesiredPage` contains `total_bytes`,
`offset`, optional `next_offset`, and `data_base64`, plus the same observation
header. Reassembly is bounded at **2 MiB** and must keep the first projection ID
for later requests. Decode the complete reassembled JSON only after validating
all offsets, lengths and the final total.

Each detail request performs a fresh kernel observation. The projection ID
fences configuration and pending intent changes; it does **not** freeze DAD,
address/route expiry, WireGuard roaming, handshakes, counters or other kernel
changes. Page totals and ordering can therefore change even with the same ID.
The API provides no atomic all-page snapshot guarantee. Route IDs exclude the
remaining expiration time and ownership bit; they identify routing entries,
not authorization or immutable kernel state.

## Minimal protocol dependencies

AFTR obtains only its source dependency through
`GET /v1/source?interface=NAME&address=NUMERIC_ADDRESS` on the network worker.
`NetworkClient.SourceAddress` forwards that native query; there is no full
Snapshot fallback. The result identifies the requested interface/address,
interface presence/index/up state, and at most one matching address state.

The worker reads that interface's native address state and checks ownership only
for matching source candidates. It does not read unrelated routes, policy rules,
WireGuard devices, firewall state, or the full desired projection. This response
intentionally has no projection digest: AFTR already uses its own configuration
digest and dependency generation. Source use requires an exact matching,
present, UP interface and owned address with positive preferred/valid lifetimes,
without tentative, DAD-failed or deprecated flags. A failed source query
withdraws the dependency and fences stale AFTR results; unrelated large operator
observations cannot withdraw it through a transport-size failure.

All these API changes are pure observation or response shaping. They do not
raise the global message limit, modify foreign state, change WireGuard key
ownership, or introduce shell commands.
