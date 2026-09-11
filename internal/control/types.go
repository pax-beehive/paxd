package control

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type SourceKind string

const (
	SourceLocal  SourceKind = "local"
	SourceRemote SourceKind = "remote"
)

type Source struct {
	Kind     SourceKind `json:"kind"`
	RemoteID string     `json:"remote_id,omitempty"`
}

type Service interface {
	HandleCommand(ctx context.Context, src Source, cmd Command) (CommandAck, error)
	HandleQuery(ctx context.Context, src Source, query Query) (QueryResult, error)
}

// DeferredCommandAction is notified only after a command ACK has been written
// successfully to its transport.
type DeferredCommandAction interface {
	ConfirmCommandAckDelivered(commandID string)
}

// PaxdLifecycle schedules process-level actions that must not begin until their
// durable command ACK has been delivered.
type PaxdLifecycle interface {
	BootID() string
	ScheduleRestart(commandID string, command RestartPaxdCommand) error
	ScheduleUpgrade(commandID string, remoteID string, command UpgradePaxdCommand) error
	Cancel(commandID string) error
	ConfirmAckDelivered(commandID string)
}

type ReportService interface {
	BuildRuntimeSnapshot(ctx context.Context, remoteID string, nodeID string) (RuntimeSnapshotReport, error)
}

type SessionRuntimeReportService interface {
	BuildSessionRuntimeSnapshots(ctx context.Context, remoteID string, nodeID string) ([]SessionRuntimeSnapshotReport, error)
	SubscribeSessionRuntime(remoteID string) (<-chan struct{}, func())
}

type SessionRuntimeResetService interface {
	ResetSessionRuntime(ctx context.Context, remoteID string, command ResetSessionRuntimeCommand) (SessionRuntimeResetResult, error)
}

type HostMetricsProvider interface {
	CurrentHostMetrics(ctx context.Context) (*HostMetricsReport, error)
}

type ACPPoolCapabilitySource interface {
	ACPPoolCapabilityReport(ctx context.Context, connectionID string) (*ACPPoolCapabilityReport, bool)
}

type ControlError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Target  string `json:"target,omitempty"`
}

