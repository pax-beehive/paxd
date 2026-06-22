package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
)

func TestServiceRejectsInvalidCommandWithoutWake(t *testing.T) {
	store := openControlTestStore(t)
	wakes := &fakeSupervisors{}
	service := control.NewService(control.ServiceOptions{Store: store, Supervisors: wakes})

	ack, err := service.HandleCommand(context.Background(), control.Source{Kind: control.SourceLocal}, control.Command{
		CommandID: "cmd_bad",
		Type:      control.CommandAgentConnectionRestart,
	})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if ack.OK || ack.Status != control.CommandStatusRejected {
		t.Fatalf("ack = %+v, want rejected", ack)
	}
	if wakes.remote != 0 || wakes.agent != 0 {
		t.Fatalf("wakes = remote %d agent %d, want 0", wakes.remote, wakes.agent)
	}
}

func TestServiceCreatesRemoteRecordsCommandAndWakes(t *testing.T) {
	ctx := context.Background()
	store := openControlTestStore(t)
	wakes := &fakeSupervisors{}
	service := control.NewService(control.ServiceOptions{Store: store, Supervisors: wakes})

	cmd := createRemoteCommand("cmd_remote_create_1", "remote_prod", "https://api.example.test")
	ack, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, cmd)
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if !ack.OK || ack.Status != control.CommandStatusReceived || ack.TargetID != "remote_prod" {
		t.Fatalf("ack = %+v", ack)
	}
	if wakes.remote != 1 || wakes.agent != 0 {
		t.Fatalf("wakes = remote %d agent %d", wakes.remote, wakes.agent)
	}

	record, err := store.GetCommandRecord(ctx, "cmd_remote_create_1")
	if err != nil {
		t.Fatalf("GetCommandRecord() error = %v", err)
	}
	if record.Status != control.CommandStatusReceived || record.Type != control.CommandRemoteCreate {
		t.Fatalf("command record = %+v", record)
	}

	remotes, err := store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListRemotes() error = %v", err)
	}
	if len(remotes) != 1 || remotes[0].Generation != 1 {
		t.Fatalf("remotes = %+v", remotes)
	}
}

func TestServiceDuplicateCommandIDIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openControlTestStore(t)
	wakes := &fakeSupervisors{}
	service := control.NewService(control.ServiceOptions{Store: store, Supervisors: wakes})

	cmd := createRemoteCommand("cmd_remote_create_1", "remote_prod", "https://api.example.test")
	if _, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, cmd); err != nil {
		t.Fatalf("first HandleCommand() error = %v", err)
	}
	ack, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, cmd)
	if err != nil {
		t.Fatalf("duplicate HandleCommand() error = %v", err)
	}
	if !ack.OK || ack.Status != control.CommandStatusReceived {
		t.Fatalf("duplicate ack = %+v", ack)
	}
	if wakes.remote != 1 {
		t.Fatalf("remote wakes = %d, want one wake only", wakes.remote)
	}

	changed := createRemoteCommand("cmd_remote_create_1", "remote_other", "https://api2.example.test")
	ack, err = service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, changed)
	if err != nil {
		t.Fatalf("changed duplicate HandleCommand() error = %v", err)
	}
	if ack.OK || ack.Status != control.CommandStatusRejected || ack.Error == nil || ack.Error.Code != control.ErrCodeConflict {
		t.Fatalf("changed duplicate ack = %+v", ack)
	}
}

func TestServiceDuplicateRejectedCommandReturnsStoredAck(t *testing.T) {
	ctx := context.Background()
	store := openControlTestStore(t)
	service := control.NewService(control.ServiceOptions{Store: store})

	cmd := createRemoteCommand("cmd_remote_rejected", "remote_prod", "https://api.example.test")
	payload, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	generation := int64(12)
	if err := store.InsertCommand(ctx, control.CommandRecord{
		CommandID:         cmd.CommandID,
		Source:            control.Source{Kind: control.SourceLocal},
		Type:              cmd.Type,
		TargetType:        "remote",
		TargetID:          "remote_prod",
		PayloadJSON:       string(payload),
		Status:            control.CommandStatusRejected,
		DesiredGeneration: &generation,
		ErrorCode:         control.ErrCodeConflict,
		ErrorMessage:      "duplicate remote",
	}); err != nil {
		t.Fatalf("InsertCommand() error = %v", err)
	}

	ack, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, cmd)
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if ack.OK || ack.Status != control.CommandStatusRejected || ack.DesiredGeneration != generation {
		t.Fatalf("ack = %+v, want stored rejected ack", ack)
	}
	if ack.Error == nil || ack.Error.Code != control.ErrCodeConflict || ack.Error.Message != "duplicate remote" {
		t.Fatalf("ack error = %+v", ack.Error)
	}
}

