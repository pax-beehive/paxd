package daemonstore

import "time"

type Remote struct {
	ID             string     `gorm:"primaryKey;type:text"`
	Name           string     `gorm:"type:text;not null"`
	CloudAPIURL    string     `gorm:"type:text;not null;uniqueIndex:idx_remote_cloud_api_url"`
	NodeID         string     `gorm:"type:text"`
	CloudAPIKeyRef string     `gorm:"column:cloud_api_key_ref;type:text"`
	Enabled        bool       `gorm:"not null"`
	IsDefault      bool       `gorm:"not null"`
	Generation     int64      `gorm:"not null;default:1"`
	RestartNonce   int64      `gorm:"not null;default:0"`
	RegisteredAt   *time.Time `gorm:"column:registered_at"`
	CreatedAt      time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt      time.Time  `gorm:"column:updated_at;not null"`
}

func (Remote) TableName() string { return "remote" }

type RemoteAuth struct {
	RemoteID   string    `gorm:"primaryKey;type:text"`
	Kind       string    `gorm:"type:text;not null"`
	ConfigJSON string    `gorm:"type:text;not null;default:'{}'"`
	CreatedAt  time.Time `gorm:"column:created_at;not null"`
	UpdatedAt  time.Time `gorm:"column:updated_at;not null"`
}

func (RemoteAuth) TableName() string { return "remote_auth" }

type RemoteStatus struct {
	RemoteID             string     `gorm:"primaryKey;type:text"`
	ObservedGeneration   int64      `gorm:"not null;default:0"`
	ObservedRestartNonce int64      `gorm:"not null;default:0"`
	Phase                string     `gorm:"type:text;not null"`
	LastErrorCode        string     `gorm:"type:text;not null;default:''"`
	LastErrorMessage     string     `gorm:"type:text;not null;default:''"`
	FailureClass         string     `gorm:"type:text;not null;default:''"`
	ReconnectAttempt     int        `gorm:"not null;default:0"`
	NextRetryAt          *time.Time `gorm:"column:next_retry_at"`
	ConnectedAt          *time.Time `gorm:"column:connected_at"`
	StoppedAt            *time.Time `gorm:"column:stopped_at"`
	UpdatedAt            time.Time  `gorm:"column:updated_at;not null"`
}

func (RemoteStatus) TableName() string { return "remote_status" }

type AgentConnection struct {
	ID           string     `gorm:"primaryKey;type:text"`
	RemoteID     string     `gorm:"type:text;not null;uniqueIndex:idx_agent_connection_remote_name;uniqueIndex:idx_agent_connection_remote_cloud_agent"`
	Name         string     `gorm:"type:text;not null;uniqueIndex:idx_agent_connection_remote_name"`
	CloudAgentID *string    `gorm:"type:text;uniqueIndex:idx_agent_connection_remote_cloud_agent"`
	InstanceID   string     `gorm:"type:text;not null"`
	AgentType    string     `gorm:"type:text;not null"`
	Harness      string     `gorm:"type:text;not null"`
	CommandJSON  string     `gorm:"type:text;not null"`
	WorkingDir   string     `gorm:"type:text;not null;default:''"`
	TunnelPath   string     `gorm:"type:text;not null;default:'/api/v1/agent/tunnel'"`
	EnvJSON      string     `gorm:"type:text;not null;default:'{}'"`
	Enabled      bool       `gorm:"not null"`
	DesiredState string     `gorm:"type:text;not null"`
	Generation   int64      `gorm:"not null;default:1"`
	RestartNonce int64      `gorm:"not null;default:0"`
	CreatedAt    time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt    time.Time  `gorm:"column:updated_at;not null"`
	DeletedAt    *time.Time `gorm:"column:deleted_at"`
}

func (AgentConnection) TableName() string { return "agent_connection" }

type AgentConnectionStatus struct {
	ConnectionID         string     `gorm:"primaryKey;type:text"`
	ObservedGeneration   int64      `gorm:"not null;default:0"`
	ObservedRestartNonce int64      `gorm:"not null;default:0"`
	Phase                string     `gorm:"type:text;not null"`
	PID                  *int       `gorm:"column:pid"`
	LastErrorCode        string     `gorm:"type:text;not null;default:''"`
	LastErrorMessage     string     `gorm:"type:text;not null;default:''"`
	FailureClass         string     `gorm:"type:text;not null;default:''"`
	ReconnectAttempt     int        `gorm:"not null;default:0"`
	NextRetryAt          *time.Time `gorm:"column:next_retry_at"`
	StartedAt            *time.Time `gorm:"column:started_at"`
	ConnectedAt          *time.Time `gorm:"column:connected_at"`
	StoppedAt            *time.Time `gorm:"column:stopped_at"`
	UpdatedAt            time.Time  `gorm:"column:updated_at;not null"`
	DetailsJSON          string     `gorm:"type:text;not null;default:'{}'"`
}

