package control

import (
	"context"
	"fmt"
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
	CommandUpgradePaxd            CommandType = "paxd.upgrade"
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

	UpgradePaxd *UpgradePaxdCommand `json:"upgrade_paxd,omitempty"`
}

type Remote struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	CloudAPIURL string `json:"cloud_api_url"`
	NodeID      string `json:"node_id,omitempty"`
	Enabled     *bool  `json:"enabled,omitempty"`
	IsDefault   *bool  `json:"is_default,omitempty"`
}

type RemotePatch struct {
	Name        *string `json:"name,omitempty"`
	CloudAPIURL *string `json:"cloud_api_url,omitempty"`
	NodeID      *string `json:"node_id,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
	IsDefault   *bool   `json:"is_default,omitempty"`
}

type CreateRemoteCommand struct {
	Remote      Remote `json:"remote"`
	CloudAPIKey string `json:"cloud_api_key,omitempty"`
}

type UpdateRemoteCommand struct {
	RemoteID    string      `json:"remote_id"`
	Remote      RemotePatch `json:"remote,omitempty"`
	CloudAPIKey *string     `json:"cloud_api_key,omitempty"`
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

type DesiredState string

const (
	DesiredStateRunning DesiredState = "running"
	DesiredStateStopped DesiredState = "stopped"
	DesiredStateDeleted DesiredState = "deleted"
)

type CreateAgentConnectionCommand struct {
	ID           string            `json:"id,omitempty"`
	RemoteID     string            `json:"remote_id"`
	Name         string            `json:"name"`
	CloudAgentID string            `json:"cloud_agent_id,omitempty"`
	InstanceID   string            `json:"instance_id"`
	AgentType    string            `json:"agent_type"`
	Harness      string            `json:"harness"`
	Command      []string          `json:"command"`
	WorkingDir   string            `json:"working_dir,omitempty"`
	TunnelPath   string            `json:"tunnel_path,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Enabled      *bool             `json:"enabled,omitempty"`
	DesiredState DesiredState      `json:"desired_state,omitempty"`
}

type UpdateAgentConnectionCommand struct {
	ConnectionID string             `json:"connection_id"`
	Name         *string            `json:"name,omitempty"`
	CloudAgentID *string            `json:"cloud_agent_id,omitempty"`
	InstanceID   *string            `json:"instance_id,omitempty"`
	AgentType    *string            `json:"agent_type,omitempty"`
	Harness      *string            `json:"harness,omitempty"`
	Command      *[]string          `json:"command,omitempty"`
	WorkingDir   *string            `json:"working_dir,omitempty"`
	TunnelPath   *string            `json:"tunnel_path,omitempty"`
	Env          *map[string]string `json:"env,omitempty"`
	Enabled      *bool              `json:"enabled,omitempty"`
	DesiredState *DesiredState      `json:"desired_state,omitempty"`
}

type DeleteAgentConnectionCommand struct {
	ConnectionID string `json:"connection_id"`
	Deregister   bool   `json:"deregister,omitempty"`
}

type RestartAgentConnectionCommand struct {
	ConnectionID string `json:"connection_id"`
}

type UpgradePaxdCommand struct {
	Version string `json:"version,omitempty"`
	URL     string `json:"url,omitempty"`
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
	Remote          *RemoteView          `json:"remote,omitempty"`
	AgentConnection *AgentConnectionView `json:"agent_connection,omitempty"`
	Command         *CommandView         `json:"command,omitempty"`
}

type QueryType string

const (
	QueryStatusGet            QueryType = "status.get"
	QueryRemotesList          QueryType = "remotes.list"
	QueryRemoteGet            QueryType = "remote.get"
	QueryAgentConnectionsList QueryType = "agent_connections.list"
	QueryAgentConnectionGet   QueryType = "agent_connection.get"
	QueryHarnessesList        QueryType = "harnesses.list"
	QueryHarnessesDiscover    QueryType = "harnesses.discover"
	QueryLocalOverviewGet     QueryType = "local_overview.get"
	QueryLocalSessionsList    QueryType = "local_sessions.list"
	QueryLocalSessionsSync    QueryType = "local_sessions.sync"
	QueryLocalSessionGet      QueryType = "local_session.get"
	QueryCommandGet           QueryType = "command.get"
)

