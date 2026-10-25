# Health history delivery

The Health worker records completed probe events in a bounded durable queue
before publishing the corresponding finite observation. History never renews a
current observation, changes worker identity, or authorizes kernel effects.

Control durably registers the per-service queue before starting the worker.
`POST /v1/protocol-events` accepts `{resource, events}` for a registered queue.
The private Unix socket supplies the existing same-UID trust boundary; topics,
resource identity and the whole bounded page are validated before any writes.
History can come from an earlier boot, configuration or worker generation.
Delivery does not query supervisor readiness or require a current observation.
Those checks belong to the separate current-observation API.

Each event keeps its original content and random stable ID. The worker sends at
most 16 events per page and requires an acknowledgement of those exact IDs.
Lost acknowledgements retry the same events; the target journal deduplicates
them. Removing an acknowledged prefix preserves concurrent appends.

The queue holds at most 64 observations and 64 KiB of encoded state. A full
queue blocks the producer until delivery frees space or shutdown cancels it;
it does not cause a restart loop. While blocked, no new probes are taken and
the last current observation can expire. A crash before a pending observation
is durably enqueued can lose that observation. Permanent storage errors remain
errors. Target journal storage errors are visible in control health and prevent
trial confirmation.

A relay retries delivery every 200 ms with a one-second request timeout,
independently of the current-observation callback. Recovery of queues whose
workers have stopped retains the existing exclusive store ownership and runtime
activation fence. Control drains only registered queues, preserves declared
registrations, and removes a retired registration only after its queue is empty.
Missing queues are errors rather than new empty stores.

The relay is implemented for Health probes. Other protocol history producers,
long-running retention and physical storage qualification remain unfinished.