func TestServiceCommandFailurePaths(t *testing.T) {
	ctx := context.Background()

	t.Run("missing store", func(t *testing.T) {
		service := control.NewService(control.ServiceOptions{})
		ack, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, createRemoteCommand("cmd_missing_store", "remote_prod", "https://api.example.test"))
		if err != nil {
			t.Fatalf("HandleCommand() error = %v", err)
		}
		if ack.OK || ack.Status != control.CommandStatusFailed || ack.Error == nil || ack.Error.Code != control.ErrCodeInternal {
			t.Fatalf("ack = %+v, want internal failed ack", ack)
		}
	})

	t.Run("command record lookup fails", func(t *testing.T) {
		store := &failingCommandRecordStore{Store: openControlTestStore(t), err: errors.New("lookup failed")}
		service := control.NewService(control.ServiceOptions{Store: store})
		ack, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, createRemoteCommand("cmd_lookup_fails", "remote_prod", "https://api.example.test"))
		if err != nil {
			t.Fatalf("HandleCommand() error = %v", err)
		}
		if ack.OK || ack.Status != control.CommandStatusFailed || ack.Error == nil || ack.Error.Code != control.ErrCodeInternal {
			t.Fatalf("ack = %+v, want internal failed ack", ack)
		}
	})
}

func TestServiceRejectsMissingRemoteAgentConnectionAndRollsBackCommand(t *testing.T) {
	ctx := context.Background()
	store := openControlTestStore(t)
	wakes := &fakeSupervisors{}
	service := control.NewService(control.ServiceOptions{Store: store, Supervisors: wakes})

	cmd := createAgentConnectionCommand("cmd_conn_create_1", "remote_missing", "conn_codex")
	ack, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, cmd)
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if ack.OK || ack.Status != control.CommandStatusRejected {
		t.Fatalf("ack = %+v, want rejected", ack)
	}
	if wakes.agent != 0 {
		t.Fatalf("agent wakes = %d, want 0", wakes.agent)
	}
	if _, err := store.GetCommandRecord(ctx, "cmd_conn_create_1"); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("GetCommandRecord() error = %v, want ErrNotFound", err)
	}
}

func TestServiceCreatesAgentConnectionAndWakes(t *testing.T) {
	ctx := context.Background()
	store := openControlTestStore(t)
	_, err := store.CreateRemote(ctx, *createRemoteCommand("seed_remote", "remote_prod", "https://api.example.test").CreateRemote)
	if err != nil {
		t.Fatalf("seed CreateRemote() error = %v", err)
	}
	wakes := &fakeSupervisors{}
	service := control.NewService(control.ServiceOptions{Store: store, Supervisors: wakes})

	cmd := createAgentConnectionCommand("cmd_conn_create_1", "remote_prod", "conn_codex")
	ack, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, cmd)
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if !ack.OK || ack.Status != control.CommandStatusReceived || ack.TargetID != "conn_codex" {
		t.Fatalf("ack = %+v", ack)
	}
	if wakes.remote != 0 || wakes.agent != 1 {
		t.Fatalf("wakes = remote %d agent %d", wakes.remote, wakes.agent)
	}
	conns, err := store.ListAgentConnections(ctx, control.ListAgentConnectionsQuery{IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListAgentConnections() error = %v", err)
	}
	if len(conns) != 1 || conns[0].CloudAgentID != "" {
		t.Fatalf("connections = %+v", conns)
	}
}

