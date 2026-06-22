package daemonstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type CommandRecord = control.CommandRecord

type CommandCompletion = control.CommandCompletion

type RemoteStatusUpdate struct {
	RemoteID             string
	ObservedGeneration   int64
	ObservedRestartNonce int64
	Phase                string
	LastErrorCode        string
	LastErrorMessage     string
	FailureClass         string
	ReconnectAttempt     int
	NextRetryAt          *time.Time
	ConnectedAt          *time.Time
	StoppedAt            *time.Time
}

type AgentConnectionStatusUpdate struct {
	ConnectionID         string
	ObservedGeneration   int64
	ObservedRestartNonce int64
	Phase                string
	PID                  *int
	LastErrorCode        string
	LastErrorMessage     string
	FailureClass         string
	ReconnectAttempt     int
	NextRetryAt          *time.Time
	StartedAt            *time.Time
	ConnectedAt          *time.Time
	StoppedAt            *time.Time
	DetailsJSON          string
}

type RemoteDesiredSpec struct {
	RemoteID     string
	Name         string
	CloudAPIURL  string
	NodeID       string
	Generation   int64
	RestartNonce int64
}

type AgentConnectionDesiredSpec struct {
	ConnectionID string
	RemoteID     string
	CloudAPIURL  string
	CloudAgentID string
	InstanceID   string
	AgentType    string
	Harness      string
	Command      []string
	WorkingDir   string
	TunnelPath   string
	Env          map[string]string
	Generation   int64
	RestartNonce int64
}

type RemoteAuthMaterial = control.RemoteAuthMaterial

type RemoteRegistrationUpdate struct {
	RemoteID           string
	ObservedGeneration int64
	NodeID             string
	RegisteredAt       time.Time
}

type AgentConnectionBindingUpdate struct {
	ConnectionID       string
	ObservedGeneration int64
	CloudAgentID       string
}

func (s *Store) CreateRemote(ctx context.Context, cmd control.CreateRemoteCommand) (control.RemoteView, error) {
	now := s.currentTime()
	id := strings.TrimSpace(cmd.Remote.ID)
	if id == "" {
		id = "remote_" + strings.ToLower(strings.ReplaceAll(strings.TrimSpace(cmd.Remote.Name), " ", "_"))
	}
	remote := Remote{
		ID:             id,
		Name:           cmd.Remote.Name,
		CloudAPIURL:    cmd.Remote.CloudAPIURL,
		NodeID:         cmd.Remote.NodeID,
		CloudAPIKeyRef: cmd.CloudAPIKeyRef,
		Enabled:        boolDefault(cmd.Remote.Enabled, true),
		IsDefault:      boolDefault(cmd.Remote.IsDefault, false),
		Generation:     1,
		RestartNonce:   0,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.db.WithContext(ctx).Select("*").Create(&remote).Error; err != nil {
		return control.RemoteView{}, mapCreateErr(err)
	}
	return remoteView(remote), nil
}

func (s *Store) UpdateRemote(ctx context.Context, cmd control.UpdateRemoteCommand) (control.RemoteView, error) {
	var remote Remote
	if err := s.db.WithContext(ctx).First(&remote, "id = ?", cmd.RemoteID).Error; err != nil {
		return control.RemoteView{}, mapGormErr(err)
	}
	if cmd.Remote.Name != nil {
		remote.Name = *cmd.Remote.Name
	}
	if cmd.Remote.CloudAPIURL != nil {
		remote.CloudAPIURL = *cmd.Remote.CloudAPIURL
	}
	if cmd.Remote.NodeID != nil {
		remote.NodeID = *cmd.Remote.NodeID
	}
	if cmd.Remote.Enabled != nil {
		remote.Enabled = *cmd.Remote.Enabled
	}
	if cmd.Remote.IsDefault != nil {
		remote.IsDefault = *cmd.Remote.IsDefault
	}
	if cmd.CloudAPIKeyRef != nil {
		remote.CloudAPIKeyRef = *cmd.CloudAPIKeyRef
	}
	remote.Generation++
	remote.UpdatedAt = s.currentTime()
	if err := s.db.WithContext(ctx).Save(&remote).Error; err != nil {
		return control.RemoteView{}, mapCreateErr(err)
	}
	return remoteView(remote), nil
}

func (s *Store) DeleteRemote(ctx context.Context, cmd control.DeleteRemoteCommand) (control.RemoteView, error) {
	enabled := false
	return s.UpdateRemote(ctx, control.UpdateRemoteCommand{
		RemoteID: cmd.RemoteID,
		Remote: control.RemotePatch{
			Enabled: &enabled,
		},
	})
}

func (s *Store) RestartRemote(ctx context.Context, cmd control.RestartRemoteCommand) (control.RemoteView, error) {
	var remote Remote
	if err := s.db.WithContext(ctx).First(&remote, "id = ?", cmd.RemoteID).Error; err != nil {
		return control.RemoteView{}, mapGormErr(err)
	}
	remote.RestartNonce++
	remote.UpdatedAt = s.currentTime()
	if err := s.db.WithContext(ctx).Save(&remote).Error; err != nil {
		return control.RemoteView{}, mapGormErr(err)
	}
	return remoteView(remote), nil
}

func (s *Store) ListRemotes(ctx context.Context, filter control.ListRemotesQuery) ([]control.RemoteView, error) {
	var remotes []Remote
	q := s.db.WithContext(ctx).Order("created_at asc")
	if !filter.IncludeDisabled {
		q = q.Where("enabled = ?", true)
	}
	if err := q.Find(&remotes).Error; err != nil {
		return nil, err
	}
	views := make([]control.RemoteView, 0, len(remotes))
	for _, remote := range remotes {
		view, err := s.remoteViewWithDetails(ctx, remote)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Store) ConfigureRemoteAuth(ctx context.Context, cmd control.ConfigureRemoteAuthCommand) error {
	if err := s.ensureRemote(ctx, cmd.RemoteID); err != nil {
		return err
	}
	config := "{}"
	if cmd.Kind == control.RemoteAuthCloudflareAccess && cmd.CloudflareAccess != nil {
		raw, err := json.Marshal(map[string]any{
			"cloudflareAccess": map[string]string{
				"clientId":        cmd.CloudflareAccess.ClientID,
				"clientSecretRef": cmd.CloudflareAccess.ClientSecretRef,
			},
		})
		if err != nil {
			return err
		}
		config = string(raw)
	}
	now := s.currentTime()
	auth := RemoteAuth{
		RemoteID:   cmd.RemoteID,
		Kind:       string(cmd.Kind),
		ConfigJSON: config,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "remote_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"kind":        auth.Kind,
			"config_json": auth.ConfigJSON,
			"updated_at":  auth.UpdatedAt,
		}),
	}).Create(&auth).Error
}

