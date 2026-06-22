package control

import "testing"

func TestCommandValidateAcceptsMatchingPayload(t *testing.T) {
	cmd := Command{
		CommandID: "cmd_restart_1",
		Type:      CommandAgentConnectionRestart,
		RestartAgentConnection: &RestartAgentConnectionCommand{
			ConnectionID: "conn_codex",
		},
	}

	if err := cmd.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestCommandStatusValid(t *testing.T) {
	valid := []CommandStatus{
		CommandStatusUnknown,
		CommandStatusReceived,
		CommandStatusRejected,
		CommandStatusApplied,
		CommandStatusFailed,
	}
	for _, status := range valid {
		if !status.Valid() {
			t.Fatalf("%q.Valid() = false", status)
		}
	}
	if CommandStatus("mystery").Valid() {
		t.Fatal(`CommandStatus("mystery").Valid() = true`)
	}
}

func TestCommandAckValidate(t *testing.T) {
	tests := []struct {
		name    string
		ack     CommandAck
		wantErr bool
	}{
		{
			name: "received ok",
			ack: CommandAck{
				CommandID: "cmd_1",
				OK:        true,
				Status:    CommandStatusReceived,
			},
		},
		{
			name: "rejected non ok",
			ack: CommandAck{
				CommandID: "cmd_1",
				OK:        false,
				Status:    CommandStatusRejected,
				Error:     &ControlError{Code: ErrCodeInvalidArgument, Message: "bad command"},
			},
		},
		{
			name: "ok failed contradiction",
			ack: CommandAck{
				CommandID: "cmd_1",
				OK:        true,
				Status:    CommandStatusFailed,
			},
			wantErr: true,
		},
		{
			name: "non ok without error",
			ack: CommandAck{
				CommandID: "cmd_1",
				OK:        false,
				Status:    CommandStatusRejected,
			},
			wantErr: true,
		},
		{
			name: "unknown wire status",
			ack: CommandAck{
				CommandID: "cmd_1",
				OK:        false,
				Status:    CommandStatus("wire_mystery"),
				Error:     &ControlError{Code: "unknown", Message: "unknown status"},
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.ack.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestControlErrorError(t *testing.T) {
	tests := []struct {
		name string
		err  ControlError
		want string
	}{
		{
			name: "message only",
			err:  ControlError{Message: "bad input"},
			want: "bad input",
		},
		{
			name: "code only",
			err:  ControlError{Code: ErrCodeInternal},
			want: ErrCodeInternal,
		},
		{
			name: "code and message",
			err:  ControlError{Code: ErrCodeInvalidArgument, Message: "bad input"},
			want: ErrCodeInvalidArgument + ": bad input",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCommandValidateRejectsInvalidOneof(t *testing.T) {
	validUpdate := UpdateAgentConnectionCommand{ConnectionID: "conn_codex", Enabled: boolPtr(false)}

	tests := []struct {
		name string
		cmd  Command
	}{
		{
			name: "no payload",
			cmd: Command{
				CommandID: "cmd_restart_1",
				Type:      CommandAgentConnectionRestart,
			},
		},
		{
			name: "multiple payloads",
			cmd: Command{
				CommandID:              "cmd_restart_1",
				Type:                   CommandAgentConnectionRestart,
				RestartAgentConnection: &RestartAgentConnectionCommand{ConnectionID: "conn_codex"},
				UpdateAgentConnection:  &validUpdate,
			},
		},
		{
			name: "payload type mismatch",
			cmd: Command{
				CommandID:             "cmd_restart_1",
				Type:                  CommandAgentConnectionRestart,
				UpdateAgentConnection: &validUpdate,
			},
		},
		{
			name: "missing command id",
			cmd: Command{
				Type:                   CommandAgentConnectionRestart,
				RestartAgentConnection: &RestartAgentConnectionCommand{ConnectionID: "conn_codex"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cmd.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
		})
	}
}

func TestCommandValidateRejectsInvalidPayloadFields(t *testing.T) {
	badDesiredState := DesiredState("sideways")

	tests := []struct {
		name string
		cmd  Command
	}{
		{
			name: "create remote missing url",
			cmd: Command{
				CommandID: "cmd_remote_create_1",
				Type:      CommandRemoteCreate,
				CreateRemote: &CreateRemoteCommand{
					Remote: Remote{Name: "Production"},
				},
			},
		},
		{
			name: "update agent connection empty command",
			cmd: Command{
				CommandID: "cmd_conn_update_1",
				Type:      CommandAgentConnectionUpdate,
				UpdateAgentConnection: &UpdateAgentConnectionCommand{
					ConnectionID: "conn_codex",
					Command:      &[]string{},
				},
			},
		},
		{
			name: "remote auth missing secret ref",
			cmd: Command{
				CommandID: "cmd_auth_1",
				Type:      CommandRemoteAuthConfigure,
				ConfigureRemoteAuth: &ConfigureRemoteAuthCommand{
					RemoteID: "remote_prod",
					Kind:     RemoteAuthCloudflareAccess,
					CloudflareAccess: &CloudflareAccessAuth{
						ClientID: "client_id",
					},
				},
			},
		},
		{
			name: "remote auth none with config",
			cmd: Command{
				CommandID: "cmd_auth_2",
				Type:      CommandRemoteAuthConfigure,
				ConfigureRemoteAuth: &ConfigureRemoteAuthCommand{
					RemoteID: "remote_prod",
					Kind:     RemoteAuthNone,
					CloudflareAccess: &CloudflareAccessAuth{
						ClientID:        "client_id",
						ClientSecretRef: "secret_ref",
					},
				},
			},
		},
		{
			name: "remote auth unsupported kind",
			cmd: Command{
				CommandID: "cmd_auth_3",
				Type:      CommandRemoteAuthConfigure,
				ConfigureRemoteAuth: &ConfigureRemoteAuthCommand{
					RemoteID: "remote_prod",
					Kind:     RemoteAuthKind("other"),
				},
			},
		},
		{
			name: "update remote empty patch",
			cmd: Command{
				CommandID:    "cmd_remote_update_1",
				Type:         CommandRemoteUpdate,
				UpdateRemote: &UpdateRemoteCommand{RemoteID: "remote_prod"},
			},
		},
		{
			name: "delete remote missing id",
			cmd: Command{
				CommandID:    "cmd_remote_delete_1",
				Type:         CommandRemoteDelete,
				DeleteRemote: &DeleteRemoteCommand{},
			},
		},
		{
			name: "restart remote missing id",
			cmd: Command{
				CommandID:     "cmd_remote_restart_1",
				Type:          CommandRemoteRestart,
				RestartRemote: &RestartRemoteCommand{},
			},
		},
		{
			name: "delete agent connection missing id",
			cmd: Command{
				CommandID:             "cmd_conn_delete_1",
				Type:                  CommandAgentConnectionDelete,
				DeleteAgentConnection: &DeleteAgentConnectionCommand{},
			},
		},
		{
			name: "restart agent connection missing id",
			cmd: Command{
				CommandID:              "cmd_conn_restart_1",
				Type:                   CommandAgentConnectionRestart,
				RestartAgentConnection: &RestartAgentConnectionCommand{},
			},
		},
		{
			name: "create agent invalid desired state",
			cmd: Command{
				CommandID: "cmd_conn_create_1",
				Type:      CommandAgentConnectionCreate,
				CreateAgentConnection: &CreateAgentConnectionCommand{
					RemoteID:     "remote_prod",
					Name:         "codex-main",
					InstanceID:   "inst_1",
					AgentType:    "codex",
					Harness:      "codex",
					Command:      []string{"codex"},
					DesiredState: DesiredState("sideways"),
				},
			},
		},
		{
			name: "update agent invalid desired state",
			cmd: Command{
				CommandID: "cmd_conn_update_2",
				Type:      CommandAgentConnectionUpdate,
				UpdateAgentConnection: &UpdateAgentConnectionCommand{
					ConnectionID: "conn_codex",
					DesiredState: &badDesiredState,
				},
			},
		},
		{
			name: "upgrade paxd missing target",
			cmd: Command{
				CommandID:   "cmd_upgrade_1",
				Type:        CommandUpgradePaxd,
				UpgradePaxd: &UpgradePaxdCommand{},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cmd.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
		})
	}
}

func TestCommandPayloadValidateAcceptsValidFields(t *testing.T) {
	name := "Prod"
	url := "https://api.example.test"
	nodeID := "node_1"
	enabled := false
	isDefault := true
	key := "env:PAX_API_KEY"
	workingDir := "/tmp/project"
	desiredState := DesiredStateRunning
	command := []string{"codex", "--acp"}

	tests := []struct {
		name    string
		payload validatable
	}{
		{
			name: "update remote patch",
			payload: UpdateRemoteCommand{
				RemoteID: "remote_prod",
				Remote: RemotePatch{
					Name:        &name,
					CloudAPIURL: &url,
					NodeID:      &nodeID,
					Enabled:     &enabled,
					IsDefault:   &isDefault,
				},
			},
		},
		{
			name:    "update remote api key only",
			payload: UpdateRemoteCommand{RemoteID: "remote_prod", CloudAPIKey: &key},
		},
		{
			name:    "delete remote",
			payload: DeleteRemoteCommand{RemoteID: "remote_prod"},
		},
		{
			name:    "restart remote",
			payload: RestartRemoteCommand{RemoteID: "remote_prod"},
		},
		{
			name:    "remote auth none",
			payload: ConfigureRemoteAuthCommand{RemoteID: "remote_prod", Kind: RemoteAuthNone},
		},
		{
			name: "remote auth cloudflare",
			payload: ConfigureRemoteAuthCommand{
				RemoteID: "remote_prod",
				Kind:     RemoteAuthCloudflareAccess,
				CloudflareAccess: &CloudflareAccessAuth{
					ClientID:        "client_id",
					ClientSecretRef: "env:PAX_CF_SECRET",
				},
			},
		},
		{
			name:    "clear remote auth",
			payload: ClearRemoteAuthCommand{RemoteID: "remote_prod"},
		},
		{
			name: "create agent connection",
			payload: CreateAgentConnectionCommand{
				RemoteID:     "remote_prod",
				Name:         "codex-main",
				InstanceID:   "inst_1",
				AgentType:    "codex",
				Harness:      "codex",
				Command:      command,
				DesiredState: desiredState,
			},
		},
		{
			name: "update agent connection",
			payload: UpdateAgentConnectionCommand{
				ConnectionID: "conn_codex",
				Command:      &command,
				WorkingDir:   &workingDir,
				DesiredState: &desiredState,
			},
		},
		{
			name:    "delete agent connection",
			payload: DeleteAgentConnectionCommand{ConnectionID: "conn_codex"},
		},
		{
			name:    "restart agent connection",
			payload: RestartAgentConnectionCommand{ConnectionID: "conn_codex"},
		},
		{
			name:    "upgrade paxd",
			payload: UpgradePaxdCommand{Version: "v1.2.3"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.payload.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestCommandPayloadValidateRejectsMissingRequiredFields(t *testing.T) {
	badDesiredState := DesiredState("sideways")
	emptyCommand := []string{}
	blankCommand := []string{"codex", " "}

	tests := []struct {
		name    string
		payload validatable
	}{
		{name: "create remote missing name", payload: CreateRemoteCommand{Remote: Remote{CloudAPIURL: "https://api.example.test"}}},
		{name: "update remote missing id", payload: UpdateRemoteCommand{Remote: RemotePatch{Name: stringPtr("Prod")}}},
		{name: "configure auth missing remote id", payload: ConfigureRemoteAuthCommand{Kind: RemoteAuthNone}},
		{name: "configure auth missing cloudflare config", payload: ConfigureRemoteAuthCommand{RemoteID: "remote_prod", Kind: RemoteAuthCloudflareAccess}},
		{
			name: "configure auth missing client id",
			payload: ConfigureRemoteAuthCommand{
				RemoteID: "remote_prod",
				Kind:     RemoteAuthCloudflareAccess,
				CloudflareAccess: &CloudflareAccessAuth{
					ClientSecretRef: "env:PAX_CF_SECRET",
				},
			},
		},
		{name: "clear auth missing remote id", payload: ClearRemoteAuthCommand{}},
		{name: "create agent missing remote id", payload: CreateAgentConnectionCommand{Name: "codex-main", InstanceID: "inst_1", AgentType: "codex", Harness: "codex", Command: []string{"codex"}}},
		{name: "create agent missing name", payload: CreateAgentConnectionCommand{RemoteID: "remote_prod", InstanceID: "inst_1", AgentType: "codex", Harness: "codex", Command: []string{"codex"}}},
		{name: "create agent missing instance id", payload: CreateAgentConnectionCommand{RemoteID: "remote_prod", Name: "codex-main", AgentType: "codex", Harness: "codex", Command: []string{"codex"}}},
		{name: "create agent missing agent type", payload: CreateAgentConnectionCommand{RemoteID: "remote_prod", Name: "codex-main", InstanceID: "inst_1", Harness: "codex", Command: []string{"codex"}}},
		{name: "create agent missing harness", payload: CreateAgentConnectionCommand{RemoteID: "remote_prod", Name: "codex-main", InstanceID: "inst_1", AgentType: "codex", Command: []string{"codex"}}},
		{name: "create agent missing command", payload: CreateAgentConnectionCommand{RemoteID: "remote_prod", Name: "codex-main", InstanceID: "inst_1", AgentType: "codex", Harness: "codex"}},
		{name: "create agent blank command word", payload: CreateAgentConnectionCommand{RemoteID: "remote_prod", Name: "codex-main", InstanceID: "inst_1", AgentType: "codex", Harness: "codex", Command: blankCommand}},
		{name: "update agent missing id", payload: UpdateAgentConnectionCommand{Name: stringPtr("codex-main")}},
		{name: "update agent empty patch", payload: UpdateAgentConnectionCommand{ConnectionID: "conn_codex"}},
		{name: "update agent empty command", payload: UpdateAgentConnectionCommand{ConnectionID: "conn_codex", Command: &emptyCommand}},
		{name: "update agent blank command word", payload: UpdateAgentConnectionCommand{ConnectionID: "conn_codex", Command: &blankCommand}},
		{name: "update agent invalid desired state", payload: UpdateAgentConnectionCommand{ConnectionID: "conn_codex", DesiredState: &badDesiredState}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.payload.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
		})
	}
}

func TestQueryValidateAcceptsMatchingPayload(t *testing.T) {
	query := Query{
		Type:        QueryRemotesList,
		ListRemotes: &ListRemotesQuery{IncludeDisabled: true},
	}

	if err := query.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestQueryPayloadValidateAcceptsValidFields(t *testing.T) {
	tests := []struct {
		name    string
		payload validatable
	}{
		{name: "status", payload: GetStatusQuery{}},
		{name: "list remotes", payload: ListRemotesQuery{IncludeDisabled: true}},
		{name: "get remote", payload: GetRemoteQuery{RemoteID: "remote_prod"}},
		{name: "list agent connections", payload: ListAgentConnectionsQuery{RemoteID: "remote_prod", IncludeDisabled: true}},
		{name: "get agent connection", payload: GetAgentConnectionQuery{ConnectionID: "conn_codex"}},
		{name: "list harnesses", payload: ListHarnessesQuery{IncludeMissing: true}},
		{name: "discover harnesses", payload: DiscoverHarnessesQuery{Probe: true, Names: []string{"codex"}}},
		{name: "local overview", payload: GetLocalOverviewQuery{}},
		{name: "list local sessions", payload: ListLocalSessionsQuery{Agent: "codex", Limit: 10}},
		{name: "sync local sessions", payload: SyncLocalSessionsQuery{Agent: "codex", Limit: 10, TimeoutMillis: 1000}},
		{name: "get local session", payload: GetLocalSessionQuery{SessionID: "codex:sess_1"}},
		{name: "get command", payload: GetCommandQuery{CommandID: "cmd_1"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.payload.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestQueryValidateRejectsInvalidOneof(t *testing.T) {
	tests := []struct {
		name  string
		query Query
	}{
		{
			name:  "no payload",
			query: Query{Type: QueryRemotesList},
		},
		{
			name: "multiple payloads",
			query: Query{
				Type:              QueryRemotesList,
				ListRemotes:       &ListRemotesQuery{},
				DiscoverHarnesses: &DiscoverHarnessesQuery{},
			},
		},
		{
			name: "payload type mismatch",
			query: Query{
				Type:              QueryRemotesList,
				DiscoverHarnesses: &DiscoverHarnessesQuery{},
			},
		},
		{
			name: "missing required field",
			query: Query{
				Type:       QueryCommandGet,
				GetCommand: &GetCommandQuery{},
			},
		},
		{
			name: "get remote missing id",
			query: Query{
				Type:      QueryRemoteGet,
				GetRemote: &GetRemoteQuery{},
			},
		},
		{
			name: "get agent connection missing id",
			query: Query{
				Type:               QueryAgentConnectionGet,
				GetAgentConnection: &GetAgentConnectionQuery{},
			},
		},
		{
			name: "discover harness empty name",
			query: Query{
				Type:              QueryHarnessesDiscover,
				DiscoverHarnesses: &DiscoverHarnessesQuery{Names: []string{""}},
			},
		},
		{
			name: "discover harness surrounding whitespace",
			query: Query{
				Type:              QueryHarnessesDiscover,
				DiscoverHarnesses: &DiscoverHarnessesQuery{Names: []string{" codex "}},
			},
		},
		{
			name: "list local sessions negative limit",
			query: Query{
				Type:              QueryLocalSessionsList,
				ListLocalSessions: &ListLocalSessionsQuery{Limit: -1},
			},
		},
		{
			name: "sync local sessions negative timeout",
			query: Query{
				Type:              QueryLocalSessionsSync,
				SyncLocalSessions: &SyncLocalSessionsQuery{TimeoutMillis: -1},
			},
		},
		{
			name: "get local session missing id",
			query: Query{
				Type:            QueryLocalSessionGet,
				GetLocalSession: &GetLocalSessionQuery{},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.query.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
		})
	}
}

func boolPtr(value bool) *bool {
	return &value
}

func stringPtr(value string) *string {
	return &value
}