func TestServiceRemoteMutationCommands(t *testing.T) {
	ctx := context.Background()
	store := openControlTestStore(t)
	_, err := store.CreateRemote(ctx, *createRemoteCommand("seed_remote", "remote_prod", "https://api.example.test").CreateRemote)
	if err != nil {
		t.Fatalf("seed CreateRemote() error = %v", err)
	}
	wakes := &fakeSupervisors{}
	service := control.NewService(control.ServiceOptions{Store: store, Supervisors: wakes})

	newURL := "https://api2.example.test"
	commands := []control.Command{
		{
			CommandID: "cmd_remote_update",
			Type:      control.CommandRemoteUpdate,
			UpdateRemote: &control.UpdateRemoteCommand{
				RemoteID: "remote_prod",
				Remote: control.RemotePatch{
					CloudAPIURL: &newURL,
				},
			},
		},
		{
			CommandID:     "cmd_remote_restart",
			Type:          control.CommandRemoteRestart,
			RestartRemote: &control.RestartRemoteCommand{RemoteID: "remote_prod"},
		},
		{
			CommandID:    "cmd_remote_delete",
			Type:         control.CommandRemoteDelete,
			DeleteRemote: &control.DeleteRemoteCommand{RemoteID: "remote_prod"},
		},
		{
			CommandID:       "cmd_remote_auth_clear",
			Type:            control.CommandRemoteAuthClear,
			ClearRemoteAuth: &control.ClearRemoteAuthCommand{RemoteID: "remote_prod"},
		},
	}

	for _, cmd := range commands {
		ack, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, cmd)
		if err != nil {
			t.Fatalf("%s HandleCommand() error = %v", cmd.Type, err)
		}
		if !ack.OK || ack.Status != control.CommandStatusReceived {
			t.Fatalf("%s ack = %+v", cmd.Type, ack)
		}
	}
	if wakes.remote != 4 {
		t.Fatalf("remote wakes = %d, want 4", wakes.remote)
	}
	if wakes.agent != 1 {
		t.Fatalf("agent wakes = %d, want 1 for auth clear", wakes.agent)
	}
}

func TestServiceAgentConnectionMutationCommands(t *testing.T) {
	ctx := context.Background()
	store := openControlTestStore(t)
	_, err := store.CreateRemote(ctx, *createRemoteCommand("seed_remote", "remote_prod", "https://api.example.test").CreateRemote)
	if err != nil {
		t.Fatalf("seed CreateRemote() error = %v", err)
	}
	_, err = store.CreateAgentConnection(ctx, *createAgentConnectionCommand("seed_conn", "remote_prod", "conn_codex").CreateAgentConnection)
	if err != nil {
		t.Fatalf("seed CreateAgentConnection() error = %v", err)
	}
	wakes := &fakeSupervisors{}
	service := control.NewService(control.ServiceOptions{Store: store, Supervisors: wakes})

	workingDir := "/tmp/project"
	commands := []control.Command{
		{
			CommandID: "cmd_conn_update",
			Type:      control.CommandAgentConnectionUpdate,
			UpdateAgentConnection: &control.UpdateAgentConnectionCommand{
				ConnectionID: "conn_codex",
				WorkingDir:   &workingDir,
			},
		},
		{
			CommandID:              "cmd_conn_restart",
			Type:                   control.CommandAgentConnectionRestart,
			RestartAgentConnection: &control.RestartAgentConnectionCommand{ConnectionID: "conn_codex"},
		},
		{
			CommandID:             "cmd_conn_delete",
			Type:                  control.CommandAgentConnectionDelete,
			DeleteAgentConnection: &control.DeleteAgentConnectionCommand{ConnectionID: "conn_codex"},
		},
	}

	for _, cmd := range commands {
		ack, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, cmd)
		if err != nil {
			t.Fatalf("%s HandleCommand() error = %v", cmd.Type, err)
		}
		if !ack.OK || ack.Status != control.CommandStatusReceived {
			t.Fatalf("%s ack = %+v", cmd.Type, ack)
		}
	}
	if wakes.agent != 3 {
		t.Fatalf("agent wakes = %d, want 3", wakes.agent)
	}
}

func TestServiceRemoteAuthWakesBothSupervisors(t *testing.T) {
	ctx := context.Background()
	store := openControlTestStore(t)
	_, err := store.CreateRemote(ctx, *createRemoteCommand("seed_remote", "remote_prod", "https://api.example.test").CreateRemote)
	if err != nil {
		t.Fatalf("seed CreateRemote() error = %v", err)
	}
	wakes := &fakeSupervisors{}
	service := control.NewService(control.ServiceOptions{Store: store, Supervisors: wakes})

	ack, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, control.Command{
		CommandID: "cmd_auth_1",
		Type:      control.CommandRemoteAuthConfigure,
		ConfigureRemoteAuth: &control.ConfigureRemoteAuthCommand{
			RemoteID: "remote_prod",
			Kind:     control.RemoteAuthCloudflareAccess,
			CloudflareAccess: &control.CloudflareAccessAuth{
				ClientID:        "client_id",
				ClientSecretRef: "env:PAX_CF_SECRET",
			},
		},
	})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if !ack.OK {
		t.Fatalf("ack = %+v", ack)
	}
	if wakes.remote != 1 || wakes.agent != 1 {
		t.Fatalf("wakes = remote %d agent %d", wakes.remote, wakes.agent)
	}
	record, err := store.GetCommandRecord(ctx, "cmd_auth_1")
	if err != nil {
		t.Fatalf("GetCommandRecord() error = %v", err)
	}
	if record.PayloadJSON == "" || contains(record.PayloadJSON, "super-secret") {
		t.Fatalf("payload json leaked resolved secret: %s", record.PayloadJSON)
	}
}

