# Connection traffic history

The bundled `traffic_flow` worker reads the current kernel connection table with
existing Go netlink code. It reuses routerd `pkg/logstore.FlowKey`,
the `SyncActive` transaction/schema and retention processing.
The sample transaction uses one JSON array for missing-connection detection so
large tables cannot exceed SQLite's per-statement parameter count. It adds no external
conntrack command, classifier or daemon. The private database is below
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

`TrafficFlowLog` imports only enabled `source: conntrack` (or the old empty-source
default) without application/TLS classification requests. The old
`includeApplicationLayer` and `includeTLSSNI` flags activate real packet sampling
and nDPI helpers, so they remain diagnosed rather than silently discarded. The
saved homert02/03 configurations enable both flags: this module alone does not
complete those configurations or establish physical-router acceptance.

`make check-traffic-flow` uses the existing isolated daemon fixture. Two
independent peers exchange TCP and UDP through the product's source NAT. The
gate checks recorded counters, a bad history path, a locked database, recovery,
restart, natural UDP conntrack expiry, retention/UDP archive and collector
removal without disrupting forwarding. Static peer addresses, shortened
sampling/maintenance intervals and private namespace conntrack settings are
explicit fixture conditions, not homert's acceptance result.
