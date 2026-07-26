package control

import (
	"path/filepath"
	"strings"
)

const (
	ErrCodeInvalidArgument = "invalid_argument"
	maxDesiredACPSlots     = 16
)

func (status CommandStatus) Valid() bool {
	switch status {
	case CommandStatusUnknown,
		CommandStatusReceived,
		CommandStatusRejected,
		CommandStatusApplied,
		CommandStatusFailed:
		return true
	default:
		return false
	}
}

func (ack CommandAck) Validate() error {
	if strings.TrimSpace(ack.CommandID) == "" {
		return invalid("command_id", "command id is required")
	}
	if !ack.Status.Valid() {
		return invalid("status", "unknown command status")
	}
	if ack.OK {
		switch ack.Status {
		case CommandStatusReceived, CommandStatusApplied:
		default:
			return invalid("status", "ok command ack must be received or applied")
		}
		if ack.Error != nil {
			return invalid("error", "ok command ack cannot include an error")
		}
		return nil
	}
	switch ack.Status {
	case CommandStatusUnknown, CommandStatusRejected, CommandStatusFailed:
	default:
		return invalid("status", "non-ok command ack must be unknown, rejected, or failed")
	}
	if ack.Error == nil {
		return invalid("error", "non-ok command ack requires an error")
	}
	return nil
}

func (cmd Command) Validate() error {
	if strings.TrimSpace(cmd.CommandID) == "" {
		return invalid("command_id", "command id is required")
	}
	if cmd.Type == "" {
		return invalid("type", "command type is required")
	}

	payloads := []struct {
		typ CommandType
		set bool
		err error
	}{
		{CommandRemoteCreate, cmd.CreateRemote != nil, validatePtr(cmd.CreateRemote)},
		{CommandRemoteUpdate, cmd.UpdateRemote != nil, validatePtr(cmd.UpdateRemote)},
		{CommandRemoteDelete, cmd.DeleteRemote != nil, validatePtr(cmd.DeleteRemote)},
		{CommandRemoteRestart, cmd.RestartRemote != nil, validatePtr(cmd.RestartRemote)},
		{CommandRemoteAuthConfigure, cmd.ConfigureRemoteAuth != nil, validatePtr(cmd.ConfigureRemoteAuth)},
		{CommandRemoteAuthClear, cmd.ClearRemoteAuth != nil, validatePtr(cmd.ClearRemoteAuth)},
		{CommandAgentConnectionCreate, cmd.CreateAgentConnection != nil, validatePtr(cmd.CreateAgentConnection)},
		{CommandAgentConnectionUpdate, cmd.UpdateAgentConnection != nil, validatePtr(cmd.UpdateAgentConnection)},
		{CommandAgentConnectionDelete, cmd.DeleteAgentConnection != nil, validatePtr(cmd.DeleteAgentConnection)},
		{CommandAgentConnectionRestart, cmd.RestartAgentConnection != nil, validatePtr(cmd.RestartAgentConnection)},
		{CommandUpgradePaxd, cmd.UpgradePaxd != nil, validatePtr(cmd.UpgradePaxd)},
		{CommandAttachmentEnsureLocal, cmd.EnsureAttachmentLocal != nil, validatePtr(cmd.EnsureAttachmentLocal)},
	}

	count := 0
	var got CommandType
	var payloadErr error
	for _, payload := range payloads {
		if !payload.set {
			continue
		}
		count++
		got = payload.typ
		payloadErr = payload.err
	}
	if count == 0 {
		return invalid("payload", "exactly one command payload is required")
	}
	if count > 1 {
		return invalid("payload", "exactly one command payload is required")
	}
	if got != cmd.Type {
		return invalid("type", "command payload does not match command type")
	}
	if payloadErr != nil {
		return payloadErr
	}
	if !knownCommandType(cmd.Type) {
		return invalid("type", "unknown command type")
	}
	return nil
}

func (cmd CreateRemoteCommand) Validate() error {
	if strings.TrimSpace(cmd.Remote.Name) == "" {
		return invalid("create_remote.remote.name", "remote name is required")
	}
	if strings.TrimSpace(cmd.Remote.CloudAPIURL) == "" {
		return invalid("create_remote.remote.cloud_api_url", "cloud API URL is required")
	}
	return nil
}

func (cmd UpdateRemoteCommand) Validate() error {
	if strings.TrimSpace(cmd.RemoteID) == "" {
		return invalid("update_remote.remote_id", "remote id is required")
	}
	if cmd.Remote.IsZero() && cmd.CloudAPIKeyRef == nil {
		return invalid("update_remote", "at least one update field is required")
	}
	return nil
}

