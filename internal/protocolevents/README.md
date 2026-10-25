# Health history delivery

Health probing and current finite observations continue independently of history
storage and delivery. A dedicated recorder owns the per-service SQLite store,
enqueue and replay. The probe loop offers events through a bounded 16-entry
channel without waiting for disk or the history receiver.

The persistent queue holds at most 64 observations and 64 KiB. Committed entries
keep their original timestamp, content and random stable ID until acknowledged.
A page contains at most 16 entries; lost acknowledgements retry the same IDs,
and the target journal deduplicates them. An acknowledged prefix is removed
without removing later events.

A full channel, full persistent queue or failed enqueue can lose an uncommitted
observation. `history_dropped` in the current Health snapshot counts detected
losses during that worker's producer epoch; `history_error` reports the recorder
or delivery failure. The count is not durable across worker replacement.
A process crash or shutdown can also lose events still in the volatile channel.
The recorder retries opening an unavailable store and sending pending history
every 200 ms; each send has a one-second deadline. These operations never stop
the probe loop. This is bounded history with visible loss, not guaranteed
recording of every completed probe.

Control durably registers each per-service queue before starting its worker.
`POST /v1/protocol-events` accepts `{resource, events}` for a registered queue,
independently of the current worker generation or observation. The private Unix
socket supplies the existing same-UID trust boundary. Resource/topic/detail and
the entire page are validated before writes. Historical delivery cannot renew
current facts or authorize kernel effects.

Control can recover registered queues after their workers stop, using exclusive
store ownership and the runtime activation fence. A retired registration is
removed only after its queue is empty. Missing queues are errors. Target journal
storage failures are visible in control health and prevent trial confirmation.
Other protocol history producers, long-running retention and physical storage
qualification remain unfinished.