func (s *Store) ClearRemoteAuth(ctx context.Context, cmd control.ClearRemoteAuthCommand) error {
	if err := s.ensureRemote(ctx, cmd.RemoteID); err != nil {
		return err
	}
	now := s.currentTime()
	auth := RemoteAuth{
		RemoteID:   cmd.RemoteID,
		Kind:       string(control.RemoteAuthNone),
		ConfigJSON: "{}",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "remote_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"kind":        auth.Kind,
			"config_json": auth.ConfigJSON,
			"updated_at":  auth.UpdatedAt,
		}),
	}).Create(&auth).Error
}

func (s *Store) GetRemoteAuthMaterial(ctx context.Context, remoteID string) (RemoteAuthMaterial, error) {
	remote, err := s.getRemote(ctx, remoteID)
	if err != nil {
		return RemoteAuthMaterial{}, err
	}
	material := RemoteAuthMaterial{
		RemoteID:       remote.ID,
		CloudAPIURL:    remote.CloudAPIURL,
		NodeID:         remote.NodeID,
		CloudAPIKeyRef: remote.CloudAPIKeyRef,
		AuthKind:       control.RemoteAuthNone,
	}
	auth, err := s.getRemoteAuth(ctx, remoteID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return material, nil
		}
		return RemoteAuthMaterial{}, err
	}
	material.AuthKind = control.RemoteAuthKind(auth.Kind)
	if material.AuthKind == control.RemoteAuthCloudflareAccess {
		cf, err := decodeCloudflareAccess(auth.ConfigJSON)
		if err != nil {
			return RemoteAuthMaterial{}, err
		}
		material.CloudflareAccess = cf
	}
	return material, nil
}

