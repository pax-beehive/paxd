package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPublicationID = "apub_0123456789abcdef0123456789abcdef"

func validFetchCommand() Command {
	return Command{
		CommandID: testPublicationID,
		Type:      CommandArtifactFetch,
		FetchArtifact: &FetchArtifactCommand{
			PublicationID: testPublicationID,
			AgentID:       "agt_x",
			SessionID:     "ses_x",
			Path:          "/Users/me/proj/x.md",
		},
	}
}

func TestFetchArtifactCommandValidation(t *testing.T) {
	require.NoError(t, validFetchCommand().Validate())
	cases := []struct {
		name   string
		mutate func(*FetchArtifactCommand)
		target string
	}{
		{"missing publication id", func(c *FetchArtifactCommand) { c.PublicationID = "" }, "fetch_artifact.publication_id"},
		{"uppercase hex", func(c *FetchArtifactCommand) { c.PublicationID = "apub_0123456789ABCDEF0123456789abcdef" }, "fetch_artifact.publication_id"},
		{"short hex", func(c *FetchArtifactCommand) { c.PublicationID = "apub_0123" }, "fetch_artifact.publication_id"},
		{"wrong prefix", func(c *FetchArtifactCommand) { c.PublicationID = "art_0123456789abcdef0123456789abcdef" }, "fetch_artifact.publication_id"},
		{"missing agent", func(c *FetchArtifactCommand) { c.AgentID = "  " }, "fetch_artifact.agent_id"},
		{"long agent", func(c *FetchArtifactCommand) { c.AgentID = strings.Repeat("a", 257) }, "fetch_artifact.agent_id"},
		{"missing session", func(c *FetchArtifactCommand) { c.SessionID = "" }, "fetch_artifact.session_id"},
		{"long session", func(c *FetchArtifactCommand) { c.SessionID = strings.Repeat("s", 257) }, "fetch_artifact.session_id"},
		{"relative path", func(c *FetchArtifactCommand) { c.Path = "proj/x.md" }, "fetch_artifact.path"},
		{"empty path", func(c *FetchArtifactCommand) { c.Path = "" }, "fetch_artifact.path"},
		{"nul path", func(c *FetchArtifactCommand) { c.Path = "/Users/me/x\x00.md" }, "fetch_artifact.path"},
		{"newline path", func(c *FetchArtifactCommand) { c.Path = "/Users/me/x\n.md" }, "fetch_artifact.path"},
		{"long path", func(c *FetchArtifactCommand) { c.Path = "/" + strings.Repeat("p", 8192) }, "fetch_artifact.path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := validFetchCommand()
			tc.mutate(cmd.FetchArtifact)

			err := cmd.Validate()

			var controlErr ControlError
			require.ErrorAs(t, err, &controlErr)
			assert.Equal(t, ErrCodeInvalidArgument, controlErr.Code)
			assert.Equal(t, tc.target, controlErr.Target)
		})
	}
	agent256 := validFetchCommand()
	agent256.FetchArtifact.AgentID = strings.Repeat("a", 256)
	require.NoError(t, agent256.Validate())
}

func TestCheckArtifactFetchQueryValidation(t *testing.T) {
	ok := Query{Type: QueryArtifactFetchCheck, CheckArtifactFetch: &CheckArtifactFetchQuery{Path: "/Users/me/x.md"}}
	require.NoError(t, ok.Validate())
	// Relative paths are a policy denial (invalid_path), not a malformed request.
	relative := Query{Type: QueryArtifactFetchCheck, CheckArtifactFetch: &CheckArtifactFetchQuery{Path: "x.md"}}
	require.NoError(t, relative.Validate())
	for _, path := range []string{"", "   ", "/" + strings.Repeat("p", 8192)} {
		q := Query{Type: QueryArtifactFetchCheck, CheckArtifactFetch: &CheckArtifactFetchQuery{Path: path}}
		require.Error(t, q.Validate(), "%q", path)
	}
	mismatch := Query{Type: QueryStatusGet, CheckArtifactFetch: &CheckArtifactFetchQuery{Path: "/x"}}
	require.Error(t, mismatch.Validate())
}