func TestServiceQueries(t *testing.T) {
	ctx := context.Background()
	store := openControlTestStore(t)
	_, err := store.CreateRemote(ctx, *createRemoteCommand("seed_remote", "remote_prod", "https://api.example.test").CreateRemote)
	if err != nil {
		t.Fatalf("seed CreateRemote() error = %v", err)
	}
	harnesses := &fakeHarnessRegistry{items: []control.HarnessView{{Harness: "codex", DisplayName: "Codex", State: "available"}}}
	sessions := &fakeLocalSessions{items: []control.LocalSessionView{{ID: "codex:sess_1", Agent: "codex", NativeID: "sess_1"}}}
	service := control.NewService(control.ServiceOptions{Store: store, Harnesses: harnesses, LocalSessions: sessions})

	remotes, err := service.HandleQuery(ctx, control.Source{Kind: control.SourceLocal}, control.Query{
		Type:        control.QueryRemotesList,
		ListRemotes: &control.ListRemotesQuery{IncludeDisabled: true},
	})
	if err != nil {
		t.Fatalf("HandleQuery(remotes) error = %v", err)
	}
	if remotes.Remotes == nil || len(remotes.Remotes.Items) != 1 {
		t.Fatalf("remotes result = %+v", remotes)
	}

	discovered, err := service.HandleQuery(ctx, control.Source{Kind: control.SourceLocal}, control.Query{
		Type:              control.QueryHarnessesDiscover,
		DiscoverHarnesses: &control.DiscoverHarnessesQuery{Probe: true},
	})
	if err != nil {
		t.Fatalf("HandleQuery(discover) error = %v", err)
	}
	if discovered.Harnesses == nil || len(discovered.Harnesses.Items) != 1 || !harnesses.discovered {
		t.Fatalf("discover result = %+v discovered=%v", discovered, harnesses.discovered)
	}

	sync, err := service.HandleQuery(ctx, control.Source{Kind: control.SourceLocal}, control.Query{
		Type:              control.QueryLocalSessionsSync,
		SyncLocalSessions: &control.SyncLocalSessionsQuery{Agent: "codex"},
	})
	if err != nil {
		t.Fatalf("HandleQuery(sync) error = %v", err)
	}
	if sync.LocalSessionSync == nil || sync.LocalSessionSync.Synced != 1 || !sessions.synced {
		t.Fatalf("sync result = %+v synced=%v", sync, sessions.synced)
	}
}