func (s *Store) MarkRemoteRegistered(ctx context.Context, update RemoteRegistrationUpdate) (bool, error) {
	registeredAt := update.RegisteredAt
	if registeredAt.IsZero() {
		registeredAt = s.currentTime()
	}
	res := s.db.WithContext(ctx).Model(&Remote{}).
		Where("id = ?", update.RemoteID).
		Where("generation = ?", update.ObservedGeneration).
		Updates(map[string]any{
			"node_id":       update.NodeID,
			"registered_at": registeredAt.UTC(),
			"updated_at":    s.currentTime(),
		})
	if res.Error != nil {
		return false, mapGormErr(res.Error)
	}
	if res.RowsAffected > 0 {
		return true, nil
	}
	if err := s.ensureRemote(ctx, update.RemoteID); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) CreateAgentConnection(ctx context.Context, cmd control.CreateAgentConnectionCommand) (control.AgentConnectionView, error) {
	remote, err := s.getRemote(ctx, cmd.RemoteID)
	if err != nil {
		return control.AgentConnectionView{}, err
	}
	commandJSON, err := marshalJSON(cmd.Command, "[]")
	if err != nil {
		return control.AgentConnectionView{}, err
	}
	envJSON, err := marshalJSON(cmd.Env, "{}")
	if err != nil {
		return control.AgentConnectionView{}, err
	}
	now := s.currentTime()
	desired := cmd.DesiredState
	if desired == "" {
		desired = control.DesiredStateRunning
	}
	conn := AgentConnection{
		ID:           cmd.ID,
		RemoteID:     cmd.RemoteID,
		Name:         cmd.Name,
		CloudAgentID: stringPtrOrNil(cmd.CloudAgentID),
		InstanceID:   cmd.InstanceID,
		AgentType:    cmd.AgentType,
		Harness:      cmd.Harness,
		CommandJSON:  commandJSON,
		WorkingDir:   cmd.WorkingDir,
		TunnelPath:   stringDefault(cmd.TunnelPath, "/api/v1/agent/tunnel"),
		EnvJSON:      envJSON,
		Enabled:      boolDefault(cmd.Enabled, true),
		DesiredState: string(desired),
		Generation:   1,
		RestartNonce: 0,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if conn.ID == "" {
		conn.ID = "conn_" + strings.ToLower(strings.ReplaceAll(strings.TrimSpace(cmd.Name), " ", "_"))
	}
	if err := s.db.WithContext(ctx).Select("*").Create(&conn).Error; err != nil {
		return control.AgentConnectionView{}, mapCreateErr(err)
	}
	return agentConnectionView(conn, remote.CloudAPIURL), nil
}

func (s *Store) UpdateAgentConnection(ctx context.Context, cmd control.UpdateAgentConnectionCommand) (control.AgentConnectionView, error) {
	conn, remote, err := s.getAgentConnectionWithRemote(ctx, cmd.ConnectionID)
	if err != nil {
		return control.AgentConnectionView{}, err
	}
	if cmd.Name != nil {
		conn.Name = *cmd.Name
	}
	if cmd.CloudAgentID != nil {
		conn.CloudAgentID = stringPtrOrNil(*cmd.CloudAgentID)
	}
	if cmd.InstanceID != nil {
		conn.InstanceID = *cmd.InstanceID
	}
	if cmd.AgentType != nil {
		conn.AgentType = *cmd.AgentType
	}
	if cmd.Harness != nil {
		conn.Harness = *cmd.Harness
	}
	if cmd.Command != nil {
		raw, err := marshalJSON(*cmd.Command, "[]")
		if err != nil {
			return control.AgentConnectionView{}, err
		}
		conn.CommandJSON = raw
	}
	if cmd.WorkingDir != nil {
		conn.WorkingDir = *cmd.WorkingDir
	}
	if cmd.TunnelPath != nil {
		conn.TunnelPath = *cmd.TunnelPath
	}
	if cmd.Env != nil {
		raw, err := marshalJSON(*cmd.Env, "{}")
		if err != nil {
			return control.AgentConnectionView{}, err
		}
		conn.EnvJSON = raw
	}
	if cmd.Enabled != nil {
		conn.Enabled = *cmd.Enabled
	}
	if cmd.DesiredState != nil {
		conn.DesiredState = string(*cmd.DesiredState)
	}
	conn.Generation++
	conn.UpdatedAt = s.currentTime()
	if err := s.db.WithContext(ctx).Save(&conn).Error; err != nil {
		return control.AgentConnectionView{}, mapCreateErr(err)
	}
	return agentConnectionView(conn, remote.CloudAPIURL), nil
}

func (s *Store) DeleteAgentConnection(ctx context.Context, cmd control.DeleteAgentConnectionCommand) (control.AgentConnectionView, error) {
	state := control.DesiredStateDeleted
	enabled := false
	view, err := s.UpdateAgentConnection(ctx, control.UpdateAgentConnectionCommand{
		ConnectionID: cmd.ConnectionID,
		Enabled:      &enabled,
		DesiredState: &state,
	})
	if err != nil {
		return control.AgentConnectionView{}, err
	}
	now := s.currentTime()
	if err := s.db.WithContext(ctx).Model(&AgentConnection{}).Where("id = ?", cmd.ConnectionID).Update("deleted_at", &now).Error; err != nil {
		return control.AgentConnectionView{}, err
	}
	return view, nil
}

func (s *Store) RestartAgentConnection(ctx context.Context, cmd control.RestartAgentConnectionCommand) (control.AgentConnectionView, error) {
	conn, remote, err := s.getAgentConnectionWithRemote(ctx, cmd.ConnectionID)
	if err != nil {
		return control.AgentConnectionView{}, err
	}
	conn.RestartNonce++
	conn.UpdatedAt = s.currentTime()
	if err := s.db.WithContext(ctx).Save(&conn).Error; err != nil {
		return control.AgentConnectionView{}, mapGormErr(err)
	}
	return agentConnectionView(conn, remote.CloudAPIURL), nil
}

func (s *Store) ListAgentConnections(ctx context.Context, filter control.ListAgentConnectionsQuery) ([]control.AgentConnectionView, error) {
	var conns []AgentConnection
	q := s.db.WithContext(ctx).Order("created_at asc")
	if filter.RemoteID != "" {
		q = q.Where("remote_id = ?", filter.RemoteID)
	}
	if !filter.IncludeDisabled {
		q = q.Where("enabled = ?", true).Where("desired_state <> ?", string(control.DesiredStateDeleted))
	}
	if err := q.Find(&conns).Error; err != nil {
		return nil, err
	}
	views := make([]control.AgentConnectionView, 0, len(conns))
	for _, conn := range conns {
		remote, err := s.getRemote(ctx, conn.RemoteID)
		if err != nil {
			return nil, err
		}
		view, err := s.agentConnectionViewWithStatus(ctx, conn, remote.CloudAPIURL)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Store) SetAgentConnectionCloudAgentID(ctx context.Context, update AgentConnectionBindingUpdate) (bool, error) {
	res := s.db.WithContext(ctx).Model(&AgentConnection{}).
		Where("id = ?", update.ConnectionID).
		Where("generation = ?", update.ObservedGeneration).
		Updates(map[string]any{
			"cloud_agent_id": stringPtrOrNil(update.CloudAgentID),
			"updated_at":     s.currentTime(),
		})
	if res.Error != nil {
		return false, mapCreateErr(res.Error)
	}
	if res.RowsAffected > 0 {
		return true, nil
	}
	if _, _, err := s.getAgentConnectionWithRemote(ctx, update.ConnectionID); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) InsertCommand(ctx context.Context, rec CommandRecord) error {
	now := s.currentTime()
	model := ControlCommand{
		CommandID:         rec.CommandID,
		Source:            string(rec.Source.Kind),
		Type:              string(rec.Type),
		TargetType:        rec.TargetType,
		TargetID:          rec.TargetID,
		PayloadJSON:       stringDefault(rec.PayloadJSON, "{}"),
		Status:            string(statusDefault(rec.Status, control.CommandStatusReceived)),
		DesiredGeneration: rec.DesiredGeneration,
		ErrorCode:         rec.ErrorCode,
		ErrorMessage:      rec.ErrorMessage,
		ResultJSON:        stringDefault(rec.ResultJSON, "{}"),
		ReceivedAt:        now,
		UpdatedAt:         now,
	}
	if err := s.db.WithContext(ctx).Create(&model).Error; err != nil {
		return mapCreateErr(err)
	}
	return nil
}

func (s *Store) GetCommand(ctx context.Context, commandID string) (*control.CommandView, error) {
	var cmd ControlCommand
	if err := s.db.WithContext(ctx).First(&cmd, "command_id = ?", commandID).Error; err != nil {
		return nil, mapGormErr(err)
	}
	view := commandView(cmd)
	return &view, nil
}

func (s *Store) GetCommandRecord(ctx context.Context, commandID string) (*control.CommandRecord, error) {
	var cmd ControlCommand
	if err := s.db.WithContext(ctx).First(&cmd, "command_id = ?", commandID).Error; err != nil {
		return nil, mapGormErr(err)
	}
	return &control.CommandRecord{
		CommandID:         cmd.CommandID,
		Source:            control.Source{Kind: control.SourceKind(cmd.Source)},
		Type:              control.CommandType(cmd.Type),
		TargetType:        cmd.TargetType,
		TargetID:          cmd.TargetID,
		PayloadJSON:       cmd.PayloadJSON,
		Status:            control.CommandStatus(cmd.Status),
		DesiredGeneration: cmd.DesiredGeneration,
		ErrorCode:         cmd.ErrorCode,
		ErrorMessage:      cmd.ErrorMessage,
		ResultJSON:        cmd.ResultJSON,
	}, nil
}

func (s *Store) CompleteCommand(ctx context.Context, commandID string, completion CommandCompletion) error {
	updates := map[string]any{
		"status":        string(completion.Status),
		"error_code":    completion.ErrorCode,
		"error_message": completion.ErrorMessage,
		"result_json":   stringDefault(completion.ResultJSON, "{}"),
		"updated_at":    s.currentTime(),
	}
	if completion.DesiredGeneration != nil {
		updates["desired_generation"] = completion.DesiredGeneration
	}
	if completion.Status == control.CommandStatusApplied {
		now := s.currentTime()
		updates["applied_at"] = &now
	}
	res := s.db.WithContext(ctx).Model(&ControlCommand{}).Where("command_id = ?", commandID).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListDesiredRemotes(ctx context.Context) ([]RemoteDesiredSpec, error) {
	var remotes []Remote
	if err := s.db.WithContext(ctx).Where("enabled = ?", true).Find(&remotes).Error; err != nil {
		return nil, err
	}
	specs := make([]RemoteDesiredSpec, 0, len(remotes))
	for _, remote := range remotes {
		specs = append(specs, RemoteDesiredSpec{
			RemoteID:     remote.ID,
			Name:         remote.Name,
			CloudAPIURL:  remote.CloudAPIURL,
			NodeID:       remote.NodeID,
			Generation:   remote.Generation,
			RestartNonce: remote.RestartNonce,
		})
	}
	return specs, nil
}

func (s *Store) UpsertRemoteStatus(ctx context.Context, update RemoteStatusUpdate) error {
	status := remoteStatusModel(update, s.currentTime())
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "remote_id"}},
		UpdateAll: true,
	}).Create(&status).Error
}

