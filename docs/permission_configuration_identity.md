# Permission configuration identity

ACP capability report schema v2 now includes optional `client_capabilities_hash`.
It hashes the canonical initialization protocol version and actual
`clientCapabilities`, excluding `clientInfo`. Both pooled slots and persistent
processes forward the hash through runtime snapshots.

`client_profile_hash` still hashes the entire initialize request for diagnostics.
It changes when the paxd version changes. It must not be used as the client
compatibility component of a new permission configuration fingerprint.

Manager combines the capability hash with the adapter/runtime implementation
fingerprint, command fingerprint, worker initialize-result hash, and protocol
version. The resulting `config-v1:` key allows observations to survive a daemon
version-only upgrade. Changed capabilities or agent implementations remain
separate. Harness identity is included when the adapter or existing runtime
probe reports it; this change does not add a Claude harness version probe.

Older Managers ignore the new field. Older daemons omit it and retain the old
exact-match behavior, with Manager's legacy display fallback. Manager and paxd
must both be updated for stable cache reuse across future daemon upgrades.
