package controltest

import (
	"context"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
)

func TestMockServiceExpectedCommand(t *testing.T) {
	cmd := LoadCommandRequest(t, "agent_connection_create")
	ack := LoadCommandResponse(t, "agent_connection_create")
	src := control.Source{Kind: control.SourceLocal}

	service := NewMockService(t).
		ExpectCommandFrom(src, cmd).
		ReturnCommandAck(ack)

	got, err := service.HandleCommand(context.Background(), src, cmd)
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if got.CommandID != ack.CommandID || !got.OK {
		t.Fatalf("HandleCommand() = %+v, want %+v", got, ack)
	}
}

func TestMockServiceExpectedQuery(t *testing.T) {
	query := LoadQueryRequest(t, "remotes_list")
	result := LoadQueryResponse(t, "remotes_list")
	src := control.Source{Kind: control.SourceRemote, RemoteID: "remote_prod"}

	service := NewMockService(t).
		ExpectQueryFrom(src, query).
		ReturnQueryResult(result)

	got, err := service.HandleQuery(context.Background(), src, query)
	if err != nil {
		t.Fatalf("HandleQuery() error = %v", err)
	}
	if got.Type != result.Type {
		t.Fatalf("HandleQuery() = %+v, want %+v", got, result)
	}
}