func (s *Store) ConditionalRemoteStatusUpdate(ctx context.Context, update RemoteStatusUpdate) (bool, error) {
	if err := s.UpsertRemoteStatusIfMissing(ctx, update); err != nil {
		return false, err
	}
	status := remoteStatusModel(update, s.currentTime())
	res := s.db.WithContext(ctx).Model(&RemoteStatus{}).
		Where("remote_id = ?", update.RemoteID).
		Where("observed_generation < ? OR (observed_generation = ? AND observed_restart_nonce <= ?)", update.ObservedGeneration, update.ObservedGeneration, update.ObservedRestartNonce).
		Updates(remoteStatusUpdates(status))
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func (s *Store) UpsertRemoteStatusIfMissing(ctx context.Context, update RemoteStatusUpdate) error {
	status := remoteStatusModel(update, s.currentTime())
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "remote_id"}},
		DoNothing: true,
	}).Create(&status).Error
}

func (s *Store) ListDesiredAgentConnections(ctx context.Context) ([]AgentConnectionDesiredSpec, error) {
	var conns []AgentConnection
	if err := s.db.WithContext(ctx).
		Where("enabled = ?", true).
		Where("desired_state = ?", string(control.DesiredStateRunning)).
		Find(&conns).Error; err != nil {
		return nil, err
	}
	specs := make([]AgentConnectionDesiredSpec, 0, len(conns))
	for _, conn := range conns {
		remote, err := s.getRemote(ctx, conn.RemoteID)
		if err != nil {
			return nil, err
		}
		command, err := decodeStringSlice(conn.CommandJSON)
		if err != nil {
			return nil, err
		}
		env, err := decodeStringMap(conn.EnvJSON)
		if err != nil {
			return nil, err
		}
		specs = append(specs, AgentConnectionDesiredSpec{
			ConnectionID: conn.ID,
			RemoteID:     conn.RemoteID,
			CloudAPIURL:  remote.CloudAPIURL,
			CloudAgentID: stringValue(conn.CloudAgentID),
			InstanceID:   conn.InstanceID,
			AgentType:    conn.AgentType,
			Harness:      conn.Harness,
			Command:      command,
			WorkingDir:   conn.WorkingDir,
			TunnelPath:   conn.TunnelPath,
			Env:          env,
			Generation:   conn.Generation,
			RestartNonce: conn.RestartNonce,
		})
	}
	return specs, nil
}

