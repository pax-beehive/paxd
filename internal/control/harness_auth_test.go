package control

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
)

type fakeHarnessAuth struct{ calls int }

func (f *fakeHarnessAuth) Login(context.Context, Source, string, HarnessAuthLoginCommand) (HarnessAuthView, error) {
	f.calls++
	return HarnessAuthView{Harness: "claude", State: "starting", SessionID: "test-session"}, nil
}
func (f *fakeHarnessAuth) Status(context.Context, Source, HarnessAuthStatusQuery) (HarnessAuthView, error) {
	f.calls++
	return HarnessAuthView{Harness: "claude", State: "logged_out"}, nil
}

func TestHarnessAuthBypassesDurableStoreAndValidatesSource(t *testing.T) {
	f := &fakeHarnessAuth{}
	s := NewService(ServiceOptions{HarnessAuth: f})
	cmd := Command{CommandID: "start", Type: CommandHarnessAuthLogin, HarnessAuthLogin: &HarnessAuthLoginCommand{Harness: "claude", Operation: "start"}}
	ack, err := s.HandleCommand(context.Background(), Source{Kind: SourceRemote, RemoteID: "remote-a"}, cmd)
	require.NoError(t, err)
	require.True(t, ack.OK)
	require.Equal(t, "test-session", ack.Result.HarnessAuth.SessionID)
	ack, err = s.HandleCommand(context.Background(), Source{}, cmd)
	require.NoError(t, err)
	require.False(t, ack.OK)
	require.Equal(t, 1, f.calls)
	query := Query{Type: QueryHarnessAuthStatus, HarnessAuthStatus: &HarnessAuthStatusQuery{Harness: "claude"}}
	result, err := s.HandleQuery(context.Background(), Source{Kind: SourceLocal}, query)
	require.NoError(t, err)
	require.Nil(t, result.Error)
	require.Equal(t, "logged_out", result.HarnessAuth.State)
}
func TestHarnessAuthRejectsInvalidPayloads(t *testing.T) {
	for _, req := range []HarnessAuthLoginCommand{
		{Harness: "unsupported", Operation: "start"},
		{Harness: "claude", Operation: "start", Code: "secret"},
		{Harness: "claude", Operation: "submit"},
		{Harness: "claude", Operation: "submit", SessionID: "s", Code: "secret\nextra"},
		{Harness: "claude", Operation: "cancel", SessionID: "s", Code: "secret"},
	} {
		require.Error(t, req.Validate())
	}
	cmd := Command{CommandID: "start", Type: CommandHarnessAuthLogin, HarnessAuthLogin: &HarnessAuthLoginCommand{Harness: "claude", Operation: "start"}, PushSecretChannel: &PushSecretChannelCommand{}}
	require.Error(t, cmd.Validate())
}
