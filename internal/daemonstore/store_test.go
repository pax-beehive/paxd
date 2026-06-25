package daemonstore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestMigrateCreatesTargetTablesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate() error = %v", err)
	}
	for _, table := range []string{
		"remote",
		"remote_auth",
		"remote_status",
		"agent_connection",
		"agent_connection_status",
		"control_command",
		"harness_inventory",
		"local_session",
		"local_session_element",
		"messages",
		"message_parts",
		"setting",
	} {
		if !store.DB().Migrator().HasTable(table) {
			t.Fatalf("missing migrated table %q", table)
		}
	}
	for _, index := range []struct {
		model any
		name  string
	}{
		{&AgentConnection{}, "idx_agent_connection_remote_name"},
		{&AgentConnection{}, "idx_agent_connection_remote_cloud_agent"},
		{&LocalSession{}, "idx_local_session_agent_native"},
		{&LocalSessionElement{}, "idx_local_session_element_session_seq"},
		{&Message{}, "idx_messages_message_id"},
		{&Message{}, "idx_messages_logical_key"},
		{&MessagePart{}, "idx_message_parts_message_part"},
	} {
		if !store.DB().Migrator().HasIndex(index.model, index.name) {
			t.Fatalf("missing migrated index %q", index.name)
		}
	}
	if store.DB().Migrator().HasTable("transport_journal") {
		t.Fatal("daemonstore migration created transport_journal")
	}

	_, err := store.CreateRemote(ctx, createRemoteCommand("remote_prod", "https://api.example.test"))
	if err != nil {
		t.Fatalf("CreateRemote() error = %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
	remotes, err := store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListRemotes() error = %v", err)
	}
	if len(remotes) != 1 {
		t.Fatalf("remotes length = %d, want 1", len(remotes))
	}
}

func TestOpenSQLite(t *testing.T) {
	store, err := OpenSQLite(t.TempDir() + "/daemonstore.db")
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if !store.DB().Migrator().HasTable("remote") {
		t.Fatal("OpenSQLite store did not migrate remote table")
	}
}

func TestOpenSQLiteConfiguresSQLiteForSingleWriter(t *testing.T) {
	store, err := OpenSQLite(t.TempDir() + "/daemonstore.db")
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v", err)
	}
	sqlDB, err := store.DB().DB()
	if err != nil {
		t.Fatalf("get sql db: %v", err)
	}
	defer sqlDB.Close()

	if got := sqlDB.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", got)
	}

	var busyTimeout int
	if err := sqlDB.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}

	var journalMode string
	if err := sqlDB.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}
}

func TestMigrateDropsRetiredRemoteSchema(t *testing.T) {
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "legacy.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open legacy sqlite: %v", err)
	}
	if err := db.Exec(`
		CREATE TABLE remote (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			cloud_api_url TEXT NOT NULL,
			node_control_path TEXT NOT NULL DEFAULT '/api/v1/node/control',
			agent_tunnel_path TEXT NOT NULL DEFAULT '/api/v1/agent/tunnel',
			node_id TEXT,
			cloud_api_key_ref TEXT,
			enabled INTEGER NOT NULL,
			is_default INTEGER NOT NULL DEFAULT 0,
			generation INTEGER NOT NULL DEFAULT 1,
			restart_nonce INTEGER NOT NULL DEFAULT 0,
			registered_at TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE UNIQUE INDEX idx_remote_cloud_api_url ON remote(cloud_api_url);
	`).Error; err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	store := New(db)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if store.DB().Migrator().HasColumn(&Remote{}, "is_default") {
		t.Fatal("Migrate() left retired remote.is_default column")
	}
	if store.DB().Migrator().HasIndex(&Remote{}, "idx_remote_cloud_api_url") {
		t.Fatal("Migrate() left retired remote cloud_api_url unique index")
	}
}

func TestOpenSQLiteCreatesMissingParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "nested", "daemonstore.db")

	store, err := OpenSQLite(path)

	require.NoError(t, err)
	require.NoError(t, store.Migrate(context.Background()))
	assert.FileExists(t, path)
}