func (s *Store) UpsertAgentConnectionStatus(ctx context.Context, update AgentConnectionStatusUpdate) error {
	status := agentConnectionStatusModel(update, s.currentTime())
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "connection_id"}},
		UpdateAll: true,
	}).Create(&status).Error
}

func (s *Store) ConditionalAgentConnectionStatusUpdate(ctx context.Context, update AgentConnectionStatusUpdate) (bool, error) {
	if err := s.UpsertAgentConnectionStatusIfMissing(ctx, update); err != nil {
		return false, err
	}
	status := agentConnectionStatusModel(update, s.currentTime())
	res := s.db.WithContext(ctx).Model(&AgentConnectionStatus{}).
		Where("connection_id = ?", update.ConnectionID).
		Where("observed_generation < ? OR (observed_generation = ? AND observed_restart_nonce <= ?)", update.ObservedGeneration, update.ObservedGeneration, update.ObservedRestartNonce).
		Updates(agentConnectionStatusUpdates(status))
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func (s *Store) UpsertAgentConnectionStatusIfMissing(ctx context.Context, update AgentConnectionStatusUpdate) error {
	status := agentConnectionStatusModel(update, s.currentTime())
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "connection_id"}},
		DoNothing: true,
	}).Create(&status).Error
}

func (s *Store) ListHarnesses(ctx context.Context) ([]control.HarnessView, error) {
	var rows []HarnessInventory
	if err := s.db.WithContext(ctx).Order("harness asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	views := make([]control.HarnessView, 0, len(rows))
	for _, row := range rows {
		command, err := decodeStringSlice(row.CommandJSON)
		if err != nil {
			return nil, err
		}
		views = append(views, control.HarnessView{
			Harness:     row.Harness,
			DisplayName: row.DisplayName,
			State:       row.State,
			Capability:  row.Capability,
			Command:     command,
			Version:     row.Version,
			Source:      row.Source,
			InstallHint: row.InstallHint,
			LastError:   row.LastError,
		})
	}
	return views, nil
}

func (s *Store) UpsertHarnesses(ctx context.Context, harnesses []control.HarnessView) error {
	now := s.currentTime()
	for _, view := range harnesses {
		commandJSON, err := marshalJSON(view.Command, "[]")
		if err != nil {
			return err
		}
		row := HarnessInventory{
			Harness:      view.Harness,
			DisplayName:  view.DisplayName,
			State:        view.State,
			Capability:   view.Capability,
			CommandJSON:  commandJSON,
			Version:      view.Version,
			Source:       view.Source,
			InstallHint:  view.InstallHint,
			LastError:    view.LastError,
			DiscoveredAt: now,
			UpdatedAt:    now,
		}
		if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "harness"}},
			UpdateAll: true,
		}).Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) UpsertSetting(ctx context.Context, key string, valueJSON string) error {
	row := Setting{Key: key, ValueJSON: valueJSON, UpdatedAt: s.currentTime()}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		UpdateAll: true,
	}).Create(&row).Error
}

func (s *Store) GetSetting(ctx context.Context, key, defaultValueJSON string) (string, error) {
	var row Setting
	if err := s.db.WithContext(ctx).First(&row, "key = ?", key).Error; err != nil {
		if isMissing(err) {
			return defaultValueJSON, nil
		}
		return "", err
	}
	return row.ValueJSON, nil
}

