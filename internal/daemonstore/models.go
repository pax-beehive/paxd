package daemonstore

import "time"

type Remote struct {
	ID              string     `gorm:"primaryKey;type:text"`
	Name            string     `gorm:"type:text;not null"`
	CloudAPIURL     string     `gorm:"type:text;not null"`
	NodeControlPath string     `gorm:"type:text;not null;default:'/api/v1/node/control'"`
	AgentTunnelPath string     `gorm:"type:text;not null;default:'/api/v1/agent/tunnel'"`
	NodeID          string     `gorm:"type:text"`
	CloudAPIKeyRef  string     `gorm:"column:cloud_api_key_ref;type:text"`
	Enabled         bool       `gorm:"not null"`
	Generation      int64      `gorm:"not null;default:1"`
	RestartNonce    int64      `gorm:"not null;default:0"`
	RegisteredAt    *time.Time `gorm:"column:registered_at"`
	CreatedAt       time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt       time.Time  `gorm:"column:updated_at;not null"`
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
	ID               string     `gorm:"primaryKey;type:text"`
	RemoteID         string     `gorm:"type:text;not null;uniqueIndex:idx_agent_connection_remote_name;uniqueIndex:idx_agent_connection_remote_cloud_agent"`
	Name             string     `gorm:"type:text;not null;uniqueIndex:idx_agent_connection_remote_name"`
	CloudAgentID     *string    `gorm:"type:text;uniqueIndex:idx_agent_connection_remote_cloud_agent"`
	TransportQueueID string     `gorm:"type:text;not null;default:''"`
	InstanceID       string     `gorm:"type:text;not null"`
	AgentType        string     `gorm:"type:text;not null"`
	Harness          string     `gorm:"type:text;not null"`
	CommandJSON      string     `gorm:"type:text;not null"`
	WorkingDir       string     `gorm:"type:text;not null;default:''"`
	EnvJSON          string     `gorm:"type:text;not null;default:'{}'"`
	Enabled          bool       `gorm:"not null"`
	DesiredState     string     `gorm:"type:text;not null"`
	DesiredACPSlots  int        `gorm:"column:desired_acp_slots;not null;default:1"`
	Generation       int64      `gorm:"not null;default:1"`
	RestartNonce     int64      `gorm:"not null;default:0"`
	CreatedAt        time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt        time.Time  `gorm:"column:updated_at;not null"`
	DeletedAt        *time.Time `gorm:"column:deleted_at"`
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

type ACPSlotStatus struct {
	SlotID            string     `gorm:"column:slot_id;primaryKey;type:text"`
	ConnectionID      string     `gorm:"type:text;not null;uniqueIndex:idx_acp_slot_status_connection_ordinal"`
	Ordinal           int        `gorm:"not null;uniqueIndex:idx_acp_slot_status_connection_ordinal"`
	ProcessEpoch      string     `gorm:"type:text"`
	PID               *int       `gorm:"column:pid"`
	ProcessGroupID    *int       `gorm:"column:process_group_id"`
	ProcessStartToken string     `gorm:"type:text"`
	Phase             string     `gorm:"type:text;not null"`
	FailureClass      string     `gorm:"type:text;not null;default:''"`
	LastErrorCode     string     `gorm:"type:text;not null;default:''"`
	LastErrorMessage  string     `gorm:"type:text;not null;default:''"`
	StartedAt         *time.Time `gorm:"column:started_at"`
	ReadyAt           *time.Time `gorm:"column:ready_at"`
	ActiveSince       *time.Time `gorm:"column:active_since"`
	StoppedAt         *time.Time `gorm:"column:stopped_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at;not null"`
}

func (ACPSlotStatus) TableName() string { return "acp_slot_status" }

type ACPSessionRoute struct {
	ConnectionID      string    `gorm:"primaryKey;type:text;index:idx_acp_session_route_process_binding,where:bound_process_epoch IS NOT NULL"`
	NativeSessionID   string    `gorm:"primaryKey;type:text"`
	BoundSlotID       *string   `gorm:"type:text;index:idx_acp_session_route_process_binding,where:bound_process_epoch IS NOT NULL"`
	BoundProcessEpoch *string   `gorm:"type:text;index:idx_acp_session_route_process_binding,where:bound_process_epoch IS NOT NULL;check:chk_acp_session_route_binding_pair,(bound_slot_id IS NULL) = (bound_process_epoch IS NULL)"`
	LastSlotID        string    `gorm:"type:text;not null;default:''"`
	ResumeParamsJSON  string    `gorm:"type:text;not null"`
	CreatedAt         time.Time `gorm:"column:created_at;not null"`
	LastUsedAt        time.Time `gorm:"column:last_used_at;not null"`
	UpdatedAt         time.Time `gorm:"column:updated_at;not null"`
	Version           int64     `gorm:"not null;default:1"`
}

func (ACPSessionRoute) TableName() string { return "acp_session_route" }

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

// Message is durable local business history projected from ACP traffic. It is
// separate from the reliable transport frame journal.
type Message struct {
	ID              int64     `gorm:"primaryKey;autoIncrement"`
	MessageID       string    `gorm:"type:text;not null;uniqueIndex:idx_messages_message_id"`
	AgentID         string    `gorm:"type:text;not null;index:idx_messages_agent_created,priority:1"`
	SessionID       string    `gorm:"type:text;index:idx_messages_session_created,priority:1"`
	Source          string    `gorm:"type:text;not null"`
	Direction       string    `gorm:"type:text;not null"`
	Role            string    `gorm:"type:text"`
	Status          string    `gorm:"type:text"`
	MessageType     string    `gorm:"type:text"`
	ParentMessageID string    `gorm:"type:text"`
	TurnID          string    `gorm:"type:text"`
	ResponseID      string    `gorm:"type:text"`
	LogicalKey      *string   `gorm:"type:text;uniqueIndex:idx_messages_logical_key"`
	RawJSON         string    `gorm:"type:text"`
	CreatedAt       time.Time `gorm:"column:created_at;not null;index:idx_messages_agent_created,priority:2;index:idx_messages_session_created,priority:2"`
	UpdatedAt       time.Time `gorm:"column:updated_at;not null"`
}

func (Message) TableName() string { return "messages" }

// MessagePart stores text, raw JSON, or future artifact references. Streaming
// deltas append to a text part instead of creating one row per token.
type MessagePart struct {
	ID          int64     `gorm:"primaryKey;autoIncrement"`
	MessageID   string    `gorm:"type:text;not null;uniqueIndex:idx_message_parts_message_part"`
	PartIndex   int       `gorm:"not null;uniqueIndex:idx_message_parts_message_part"`
	PartType    string    `gorm:"type:text;not null"`
	Text        string    `gorm:"type:text"`
	PayloadJSON string    `gorm:"type:text"`
	ArtifactURI string    `gorm:"type:text"`
	CreatedAt   time.Time `gorm:"column:created_at;not null"`
	UpdatedAt   time.Time `gorm:"column:updated_at;not null"`
}

func (MessagePart) TableName() string { return "message_parts" }

type Setting struct {
	Key       string    `gorm:"primaryKey;type:text"`
	ValueJSON string    `gorm:"type:text;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

func (Setting) TableName() string { return "setting" }