func (e ControlError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	if e.Message == "" {
		return e.Code
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

type CommandType string

const (
	CommandRemoteCreate           CommandType = "remote.create"
	CommandRemoteUpdate           CommandType = "remote.update"
	CommandRemoteDelete           CommandType = "remote.delete"
	CommandRemoteRestart          CommandType = "remote.restart"
	CommandRemoteAuthConfigure    CommandType = "remote_auth.configure"
	CommandRemoteAuthClear        CommandType = "remote_auth.clear"
	CommandAgentConnectionCreate  CommandType = "agent_connection.create"
	CommandAgentConnectionUpdate  CommandType = "agent_connection.update"
	CommandAgentConnectionDelete  CommandType = "agent_connection.delete"
	CommandAgentConnectionRestart CommandType = "agent_connection.restart"
	CommandRestartPaxd            CommandType = "paxd.restart"
	CommandUpgradePaxd            CommandType = "paxd.upgrade"
	CommandCancelPaxdMaintenance  CommandType = "paxd.maintenance.cancel"
	CommandAttachmentEnsureLocal  CommandType = "attachment.ensure_local"
	CommandSessionRuntimeReset    CommandType = "session_runtime.reset"
	CommandSecretChannelPush      CommandType = "secret_channel.push"
)

type CommandStatus string

const (
	CommandStatusUnknown  CommandStatus = "unknown"
	CommandStatusReceived CommandStatus = "received"
	CommandStatusRejected CommandStatus = "rejected"
	CommandStatusApplied  CommandStatus = "applied"
	CommandStatusFailed   CommandStatus = "failed"
)

type Command struct {
	CommandID string      `json:"command_id"`
	Type      CommandType `json:"type"`

	CreateRemote  *CreateRemoteCommand  `json:"create_remote,omitempty"`
	UpdateRemote  *UpdateRemoteCommand  `json:"update_remote,omitempty"`
	DeleteRemote  *DeleteRemoteCommand  `json:"delete_remote,omitempty"`
	RestartRemote *RestartRemoteCommand `json:"restart_remote,omitempty"`

	ConfigureRemoteAuth *ConfigureRemoteAuthCommand `json:"configure_remote_auth,omitempty"`
	ClearRemoteAuth     *ClearRemoteAuthCommand     `json:"clear_remote_auth,omitempty"`

	CreateAgentConnection  *CreateAgentConnectionCommand  `json:"create_agent_connection,omitempty"`
	UpdateAgentConnection  *UpdateAgentConnectionCommand  `json:"update_agent_connection,omitempty"`
	DeleteAgentConnection  *DeleteAgentConnectionCommand  `json:"delete_agent_connection,omitempty"`
	RestartAgentConnection *RestartAgentConnectionCommand `json:"restart_agent_connection,omitempty"`

	RestartPaxd           *RestartPaxdCommand           `json:"restart_paxd,omitempty"`
	UpgradePaxd           *UpgradePaxdCommand           `json:"upgrade_paxd,omitempty"`
	CancelPaxdMaintenance *CancelPaxdMaintenanceCommand `json:"cancel_paxd_maintenance,omitempty"`
	EnsureAttachmentLocal *EnsureAttachmentLocalCommand `json:"ensure_attachment_local,omitempty"`
	ResetSessionRuntime   *ResetSessionRuntimeCommand   `json:"reset_session_runtime,omitempty"`
	PushSecretChannel     *PushSecretChannelCommand     `json:"push_secret_channel,omitempty"`
}

// PushSecretChannelCommand redeems a short-lived secret channel (see
// internal/secretchannel). All byte fields are base64-encoded because a
// Command travels as JSON. paxd never sees the plaintext until it decrypts
// Ciphertext locally with the channel's private key; pax-manager only ever
// relays these bytes.
type PushSecretChannelCommand struct {
	ChannelID       string `json:"channel_id"`
	SenderPublicKey string `json:"sender_public_key"`
	Nonce           string `json:"nonce"`
	Ciphertext      string `json:"ciphertext"`
}

// SecretChannelPushResult is only populated when the push was applied.
// Every other outcome (expired, unauthorized, conflict, bad payload, write
// failure) is reported through CommandAck.Error instead.
type SecretChannelPushResult struct {
	FileRef   string `json:"file_ref"`
	ExpiresAt string `json:"expires_at"`
}

type ResetSessionRuntimeCommand struct {
	AgentID                string `json:"agent_id"`
	ConnectionID           string `json:"connection_id"`
	NativeSessionID        string `json:"native_session_id"`
	ExpectedTurnInstanceID string `json:"expected_turn_instance_id"`
}

type SessionRuntimeResetResult struct {
	Status             string `json:"status"`
	ProjectionRevision uint64 `json:"projection_revision"`
}

type AttachmentDescriptor struct {
	AttachmentID string `json:"attachment_id"`
	Filename     string `json:"filename"`
	ContentType  string `json:"content_type,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
	Generation   int64  `json:"generation,omitempty"`
}

type AttachmentDownloadTicket struct {
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	ExpiresAt string            `json:"expires_at,omitempty"`
}

type EnsureAttachmentLocalCommand struct {
	Attachment AttachmentDescriptor     `json:"attachment"`
	Download   AttachmentDownloadTicket `json:"download"`
}

type AttachmentLocalStateValue string

const (
	AttachmentLocalUnknown     AttachmentLocalStateValue = "unknown"
	AttachmentLocalQueued      AttachmentLocalStateValue = "queued"
	AttachmentLocalDownloading AttachmentLocalStateValue = "downloading"
	AttachmentLocalVerifying   AttachmentLocalStateValue = "verifying"
	AttachmentLocalReady       AttachmentLocalStateValue = "ready"
	AttachmentLocalFailed      AttachmentLocalStateValue = "failed"
	AttachmentLocalPaused      AttachmentLocalStateValue = "paused"
)

type AttachmentLocalState struct {
	AttachmentID    string                    `json:"attachment_id"`
	State           AttachmentLocalStateValue `json:"state"`
	BytesDownloaded int64                     `json:"bytes_downloaded,omitempty"`
	TotalBytes      int64                     `json:"total_bytes,omitempty"`
	SizeBytes       int64                     `json:"size_bytes,omitempty"`
	LocalURI        string                    `json:"local_uri,omitempty"`
	SHA256          string                    `json:"sha256,omitempty"`
	ErrorCode       string                    `json:"error_code,omitempty"`
	ErrorMessage    string                    `json:"error_message,omitempty"`
}

type AttachmentLocalizer interface {
	Ensure(ctx context.Context, source Source, command EnsureAttachmentLocalCommand) error
	Status(ctx context.Context, source Source, attachmentIDs []string) ([]AttachmentLocalState, error)
}

type Remote struct {
	ID              string `json:"id,omitempty"`
	Name            string `json:"name"`
	CloudAPIURL     string `json:"cloud_api_url"`
	NodeControlPath string `json:"node_control_path,omitempty"`
	AgentTunnelPath string `json:"agent_tunnel_path,omitempty"`
	NodeID          string `json:"node_id,omitempty"`
	Enabled         *bool  `json:"enabled,omitempty"`
}

type RemotePatch struct {
	Name            *string `json:"name,omitempty"`
	CloudAPIURL     *string `json:"cloud_api_url,omitempty"`
	NodeControlPath *string `json:"node_control_path,omitempty"`
	AgentTunnelPath *string `json:"agent_tunnel_path,omitempty"`
	NodeID          *string `json:"node_id,omitempty"`
	Enabled         *bool   `json:"enabled,omitempty"`
}

type CreateRemoteCommand struct {
	Remote         Remote `json:"remote"`
	CloudAPIKeyRef string `json:"cloud_api_key_ref,omitempty"`
}

type UpdateRemoteCommand struct {
	RemoteID       string      `json:"remote_id"`
	Remote         RemotePatch `json:"remote,omitempty"`
	CloudAPIKeyRef *string     `json:"cloud_api_key_ref,omitempty"`
}

type DeleteRemoteCommand struct {
	RemoteID                string `json:"remote_id"`
	CascadeAgentConnections bool   `json:"cascade_agent_connections,omitempty"`
}

type RestartRemoteCommand struct {
	RemoteID string `json:"remote_id"`
}

type RemoteAuthKind string

const (
	RemoteAuthNone             RemoteAuthKind = "none"
	RemoteAuthCloudflareAccess RemoteAuthKind = "cloudflare_access"
)

type ConfigureRemoteAuthCommand struct {
	RemoteID         string                `json:"remote_id"`
	Kind             RemoteAuthKind        `json:"kind"`
	CloudflareAccess *CloudflareAccessAuth `json:"cloudflare_access,omitempty"`
}

type CloudflareAccessAuth struct {
	ClientID        string `json:"client_id"`
	ClientSecretRef string `json:"client_secret_ref"`
}

type ClearRemoteAuthCommand struct {
	RemoteID string `json:"remote_id"`
}

type RemoteAuthMaterial struct {
	RemoteID         string
	CloudAPIURL      string
	NodeID           string
	CloudAPIKeyRef   string
	AuthKind         RemoteAuthKind
	CloudflareAccess *CloudflareAccessAuth
}

type DesiredState string

const (
	DesiredStateRunning DesiredState = "running"
	DesiredStateStopped DesiredState = "stopped"
	DesiredStateDeleted DesiredState = "deleted"
)

type CreateAgentConnectionCommand struct {
	ID                  string            `json:"id,omitempty"`
	RemoteID            string            `json:"remote_id"`
	Name                string            `json:"name"`
	CloudAgentID        string            `json:"cloud_agent_id,omitempty"`
	InstanceID          string            `json:"instance_id"`
	AgentType           string            `json:"agent_type"`
	Harness             string            `json:"harness"`
	Command             []string          `json:"command"`
	WorkingDir          string            `json:"working_dir,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
	Enabled             *bool             `json:"enabled,omitempty"`
	DesiredState        DesiredState      `json:"desired_state,omitempty"`
	DesiredSlots        *int              `json:"desired_slots,omitempty"`
	ReportLocalSessions *bool             `json:"report_local_sessions,omitempty"`
}

type UpdateAgentConnectionCommand struct {
	ConnectionID        string             `json:"connection_id"`
	Name                *string            `json:"name,omitempty"`
	CloudAgentID        *string            `json:"cloud_agent_id,omitempty"`
	InstanceID          *string            `json:"instance_id,omitempty"`
	AgentType           *string            `json:"agent_type,omitempty"`
	Harness             *string            `json:"harness,omitempty"`
	Command             *[]string          `json:"command,omitempty"`
	WorkingDir          *string            `json:"working_dir,omitempty"`
	Env                 *map[string]string `json:"env,omitempty"`
	Enabled             *bool              `json:"enabled,omitempty"`
	DesiredState        *DesiredState      `json:"desired_state,omitempty"`
	DesiredSlots        *int               `json:"desired_slots,omitempty"`
	ReportLocalSessions *bool              `json:"report_local_sessions,omitempty"`
}

type DeleteAgentConnectionCommand struct {
	ConnectionID string `json:"connection_id"`
	Deregister   bool   `json:"deregister,omitempty"`
}

type RestartAgentConnectionCommand struct {
	ConnectionID string `json:"connection_id"`
}

type PaxdRestartMode string

const (
	PaxdRestartImmediate PaxdRestartMode = "immediate"
	PaxdRestartWhenIdle  PaxdRestartMode = "when_idle"
)

type RestartPaxdCommand struct {
	Mode                 PaxdRestartMode `json:"mode,omitempty"`
	ShutdownGraceSeconds int             `json:"shutdown_grace_seconds,omitempty"`
	IdleGraceSeconds     int             `json:"idle_grace_seconds,omitempty"`
	DrainTimeoutSeconds  int             `json:"drain_timeout_seconds,omitempty"`
	ForceAtDeadline      bool            `json:"force_at_deadline,omitempty"`
	Reason               string          `json:"reason,omitempty"`
}

type PaxdUpgradeMode string

const (
	PaxdUpgradeImmediate     PaxdUpgradeMode = "immediate"
	PaxdUpgradeWhenIdle      PaxdUpgradeMode = "when_idle"
	PaxdUpgradeOpportunistic PaxdUpgradeMode = "opportunistic"
)

type UpgradePaxdCommand struct {
	Version              string          `json:"version,omitempty"`
	Tag                  string          `json:"tag,omitempty"`
	Mode                 PaxdUpgradeMode `json:"mode,omitempty"`
	ShutdownGraceSeconds int             `json:"shutdown_grace_seconds,omitempty"`
	IdleGraceSeconds     int             `json:"idle_grace_seconds,omitempty"`
	DrainTimeoutSeconds  int             `json:"drain_timeout_seconds,omitempty"`
	ForceAtDeadline      bool            `json:"force_at_deadline,omitempty"`
	Reason               string          `json:"reason,omitempty"`
}

type CancelPaxdMaintenanceCommand struct {
	MaintenanceCommandID string `json:"maintenance_command_id"`
}

type CommandAck struct {
	CommandID         string         `json:"command_id"`
	OK                bool           `json:"ok"`
	Status            CommandStatus  `json:"status"`
	TargetType        string         `json:"target_type,omitempty"`
	TargetID          string         `json:"target_id,omitempty"`
	DesiredGeneration int64          `json:"desired_generation,omitempty"`
	Error             *ControlError  `json:"error,omitempty"`
	Result            *CommandResult `json:"result,omitempty"`
}

type CommandResult struct {
	PaxdRestart         *PaxdRestartResult         `json:"paxd_restart,omitempty"`
	PaxdUpgrade         *PaxdUpgradeResult         `json:"paxd_upgrade,omitempty"`
	Remote              *RemoteView                `json:"remote,omitempty"`
	AgentConnection     *AgentConnectionView       `json:"agent_connection,omitempty"`
	Command             *CommandView               `json:"command,omitempty"`
	SessionRuntimeReset *SessionRuntimeResetResult `json:"session_runtime_reset,omitempty"`
	SecretChannelPush   *SecretChannelPushResult   `json:"secret_channel_push,omitempty"`
}

type PaxdRestartResult struct {
	RequestedBootID string `json:"requested_boot_id"`
	Phase           string `json:"phase,omitempty"`
}

type PaxdUpgradeResult struct {
	RequestedBootID string `json:"requested_boot_id"`
	TargetVersion   string `json:"target_version,omitempty"`
	Phase           string `json:"phase,omitempty"`
	StagedPath      string `json:"staged_path,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
}

type ReportType string

const (
	ReportHeartbeat              ReportType = "heartbeat"
	ReportRuntimeSnapshot        ReportType = "runtime.snapshot"
	ReportSessionRuntimeSnapshot ReportType = "session_runtime.snapshot"
	ReportAttachmentLocalState   ReportType = "attachment.local_state"
)

type Report struct {
	Type     ReportType `json:"type"`
	RemoteID string     `json:"remote_id"`
	NodeID   string     `json:"node_id,omitempty"`
	SentAt   string     `json:"sent_at"`

	Heartbeat              *HeartbeatReport              `json:"heartbeat,omitempty"`
	RuntimeSnapshot        *RuntimeSnapshotReport        `json:"runtime_snapshot,omitempty"`
	SessionRuntimeSnapshot *SessionRuntimeSnapshotReport `json:"session_runtime_snapshot,omitempty"`
	AttachmentLocalState   *AttachmentLocalState         `json:"attachment_local_state,omitempty"`
}

type SessionRuntimeSnapshotReport struct {
	AgentID       string                    `json:"agent_id"`
	ConnectionID  string                    `json:"connection_id,omitempty"`
	Sequence      int64                     `json:"sequence"`
	GeneratedAt   string                    `json:"generated_at"`
	SchemaVersion int                       `json:"schema_version"`
	ActiveTurns   []SessionActiveTurnReport `json:"active_turns"`
}

type SessionActiveTurnReport struct {
	NativeSessionID   string          `json:"native_session_id"`
	TurnInstanceID    string          `json:"turn_instance_id"`
	PromptRequestID   json.RawMessage `json:"prompt_request_id"`
	RuntimeStatus     string          `json:"runtime_status"`
	PendingApprovalID json.RawMessage `json:"pending_approval_id,omitempty"`
	SlotID            string          `json:"slot_id,omitempty"`
	ProcessEpoch      string          `json:"process_epoch,omitempty"`
}

type HeartbeatReport struct {
	BootID      string `json:"boot_id,omitempty"`
	PaxdVersion string `json:"paxd_version,omitempty"`
	DaemonPhase string `json:"daemon_phase,omitempty"`
}

type RuntimeSnapshotReport struct {
	SnapshotID string               `json:"snapshot_id"`
	Host       *HostMetricsReport   `json:"host,omitempty"`
	Agents     []AgentRuntimeReport `json:"agents"`
}

type HostMetricsReport struct {
	MachineName   string  `json:"machine_name,omitempty"`
	OS            string  `json:"os,omitempty"`
	Arch          string  `json:"arch,omitempty"`
	CPUPercent    float64 `json:"cpu_percent,omitempty"`
	MemoryPercent float64 `json:"memory_percent,omitempty"`
	UptimeSeconds int64   `json:"uptime_seconds,omitempty"`
	CollectedAt   string  `json:"collected_at,omitempty"`
}

type AgentRuntimeReport struct {
	ConnectionID            string                   `json:"connection_id"`
	CloudAgentID            string                   `json:"cloud_agent_id,omitempty"`
	RemoteID                string                   `json:"remote_id"`
	NodeID                  string                   `json:"node_id,omitempty"`
	Name                    string                   `json:"name,omitempty"`
	AgentType               string                   `json:"agent_type,omitempty"`
	DesiredState            DesiredState             `json:"desired_state,omitempty"`
	RuntimePhase            string                   `json:"runtime_phase,omitempty"`
	ObservedGeneration      int64                    `json:"observed_generation,omitempty"`
	ObservedRestartNonce    int64                    `json:"observed_restart_nonce,omitempty"`
	StatusUpdatedAt         string                   `json:"status_updated_at,omitempty"`
	FailureClass            string                   `json:"failure_class,omitempty"`
	LastErrorCode           string                   `json:"last_error_code,omitempty"`
	LastErrorMessage        string                   `json:"last_error_message,omitempty"`
	ACPPoolCapabilityReport *ACPPoolCapabilityReport `json:"acp_pool_capability_report,omitempty"`
}

type ACPPoolCapabilityReport struct {
	SchemaVersion        int                        `json:"schema_version"`
	ConnectionID         string                     `json:"connection_id"`
	ReportGeneration     int64                      `json:"report_generation"`
	PaxdVersion          string                     `json:"paxd_version"`
	CommandFingerprint   string                     `json:"command_fingerprint"`
	ClientProfileHash    string                     `json:"client_profile_hash"`
	WorkerResultHash     string                     `json:"worker_result_hash"`
	ProtocolVersion      int                        `json:"protocol_version,omitempty"`
	ClientCapabilityKeys []string                   `json:"client_capability_keys,omitempty"`
	WorkerCapabilityKeys []string                   `json:"worker_capability_keys,omitempty"`
	Implementation       *ACPImplementationIdentity `json:"implementation,omitempty"`
	PoolConsistency      string                     `json:"pool_consistency"`
	InitPhase            string                     `json:"init_phase"`
	InitializedAt        time.Time                  `json:"initialized_at,omitempty"`
	LastErrorCode        string                     `json:"last_error_code,omitempty"`
	LastErrorMessage     string                     `json:"last_error_message,omitempty"`
}

type ACPImplementationIdentity struct {
	ACPAgent            *ACPAgentImplementation   `json:"acp_agent,omitempty"`
	Runtime             *ACPRuntimeImplementation `json:"runtime,omitempty"`
	IdentityFingerprint string                    `json:"identity_fingerprint"`
}

type ACPAgentImplementation struct {
	Name    string `json:"name,omitempty"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

type ACPRuntimeImplementation struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	Build   string `json:"build,omitempty"`
	Channel string `json:"channel,omitempty"`
}

type QueryType string

const (
	QueryStatusGet             QueryType = "status.get"
	QueryDiagnosticsGet        QueryType = "diagnostics.get"
	QueryRemotesList           QueryType = "remotes.list"
	QueryRemoteGet             QueryType = "remote.get"
	QueryAgentConnectionsList  QueryType = "agent_connections.list"
	QueryAgentConnectionGet    QueryType = "agent_connection.get"
	QueryHarnessesList         QueryType = "harnesses.list"
	QueryHarnessesDiscover     QueryType = "harnesses.discover"
	QueryLocalOverviewGet      QueryType = "local_overview.get"
	QueryLocalSessionsList     QueryType = "local_sessions.list"
	QueryLocalSessionsSync     QueryType = "local_sessions.sync"
	QueryLocalSessionGet       QueryType = "local_session.get"
	QueryCommandGet            QueryType = "command.get"
	QueryAttachmentLocalStatus QueryType = "attachment.local_status"
	QueryBrowserControl        QueryType = "browser.control"
	QuerySecretChannelOpen     QueryType = "secret_channel.open"
)

type Query struct {
	Type QueryType `json:"type"`

	GetStatus                *GetStatusQuery                `json:"get_status,omitempty"`
	GetDiagnostics           *GetDiagnosticsQuery           `json:"get_diagnostics,omitempty"`
	ListRemotes              *ListRemotesQuery              `json:"list_remotes,omitempty"`
	GetRemote                *GetRemoteQuery                `json:"get_remote,omitempty"`
	ListAgentConnections     *ListAgentConnectionsQuery     `json:"list_agent_connections,omitempty"`
	GetAgentConnection       *GetAgentConnectionQuery       `json:"get_agent_connection,omitempty"`
	ListHarnesses            *ListHarnessesQuery            `json:"list_harnesses,omitempty"`
	DiscoverHarnesses        *DiscoverHarnessesQuery        `json:"discover_harnesses,omitempty"`
	GetLocalOverview         *GetLocalOverviewQuery         `json:"get_local_overview,omitempty"`
	ListLocalSessions        *ListLocalSessionsQuery        `json:"list_local_sessions,omitempty"`
	SyncLocalSessions        *SyncLocalSessionsQuery        `json:"sync_local_sessions,omitempty"`
	GetLocalSession          *GetLocalSessionQuery          `json:"get_local_session,omitempty"`
	GetCommand               *GetCommandQuery               `json:"get_command,omitempty"`
	GetAttachmentLocalStatus *GetAttachmentLocalStatusQuery `json:"get_attachment_local_status,omitempty"`
	BrowserControl           *BrowserControlQuery           `json:"browser_control,omitempty"`
	OpenSecretChannel        *OpenSecretChannelQuery        `json:"open_secret_channel,omitempty"`
}

// OpenSecretChannelQuery has no fields: the channel is bound to whatever
// authenticated Source issued the query, never to a caller-supplied id.
type OpenSecretChannelQuery struct{}

type BrowserControlQuery struct {
	Operation string          `json:"operation"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type BrowserControl interface {
	Request(context.Context, string, string, json.RawMessage) (json.RawMessage, error)
}

// SecretChannelOpenResult hands an untrusted browser a fresh, single-use
// public key. PublicKey is base64-encoded raw ECDH(P-256) point bytes.
//
// NodeID must be echoed back verbatim by the caller when sealing and
// pushing the secret: it is an internal AAD-binding value (see
// secretchannel.ChannelInfo), not necessarily the same identifier the
// caller otherwise knows this node by. A caller that substitutes its own
// idea of the node's ID here will produce a ciphertext paxd cannot decrypt.
type SecretChannelOpenResult struct {
	ChannelID string `json:"channel_id"`
	NodeID    string `json:"node_id"`
	PublicKey string `json:"public_key"`
	ExpiresAt string `json:"expires_at"`
}

type GetAttachmentLocalStatusQuery struct {
	AttachmentIDs []string `json:"attachment_ids"`
}

type GetStatusQuery struct{}

type GetDiagnosticsQuery struct{}

// DiagnosticsProvider supplies live runtime diagnostics that are not persisted
// in the daemon store, such as transport producer stats and process metadata.
type DiagnosticsProvider interface {
	RuntimeDiagnostics(ctx context.Context) RuntimeDiagnostics
}

type RuntimeDiagnostics struct {
	PaxdVersion          string                      `json:"paxd_version,omitempty"`
	StartedAt            *time.Time                  `json:"started_at,omitempty"`
	LogFile              *LogFileInfo                `json:"log_file,omitempty"`
	TransportQueues      []TransportQueueDiagnostics `json:"transport_queues,omitempty"`
	TransportWriteBehind *TransportWriteBehindStats  `json:"transport_write_behind,omitempty"`
	TransportJournalGC   *TransportJournalGCStats    `json:"transport_journal_gc,omitempty"`
}

type LogFileInfo struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

type TransportQueueDiagnostics struct {
	QueueID      string `json:"queue_id"`
	Bound        bool   `json:"bound"`
	Tail         int64  `json:"tail"`
	AckedThrough int64  `json:"acked_through"`
	NextToSend   int64  `json:"next_to_send"`
	Unacked      int64  `json:"unacked"`
	LastError    string `json:"last_error,omitempty"`
}

type TransportWriteBehindStats struct {
	DirtyFrames              int    `json:"dirty_frames"`
	DirtyPatches             int    `json:"dirty_patches"`
	DirtyBytes               int64  `json:"dirty_bytes"`
	Degraded                 bool   `json:"degraded"`
	ConsecutiveFlushFailures int    `json:"consecutive_flush_failures"`
	LastFlushError           string `json:"last_flush_error,omitempty"`
}

type TransportJournalGCStats struct {
	LastRunAt    *time.Time `json:"last_run_at,omitempty"`
	LastDeleted  int64      `json:"last_deleted"`
	TotalDeleted int64      `json:"total_deleted"`
	LastError    string     `json:"last_error,omitempty"`
}

type ACPSlotStatusInfo struct {
	SlotID           string `json:"slot_id"`
	ConnectionID     string `json:"connection_id"`
	Ordinal          int    `json:"ordinal"`
	Phase            string `json:"phase"`
	FailureClass     string `json:"failure_class,omitempty"`
	LastErrorCode    string `json:"last_error_code,omitempty"`
	LastErrorMessage string `json:"last_error_message,omitempty"`
	UpdatedAt        string `json:"updated_at,omitempty"`
}

// ACPSlotStatusSource is an optional store capability used by the diagnostics
// query; stores that do not implement it simply omit slot statuses.
type ACPSlotStatusSource interface {
	ListACPSlotStatuses(ctx context.Context) ([]ACPSlotStatusInfo, error)
}

type DiagnosticsView struct {
	RuntimeDiagnostics
	Remotes          []RemoteStatusView  `json:"remotes,omitempty"`
	AgentConnections []AgentStatusView   `json:"agent_connections,omitempty"`
	ACPSlots         []ACPSlotStatusInfo `json:"acp_slots,omitempty"`
}

type ListRemotesQuery struct {
	IncludeDisabled bool `json:"include_disabled,omitempty"`
}

type GetRemoteQuery struct {
	RemoteID string `json:"remote_id"`
}

type ListAgentConnectionsQuery struct {
	RemoteID        string `json:"remote_id,omitempty"`
	IncludeDisabled bool   `json:"include_disabled,omitempty"`
}

type GetAgentConnectionQuery struct {
	ConnectionID string `json:"connection_id"`
}

type ListHarnessesQuery struct {
	IncludeMissing bool `json:"include_missing,omitempty"`
}

type DiscoverHarnessesQuery struct {
	Probe bool     `json:"probe,omitempty"`
	Names []string `json:"names,omitempty"`
}

type GetLocalOverviewQuery struct{}

type ListLocalSessionsQuery struct {
	Agent string `json:"agent,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type SyncLocalSessionsQuery struct {
	Agent         string `json:"agent,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	TimeoutMillis int64  `json:"timeout_millis,omitempty"`
}

type GetLocalSessionQuery struct {
	SessionID string `json:"session_id"`
}

type GetCommandQuery struct {
	CommandID string `json:"command_id"`
}

type QueryResult struct {
	Type  QueryType     `json:"type"`
	Error *ControlError `json:"error,omitempty"`

	Status                *DaemonStatus                `json:"status,omitempty"`
	Diagnostics           *DiagnosticsView             `json:"diagnostics,omitempty"`
	Remotes               *ListRemotesResult           `json:"remotes,omitempty"`
	Remote                *RemoteView                  `json:"remote,omitempty"`
	AgentConnections      *ListAgentConnectionsResult  `json:"agent_connections,omitempty"`
	AgentConnection       *AgentConnectionView         `json:"agent_connection,omitempty"`
	Harnesses             *ListHarnessesResult         `json:"harnesses,omitempty"`
	LocalOverview         *LocalOverview               `json:"local_overview,omitempty"`
	LocalSessions         *ListLocalSessionsResult     `json:"local_sessions,omitempty"`
	LocalSession          *LocalSessionView            `json:"local_session,omitempty"`
	LocalSessionSync      *LocalSessionSyncResult      `json:"local_session_sync,omitempty"`
	Command               *CommandView                 `json:"command,omitempty"`
	AttachmentLocalStatus *AttachmentLocalStatusResult `json:"attachment_local_status,omitempty"`
	BrowserControl        json.RawMessage              `json:"browser_control,omitempty"`
	SecretChannelOpen     *SecretChannelOpenResult     `json:"secret_channel_open,omitempty"`
}

type AttachmentLocalStatusResult struct {
	Items []AttachmentLocalState `json:"items"`
}

type ListRemotesResult struct {
	Items []RemoteView `json:"items"`
}

type ListAgentConnectionsResult struct {
	Items []AgentConnectionView `json:"items"`
}

type ListHarnessesResult struct {
	Items []HarnessView `json:"items"`
}

type ListLocalSessionsResult struct {
	Items []LocalSessionView `json:"items"`
}

type DaemonStatus struct {
	Phase               string              `json:"phase"`
	Remotes             []RemoteStatusView  `json:"remotes,omitempty"`
	AgentConnections    []AgentStatusView   `json:"agent_connections,omitempty"`
	Harnesses           []HarnessView       `json:"harnesses,omitempty"`
	LocalSessionSummary LocalSessionSummary `json:"local_session_summary,omitempty"`
}

type RemoteView struct {
	Remote       Remote            `json:"remote"`
	Generation   int64             `json:"generation"`
	RestartNonce int64             `json:"restart_nonce"`
	Auth         *RemoteAuthView   `json:"auth,omitempty"`
	Status       *RemoteStatusView `json:"status,omitempty"`
}

type RemoteAuthView struct {
	Kind             RemoteAuthKind `json:"kind"`
	ClientID         string         `json:"client_id,omitempty"`
	ClientSecretRef  string         `json:"client_secret_ref,omitempty"`
	ClientSecretHint string         `json:"client_secret_hint,omitempty"`
}

type RemoteStatusView struct {
	RemoteID             string `json:"remote_id"`
	ObservedGeneration   int64  `json:"observed_generation"`
	ObservedRestartNonce int64  `json:"observed_restart_nonce"`
	Phase                string `json:"phase"`
	LastErrorCode        string `json:"last_error_code,omitempty"`
	LastErrorMessage     string `json:"last_error_message,omitempty"`
	FailureClass         string `json:"failure_class,omitempty"`
	ReconnectAttempt     int    `json:"reconnect_attempt,omitempty"`
	NextRetryAt          string `json:"next_retry_at,omitempty"`
	ConnectedAt          string `json:"connected_at,omitempty"`
	StoppedAt            string `json:"stopped_at,omitempty"`
	UpdatedAt            string `json:"updated_at,omitempty"`
}

type AgentConnectionView struct {
	ID                  string            `json:"id"`
	RemoteID            string            `json:"remote_id"`
	Name                string            `json:"name"`
	CloudAgentID        string            `json:"cloud_agent_id,omitempty"`
	InstanceID          string            `json:"instance_id"`
	AgentType           string            `json:"agent_type"`
	Harness             string            `json:"harness"`
	Command             []string          `json:"command"`
	WorkingDir          string            `json:"working_dir,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
	Enabled             bool              `json:"enabled"`
	DesiredState        DesiredState      `json:"desired_state"`
	DesiredACPSlots     int               `json:"desired_acp_slots"`
	ReportLocalSessions bool              `json:"report_local_sessions"`
	Generation          int64             `json:"generation"`
	RestartNonce        int64             `json:"restart_nonce"`
	Status              *AgentStatusView  `json:"status,omitempty"`
}

type AgentStatusView struct {
	ConnectionID         string            `json:"connection_id"`
	ObservedGeneration   int64             `json:"observed_generation"`
	ObservedRestartNonce int64             `json:"observed_restart_nonce"`
	Phase                string            `json:"phase"`
	PID                  int               `json:"pid,omitempty"`
	LastErrorCode        string            `json:"last_error_code,omitempty"`
	LastErrorMessage     string            `json:"last_error_message,omitempty"`
	FailureClass         string            `json:"failure_class,omitempty"`
	ReconnectAttempt     int               `json:"reconnect_attempt,omitempty"`
	NextRetryAt          string            `json:"next_retry_at,omitempty"`
	StartedAt            string            `json:"started_at,omitempty"`
	ConnectedAt          string            `json:"connected_at,omitempty"`
	StoppedAt            string            `json:"stopped_at,omitempty"`
	UpdatedAt            string            `json:"updated_at,omitempty"`
	Details              map[string]string `json:"details,omitempty"`
}

type HarnessView struct {
	Harness     string   `json:"harness"`
	DisplayName string   `json:"display_name"`
	State       string   `json:"state"`
	Capability  string   `json:"capability,omitempty"`
	Command     []string `json:"command,omitempty"`
	Version     string   `json:"version,omitempty"`
	Source      string   `json:"source,omitempty"`
	InstallHint string   `json:"install_hint,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
}

type LocalOverview struct {
	Sessions  []LocalSessionView `json:"sessions,omitempty"`
	Harnesses []HarnessView      `json:"harnesses,omitempty"`
}

type LocalSessionSummary struct {
	Total int `json:"total"`
}

type LocalSessionView struct {
	ID           string                    `json:"id"`
	Agent        string                    `json:"agent"`
	NativeID     string                    `json:"native_id"`
	Title        string                    `json:"title,omitempty"`
	Status       string                    `json:"status,omitempty"`
	Preview      string                    `json:"preview,omitempty"`
	ProjectID    string                    `json:"project_id,omitempty"`
	UpdatedAt    string                    `json:"updated_at,omitempty"`
	LastActive   string                    `json:"last_active,omitempty"`
	LastListedAt string                    `json:"last_listed_at,omitempty"`
	LastSyncedAt string                    `json:"last_synced_at,omitempty"`
	Metadata     map[string]string         `json:"metadata,omitempty"`
	Elements     []LocalSessionElementView `json:"elements,omitempty"`
}

type LocalSessionElementView struct {
	Seq         int64             `json:"seq"`
	Kind        string            `json:"kind"`
	Role        string            `json:"role,omitempty"`
	Text        string            `json:"text,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	StartedAt   string            `json:"started_at,omitempty"`
	CompletedAt string            `json:"completed_at,omitempty"`
}

type LocalSessionSyncResult struct {
	Synced int            `json:"synced"`
	Failed int            `json:"failed"`
	Errors []ControlError `json:"errors,omitempty"`
}

type CommandView struct {
	CommandID         string          `json:"command_id"`
	Source            Source          `json:"source"`
	Type              CommandType     `json:"type"`
	TargetType        string          `json:"target_type,omitempty"`
	TargetID          string          `json:"target_id,omitempty"`
	Status            CommandStatus   `json:"status"`
	DesiredGeneration int64           `json:"desired_generation,omitempty"`
	ErrorCode         string          `json:"error_code,omitempty"`
	ErrorMessage      string          `json:"error_message,omitempty"`
	Result            json.RawMessage `json:"result,omitempty"`
	ReceivedAt        string          `json:"received_at,omitempty"`
	AppliedAt         string          `json:"applied_at,omitempty"`
	UpdatedAt         string          `json:"updated_at,omitempty"`
}