func TestRemoteRepositoryCreateUpdateRestartAndDuplicateID(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	remote, err := store.CreateRemote(ctx, createRemoteCommand("remote_prod", "https://api.example.test"))
	if err != nil {
		t.Fatalf("CreateRemote() error = %v", err)
	}
	if remote.Generation != 1 || remote.RestartNonce != 0 {
		t.Fatalf("remote version = generation %d nonce %d", remote.Generation, remote.RestartNonce)
	}

	if _, err := store.CreateRemote(ctx, createRemoteCommand("remote_prod", "https://api2.example.test")); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate id CreateRemote() error = %v, want ErrDuplicate", err)
	}

	newURL := "https://api2.example.test"
	updated, err := store.UpdateRemote(ctx, control.UpdateRemoteCommand{
		RemoteID: "remote_prod",
		Remote: control.RemotePatch{
			CloudAPIURL: &newURL,
		},
	})
	if err != nil {
		t.Fatalf("UpdateRemote() error = %v", err)
	}
	if updated.Generation != 2 || updated.RestartNonce != 0 {
		t.Fatalf("updated version = generation %d nonce %d", updated.Generation, updated.RestartNonce)
	}

	restarted, err := store.RestartRemote(ctx, control.RestartRemoteCommand{RemoteID: "remote_prod"})
	if err != nil {
		t.Fatalf("RestartRemote() error = %v", err)
	}
	if restarted.Generation != 2 || restarted.RestartNonce != 1 {
		t.Fatalf("restarted version = generation %d nonce %d", restarted.Generation, restarted.RestartNonce)
	}

	deleted, err := store.DeleteRemote(ctx, control.DeleteRemoteCommand{RemoteID: "remote_prod"})
	if err != nil {
		t.Fatalf("DeleteRemote() error = %v", err)
	}
	if deleted.Generation != 3 || deleted.Remote.Enabled == nil || *deleted.Remote.Enabled {
		t.Fatalf("deleted remote = %+v", deleted)
	}
	remotes, err := store.ListRemotes(ctx, control.ListRemotesQuery{})
	if err != nil {
		t.Fatalf("ListRemotes(enabled) error = %v", err)
	}
	if len(remotes) != 0 {
		t.Fatalf("enabled remotes = %+v, want none", remotes)
	}
	remotes, err = store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListRemotes(all) error = %v", err)
	}
	if len(remotes) != 1 {
		t.Fatalf("all remotes = %+v, want deleted remote included", remotes)
	}
}

func TestRemoteRepositoryAllowsSameCloudAPIURL(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	if _, err := store.CreateRemote(ctx, createRemoteCommand("remote_prod", "https://api.example.test")); err != nil {
		t.Fatalf("CreateRemote(remote_prod) error = %v", err)
	}
	if _, err := store.CreateRemote(ctx, createRemoteCommand("remote_other", "https://api.example.test")); err != nil {
		t.Fatalf("CreateRemote(remote_other same cloud URL) error = %v", err)
	}
	remotes, err := store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListRemotes() error = %v", err)
	}
	if len(remotes) != 2 {
		t.Fatalf("remotes = %+v, want both remotes with same cloud URL", remotes)
	}
}