func (AgentConnectionStatus) TableName() string { return "agent_connection_status" }

type ControlCommand struct {
	CommandID         string     `gorm:"primaryKey;type:text"`
	Source            string     `gorm:"type:text;not null"`
	Type              string     `gorm:"type:text;not null"`
	TargetType        string     `gorm:"type:text;not null;default:''"`
	TargetID          string     `gorm:"type:text;not null;default:''"`
	PayloadJSON       string     `gorm:"type:text;not null;default:'{}'"`
	Status            string     `gorm:"type:text;not null"`
	DesiredGeneration *int64     `gorm:"column:desired_generation"`
	ErrorCode         string     `gorm:"type:text;not null;default:''"`
	ErrorMessage      string     `gorm:"type:text;not null;default:''"`
	ResultJSON        string     `gorm:"type:text;not null;default:'{}'"`
	ReceivedAt        time.Time  `gorm:"column:received_at;not null"`
	AppliedAt         *time.Time `gorm:"column:applied_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at;not null"`
}

func (ControlCommand) TableName() string { return "control_command" }

type HarnessInventory struct {
	Harness      string    `gorm:"primaryKey;type:text"`
	DisplayName  string    `gorm:"type:text;not null"`
	State        string    `gorm:"type:text;not null"`
	Capability   string    `gorm:"type:text;not null;default:''"`
	CommandJSON  string    `gorm:"type:text;not null;default:'[]'"`
	Version      string    `gorm:"type:text;not null;default:''"`
	Source       string    `gorm:"type:text;not null;default:''"`
	InstallHint  string    `gorm:"type:text;not null;default:''"`
	LastError    string    `gorm:"type:text;not null;default:''"`
	DiscoveredAt time.Time `gorm:"column:discovered_at;not null"`
	UpdatedAt    time.Time `gorm:"column:updated_at;not null"`
}

func (HarnessInventory) TableName() string { return "harness_inventory" }

type LocalSession struct {
	ID           string     `gorm:"primaryKey;type:text"`
	Agent        string     `gorm:"type:text;not null;uniqueIndex:idx_local_session_agent_native"`
	NativeID     string     `gorm:"type:text;not null;uniqueIndex:idx_local_session_agent_native"`
	Title        string     `gorm:"type:text;not null;default:''"`
	Status       string     `gorm:"type:text;not null;default:''"`
	Preview      string     `gorm:"type:text;not null;default:''"`
	ProjectID    string     `gorm:"type:text;not null;default:''"`
	UpdatedAtPtr *time.Time `gorm:"column:updated_at"`
	LastActive   *time.Time `gorm:"column:last_active"`
	LastListedAt time.Time  `gorm:"column:last_listed_at;not null"`
	LastSyncedAt *time.Time `gorm:"column:last_synced_at"`
	MetadataJSON string     `gorm:"type:text;not null;default:'{}'"`
}

func (LocalSession) TableName() string { return "local_session" }

type LocalSessionElement struct {
	ID          int64      `gorm:"primaryKey;autoIncrement"`
	SessionID   string     `gorm:"type:text;not null;uniqueIndex:idx_local_session_element_session_seq"`
	Seq         int64      `gorm:"not null;uniqueIndex:idx_local_session_element_session_seq"`
	Kind        string     `gorm:"type:text;not null"`
	Role        string     `gorm:"type:text;not null;default:''"`
	Text        string     `gorm:"type:text;not null;default:''"`
	RawJSON     string     `gorm:"type:text;not null;default:'{}'"`
	StartedAt   *time.Time `gorm:"column:started_at"`
	CompletedAt *time.Time `gorm:"column:completed_at"`
}

func (LocalSessionElement) TableName() string { return "local_session_element" }

type Setting struct {
	Key       string    `gorm:"primaryKey;type:text"`
	ValueJSON string    `gorm:"type:text;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

func (Setting) TableName() string { return "setting" }
