# Traffic report reliability

## Child authentication identities

64-bit nodes advertise `X-Mi-Node-Child-Rate: 1` on panel HTTP requests,
including bandwidth requests and WebSocket handshakes. 32-bit nodes do not
advertise this capability. Existing accounts retain their original ID and UUID.
The panel may additionally send `{id: 1099511627776 + DB_identity_id, uuid,
speed_limit, device_limit, billing_user_id, billing_node_id}`; IDs must remain
below 2^53 and require a 64-bit runtime. This is an allocated ID range, not a
bit encoding of an account or node.

Traffic keys and access entry user IDs remain authentication IDs, including
bytes written after user deletion or UUID rotation. The panel resolves the
identity to its billing account and child node. No child traffic payload,
report field, multiplier, or spool format is added.

`billing_user_id` (falling back to the authentication ID) groups token buckets,
device admission, alive IPs, online counts, device reports and user metrics.
IP sets are merged per account. If alias limits disagree, the strictest positive
speed/device limit wins; zero means unlimited only when no alias sets a limit.
Device sync from the panel uses real account IDs. `billing_node_id` is preserved
as metadata and participates in user change detection.

A remove delta naming a real account expands to its authentication aliases;
a delta naming a child authentication ID removes only that identity. Sing-box
can close tracked TCP connections for a parent UUID across its account aliases.
UDP force-close and Xray connection termination retain their existing kernel
limitations; Xray device admission retains its existing TCP-only IP tracking.

## Delivery and recovery

The node sends an optional top-level `report_id` string to
`POST /api/v2/server/report`. Each batch gets a random 128-bit ID. Retries
retain the original ID and payload, while newly sampled bytes remain pending
for a separate batch. Only HTTP 200 with JSON `{"data":true}` acknowledges a batch.

Deploy the panel's transactional deduplication by `(node_id, report_id)` before
these nodes. Legacy panels may accept and ignore the optional field; that does
not prevent duplicate billing when a successful response is lost. Panel-side
transactional deduplication is not implemented by this node repository.

Both kernels expose service-lifetime cumulative counters, including across
Start/Reload/Stop. Tracker baselines therefore remain valid and pending bytes
are never reset at an instance replacement. Xray tracks removed-user counters
until the instance is closed and samples after draining it; replacement may
wait up to the existing five-second drain interval. SingBox shares counters
with draining instances and waits for their recycling during Stop.

Shutdown blocks new reports, waits for the in-flight worker, stops the kernel,
samples final counters, retries any older batch, then reports the final batch.
Reports use an independent shutdown context with a 90-second deadline rather
than the cancelled service context. Normal HTTP attempts have a 30-second
context. Service failures propagate through machine mode and the executable,
including configuration reload; an unsuccessful final report is not a clean
shutdown.

Before sending, accounting batches are written atomically to a checksummed
`report-<identity>.json` file in `kernel.config_dir`. The identity hashes the
panel URL and node ID. Only report IDs and traffic bytes are saved; credentials,
IP addresses and runtime metrics are excluded. ACK processing atomically
updates the queue; if that update fails, the same ID is replayed safely.

Startup validates and restores pending batches before sampling. Corrupt or
mismatched files are preserved and startup fails explicitly. Shutdown persists
new tail bytes as a separate batch before retrying older reports, so a network
failure can be recovered on the next start. Do not delete pending spool files.

The executable allows 120 seconds for shutdown and the installed systemd unit
allows 130 seconds. Existing units need the updated timeout applied separately.
Hard termination can still lose bytes sampled after the last durable batch;
persistent disk errors prevent durable recovery and return an explicit error.