func (patch RemotePatch) IsZero() bool {
	return patch.Name == nil &&
		patch.CloudAPIURL == nil &&
		patch.NodeControlPath == nil &&
		patch.AgentTunnelPath == nil &&
		patch.NodeID == nil &&
		patch.Enabled == nil
}

func (cmd DeleteRemoteCommand) Validate() error {
	if strings.TrimSpace(cmd.RemoteID) == "" {
		return invalid("delete_remote.remote_id", "remote id is required")
	}
	return nil
}

func (cmd RestartRemoteCommand) Validate() error {
	if strings.TrimSpace(cmd.RemoteID) == "" {
		return invalid("restart_remote.remote_id", "remote id is required")
	}
	return nil
}

func (cmd ConfigureRemoteAuthCommand) Validate() error {
	if strings.TrimSpace(cmd.RemoteID) == "" {
		return invalid("configure_remote_auth.remote_id", "remote id is required")
	}
	switch cmd.Kind {
	case RemoteAuthNone:
		if cmd.CloudflareAccess != nil {
			return invalid("configure_remote_auth.cloudflare_access", "cloudflare access config is only valid for cloudflare_access auth")
		}
	case RemoteAuthCloudflareAccess:
		if cmd.CloudflareAccess == nil {
			return invalid("configure_remote_auth.cloudflare_access", "cloudflare access config is required")
		}
		if strings.TrimSpace(cmd.CloudflareAccess.ClientID) == "" {
			return invalid("configure_remote_auth.cloudflare_access.client_id", "client id is required")
		}
		if strings.TrimSpace(cmd.CloudflareAccess.ClientSecretRef) == "" {
			return invalid("configure_remote_auth.cloudflare_access.client_secret_ref", "client secret ref is required")
		}
	default:
		return invalid("configure_remote_auth.kind", "unsupported remote auth kind")
	}
	return nil
}

func (cmd ClearRemoteAuthCommand) Validate() error {
	if strings.TrimSpace(cmd.RemoteID) == "" {
		return invalid("clear_remote_auth.remote_id", "remote id is required")
	}
	return nil
}

func (cmd CreateAgentConnectionCommand) Validate() error {
	if strings.TrimSpace(cmd.RemoteID) == "" {
		return invalid("create_agent_connection.remote_id", "remote id is required")
	}
	if strings.TrimSpace(cmd.Name) == "" {
		return invalid("create_agent_connection.name", "agent connection name is required")
	}
	if strings.TrimSpace(cmd.InstanceID) == "" {
		return invalid("create_agent_connection.instance_id", "instance id is required")
	}
	if strings.TrimSpace(cmd.AgentType) == "" {
		return invalid("create_agent_connection.agent_type", "agent type is required")
	}
	if strings.TrimSpace(cmd.Harness) == "" {
		return invalid("create_agent_connection.harness", "harness is required")
	}
	if len(cmd.Command) == 0 {
		return invalid("create_agent_connection.command", "command is required")
	}
	if err := validateCommandWords("create_agent_connection.command", cmd.Command); err != nil {
		return err
	}
	if err := validateDesiredSlots("create_agent_connection.desired_slots", cmd.DesiredSlots); err != nil {
		return err
	}
	if cmd.DesiredState != "" {
		return validateDesiredState("create_agent_connection.desired_state", cmd.DesiredState)
	}
	return nil
}

func (cmd UpdateAgentConnectionCommand) Validate() error {
	if strings.TrimSpace(cmd.ConnectionID) == "" {
		return invalid("update_agent_connection.connection_id", "connection id is required")
	}
	if cmd.Name == nil && cmd.CloudAgentID == nil && cmd.InstanceID == nil && cmd.AgentType == nil && cmd.Harness == nil && cmd.Command == nil && cmd.WorkingDir == nil && cmd.Env == nil && cmd.Enabled == nil && cmd.DesiredState == nil && cmd.DesiredSlots == nil {
		return invalid("update_agent_connection", "at least one update field is required")
	}
	if cmd.Command != nil {
		if len(*cmd.Command) == 0 {
			return invalid("update_agent_connection.command", "command cannot be empty")
		}
		if err := validateCommandWords("update_agent_connection.command", *cmd.Command); err != nil {
			return err
		}
	}
	if cmd.DesiredState != nil {
		return validateDesiredState("update_agent_connection.desired_state", *cmd.DesiredState)
	}
	if err := validateDesiredSlots("update_agent_connection.desired_slots", cmd.DesiredSlots); err != nil {
		return err
	}
	return nil
}

