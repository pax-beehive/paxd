package artifactpublisher_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pax-beehive/paxd/internal/artifactpublisher"
	"github.com/pax-beehive/paxd/internal/daemonstore"
)

func TestRemoteTargetResolver(t *testing.T) {
	t.Run("Given an invalid resolver or blank agent then it rejects before dependency calls", func(t *testing.T) {
		_, err := (*artifactpublisher.RemoteTargetResolver)(nil).Resolve(t.Context(), "", "agent_1")
		require.Error(t, err)
		assert.ErrorContains(t, err, "not configured")

		source := &recordingDesiredConnections{}
		resolver := artifactpublisher.NewRemoteTargetResolver(source, &recordingArtifactHeaderProvider{})
		_, err = resolver.Resolve(t.Context(), "", "   ")
		require.Error(t, err)
		assert.ErrorContains(t, err, "agent id is required")
		assert.Zero(t, source.calls)
	})

	t.Run("Given one matching agent when resolving an unbound job then it selects that remote", func(t *testing.T) {
		headers := &recordingArtifactHeaderProvider{}
		resolver := artifactpublisher.NewRemoteTargetResolver(staticDesiredConnections{connections: []daemonstore.AgentConnectionDesiredSpec{{
			RemoteID: "remote_home", CloudAgentID: "agent_1", CloudAPIURL: "https://home.example",
		}}}, headers)

		target, err := resolver.Resolve(t.Context(), "", "agent_1")

		require.NoError(t, err)
		assert.Equal(t, "remote_home", target.RemoteID)
		assert.Equal(t, "https://home.example", target.BaseURL)
		assert.Equal(t, []string{"remote_home"}, headers.remoteIDs)
	})

	t.Run("Given the same agent on two remotes when resolving an unbound job then it rejects ambiguity", func(t *testing.T) {
		headers := &recordingArtifactHeaderProvider{}
		resolver := artifactpublisher.NewRemoteTargetResolver(staticDesiredConnections{connections: []daemonstore.AgentConnectionDesiredSpec{
			{RemoteID: "remote_home", CloudAgentID: "agent_1", CloudAPIURL: "https://home.example"},
			{RemoteID: "remote_work", CloudAgentID: "agent_1", CloudAPIURL: "https://work.example"},
		}}, headers)

		_, err := resolver.Resolve(t.Context(), "", "agent_1")

		require.Error(t, err)
		assert.ErrorContains(t, err, "multiple enabled manager connections")
		assert.ErrorContains(t, err, "agent_1")
		assert.Empty(t, headers.remoteIDs)
	})

	t.Run("Given no matching agent when resolving an unbound job then it fails explicitly", func(t *testing.T) {
		resolver := artifactpublisher.NewRemoteTargetResolver(
			staticDesiredConnections{},
			&recordingArtifactHeaderProvider{},
		)

		_, err := resolver.Resolve(t.Context(), "", "agent_missing")

		require.Error(t, err)
		assert.ErrorContains(t, err, "no enabled manager connection")
		assert.ErrorContains(t, err, "agent_missing")
	})

	t.Run("Given a bound job when another remote has the same agent then it stays on the persisted remote", func(t *testing.T) {
		headers := &recordingArtifactHeaderProvider{}
		resolver := artifactpublisher.NewRemoteTargetResolver(staticDesiredConnections{connections: []daemonstore.AgentConnectionDesiredSpec{
			{RemoteID: "remote_other", CloudAgentID: "agent_1", CloudAPIURL: "https://other.example"},
			{RemoteID: "remote_home", CloudAgentID: "agent_1", CloudAPIURL: "https://home.example"},
		}}, headers)

		target, err := resolver.Resolve(t.Context(), "remote_home", "agent_1")

		require.NoError(t, err)
		assert.Equal(t, "remote_home", target.RemoteID)
		assert.Equal(t, "https://home.example", target.BaseURL)
		assert.Equal(t, []string{"remote_home"}, headers.remoteIDs)
	})

	t.Run("Given a bound remote disappears when another remote has the agent then it refuses to drift", func(t *testing.T) {
		headers := &recordingArtifactHeaderProvider{}
		resolver := artifactpublisher.NewRemoteTargetResolver(staticDesiredConnections{connections: []daemonstore.AgentConnectionDesiredSpec{{
			RemoteID: "remote_other", CloudAgentID: "agent_1", CloudAPIURL: "https://other.example",
		}}}, headers)

		_, err := resolver.Resolve(t.Context(), "remote_home", "agent_1")

		require.Error(t, err)
		assert.ErrorContains(t, err, "remote_home")
		assert.ErrorContains(t, err, "agent_1")
		assert.Empty(t, headers.remoteIDs)
	})

	t.Run("Given connection lookup or header lookup fails then it returns the dependency error", func(t *testing.T) {
		connectionErr := errors.New("connections unavailable")
		resolver := artifactpublisher.NewRemoteTargetResolver(
			staticDesiredConnections{err: connectionErr},
			&recordingArtifactHeaderProvider{},
		)
		_, err := resolver.Resolve(t.Context(), "", "agent_1")
		assert.ErrorIs(t, err, connectionErr)

		headerErr := errors.New("credentials unavailable")
		headers := &recordingArtifactHeaderProvider{err: headerErr}
		resolver = artifactpublisher.NewRemoteTargetResolver(staticDesiredConnections{connections: []daemonstore.AgentConnectionDesiredSpec{{
			RemoteID: "remote_home", CloudAgentID: "agent_1", CloudAPIURL: "https://home.example",
		}}}, headers)
		_, err = resolver.Resolve(t.Context(), "", "agent_1")
		assert.ErrorIs(t, err, headerErr)
	})
}

type staticDesiredConnections struct {
	connections []daemonstore.AgentConnectionDesiredSpec
	err         error
}

type recordingDesiredConnections struct {
	calls int
}

func (s *recordingDesiredConnections) ListDesiredAgentConnections(
	context.Context,
) ([]daemonstore.AgentConnectionDesiredSpec, error) {
	s.calls++
	return nil, nil
}

func (s staticDesiredConnections) ListDesiredAgentConnections(
	context.Context,
) ([]daemonstore.AgentConnectionDesiredSpec, error) {
	return s.connections, s.err
}

type recordingArtifactHeaderProvider struct {
	remoteIDs []string
	err       error
}

func (p *recordingArtifactHeaderProvider) Headers(
	_ context.Context,
	remoteID string,
) (http.Header, error) {
	p.remoteIDs = append(p.remoteIDs, remoteID)
	return http.Header{"X-Pax-Key": []string{"node-key"}}, p.err
}