func TestRemoteAuthMaterialAndStatusViews(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	_, err := store.CreateRemote(ctx, createRemoteCommand("remote_prod", "https://api.example.test"))
	if err != nil {
		t.Fatalf("CreateRemote() error = %v", err)
	}

	if err := store.ConfigureRemoteAuth(ctx, control.ConfigureRemoteAuthCommand{
		RemoteID: "remote_prod",
		Kind:     control.RemoteAuthCloudflareAccess,
		CloudflareAccess: &control.CloudflareAccessAuth{
			ClientID:        "cf-client",
			ClientSecretRef: "env:PAX_CF_SECRET",
		},
	}); err != nil {
		t.Fatalf("ConfigureRemoteAuth() error = %v", err)
	}
	material, err := store.GetRemoteAuthMaterial(ctx, "remote_prod")
	if err != nil {
		t.Fatalf("GetRemoteAuthMaterial() error = %v", err)
	}
	if material.CloudAPIKeyRef != "env:PAX_NODE_KEY" || material.AuthKind != control.RemoteAuthCloudflareAccess {
		t.Fatalf("auth material = %+v", material)
	}
	if material.CloudflareAccess == nil || material.CloudflareAccess.ClientSecretRef != "env:PAX_CF_SECRET" {
		t.Fatalf("cloudflare material = %+v", material.CloudflareAccess)
	}
	remotes, err := store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListRemotes(auth view) error = %v", err)
	}
	if len(remotes) != 1 || remotes[0].Auth == nil || remotes[0].Auth.ClientID != "cf-client" || remotes[0].Auth.ClientSecretRef != "env:PAX_CF_SECRET" {
		t.Fatalf("remote auth view = %+v", remotes)
	}
	if err := store.ClearRemoteAuth(ctx, control.ClearRemoteAuthCommand{RemoteID: "remote_prod"}); err != nil {
		t.Fatalf("ClearRemoteAuth() error = %v", err)
	}
	material, err = store.GetRemoteAuthMaterial(ctx, "remote_prod")
	if err != nil {
		t.Fatalf("GetRemoteAuthMaterial(after clear) error = %v", err)
	}
	if material.AuthKind != control.RemoteAuthNone || material.CloudflareAccess != nil {
		t.Fatalf("cleared material = %+v", material)
	}

	nextRetry := time.Date(2026, 6, 21, 12, 1, 0, 0, time.UTC)
	if err := store.UpsertRemoteStatus(ctx, RemoteStatusUpdate{
		RemoteID:             "remote_prod",
		ObservedGeneration:   1,
		ObservedRestartNonce: 0,
		Phase:                "backoff",
		FailureClass:         "transient",
		ReconnectAttempt:     2,
		NextRetryAt:          &nextRetry,
	}); err != nil {
		t.Fatalf("UpsertRemoteStatus() error = %v", err)
	}
	remotes, err = store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListRemotes() error = %v", err)
	}
	if len(remotes) != 1 || remotes[0].Auth == nil || remotes[0].Auth.Kind != control.RemoteAuthNone {
		t.Fatalf("remote auth view = %+v", remotes)
	}
	if remotes[0].Status == nil || remotes[0].Status.Phase != "backoff" || remotes[0].Status.NextRetryAt == "" {
		t.Fatalf("remote status view = %+v", remotes[0].Status)
	}
}

func TestRemoteRuntimeRegistrationIsConditionalAndDoesNotBumpGeneration(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	_, err := store.CreateRemote(ctx, createRemoteCommand("remote_prod", "https://api.example.test"))
	if err != nil {
		t.Fatalf("CreateRemote() error = %v", err)
	}

	registeredAt := time.Date(2026, 6, 21, 12, 2, 0, 0, time.UTC)
	ok, err := store.MarkRemoteRegistered(ctx, RemoteRegistrationUpdate{
		RemoteID:           "remote_prod",
		ObservedGeneration: 1,
		NodeID:             "node_runtime",
		RegisteredAt:       registeredAt,
	})
	if err != nil {
		t.Fatalf("MarkRemoteRegistered() error = %v", err)
	}
	if !ok {
		t.Fatal("MarkRemoteRegistered() ok = false")
	}
	remotes, err := store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListRemotes() error = %v", err)
	}
	if remotes[0].Remote.NodeID != "node_runtime" || remotes[0].Generation != 1 {
		t.Fatalf("remote after registration = %+v", remotes[0])
	}

	name := "Prod 2"
	if _, err := store.UpdateRemote(ctx, control.UpdateRemoteCommand{
		RemoteID: "remote_prod",
		Remote:   control.RemotePatch{Name: &name},
	}); err != nil {
		t.Fatalf("UpdateRemote() error = %v", err)
	}
	ok, err = store.MarkRemoteRegistered(ctx, RemoteRegistrationUpdate{
		RemoteID:           "remote_prod",
		ObservedGeneration: 1,
		NodeID:             "stale_node",
	})
	if err != nil {
		t.Fatalf("stale MarkRemoteRegistered() error = %v", err)
	}
	if ok {
		t.Fatal("stale MarkRemoteRegistered() ok = true")
	}
	remotes, err = store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListRemotes(after stale) error = %v", err)
	}
	if remotes[0].Remote.NodeID != "node_runtime" || remotes[0].Generation != 2 {
		t.Fatalf("remote after stale registration = %+v", remotes[0])
	}
}