func TestArtifactFetchCheckQueryDelegatesForRemoteSource(t *testing.T) {
	size := int64(123)
	fetcher := &fakeArtifactFetch{check: ArtifactFetchCheckResult{
		Allowed: true, Path: "/Users/me/proj/x.md", Filename: "x.md", SizeBytes: &size,
	}}
	service := NewService(ServiceOptions{ArtifactFetch: fetcher})
	src := Source{Kind: SourceRemote, RemoteID: "remote_prod"}

	result, err := service.HandleQuery(context.Background(), src, Query{
		Type: QueryArtifactFetchCheck, CheckArtifactFetch: &CheckArtifactFetchQuery{Path: "/Users/me/proj/x.md"},
	})

	require.NoError(t, err)
	require.Nil(t, result.Error)
	require.NotNil(t, result.ArtifactFetchCheck)
	assert.Equal(t, src, fetcher.checkSource)
	raw, err := json.Marshal(result.ArtifactFetchCheck)
	require.NoError(t, err)
	assert.JSONEq(t, `{"allowed":true,"path":"/Users/me/proj/x.md","filename":"x.md","size_bytes":123}`, string(raw))
	full, err := json.Marshal(result)
	require.NoError(t, err)
	assert.Contains(t, string(full), `"artifact_fetch_check":{`)
}

func TestArtifactFetchCheckDenialJSONShape(t *testing.T) {
	result := ArtifactFetchCheckResult{
		Allowed: false, Path: "/Users/me/.ssh/id_rsa", Filename: "id_rsa",
		Denial: &ArtifactFetchDenial{Code: "hidden_path_denied", Rule: "builtin.hidden-path", Message: "Hidden paths cannot be fetched."},
	}

	raw, err := json.Marshal(result)

	require.NoError(t, err)
	assert.JSONEq(t, `{"allowed":false,"path":"/Users/me/.ssh/id_rsa","filename":"id_rsa","denial":{"code":"hidden_path_denied","rule":"builtin.hidden-path","message":"Hidden paths cannot be fetched."}}`, string(raw))
}

func TestArtifactFetchCheckQueryRejectsUnauthenticatedOrUnconfigured(t *testing.T) {
	query := Query{Type: QueryArtifactFetchCheck, CheckArtifactFetch: &CheckArtifactFetchQuery{Path: "/Users/me/x.md"}}
	fetcher := &fakeArtifactFetch{}
	service := NewService(ServiceOptions{ArtifactFetch: fetcher})
	for _, src := range []Source{{Kind: SourceLocal}, {Kind: SourceRemote}} {
		result, err := service.HandleQuery(context.Background(), src, query)
		require.NoError(t, err)
		require.NotNil(t, result.Error)
		assert.Equal(t, "unauthorized", result.Error.Code)
	}
	assert.False(t, fetcher.checkCalled)

	result, err := NewService(ServiceOptions{}).HandleQuery(context.Background(), Source{Kind: SourceRemote, RemoteID: "r"}, query)
	require.NoError(t, err)
	require.NotNil(t, result.Error)
	assert.Equal(t, ErrCodeInternal, result.Error.Code)

	failing := NewService(ServiceOptions{ArtifactFetch: &fakeArtifactFetch{checkErr: errors.New("home unavailable")}})
	result, err = failing.HandleQuery(context.Background(), Source{Kind: SourceRemote, RemoteID: "r"}, query)
	require.NoError(t, err)
	require.NotNil(t, result.Error)
	assert.Equal(t, ErrCodeInternal, result.Error.Code)
}