func (s *Store) ListSessions(ctx context.Context, query control.ListLocalSessionsQuery) ([]control.LocalSessionView, error) {
	var rows []LocalSession
	q := s.db.WithContext(ctx).Order("last_active desc, last_listed_at desc")
	if query.Agent != "" {
		q = q.Where("agent = ?", query.Agent)
	}
	if query.Limit > 0 {
		q = q.Limit(query.Limit)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	views := make([]control.LocalSessionView, 0, len(rows))
	for _, row := range rows {
		metadata, err := decodeStringMap(row.MetadataJSON)
		if err != nil {
			return nil, err
		}
		views = append(views, localSessionView(row, metadata, nil))
	}
	return views, nil
}

func (s *Store) UpsertSessions(ctx context.Context, sessions []control.LocalSessionView) error {
	for _, view := range sessions {
		metadataJSON, err := marshalJSON(view.Metadata, "{}")
		if err != nil {
			return err
		}
		row := LocalSession{
			ID:           view.ID,
			Agent:        view.Agent,
			NativeID:     view.NativeID,
			Title:        view.Title,
			Status:       view.Status,
			Preview:      view.Preview,
			ProjectID:    view.ProjectID,
			UpdatedAtPtr: parseTimePtr(view.UpdatedAt),
			LastActive:   parseTimePtr(view.LastActive),
			LastListedAt: parseTimeOrNow(view.LastListedAt, s.currentTime()),
			LastSyncedAt: parseTimePtr(view.LastSyncedAt),
			MetadataJSON: metadataJSON,
		}
		if row.ID == "" {
			row.ID = row.Agent + ":" + row.NativeID
		}
		if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "agent"}, {Name: "native_id"}},
			UpdateAll: true,
		}).Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetSession(ctx context.Context, sessionID string) (*control.LocalSessionView, error) {
	var row LocalSession
	if err := s.db.WithContext(ctx).First(&row, "id = ?", sessionID).Error; err != nil {
		return nil, mapGormErr(err)
	}
	var elements []LocalSessionElement
	if err := s.db.WithContext(ctx).Where("session_id = ?", sessionID).Order("seq asc").Find(&elements).Error; err != nil {
		return nil, err
	}
	metadata, err := decodeStringMap(row.MetadataJSON)
	if err != nil {
		return nil, err
	}
	elementViews := make([]control.LocalSessionElementView, 0, len(elements))
	for _, element := range elements {
		elementViews = append(elementViews, control.LocalSessionElementView{
			Seq:         element.Seq,
			Kind:        element.Kind,
			Role:        element.Role,
			Text:        element.Text,
			StartedAt:   formatTimePtr(element.StartedAt),
			CompletedAt: formatTimePtr(element.CompletedAt),
		})
	}
	view := localSessionView(row, metadata, elementViews)
	return &view, nil
}

