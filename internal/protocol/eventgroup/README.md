# Event group transport

`EventGroup` resolves static delivery peers from `SAMNodeSet`. Explicit
`EventPeer` entries override peers by node name. The native service uses the
existing federation event and HMAC wire protocol; it does not run conditions,
create mobility ownership, or modify forwarding.

The control process owns the existing journal. Local observation producers use
`POST /v1/event-groups/{service}/emit`; transport authenticates remote events
before delivering them to `/receive`. The original event identity, payload,
subject and expiry are retained. A repeated wire ID does not replace its first
accepted record. Received events enter the existing EventRule input path.

Each peer has durable work in the existing journal outbox. Only facts emitted
by this node are sent. A failed peer has its own delivery loop and cannot block
other peers. Successful receipt completes that work; restart resumes pending
work. Expired facts are not replayed. Configured age/count retention ends
federation delivery for discarded facts, then uses the existing history deletion
and compaction paths. Other journal consumers remain protected. Removing a peer
retires its cursor and pending delivery work in the existing maintenance loop.
No additional history database is opened.

## Communication check

Build the independent `cmd/routerd-eventd` executable from routerd and run:

```sh
make check-event-group OLD_EVENTD=/absolute/path/to/routerd-eventd
```

The check starts the native service and control handler on isolated loopback
sockets with a private journal, plus the unchanged old executable and its own
private store. It covers signed exchange, rule input, duplicate handling, stale
signature rejection, an unreachable second peer, restart recovery, expiry,
retention, recovery at a two-job capacity limit, peer removal and readdition
concurrent with retirement. The old peer retains timestamps
at whole-second precision. Temporary processes, credentials and stores are
removed by the test. No host network configuration is changed.

This check does not qualify physical routers or implement MobilityPool/network
application. Full product integration remains a separate acceptance step.