func TestServiceAdditionalQueries(t *testing.T) {
	ctx := context.Background()
	store := openControlTestStore(t)
	_, err := store.CreateRemote(ctx, *createRemoteCommand("seed_remote", "remote_prod", "https://api.example.test").CreateRemote)
	if err != nil {
		t.Fatalf("seed CreateRemote() error = %v", err)
	}
	_, err = store.CreateAgentConnection(ctx, *createAgentConnectionCommand("seed_conn", "remote_prod", "conn_codex").CreateAgentConnection)
	if err != nil {
		t.Fatalf("seed CreateAgentConnection() error = %v", err)
	}
	if err := store.InsertCommand(ctx, control.CommandRecord{
		CommandID: "cmd_seen",
		Source:    control.Source{Kind: control.SourceLocal},
		Type:      control.CommandRemoteCreate,
		Status:    control.CommandStatusReceived,
	}); err != nil {
		t.Fatalf("InsertCommand() error = %v", err)
	}
	harnesses := &fakeHarnessRegistry{items: []control.HarnessView{{Harness: "codex", DisplayName: "Codex", State: "available"}}}
	sessions := &fakeLocalSessions{items: []control.LocalSessionView{{ID: "codex:sess_1", Agent: "codex", NativeID: "sess_1"}}}
	service := control.NewService(control.ServiceOptions{Store: store, Harnesses: harnesses, LocalSessions: sessions})

	queries := []struct {
		name  string
		query control.Query
		check func(t *testing.T, result control.QueryResult)
	}{
		{
			name:  "status",
			query: control.Query{Type: control.QueryStatusGet, GetStatus: &control.GetStatusQuery{}},
			check: func(t *testing.T, result control.QueryResult) {
				if result.Status == nil || result.Status.Phase != "running" {
					t.Fatalf("status result = %+v", result)
				}
			},
		},
		{
			name:  "get remote",
			query: control.Query{Type: control.QueryRemoteGet, GetRemote: &control.GetRemoteQuery{RemoteID: "remote_prod"}},
			check: func(t *testing.T, result control.QueryResult) {
				if result.Remote == nil || result.Remote.Remote.ID != "remote_prod" {
					t.Fatalf("remote result = %+v", result)
				}
			},
		},
		{
			name:  "get agent connection",
			query: control.Query{Type: control.QueryAgentConnectionGet, GetAgentConnection: &control.GetAgentConnectionQuery{ConnectionID: "conn_codex"}},
			check: func(t *testing.T, result control.QueryResult) {
				if result.AgentConnection == nil || result.AgentConnection.ID != "conn_codex" {
					t.Fatalf("agent connection result = %+v", result)
				}
			},
		},
		{
			name:  "list harnesses",
			query: control.Query{Type: control.QueryHarnessesList, ListHarnesses: &control.ListHarnessesQuery{}},
			check: func(t *testing.T, result control.QueryResult) {
				if result.Harnesses == nil || len(result.Harnesses.Items) != 1 {
					t.Fatalf("harnesses result = %+v", result)
				}
			},
		},
		{
			name:  "local overview",
			query: control.Query{Type: control.QueryLocalOverviewGet, GetLocalOverview: &control.GetLocalOverviewQuery{}},
			check: func(t *testing.T, result control.QueryResult) {
				if result.LocalOverview == nil || len(result.LocalOverview.Harnesses) != 1 || len(result.LocalOverview.Sessions) != 1 {
					t.Fatalf("local overview result = %+v", result)
				}
			},
		},
		{
			name:  "list local sessions",
			query: control.Query{Type: control.QueryLocalSessionsList, ListLocalSessions: &control.ListLocalSessionsQuery{Agent: "codex"}},
			check: func(t *testing.T, result control.QueryResult) {
				if result.LocalSessions == nil || len(result.LocalSessions.Items) != 1 {
					t.Fatalf("local sessions result = %+v", result)
				}
			},
		},
		{
			name:  "get local session",
			query: control.Query{Type: control.QueryLocalSessionGet, GetLocalSession: &control.GetLocalSessionQuery{SessionID: "codex:sess_1"}},
			check: func(t *testing.T, result control.QueryResult) {
				if result.LocalSession == nil || result.LocalSession.ID != "codex:sess_1" {
					t.Fatalf("local session result = %+v", result)
				}
			},
		},
		{
			name:  "get command",
			query: control.Query{Type: control.QueryCommandGet, GetCommand: &control.GetCommandQuery{CommandID: "cmd_seen"}},
			check: func(t *testing.T, result control.QueryResult) {
				if result.Command == nil || result.Command.CommandID != "cmd_seen" {
					t.Fatalf("command result = %+v", result)
				}
			},
		},
	}

	for _, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			result, err := service.HandleQuery(ctx, control.Source{Kind: control.SourceLocal}, tc.query)
			if err != nil {
				t.Fatalf("HandleQuery() error = %v", err)
			}
			if result.Error != nil {
				t.Fatalf("HandleQuery() result error = %+v", result.Error)
			}
			tc.check(t, result)
		})
	}
}

func TestServiceGetQueriesReturnNotFound(t *testing.T) {
	ctx := context.Background()
	store := openControlTestStore(t)
	service := control.NewService(control.ServiceOptions{Store: store, LocalSessions: &fakeLocalSessions{}})

	for _, query := range []control.Query{
		{Type: control.QueryRemoteGet, GetRemote: &control.GetRemoteQuery{RemoteID: "missing"}},
		{Type: control.QueryAgentConnectionGet, GetAgentConnection: &control.GetAgentConnectionQuery{ConnectionID: "missing"}},
		{Type: control.QueryLocalSessionGet, GetLocalSession: &control.GetLocalSessionQuery{SessionID: "missing"}},
	} {
		result, err := service.HandleQuery(ctx, control.Source{Kind: control.SourceLocal}, query)
		if err != nil {
			t.Fatalf("HandleQuery() error = %v", err)
		}
		if result.Error == nil || result.Error.Code != control.ErrCodeNotFound {
			t.Fatalf("%s result = %+v, want not_found", query.Type, result)
		}
	}
}