func validateDesiredSlots(field string, desiredSlots *int) error {
	if desiredSlots == nil {
		return nil
	}
	if *desiredSlots < 1 {
		return invalid(field, "desired slots must be at least 1")
	}
	if *desiredSlots > maxDesiredACPSlots {
		return invalid(field, "desired slots must not exceed 16")
	}
	return nil
}

func (cmd DeleteAgentConnectionCommand) Validate() error {
	if strings.TrimSpace(cmd.ConnectionID) == "" {
		return invalid("delete_agent_connection.connection_id", "connection id is required")
	}
	return nil
}

func (cmd RestartAgentConnectionCommand) Validate() error {
	if strings.TrimSpace(cmd.ConnectionID) == "" {
		return invalid("restart_agent_connection.connection_id", "connection id is required")
	}
	return nil
}

func (cmd UpgradePaxdCommand) Validate() error {
	if strings.TrimSpace(cmd.Version) == "" && strings.TrimSpace(cmd.URL) == "" {
		return invalid("upgrade_paxd", "version or url is required")
	}
	return nil
}

func (cmd EnsureAttachmentLocalCommand) Validate() error {
	if strings.TrimSpace(cmd.Attachment.AttachmentID) == "" {
		return invalid("ensure_attachment_local.attachment.attachment_id", "attachment id is required")
	}
	filename := strings.TrimSpace(cmd.Attachment.Filename)
	if filename == "" || filename == "." || filename == ".." ||
		filepath.Base(filename) != filename {
		return invalid("ensure_attachment_local.attachment.filename", "filename is required")
	}
	if cmd.Attachment.SizeBytes < 0 {
		return invalid("ensure_attachment_local.attachment.size_bytes", "size cannot be negative")
	}
	if strings.TrimSpace(cmd.Download.URL) == "" {
		return invalid("ensure_attachment_local.download.url", "download url is required")
	}
	return nil
}

func (query Query) Validate() error {
	if query.Type == "" {
		return invalid("type", "query type is required")
	}
	payloads := []struct {
		typ QueryType
		set bool
		err error
	}{
		{QueryStatusGet, query.GetStatus != nil, validatePtr(query.GetStatus)},
		{QueryDiagnosticsGet, query.GetDiagnostics != nil, validatePtr(query.GetDiagnostics)},
		{QueryRemotesList, query.ListRemotes != nil, validatePtr(query.ListRemotes)},
		{QueryRemoteGet, query.GetRemote != nil, validatePtr(query.GetRemote)},
		{QueryAgentConnectionsList, query.ListAgentConnections != nil, validatePtr(query.ListAgentConnections)},
		{QueryAgentConnectionGet, query.GetAgentConnection != nil, validatePtr(query.GetAgentConnection)},
		{QueryHarnessesList, query.ListHarnesses != nil, validatePtr(query.ListHarnesses)},
		{QueryHarnessesDiscover, query.DiscoverHarnesses != nil, validatePtr(query.DiscoverHarnesses)},
		{QueryLocalOverviewGet, query.GetLocalOverview != nil, validatePtr(query.GetLocalOverview)},
		{QueryLocalSessionsList, query.ListLocalSessions != nil, validatePtr(query.ListLocalSessions)},
		{QueryLocalSessionsSync, query.SyncLocalSessions != nil, validatePtr(query.SyncLocalSessions)},
		{QueryLocalSessionGet, query.GetLocalSession != nil, validatePtr(query.GetLocalSession)},
		{QueryCommandGet, query.GetCommand != nil, validatePtr(query.GetCommand)},
		{QueryAttachmentLocalStatus, query.GetAttachmentLocalStatus != nil, validatePtr(query.GetAttachmentLocalStatus)},
	}

	count := 0
	var got QueryType
	var payloadErr error
	for _, payload := range payloads {
		if !payload.set {
			continue
		}
		count++
		got = payload.typ
		payloadErr = payload.err
	}
	if count == 0 {
		return invalid("payload", "exactly one query payload is required")
	}
	if count > 1 {
		return invalid("payload", "exactly one query payload is required")
	}
	if got != query.Type {
		return invalid("type", "query payload does not match query type")
	}
	if payloadErr != nil {
		return payloadErr
	}
	if !knownQueryType(query.Type) {
		return invalid("type", "unknown query type")
	}
	return nil
}