func TestAgentConnectionRepository(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	_, err := store.CreateRemote(ctx, createRemoteCommand("remote_prod", "https://api.example.test"))
	if err != nil {
		t.Fatalf("CreateRemote() error = %v", err)
	}

	create := control.CreateAgentConnectionCommand{
		ID:           "conn_codex",
		RemoteID:     "remote_prod",
		Name:         "codex-main",
		CloudAgentID: "agent_1",
		InstanceID:   "inst_1",
		AgentType:    "codex",
		Harness:      "codex",
		Command:      []string{"codex", "--acp"},
		Env:          map[string]string{"PAX_PROFILE": "prod"},
	}
	conn, err := store.CreateAgentConnection(ctx, create)
	if err != nil {
		t.Fatalf("CreateAgentConnection() error = %v", err)
	}
	if conn.Generation != 1 || conn.RestartNonce != 0 || conn.DesiredState != control.DesiredStateRunning {
		t.Fatalf("connection view = %+v", conn)
	}

	if _, err := store.CreateAgentConnection(ctx, create); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate CreateAgentConnection() error = %v, want ErrDuplicate", err)
	}

	if _, err := store.CreateAgentConnection(ctx, control.CreateAgentConnectionCommand{
		ID:         "conn_missing_remote",
		RemoteID:   "remote_missing",
		Name:       "missing",
		InstanceID: "inst_2",
		AgentType:  "codex",
		Harness:    "codex",
		Command:    []string{"codex"},
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing remote CreateAgentConnection() error = %v, want ErrNotFound", err)
	}

	workingDir := "/tmp/project"
	updated, err := store.UpdateAgentConnection(ctx, control.UpdateAgentConnectionCommand{
		ConnectionID: "conn_codex",
		WorkingDir:   &workingDir,
	})
	if err != nil {
		t.Fatalf("UpdateAgentConnection() error = %v", err)
	}
	if updated.Generation != 2 || updated.RestartNonce != 0 || updated.WorkingDir != workingDir {
		t.Fatalf("updated connection = %+v", updated)
	}

	restarted, err := store.RestartAgentConnection(ctx, control.RestartAgentConnectionCommand{ConnectionID: "conn_codex"})
	if err != nil {
		t.Fatalf("RestartAgentConnection() error = %v", err)
	}
	if restarted.Generation != 2 || restarted.RestartNonce != 1 {
		t.Fatalf("restarted connection = %+v", restarted)
	}

	newName := "codex-secondary"
	newCloudAgentID := "agent_2"
	newInstanceID := "inst_2"
	newAgentType := "codex"
	newHarness := "codex"
	newCommand := []string{"codex", "--json"}
	newEnv := map[string]string{"PAX_PROFILE": "dev"}
	enabled := false
	desiredState := control.DesiredStateStopped
	updatedAll, err := store.UpdateAgentConnection(ctx, control.UpdateAgentConnectionCommand{
		ConnectionID: "conn_codex",
		Name:         &newName,
		CloudAgentID: &newCloudAgentID,
		InstanceID:   &newInstanceID,
		AgentType:    &newAgentType,
		Harness:      &newHarness,
		Command:      &newCommand,
		Env:          &newEnv,
		Enabled:      &enabled,
		DesiredState: &desiredState,
	})
	if err != nil {
		t.Fatalf("UpdateAgentConnection(all fields) error = %v", err)
	}
	if updatedAll.Generation != 3 || updatedAll.CloudAgentID != "agent_2" || updatedAll.Command[1] != "--json" || updatedAll.Enabled {
		t.Fatalf("updated all connection = %+v", updatedAll)
	}

	deleted, err := store.DeleteAgentConnection(ctx, control.DeleteAgentConnectionCommand{ConnectionID: "conn_codex"})
	if err != nil {
		t.Fatalf("DeleteAgentConnection() error = %v", err)
	}
	if deleted.Generation != 4 || deleted.Enabled || deleted.DesiredState != control.DesiredStateDeleted {
		t.Fatalf("deleted connection = %+v", deleted)
	}
	conns, err := store.ListAgentConnections(ctx, control.ListAgentConnectionsQuery{})
	if err != nil {
		t.Fatalf("ListAgentConnections(enabled) error = %v", err)
	}
	if len(conns) != 0 {
		t.Fatalf("enabled connections = %+v, want none", conns)
	}
	conns, err = store.ListAgentConnections(ctx, control.ListAgentConnectionsQuery{IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListAgentConnections(all) error = %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("all connections = %+v, want deleted connection included", conns)
	}
}

func TestDesiredSpecsStatusViewsAndRuntimeBinding(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	remoteCommand := createRemoteCommand("remote_prod", "https://api.example.test")
	remoteCommand.Remote.NodeControlPath = "/api/v2/node/control"
	remoteCommand.Remote.AgentTunnelPath = "/api/v2/agent/tunnel"
	_, err := store.CreateRemote(ctx, remoteCommand)
	if err != nil {
		t.Fatalf("CreateRemote(remote_prod) error = %v", err)
	}
	disabled := false
	_, err = store.CreateRemote(ctx, control.CreateRemoteCommand{
		Remote: control.Remote{
			ID:          "remote_disabled",
			Name:        "Remote disabled",
			CloudAPIURL: "https://disabled.example.test",
			Enabled:     &disabled,
		},
	})
	if err != nil {
		t.Fatalf("CreateRemote(remote_disabled) error = %v", err)
	}
	_, err = store.CreateAgentConnection(ctx, control.CreateAgentConnectionCommand{
		ID:         "conn_codex",
		RemoteID:   "remote_prod",
		Name:       "codex-main",
		InstanceID: "inst_1",
		AgentType:  "codex",
		Harness:    "codex",
		Command:    []string{"codex", "--acp"},
		Env:        map[string]string{"PAX_PROFILE": "prod"},
	})
	if err != nil {
		t.Fatalf("CreateAgentConnection() error = %v", err)
	}

	remoteSpecs, err := store.ListDesiredRemotes(ctx)
	if err != nil {
		t.Fatalf("ListDesiredRemotes() error = %v", err)
	}
	if len(remoteSpecs) != 1 || remoteSpecs[0].RemoteID != "remote_prod" || remoteSpecs[0].NodeControlPath != "/api/v2/node/control" {
		t.Fatalf("remote specs = %+v", remoteSpecs)
	}
	connSpecs, err := store.ListDesiredAgentConnections(ctx)
	if err != nil {
		t.Fatalf("ListDesiredAgentConnections() error = %v", err)
	}
	if len(connSpecs) != 1 || connSpecs[0].ConnectionID != "conn_codex" || connSpecs[0].Env["PAX_PROFILE"] != "prod" || connSpecs[0].TunnelPath != "/api/v2/agent/tunnel" {
		t.Fatalf("connection specs = %+v", connSpecs)
	}

	ok, err := store.SetAgentConnectionCloudAgentID(ctx, AgentConnectionBindingUpdate{
		ConnectionID:       "conn_codex",
		ObservedGeneration: 1,
		CloudAgentID:       "agent_runtime",
	})
	if err != nil {
		t.Fatalf("SetAgentConnectionCloudAgentID() error = %v", err)
	}
	if !ok {
		t.Fatal("SetAgentConnectionCloudAgentID() ok = false")
	}
	conns, err := store.ListAgentConnections(ctx, control.ListAgentConnectionsQuery{IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListAgentConnections() error = %v", err)
	}
	if conns[0].CloudAgentID != "agent_runtime" || conns[0].Generation != 1 {
		t.Fatalf("connection after runtime binding = %+v", conns[0])
	}

	workingDir := "/tmp/new"
	if _, err := store.UpdateAgentConnection(ctx, control.UpdateAgentConnectionCommand{
		ConnectionID: "conn_codex",
		WorkingDir:   &workingDir,
	}); err != nil {
		t.Fatalf("UpdateAgentConnection() error = %v", err)
	}
	ok, err = store.SetAgentConnectionCloudAgentID(ctx, AgentConnectionBindingUpdate{
		ConnectionID:       "conn_codex",
		ObservedGeneration: 1,
		CloudAgentID:       "stale_agent",
	})
	if err != nil {
		t.Fatalf("stale SetAgentConnectionCloudAgentID() error = %v", err)
	}
	if ok {
		t.Fatal("stale SetAgentConnectionCloudAgentID() ok = true")
	}

	pid := 123
	if err := store.UpsertAgentConnectionStatus(ctx, AgentConnectionStatusUpdate{
		ConnectionID:         "conn_codex",
		ObservedGeneration:   2,
		ObservedRestartNonce: 0,
		Phase:                "running",
		PID:                  &pid,
		DetailsJSON:          `{"exit_code":"0"}`,
	}); err != nil {
		t.Fatalf("UpsertAgentConnectionStatus() error = %v", err)
	}
	conns, err = store.ListAgentConnections(ctx, control.ListAgentConnectionsQuery{IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListAgentConnections(with status) error = %v", err)
	}
	if conns[0].Status == nil || conns[0].Status.Phase != "running" || conns[0].Status.PID != 123 || conns[0].Status.Details["exit_code"] != "0" {
		t.Fatalf("connection status view = %+v", conns[0].Status)
	}
}

func TestCommandRecordsAndCompletion(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	err := store.InsertCommand(ctx, CommandRecord{
		CommandID:   "cmd_1",
		Source:      control.Source{Kind: control.SourceLocal},
		Type:        control.CommandRemoteCreate,
		TargetType:  "remote",
		TargetID:    "remote_prod",
		PayloadJSON: `{"remote":{"id":"remote_prod"}}`,
		Status:      control.CommandStatusReceived,
	})
	if err != nil {
		t.Fatalf("InsertCommand() error = %v", err)
	}
	if err := store.InsertCommand(ctx, CommandRecord{CommandID: "cmd_1", Source: control.Source{Kind: control.SourceLocal}, Type: control.CommandRemoteCreate}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate InsertCommand() error = %v, want ErrDuplicate", err)
	}

	gen := int64(2)
	if err := store.CompleteCommand(ctx, "cmd_1", CommandCompletion{
		Status:            control.CommandStatusApplied,
		DesiredGeneration: &gen,
		ResultJSON:        `{"ok":true}`,
	}); err != nil {
		t.Fatalf("CompleteCommand() error = %v", err)
	}
	view, err := store.GetCommand(ctx, "cmd_1")
	if err != nil {
		t.Fatalf("GetCommand() error = %v", err)
	}
	if view.Status != control.CommandStatusApplied || view.DesiredGeneration != 2 || view.AppliedAt == "" {
		t.Fatalf("command view = %+v", view)
	}
	record, err := store.GetCommandRecord(ctx, "cmd_1")
	if err != nil {
		t.Fatalf("GetCommandRecord() error = %v", err)
	}
	if record.PayloadJSON != `{"remote":{"id":"remote_prod"}}` || record.DesiredGeneration == nil || *record.DesiredGeneration != 2 {
		t.Fatalf("command record = %+v", record)
	}
	if err := store.CompleteCommand(ctx, "cmd_missing", CommandCompletion{Status: control.CommandStatusFailed}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing CompleteCommand() error = %v, want ErrNotFound", err)
	}
}

func TestWithTxRollsBackDesiredMutationAndCommand(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	err := store.WithTx(ctx, func(tx control.TxStore) error {
		if err := tx.InsertCommand(ctx, CommandRecord{
			CommandID: "cmd_rollback",
			Source:    control.Source{Kind: control.SourceLocal},
			Type:      control.CommandRemoteCreate,
			Status:    control.CommandStatusReceived,
		}); err != nil {
			return err
		}
		if _, err := tx.CreateRemote(ctx, createRemoteCommand("remote_rollback", "https://rollback.example.test")); err != nil {
			return err
		}
		return errors.New("force rollback")
	})
	if err == nil {
		t.Fatal("WithTx() error = nil, want rollback error")
	}
	if _, err := store.GetCommand(ctx, "cmd_rollback"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetCommand(after rollback) error = %v, want ErrNotFound", err)
	}
	remotes, err := store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListRemotes() error = %v", err)
	}
	if len(remotes) != 0 {
		t.Fatalf("remotes after rollback = %+v, want none", remotes)
	}
}

func TestConditionalStatusPreventsStaleOverwrite(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	ok, err := store.ConditionalRemoteStatusUpdate(ctx, RemoteStatusUpdate{
		RemoteID:             "remote_prod",
		ObservedGeneration:   3,
		ObservedRestartNonce: 0,
		Phase:                "connected",
	})
	if err != nil {
		t.Fatalf("ConditionalRemoteStatusUpdate() error = %v", err)
	}
	if !ok {
		t.Fatal("initial ConditionalRemoteStatusUpdate() ok = false")
	}
	ok, err = store.ConditionalRemoteStatusUpdate(ctx, RemoteStatusUpdate{
		RemoteID:             "remote_prod",
		ObservedGeneration:   2,
		ObservedRestartNonce: 0,
		Phase:                "failed",
	})
	if err != nil {
		t.Fatalf("stale ConditionalRemoteStatusUpdate() error = %v", err)
	}
	if ok {
		t.Fatal("stale ConditionalRemoteStatusUpdate() ok = true")
	}

	ok, err = store.ConditionalAgentConnectionStatusUpdate(ctx, AgentConnectionStatusUpdate{
		ConnectionID:         "conn_codex",
		ObservedGeneration:   1,
		ObservedRestartNonce: 4,
		Phase:                "running",
	})
	if err != nil {
		t.Fatalf("ConditionalAgentConnectionStatusUpdate() error = %v", err)
	}
	if !ok {
		t.Fatal("initial ConditionalAgentConnectionStatusUpdate() ok = false")
	}
	ok, err = store.ConditionalAgentConnectionStatusUpdate(ctx, AgentConnectionStatusUpdate{
		ConnectionID:         "conn_codex",
		ObservedGeneration:   1,
		ObservedRestartNonce: 3,
		Phase:                "failed",
	})
	if err != nil {
		t.Fatalf("stale ConditionalAgentConnectionStatusUpdate() error = %v", err)
	}
	if ok {
		t.Fatal("stale ConditionalAgentConnectionStatusUpdate() ok = true")
	}
}

func TestHarnessSessionsAndSettings(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	if err := store.UpsertHarnesses(ctx, []control.HarnessView{{
		Harness:     "codex",
		DisplayName: "Codex",
		State:       "available",
		Capability:  "acp",
		Command:     []string{"codex"},
	}}); err != nil {
		t.Fatalf("UpsertHarnesses() error = %v", err)
	}
	harnesses, err := store.ListHarnesses(ctx)
	if err != nil {
		t.Fatalf("ListHarnesses() error = %v", err)
	}
	if len(harnesses) != 1 || harnesses[0].Harness != "codex" {
		t.Fatalf("harnesses = %+v", harnesses)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := store.UpsertSessions(ctx, []control.LocalSessionView{{
		Agent:        "codex",
		NativeID:     "sess_1",
		Title:        "Test session",
		LastListedAt: now,
		Metadata:     map[string]string{"project": "paxd"},
	}}); err != nil {
		t.Fatalf("UpsertSessions() error = %v", err)
	}
	sessions, err := store.ListSessions(ctx, control.ListLocalSessionsQuery{Agent: "codex"})
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "codex:sess_1" {
		t.Fatalf("sessions = %+v", sessions)
	}

	if err := store.ReplaceSessionElements(ctx, "codex:sess_1", []LocalSessionElement{
		{Seq: 1, Kind: "message", Role: "user", Text: "hi"},
		{Seq: 2, Kind: "message", Role: "assistant", Text: "hello"},
	}); err != nil {
		t.Fatalf("ReplaceSessionElements() error = %v", err)
	}
	detail, err := store.GetSession(ctx, "codex:sess_1")
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if len(detail.Elements) != 2 {
		t.Fatalf("elements = %+v", detail.Elements)
	}
	if err := store.ReplaceSessionElements(ctx, "codex:sess_1", []LocalSessionElement{{Seq: 1, Kind: "message", Role: "user", Text: "new"}}); err != nil {
		t.Fatalf("second ReplaceSessionElements() error = %v", err)
	}
	detail, err = store.GetSession(ctx, "codex:sess_1")
	if err != nil {
		t.Fatalf("second GetSession() error = %v", err)
	}
	if len(detail.Elements) != 1 || detail.Elements[0].Text != "new" {
		t.Fatalf("replaced elements = %+v", detail.Elements)
	}

	value, err := store.GetSetting(ctx, "daemon.debug_http_addr", `"disabled"`)
	if err != nil {
		t.Fatalf("GetSetting(default) error = %v", err)
	}
	if value != `"disabled"` {
		t.Fatalf("default setting = %q", value)
	}
	if err := store.UpsertSetting(ctx, "daemon.debug_http_addr", `"127.0.0.1:8765"`); err != nil {
		t.Fatalf("UpsertSetting() error = %v", err)
	}
	value, err = store.GetSetting(ctx, "daemon.debug_http_addr", `"disabled"`)
	if err != nil {
		t.Fatalf("GetSetting() error = %v", err)
	}
	if value != `"127.0.0.1:8765"` {
		t.Fatalf("setting = %q", value)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/daemonstore.db"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	store := New(db, WithClock(func() time.Time { return now }))
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return store
}

func createRemoteCommand(id string, url string) control.CreateRemoteCommand {
	enabled := true
	return control.CreateRemoteCommand{
		Remote: control.Remote{
			ID:          id,
			Name:        id,
			CloudAPIURL: url,
			NodeID:      "node_1",
			Enabled:     &enabled,
		},
		CloudAPIKeyRef: "env:PAX_NODE_KEY",
	}
}