func TestArtifactFetchCommandAppliedAckBypassesCommandStore(t *testing.T) {
	fetcher := &fakeArtifactFetch{fetch: ArtifactFetchResult{
		PublicationID: testPublicationID, Status: "snapshotting", Filename: "x.md", SizeBytes: 123,
	}}
	// No Store configured: the command must not go through CommandRecord.
	service := NewService(ServiceOptions{ArtifactFetch: fetcher})
	src := Source{Kind: SourceRemote, RemoteID: "remote_prod"}

	ack, err := service.HandleCommand(context.Background(), src, validFetchCommand())

	require.NoError(t, err)
	require.NoError(t, ack.Validate())
	assert.Equal(t, src, fetcher.fetchSource)
	assert.Equal(t, *validFetchCommand().FetchArtifact, fetcher.fetchCommand)
	raw, err := json.Marshal(ack)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"command_id":"`+testPublicationID+`","ok":true,"status":"applied",
		"target_type":"artifact_publication","target_id":"`+testPublicationID+`",
		"result":{"artifact_fetch":{"publication_id":"`+testPublicationID+`","status":"snapshotting","filename":"x.md","size_bytes":123}}
	}`, string(raw))
}

func TestArtifactFetchCommandDenialIsRejectedWithRuleTarget(t *testing.T) {
	fetcher := &fakeArtifactFetch{fetchErr: ControlError{
		Code: "owner_only_denied", Message: "Run chmod 644 on the node.", Target: "builtin.owner-only",
	}}
	service := NewService(ServiceOptions{ArtifactFetch: fetcher})

	ack, err := service.HandleCommand(context.Background(), Source{Kind: SourceRemote, RemoteID: "r"}, validFetchCommand())

	require.NoError(t, err)
	require.NoError(t, ack.Validate())
	raw, err := json.Marshal(ack)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"command_id":"`+testPublicationID+`","ok":false,"status":"rejected",
		"target_type":"artifact_publication","target_id":"`+testPublicationID+`",
		"error":{"code":"owner_only_denied","message":"Run chmod 644 on the node.","target":"builtin.owner-only"}
	}`, string(raw))
}

func TestArtifactFetchCommandInternalErrorFails(t *testing.T) {
	service := NewService(ServiceOptions{ArtifactFetch: &fakeArtifactFetch{fetchErr: errors.New("database locked")}})

	ack, err := service.HandleCommand(context.Background(), Source{Kind: SourceRemote, RemoteID: "r"}, validFetchCommand())

	require.NoError(t, err)
	assert.False(t, ack.OK)
	assert.Equal(t, CommandStatusFailed, ack.Status)
	assert.Equal(t, ErrCodeInternal, ack.Error.Code)
}

func TestArtifactFetchCommandRejectsLocalSourceAndMismatchedCommandID(t *testing.T) {
	fetcher := &fakeArtifactFetch{}
	service := NewService(ServiceOptions{ArtifactFetch: fetcher})

	for _, src := range []Source{{Kind: SourceLocal}, {Kind: SourceRemote}} {
		ack, err := service.HandleCommand(context.Background(), src, validFetchCommand())
		require.NoError(t, err)
		assert.Equal(t, CommandStatusRejected, ack.Status)
		assert.Equal(t, "unauthorized", ack.Error.Code)
	}

	cmd := validFetchCommand()
	cmd.CommandID = "cmd_other"
	ack, err := service.HandleCommand(context.Background(), Source{Kind: SourceRemote, RemoteID: "r"}, cmd)
	require.NoError(t, err)
	assert.Equal(t, CommandStatusRejected, ack.Status)
	assert.Equal(t, ErrCodeInvalidArgument, ack.Error.Code)
	assert.Equal(t, "command_id", ack.Error.Target)
	assert.False(t, fetcher.fetchCalled)

	unconfigured, err := NewService(ServiceOptions{}).HandleCommand(context.Background(), Source{Kind: SourceRemote, RemoteID: "r"}, validFetchCommand())
	require.NoError(t, err)
	assert.Equal(t, CommandStatusFailed, unconfigured.Status)
}

type fakeArtifactFetch struct {
	check        ArtifactFetchCheckResult
	checkErr     error
	checkCalled  bool
	checkSource  Source
	fetch        ArtifactFetchResult
	fetchErr     error
	fetchCalled  bool
	fetchSource  Source
	fetchCommand FetchArtifactCommand
}

func (f *fakeArtifactFetch) Check(_ context.Context, src Source, _ CheckArtifactFetchQuery) (ArtifactFetchCheckResult, error) {
	f.checkCalled = true
	f.checkSource = src
	return f.check, f.checkErr
}

func (f *fakeArtifactFetch) Fetch(_ context.Context, src Source, cmd FetchArtifactCommand) (ArtifactFetchResult, error) {
	f.fetchCalled = true
	f.fetchSource = src
	f.fetchCommand = cmd
	return f.fetch, f.fetchErr
}
