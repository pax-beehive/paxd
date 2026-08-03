package control

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionRuntimeResetCommandUsesCompareAndSuppressPort(t *testing.T) {
	resetter := &fakeSessionRuntimeResetter{result: SessionRuntimeResetResult{
		Status: "suppressed", ProjectionRevision: 9,
	}}
	service := NewService(ServiceOptions{SessionRuntimeReset: resetter})
	command := Command{
		CommandID: "cmd_reset_1", Type: CommandSessionRuntimeReset,
		ResetSessionRuntime: &ResetSessionRuntimeCommand{
			AgentID: "agent_1", ConnectionID: "conn_1",
			NativeSessionID: "native_1", ExpectedTurnInstanceID: "turn_1",
		},
	}

	ack, err := service.HandleCommand(t.Context(), Source{Kind: SourceRemote, RemoteID: "remote_prod"}, command)

	require.NoError(t, err)
	assert.True(t, ack.OK)
	assert.Equal(t, CommandStatusApplied, ack.Status)
	require.NotNil(t, ack.Result)
	require.NotNil(t, ack.Result.SessionRuntimeReset)
	assert.Equal(t, "suppressed", ack.Result.SessionRuntimeReset.Status)
	require.NotNil(t, resetter.command)
	assert.Equal(t, "turn_1", resetter.command.ExpectedTurnInstanceID)
	assert.Equal(t, "remote_prod", resetter.remoteID)
}

func TestSessionRuntimeResetCommandRejectsMissingCompareIdentity(t *testing.T) {
	command := Command{
		CommandID: "cmd_reset_1", Type: CommandSessionRuntimeReset,
		ResetSessionRuntime: &ResetSessionRuntimeCommand{
			AgentID: "agent_1", ConnectionID: "conn_1", NativeSessionID: "native_1",
		},
	}

	err := command.Validate()

	require.Error(t, err)
	var controlErr ControlError
	require.ErrorAs(t, err, &controlErr)
	assert.Equal(t, "reset_session_runtime.expected_turn_instance_id", controlErr.Target)
}

type fakeSessionRuntimeResetter struct {
	remoteID string
	command  *ResetSessionRuntimeCommand
	result   SessionRuntimeResetResult
	err      error
}

func (f *fakeSessionRuntimeResetter) ResetSessionRuntime(
	_ context.Context,
	remoteID string,
	command ResetSessionRuntimeCommand,
) (SessionRuntimeResetResult, error) {
	f.remoteID = remoteID
	copy := command
	f.command = &copy
	return f.result, f.err
}