func (query GetStatusQuery) Validate() error {
	return nil
}

func (query GetDiagnosticsQuery) Validate() error {
	return nil
}

func (query ListRemotesQuery) Validate() error {
	return nil
}

func (query GetRemoteQuery) Validate() error {
	if strings.TrimSpace(query.RemoteID) == "" {
		return invalid("get_remote.remote_id", "remote id is required")
	}
	return nil
}

func (query ListAgentConnectionsQuery) Validate() error {
	return nil
}

func (query GetAgentConnectionQuery) Validate() error {
	if strings.TrimSpace(query.ConnectionID) == "" {
		return invalid("get_agent_connection.connection_id", "connection id is required")
	}
	return nil
}

func (query ListHarnessesQuery) Validate() error {
	return nil
}

func (query DiscoverHarnessesQuery) Validate() error {
	for i, name := range query.Names {
		if strings.TrimSpace(name) == "" {
			return invalid("discover_harnesses.names", "harness name cannot be empty")
		}
		if name != strings.TrimSpace(name) {
			return invalid("discover_harnesses.names", "harness name cannot include surrounding whitespace")
		}
		_ = i
	}
	return nil
}

func (query GetLocalOverviewQuery) Validate() error {
	return nil
}

func (query ListLocalSessionsQuery) Validate() error {
	if query.Limit < 0 {
		return invalid("list_local_sessions.limit", "limit cannot be negative")
	}
	return nil
}

func (query SyncLocalSessionsQuery) Validate() error {
	if query.Limit < 0 {
		return invalid("sync_local_sessions.limit", "limit cannot be negative")
	}
	if query.TimeoutMillis < 0 {
		return invalid("sync_local_sessions.timeout_millis", "timeout cannot be negative")
	}
	return nil
}

func (query GetLocalSessionQuery) Validate() error {
	if strings.TrimSpace(query.SessionID) == "" {
		return invalid("get_local_session.session_id", "session id is required")
	}
	return nil
}

func (query GetCommandQuery) Validate() error {
	if strings.TrimSpace(query.CommandID) == "" {
		return invalid("get_command.command_id", "command id is required")
	}
	return nil
}

func (query GetAttachmentLocalStatusQuery) Validate() error {
	if len(query.AttachmentIDs) == 0 {
		return invalid("get_attachment_local_status.attachment_ids", "at least one attachment id is required")
	}
	for _, id := range query.AttachmentIDs {
		if strings.TrimSpace(id) == "" {
			return invalid("get_attachment_local_status.attachment_ids", "attachment id cannot be empty")
		}
	}
	return nil
}

type validatable interface {
	Validate() error
}

func validatePtr[T validatable](value *T) error {
	if value == nil {
		return nil
	}
	return (*value).Validate()
}

func validateCommandWords(target string, words []string) error {
	for _, word := range words {
		if strings.TrimSpace(word) == "" {
			return invalid(target, "command entries cannot be empty")
		}
	}
	return nil
}

func validateDesiredState(target string, state DesiredState) error {
	switch state {
	case DesiredStateRunning, DesiredStateStopped, DesiredStateDeleted:
		return nil
	default:
		return invalid(target, "unsupported desired state")
	}
}

func knownCommandType(typ CommandType) bool {
	switch typ {
	case CommandRemoteCreate,
		CommandRemoteUpdate,
		CommandRemoteDelete,
		CommandRemoteRestart,
		CommandRemoteAuthConfigure,
		CommandRemoteAuthClear,
		CommandAgentConnectionCreate,
		CommandAgentConnectionUpdate,
		CommandAgentConnectionDelete,
		CommandAgentConnectionRestart,
		CommandUpgradePaxd,
		CommandAttachmentEnsureLocal:
		return true
	default:
		return false
	}
}

func knownQueryType(typ QueryType) bool {
	switch typ {
	case QueryStatusGet,
		QueryDiagnosticsGet,
		QueryRemotesList,
		QueryRemoteGet,
		QueryAgentConnectionsList,
		QueryAgentConnectionGet,
		QueryHarnessesList,
		QueryHarnessesDiscover,
		QueryLocalOverviewGet,
		QueryLocalSessionsList,
		QueryLocalSessionsSync,
		QueryLocalSessionGet,
		QueryCommandGet,
		QueryAttachmentLocalStatus:
		return true
	default:
		return false
	}
}

func invalid(target, message string) ControlError {
	return ControlError{Code: ErrCodeInvalidArgument, Message: message, Target: target}
}