func (s *Store) ReplaceSessionElements(ctx context.Context, sessionID string, elements []LocalSessionElement) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("session_id = ?", sessionID).Delete(&LocalSessionElement{}).Error; err != nil {
			return err
		}
		for _, element := range elements {
			element.SessionID = sessionID
			if err := tx.Create(&element).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ensureRemote(ctx context.Context, remoteID string) error {
	_, err := s.getRemote(ctx, remoteID)
	return err
}

func (s *Store) getRemoteAuth(ctx context.Context, remoteID string) (RemoteAuth, error) {
	var auth RemoteAuth
	if err := s.db.WithContext(ctx).First(&auth, "remote_id = ?", remoteID).Error; err != nil {
		return RemoteAuth{}, mapGormErr(err)
	}
	return auth, nil
}

func (s *Store) getRemote(ctx context.Context, remoteID string) (Remote, error) {
	var remote Remote
	if err := s.db.WithContext(ctx).First(&remote, "id = ?", remoteID).Error; err != nil {
		return Remote{}, mapGormErr(err)
	}
	return remote, nil
}

func (s *Store) getAgentConnectionWithRemote(ctx context.Context, connectionID string) (AgentConnection, Remote, error) {
	var conn AgentConnection
	if err := s.db.WithContext(ctx).First(&conn, "id = ?", connectionID).Error; err != nil {
		return AgentConnection{}, Remote{}, mapGormErr(err)
	}
	remote, err := s.getRemote(ctx, conn.RemoteID)
	if err != nil {
		return AgentConnection{}, Remote{}, err
	}
	return conn, remote, nil
}

func (s *Store) remoteViewWithDetails(ctx context.Context, remote Remote) (control.RemoteView, error) {
	view := remoteView(remote)
	auth, err := s.getRemoteAuth(ctx, remote.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return control.RemoteView{}, err
	}
	if err == nil {
		authView, err := remoteAuthView(auth)
		if err != nil {
			return control.RemoteView{}, err
		}
		view.Auth = authView
	}
	var status RemoteStatus
	err = s.db.WithContext(ctx).First(&status, "remote_id = ?", remote.ID).Error
	if err != nil && !isMissing(err) {
		return control.RemoteView{}, err
	}
	if err == nil {
		view.Status = remoteStatusView(status)
	}
	return view, nil
}

func (s *Store) agentConnectionViewWithStatus(ctx context.Context, conn AgentConnection, cloudAPIURL string) (control.AgentConnectionView, error) {
	view := agentConnectionView(conn, cloudAPIURL)
	var status AgentConnectionStatus
	err := s.db.WithContext(ctx).First(&status, "connection_id = ?", conn.ID).Error
	if err != nil && !isMissing(err) {
		return control.AgentConnectionView{}, err
	}
	if err == nil {
		statusView, err := agentConnectionStatusView(status)
		if err != nil {
			return control.AgentConnectionView{}, err
		}
		view.Status = statusView
	}
	return view, nil
}

func remoteView(remote Remote) control.RemoteView {
	enabled := remote.Enabled
	isDefault := remote.IsDefault
	return control.RemoteView{
		Remote: control.Remote{
			ID:          remote.ID,
			Name:        remote.Name,
			CloudAPIURL: remote.CloudAPIURL,
			NodeID:      remote.NodeID,
			Enabled:     &enabled,
			IsDefault:   &isDefault,
		},
		Generation:   remote.Generation,
		RestartNonce: remote.RestartNonce,
	}
}

func remoteAuthView(auth RemoteAuth) (*control.RemoteAuthView, error) {
	view := &control.RemoteAuthView{Kind: control.RemoteAuthKind(auth.Kind)}
	if view.Kind != control.RemoteAuthCloudflareAccess {
		return view, nil
	}
	cf, err := decodeCloudflareAccess(auth.ConfigJSON)
	if err != nil {
		return nil, err
	}
	view.ClientID = cf.ClientID
	view.ClientSecretRef = cf.ClientSecretRef
	return view, nil
}

func agentConnectionView(conn AgentConnection, _ string) control.AgentConnectionView {
	command, _ := decodeStringSlice(conn.CommandJSON)
	env, _ := decodeStringMap(conn.EnvJSON)
	return control.AgentConnectionView{
		ID:           conn.ID,
		RemoteID:     conn.RemoteID,
		Name:         conn.Name,
		CloudAgentID: stringValue(conn.CloudAgentID),
		InstanceID:   conn.InstanceID,
		AgentType:    conn.AgentType,
		Harness:      conn.Harness,
		Command:      command,
		WorkingDir:   conn.WorkingDir,
		TunnelPath:   conn.TunnelPath,
		Env:          env,
		Enabled:      conn.Enabled,
		DesiredState: control.DesiredState(conn.DesiredState),
		Generation:   conn.Generation,
		RestartNonce: conn.RestartNonce,
	}
}

func remoteStatusView(status RemoteStatus) *control.RemoteStatusView {
	return &control.RemoteStatusView{
		RemoteID:             status.RemoteID,
		ObservedGeneration:   status.ObservedGeneration,
		ObservedRestartNonce: status.ObservedRestartNonce,
		Phase:                status.Phase,
		LastErrorCode:        status.LastErrorCode,
		LastErrorMessage:     status.LastErrorMessage,
		FailureClass:         status.FailureClass,
		ReconnectAttempt:     status.ReconnectAttempt,
		NextRetryAt:          formatTimePtr(status.NextRetryAt),
		ConnectedAt:          formatTimePtr(status.ConnectedAt),
		StoppedAt:            formatTimePtr(status.StoppedAt),
		UpdatedAt:            status.UpdatedAt.Format(time.RFC3339Nano),
	}
}

func agentConnectionStatusView(status AgentConnectionStatus) (*control.AgentStatusView, error) {
	details, err := decodeStringMap(status.DetailsJSON)
	if err != nil {
		return nil, err
	}
	pid := 0
	if status.PID != nil {
		pid = *status.PID
	}
	return &control.AgentStatusView{
		ConnectionID:         status.ConnectionID,
		ObservedGeneration:   status.ObservedGeneration,
		ObservedRestartNonce: status.ObservedRestartNonce,
		Phase:                status.Phase,
		PID:                  pid,
		LastErrorCode:        status.LastErrorCode,
		LastErrorMessage:     status.LastErrorMessage,
		FailureClass:         status.FailureClass,
		ReconnectAttempt:     status.ReconnectAttempt,
		NextRetryAt:          formatTimePtr(status.NextRetryAt),
		StartedAt:            formatTimePtr(status.StartedAt),
		ConnectedAt:          formatTimePtr(status.ConnectedAt),
		StoppedAt:            formatTimePtr(status.StoppedAt),
		UpdatedAt:            status.UpdatedAt.Format(time.RFC3339Nano),
		Details:              details,
	}, nil
}

func commandView(cmd ControlCommand) control.CommandView {
	var desired int64
	if cmd.DesiredGeneration != nil {
		desired = *cmd.DesiredGeneration
	}
	view := control.CommandView{
		CommandID:         cmd.CommandID,
		Source:            control.Source{Kind: control.SourceKind(cmd.Source)},
		Type:              control.CommandType(cmd.Type),
		TargetType:        cmd.TargetType,
		TargetID:          cmd.TargetID,
		Status:            control.CommandStatus(cmd.Status),
		DesiredGeneration: desired,
		ErrorCode:         cmd.ErrorCode,
		ErrorMessage:      cmd.ErrorMessage,
		ReceivedAt:        cmd.ReceivedAt.Format(time.RFC3339Nano),
		UpdatedAt:         cmd.UpdatedAt.Format(time.RFC3339Nano),
	}
	if cmd.AppliedAt != nil {
		view.AppliedAt = cmd.AppliedAt.Format(time.RFC3339Nano)
	}
	return view
}

func remoteStatusModel(update RemoteStatusUpdate, now time.Time) RemoteStatus {
	return RemoteStatus{
		RemoteID:             update.RemoteID,
		ObservedGeneration:   update.ObservedGeneration,
		ObservedRestartNonce: update.ObservedRestartNonce,
		Phase:                update.Phase,
		LastErrorCode:        update.LastErrorCode,
		LastErrorMessage:     update.LastErrorMessage,
		FailureClass:         update.FailureClass,
		ReconnectAttempt:     update.ReconnectAttempt,
		NextRetryAt:          update.NextRetryAt,
		ConnectedAt:          update.ConnectedAt,
		StoppedAt:            update.StoppedAt,
		UpdatedAt:            now,
	}
}

func agentConnectionStatusModel(update AgentConnectionStatusUpdate, now time.Time) AgentConnectionStatus {
	return AgentConnectionStatus{
		ConnectionID:         update.ConnectionID,
		ObservedGeneration:   update.ObservedGeneration,
		ObservedRestartNonce: update.ObservedRestartNonce,
		Phase:                update.Phase,
		PID:                  update.PID,
		LastErrorCode:        update.LastErrorCode,
		LastErrorMessage:     update.LastErrorMessage,
		FailureClass:         update.FailureClass,
		ReconnectAttempt:     update.ReconnectAttempt,
		NextRetryAt:          update.NextRetryAt,
		StartedAt:            update.StartedAt,
		ConnectedAt:          update.ConnectedAt,
		StoppedAt:            update.StoppedAt,
		UpdatedAt:            now,
		DetailsJSON:          stringDefault(update.DetailsJSON, "{}"),
	}
}

func remoteStatusUpdates(status RemoteStatus) map[string]any {
	return map[string]any{
		"observed_generation":    status.ObservedGeneration,
		"observed_restart_nonce": status.ObservedRestartNonce,
		"phase":                  status.Phase,
		"last_error_code":        status.LastErrorCode,
		"last_error_message":     status.LastErrorMessage,
		"failure_class":          status.FailureClass,
		"reconnect_attempt":      status.ReconnectAttempt,
		"next_retry_at":          status.NextRetryAt,
		"connected_at":           status.ConnectedAt,
		"stopped_at":             status.StoppedAt,
		"updated_at":             status.UpdatedAt,
	}
}

func agentConnectionStatusUpdates(status AgentConnectionStatus) map[string]any {
	return map[string]any{
		"observed_generation":    status.ObservedGeneration,
		"observed_restart_nonce": status.ObservedRestartNonce,
		"phase":                  status.Phase,
		"pid":                    status.PID,
		"last_error_code":        status.LastErrorCode,
		"last_error_message":     status.LastErrorMessage,
		"failure_class":          status.FailureClass,
		"reconnect_attempt":      status.ReconnectAttempt,
		"next_retry_at":          status.NextRetryAt,
		"started_at":             status.StartedAt,
		"connected_at":           status.ConnectedAt,
		"stopped_at":             status.StoppedAt,
		"updated_at":             status.UpdatedAt,
		"details_json":           status.DetailsJSON,
	}
}

func boolDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func stringDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func statusDefault(value, fallback control.CommandStatus) control.CommandStatus {
	if value == "" {
		return fallback
	}
	return value
}

func stringPtrOrNil(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func marshalJSON(value any, empty string) (string, error) {
	if value == nil {
		return empty, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func decodeStringSlice(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeStringMap(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeCloudflareAccess(raw string) (*control.CloudflareAccessAuth, error) {
	var payload struct {
		CloudflareAccess struct {
			ClientID        string `json:"clientId"`
			ClientSecretRef string `json:"clientSecretRef"`
		} `json:"cloudflareAccess"`
	}
	if err := json.Unmarshal([]byte(stringDefault(raw, "{}")), &payload); err != nil {
		return nil, err
	}
	return &control.CloudflareAccessAuth{
		ClientID:        payload.CloudflareAccess.ClientID,
		ClientSecretRef: payload.CloudflareAccess.ClientSecretRef,
	}, nil
}

func localSessionView(row LocalSession, metadata map[string]string, elements []control.LocalSessionElementView) control.LocalSessionView {
	return control.LocalSessionView{
		ID:           row.ID,
		Agent:        row.Agent,
		NativeID:     row.NativeID,
		Title:        row.Title,
		Status:       row.Status,
		Preview:      row.Preview,
		ProjectID:    row.ProjectID,
		UpdatedAt:    formatTimePtr(row.UpdatedAtPtr),
		LastActive:   formatTimePtr(row.LastActive),
		LastListedAt: row.LastListedAt.Format(time.RFC3339Nano),
		LastSyncedAt: formatTimePtr(row.LastSyncedAt),
		Metadata:     metadata,
		Elements:     elements,
	}
}

func parseTimePtr(value string) *time.Time {
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	return &parsed
}

func parseTimeOrNow(value string, now time.Time) time.Time {
	parsed := parseTimePtr(value)
	if parsed == nil {
		return now
	}
	return *parsed
}

func formatTimePtr(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}

func mapCreateErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) || strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
		return ErrDuplicate
	}
	return mapGormErr(err)
}