func TestServiceQueriesReturnInternalErrorsForMissingPorts(t *testing.T) {
	ctx := context.Background()
	store := openControlTestStore(t)
	service := control.NewService(control.ServiceOptions{Store: store})

	queries := []control.Query{
		{Type: control.QueryHarnessesList, ListHarnesses: &control.ListHarnessesQuery{}},
		{Type: control.QueryHarnessesDiscover, DiscoverHarnesses: &control.DiscoverHarnessesQuery{}},
		{Type: control.QueryLocalOverviewGet, GetLocalOverview: &control.GetLocalOverviewQuery{}},
		{Type: control.QueryLocalSessionsList, ListLocalSessions: &control.ListLocalSessionsQuery{}},
		{Type: control.QueryLocalSessionsSync, SyncLocalSessions: &control.SyncLocalSessionsQuery{}},
		{Type: control.QueryLocalSessionGet, GetLocalSession: &control.GetLocalSessionQuery{SessionID: "codex:sess_1"}},
	}

	for _, query := range queries {
		t.Run(string(query.Type), func(t *testing.T) {
			result, err := service.HandleQuery(ctx, control.Source{Kind: control.SourceLocal}, query)
			if err != nil {
				t.Fatalf("HandleQuery() error = %v", err)
			}
			if result.Error == nil || result.Error.Code != control.ErrCodeInternal {
				t.Fatalf("result = %+v, want internal error", result)
			}
		})
	}
}

