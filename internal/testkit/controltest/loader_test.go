package controltest

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
)

func TestLoadCommandRequest(t *testing.T) {
	cmd := LoadCommandRequest(t, "agent_connection_create")

	if cmd.CommandID != "cmd_agent_connection_create_1" {
		t.Fatalf("CommandID = %q", cmd.CommandID)
	}
	if cmd.CreateAgentConnection == nil {
		t.Fatal("CreateAgentConnection = nil")
	}
	if got := cmd.CreateAgentConnection.Command; len(got) != 2 || got[0] != "codex" || got[1] != "--acp" {
		t.Fatalf("Command = %#v", got)
	}
}

func TestLoadRemoteCreateCommandRequest(t *testing.T) {
	cmd := LoadCommandRequest(t, "remote_create")

	if cmd.CreateRemote == nil {
		t.Fatal("CreateRemote = nil")
	}
	if cmd.CreateRemote.Remote.ID != "remote_prod" {
		t.Fatalf("Remote.ID = %q", cmd.CreateRemote.Remote.ID)
	}
	if cmd.CreateRemote.CloudAPIKeyRef != "env:PAX_NODE_KEY" {
		t.Fatalf("CloudAPIKeyRef = %q", cmd.CreateRemote.CloudAPIKeyRef)
	}
}

func TestLoadCommandResponse(t *testing.T) {
	ack := LoadCommandResponse(t, "agent_connection_create")

	if ack.CommandID != "cmd_agent_connection_create_1" {
		t.Fatalf("CommandID = %q", ack.CommandID)
	}
	if !ack.OK {
		t.Fatal("OK = false")
	}
}

func TestLoadRemoteCreateCommandResponse(t *testing.T) {
	ack := LoadCommandResponse(t, "remote_create")

	if ack.CommandID != "cmd_remote_create_1" {
		t.Fatalf("CommandID = %q", ack.CommandID)
	}
	if ack.Result == nil || ack.Result.Remote == nil {
		t.Fatalf("Result.Remote = %+v", ack.Result)
	}
	if ack.Result.Remote.Remote.ID != "remote_prod" {
		t.Fatalf("Result.Remote.Remote.ID = %q", ack.Result.Remote.Remote.ID)
	}
}

func TestLoadRejectedCommandResponse(t *testing.T) {
	ack := LoadCommandResponse(t, "agent_connection_restart")

	if ack.OK {
		t.Fatal("OK = true, want false")
	}
	if ack.Status != control.CommandStatusRejected {
		t.Fatalf("Status = %q", ack.Status)
	}
	if ack.Error == nil || ack.Error.Code != "not_found" {
		t.Fatalf("Error = %+v", ack.Error)
	}
}

func TestLoadQueryRequest(t *testing.T) {
	query := LoadQueryRequest(t, "remotes_list")

	if query.Type != control.QueryRemotesList {
		t.Fatalf("Type = %q", query.Type)
	}
	if query.ListRemotes == nil || !query.ListRemotes.IncludeDisabled {
		t.Fatalf("ListRemotes = %+v", query.ListRemotes)
	}
}

func TestLoadQueryResponse(t *testing.T) {
	result := LoadQueryResponse(t, "remotes_list")

	if result.Type != control.QueryRemotesList {
		t.Fatalf("Type = %q", result.Type)
	}
	if result.Remotes == nil || len(result.Remotes.Items) != 1 {
		t.Fatalf("Remotes = %+v", result.Remotes)
	}
}

func TestLoadJSONFailsFastWithFixturePath(t *testing.T) {
	path := GoldenPath(t, "control", "invalid_fixture_for_test.json")
	if err := os.WriteFile(path, []byte(`{"command_id":`), 0o600); err != nil {
		t.Fatalf("write invalid fixture: %v", err)
	}
	defer os.Remove(path)

	fake := &recordingTB{}
	_ = loadJSON[control.Command](fake, path)

	if !fake.failed {
		t.Fatal("loadJSON did not fail test")
	}
	if !strings.Contains(fake.message, path) {
		t.Fatalf("failure %q does not include path %q", fake.message, path)
	}
}

type recordingTB struct {
	testing.TB
	failed  bool
	message string
}

func (tb *recordingTB) Helper() {}

func (tb *recordingTB) Fatalf(format string, args ...any) {
	tb.failed = true
	tb.message = strings.TrimSpace(fmt.Sprintf(format, args...))
}

func (tb *recordingTB) Fatal(args ...any) {
	tb.failed = true
	tb.message = strings.TrimSpace(fmt.Sprint(args...))
}
