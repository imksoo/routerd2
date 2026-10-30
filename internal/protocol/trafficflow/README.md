# Connection traffic history

The bundled `traffic_flow` worker reads the current kernel connection table with
existing Go netlink code. It reuses routerd `pkg/logstore.FlowKey`,
the `SyncActive` transaction/schema and retention processing.
The sample transaction uses one JSON array for missing-connection detection so
large tables cannot exceed SQLite's per-statement parameter count. It adds no external
conntrack command or classifier daemon. The private database is below
`state/services/<name>/traffic-flows.db`; operator configuration cannot choose a
runtime path or adopt an old database.

A complete sample preserves the original/reply tuple, NAT source address,
cumulative byte/packet counters and first observation time. Connections absent
from that complete sample receive an end time. A failed or interrupted dump
changes no history. Sampling defaults to the old observer's 30 seconds; short
connections between samples can be missed. Kernel conntrack accounting must be
explicitly enabled by the network setting owner before new connections start.
Entries without counters are marked `accounting: false`, not measured zero-byte
traffic. This worker never modifies forwarding or shared kernel settings.

Collection, storage and expiry belong to this one worker. Disk failure is logged
and the next sample attempts storage again; forwarding and management requests
have no dependency on the history database. Retention uses the configured age,
daily sweep by default, optional before-delete UDP syslog and vacuum. An archive
error preserves expired rows. UDP submission is not receiver acknowledgement.
Cancellation closes the reader and database through the normal worker lifetime.

When `packet_group` is nonzero, this worker also binds that NFLOG group and
passes copied packets directly to routerd `pkg/dpi.Classify`. The network owner
selects the same `flow_observation_group`: its post-filter FORWARD chain copies
the first nine new/established packets, at most 2048 bytes each, as in routerd.
The observer never grants forwarding and disappears with that firewall setting.
A group cannot also be owned by a firewall-log service or used by its rule logs.
Only payload-derived results enter history; a known port is not classification
evidence. The engine/source columns report `builtin`/`builtin-payload` for Go extraction, or `ndpi`/`libndpi` for a native nDPI result.
Original/reply conntrack tuples correlate metadata across source/destination NAT.
A later counter-only sample and process restart preserve stored metadata.

The packet reader has a 512-result nonblocking queue. Its owner keeps up to
10000 classified tuples pending the next successful complete history sample;
capacity loss is reported. A failed history write retains those pending results.
This bounded transient handoff is not a second durable flow database. A failed
packet reader returns through the existing worker supervisor for restart; it
never changes forwarding.

`TrafficFlowLog` imports enabled `source: conntrack` (or the old empty-source
default), `includeTLSSNI`, and `includeApplicationLayer`. Both metadata options
use the same packet reader. TLS metadata reuses the old Go parser; application
classification uses routerd's native nDPI C API calls and result conversion in
that reader's goroutine. The module is initialized before readiness and freed
after the reader stops. No classifier agent, HTTP socket, extra daemon or new
forwarding owner is introduced.

The module binary must be built with `CGO_ENABLED=1 -tags libndpi,netgo,osusergo`
and statically linked nDPI and its dependencies. A build without nDPI explicitly
rejects `application_layer` on worker startup; it does not satisfy the request
with built-in classification. Native result metadata includes `libndpi.version`.
Unrecognized native packets may still yield explicitly labelled built-in packet
metadata; port-only fallback remains excluded. nDPI state is shared between
both packet directions. A new TCP SYN discards prior state for reused endpoints,
using nDPI's IP/transport decoding. It reuses routerd's bounded flow tracking, one-hour idle
expiry and oldest-entry eviction, at this collector's 10000-tuple capacity.
Completed classification frees the C flow while retaining its result for later
packets. Unknown C flows are freed at the existing nine-packet observation limit.
The entire state is freed on worker exit.

Both parsers receive the existing bounded packet copies; this is not TLS
decryption or an unbounded stream capture. The old Go parser does not reassemble
fragmented TLS messages. Native classification is limited by nDPI's supported
protocols and the copied packet budget. Saved homert02/03 settings enable both
options. Isolated acceptance does not establish their physical-device acceptance.

`make check-traffic-flow` uses the existing isolated daemon fixture. Two
independent peers exchange TCP and UDP through the product's source NAT. The built-in and native nDPI cases each
check recorded counters, classification provenance and SNI from a real standard-library TLS handshake,
a bad history path, a locked database, recovery, restart, natural UDP conntrack
expiry, retention/UDP archive and collector/observation-chain removal without
interrupting forwarding. The TLS peer uses X25519 so its ClientHello fits the old
single-packet extraction boundary. Static peer addresses, shortened sampling and
maintenance intervals, and private namespace conntrack settings are explicit
fixture conditions, not homert's acceptance result.