func TestServiceQueriesSurfacePortErrors(t *testing.T) {
	ctx := context.Background()
	store := openControlTestStore(t)
	wantErr := errors.New("port unavailable")

	tests := []struct {
		name    string
		service *control.ControlService
		query   control.Query
	}{
		{
			name:    "list harnesses",
			service: control.NewService(control.ServiceOptions{Store: store, Harnesses: &fakeHarnessRegistry{err: wantErr}}),
			query:   control.Query{Type: control.QueryHarnessesList, ListHarnesses: &control.ListHarnessesQuery{}},
		},
		{
			name:    "discover harnesses",
			service: control.NewService(control.ServiceOptions{Store: store, Harnesses: &fakeHarnessRegistry{discoverErr: wantErr}}),
			query:   control.Query{Type: control.QueryHarnessesDiscover, DiscoverHarnesses: &control.DiscoverHarnessesQuery{}},
		},
		{
			name:    "local overview harnesses",
			service: control.NewService(control.ServiceOptions{Store: store, Harnesses: &fakeHarnessRegistry{err: wantErr}, LocalSessions: &fakeLocalSessions{}}),
			query:   control.Query{Type: control.QueryLocalOverviewGet, GetLocalOverview: &control.GetLocalOverviewQuery{}},
		},
		{
			name:    "local overview sessions",
			service: control.NewService(control.ServiceOptions{Store: store, Harnesses: &fakeHarnessRegistry{}, LocalSessions: &fakeLocalSessions{listErr: wantErr}}),
			query:   control.Query{Type: control.QueryLocalOverviewGet, GetLocalOverview: &control.GetLocalOverviewQuery{}},
		},
		{
			name:    "list sessions",
			service: control.NewService(control.ServiceOptions{Store: store, LocalSessions: &fakeLocalSessions{listErr: wantErr}}),
			query:   control.Query{Type: control.QueryLocalSessionsList, ListLocalSessions: &control.ListLocalSessionsQuery{}},
		},
		{
			name:    "sync sessions",
			service: control.NewService(control.ServiceOptions{Store: store, LocalSessions: &fakeLocalSessions{syncErr: wantErr}}),
			query:   control.Query{Type: control.QueryLocalSessionsSync, SyncLocalSessions: &control.SyncLocalSessionsQuery{}},
		},
		{
			name:    "get session",
			service: control.NewService(control.ServiceOptions{Store: store, LocalSessions: &fakeLocalSessions{getErr: wantErr}}),
			query:   control.Query{Type: control.QueryLocalSessionGet, GetLocalSession: &control.GetLocalSessionQuery{SessionID: "codex:sess_1"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.service.HandleQuery(ctx, control.Source{Kind: control.SourceLocal}, tc.query)
			if err != nil {
				t.Fatalf("HandleQuery() error = %v", err)
			}
			if result.Error == nil || result.Error.Code != control.ErrCodeInternal {
				t.Fatalf("result = %+v, want internal error", result)
			}
		})
	}
}

func TestServiceQueryMissingStore(t *testing.T) {
	service := control.NewService(control.ServiceOptions{})
	result, err := service.HandleQuery(context.Background(), control.Source{Kind: control.SourceLocal}, control.Query{
		Type:        control.QueryRemotesList,
		ListRemotes: &control.ListRemotesQuery{},
	})
	if err != nil {
		t.Fatalf("HandleQuery() error = %v", err)
	}
	if result.Error == nil || result.Error.Code != control.ErrCodeInternal {
		t.Fatalf("result = %+v, want internal error", result)
	}
}

type failingCommandRecordStore struct {
	*daemonstore.Store
	err error
}

func (s *failingCommandRecordStore) GetCommandRecord(ctx context.Context, commandID string) (*control.CommandRecord, error) {
	_ = ctx
	_ = commandID
	return nil, s.err
}

type fakeSupervisors struct {
	remote int
	agent  int
}

func (f *fakeSupervisors) WakeRemotes() {
	f.remote++
}

func (f *fakeSupervisors) WakeAgentConnections() {
	f.agent++
}

type fakeHarnessRegistry struct {
	items       []control.HarnessView
	err         error
	discoverErr error
	discovered  bool
}

func (f *fakeHarnessRegistry) ListCached(ctx context.Context) ([]control.HarnessView, error) {
	_ = ctx
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

func (f *fakeHarnessRegistry) Discover(ctx context.Context, req control.DiscoverHarnessesQuery) ([]control.HarnessView, error) {
	_ = ctx
	_ = req
	if f.discoverErr != nil {
		return nil, f.discoverErr
	}
	f.discovered = true
	return f.items, nil
}

type fakeLocalSessions struct {
	items   []control.LocalSessionView
	listErr error
	syncErr error
	getErr  error
	synced  bool
}

func (f *fakeLocalSessions) List(ctx context.Context, query control.ListLocalSessionsQuery) ([]control.LocalSessionView, error) {
	_ = ctx
	_ = query
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.items, nil
}

func (f *fakeLocalSessions) Sync(ctx context.Context, query control.SyncLocalSessionsQuery) (control.LocalSessionSyncResult, error) {
	_ = ctx
	_ = query
	if f.syncErr != nil {
		return control.LocalSessionSyncResult{}, f.syncErr
	}
	f.synced = true
	return control.LocalSessionSyncResult{Synced: len(f.items)}, nil
}

func (f *fakeLocalSessions) Get(ctx context.Context, query control.GetLocalSessionQuery) (*control.LocalSessionView, error) {
	_ = ctx
	if f.getErr != nil {
		return nil, f.getErr
	}
	for _, item := range f.items {
		if item.ID == query.SessionID {
			return &item, nil
		}
	}
	return nil, control.ErrNotFound
}

func openControlTestStore(t *testing.T) *daemonstore.Store {
	t.Helper()
	store, err := daemonstore.OpenSQLite(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return store
}

func createRemoteCommand(commandID, remoteID, url string) control.Command {
	enabled := true
	return control.Command{
		CommandID: commandID,
		Type:      control.CommandRemoteCreate,
		CreateRemote: &control.CreateRemoteCommand{
			Remote: control.Remote{
				ID:          remoteID,
				Name:        remoteID,
				CloudAPIURL: url,
				Enabled:     &enabled,
			},
			CloudAPIKeyRef: "env:PAX_NODE_KEY",
		},
	}
}

func createAgentConnectionCommand(commandID, remoteID, connectionID string) control.Command {
	return control.Command{
		CommandID: commandID,
		Type:      control.CommandAgentConnectionCreate,
		CreateAgentConnection: &control.CreateAgentConnectionCommand{
			ID:         connectionID,
			RemoteID:   remoteID,
			Name:       "codex-main",
			InstanceID: "inst_1",
			AgentType:  "codex",
			Harness:    "codex",
			Command:    []string{"codex", "--acp"},
		},
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