type Query struct {
	Type QueryType `json:"type"`

	GetStatus            *GetStatusQuery            `json:"get_status,omitempty"`
	ListRemotes          *ListRemotesQuery          `json:"list_remotes,omitempty"`
	GetRemote            *GetRemoteQuery            `json:"get_remote,omitempty"`
	ListAgentConnections *ListAgentConnectionsQuery `json:"list_agent_connections,omitempty"`
	GetAgentConnection   *GetAgentConnectionQuery   `json:"get_agent_connection,omitempty"`
	ListHarnesses        *ListHarnessesQuery        `json:"list_harnesses,omitempty"`
	DiscoverHarnesses    *DiscoverHarnessesQuery    `json:"discover_harnesses,omitempty"`
	GetLocalOverview     *GetLocalOverviewQuery     `json:"get_local_overview,omitempty"`
	ListLocalSessions    *ListLocalSessionsQuery    `json:"list_local_sessions,omitempty"`
	SyncLocalSessions    *SyncLocalSessionsQuery    `json:"sync_local_sessions,omitempty"`
	GetLocalSession      *GetLocalSessionQuery      `json:"get_local_session,omitempty"`
	GetCommand           *GetCommandQuery           `json:"get_command,omitempty"`
}

type GetStatusQuery struct{}

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

	Status           *DaemonStatus               `json:"status,omitempty"`
	Remotes          *ListRemotesResult          `json:"remotes,omitempty"`
	Remote           *RemoteView                 `json:"remote,omitempty"`
	AgentConnections *ListAgentConnectionsResult `json:"agent_connections,omitempty"`
	AgentConnection  *AgentConnectionView        `json:"agent_connection,omitempty"`
	Harnesses        *ListHarnessesResult        `json:"harnesses,omitempty"`
	LocalOverview    *LocalOverview              `json:"local_overview,omitempty"`
	LocalSessions    *ListLocalSessionsResult    `json:"local_sessions,omitempty"`
	LocalSession     *LocalSessionView           `json:"local_session,omitempty"`
	LocalSessionSync *LocalSessionSyncResult     `json:"local_session_sync,omitempty"`
	Command          *CommandView                `json:"command,omitempty"`
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
	ID           string            `json:"id"`
	RemoteID     string            `json:"remote_id"`
	Name         string            `json:"name"`
	CloudAgentID string            `json:"cloud_agent_id,omitempty"`
	InstanceID   string            `json:"instance_id"`
	AgentType    string            `json:"agent_type"`
	Harness      string            `json:"harness"`
	Command      []string          `json:"command"`
	WorkingDir   string            `json:"working_dir,omitempty"`
	TunnelPath   string            `json:"tunnel_path,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Enabled      bool              `json:"enabled"`
	DesiredState DesiredState      `json:"desired_state"`
	Generation   int64             `json:"generation"`
	RestartNonce int64             `json:"restart_nonce"`
	Status       *AgentStatusView  `json:"status,omitempty"`
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
	CommandID         string        `json:"command_id"`
	Source            Source        `json:"source"`
	Type              CommandType   `json:"type"`
	TargetType        string        `json:"target_type,omitempty"`
	TargetID          string        `json:"target_id,omitempty"`
	Status            CommandStatus `json:"status"`
	DesiredGeneration int64         `json:"desired_generation,omitempty"`
	ErrorCode         string        `json:"error_code,omitempty"`
	ErrorMessage      string        `json:"error_message,omitempty"`
	ReceivedAt        string        `json:"received_at,omitempty"`
	AppliedAt         string        `json:"applied_at,omitempty"`
	UpdatedAt         string        `json:"updated_at,omitempty"`
}
