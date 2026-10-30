# Operational event retention acceptance

Completed: 2026-10-30T05:19:57.506713+00:00. Base: `ba46e407`; release ID: `retention-ba46e407`.
This acceptance used the isolated retention branch's built product binaries.

- Actual configuration commits produced operational events. An EventRule consumed
  each commit and produced its own observation, including during archive failure.
- The count target reclaimed old history to three records. The body-byte and age
  targets reclaimed all records except actual current-state/work references and
  the latest source anchor needed by a new stateful consumer.
- An independent UDP receiver decoded original event JSON and matched its identity
  and content to records read from the product before deletion.
- Stopping the receiver and making only its log address unreachable produced a real
  UDP submission error. Repeated status reads retained the alarm. Old candidates
  remained; new commits, rule consumption and routed UDP continued.
- Restoring the receiver path archived the retained candidates and cleared the alarm.
- Restarting the control worker preserved the committed policy, accepted new events,
  and resumed reclamation without restoring deleted history.
- A warning destination filtered INFO archive notifications while allowing eligible
  history to expire, preserving the prior LogRetention severity semantics.
- Independent client UDP: 341 replies, zero failures throughout this run.

Limited checks passed with `umask 022; go test -race` for internal/events,
internal/control, internal/legacyconfig, internal/resourceprogram and
internal/protocol/syslog.

The two focused event-storage checks also cover pending/unread inputs, a current
consumer's time-triggered work after restart, a newly registered consumer's immediate
time-triggered work, and concurrent append while before-delete delivery fails.

Reproduce: `make check-retention`. The script creates its own private namespaces,
receivers and state directory, then removes that directory on success. It does not
modify production, the shared VM or other running acceptance environments.

## Scope

Implemented storage is operational event history only. The deployed LogRetention
request also names accessLogs, pluginRuns, dnsQueries, trafficFlows, firewallEvents
and dhcpFingerprints. Import rejects those unsupported datasets atomically instead
of silently reporting a partial migration as complete. Diagnostic log rotation and
current protocol state are separate.

Retention limits are reclamation targets, not physical file quotas. Current source
references and the latest registration anchor remain. New stores support bounded
incremental vacuum; old disposable prototype stores without that mode report an
explicit error when vacuum is requested. No live full-database rewrite is added.
UDP submission is not reliable delivery: a silent receiver loss alone is not
observable, and archive retries can duplicate an already submitted notification.
This run qualifies Linux namespace behavior, not physical homert appliance acceptance.

## Built binary SHA-256

- `routerd`: `86d246530e198e20b2d80f9d6f733e77fbdfd2b1e6312ba2c95529b31f05714d`
- `routerd-modules`: `c86ad2117857e77e5d9d45e9a61f527fe33598bdc91bd2082d96a2ed2611caa7`
- `routerd-logging`: `eb20578ca36aea1b5bd4a297b5e59257d9fb5c6f8fb2a6434f17e65064a2ab6f`
