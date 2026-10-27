package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArtifactGivenActiveTurnWhenAcceptedThenCapturesItBeforePublishing(t *testing.T) {
	store := &artifactTurnStoreStub{}
	runtimes := &artifactTurnRuntimeStub{snapshot: runtime.ActiveTurnsSnapshot{
		ActiveTurns: []runtime.SessionActiveTurn{{NativeSessionID: "native", TurnID: "turn_original"}},
	}}
	service := artifactControlService{store: store, runtimes: runtimes,
		publications: artifactPublisherFunc(func(_ context.Context, req control.PublishArtifactRequest) (control.ArtifactPublication, error) {
			runtimes.snapshot.ActiveTurns[0].TurnID = "turn_next"
			assert.Equal(t, "turn_original", req.TurnID)
			assert.Equal(t, "session", req.SessionID)
			return control.ArtifactPublication{PublicationID: "apub_original"}, nil
		}),
	}
	publication, err := service.PublishArtifact(t.Context(), control.PublishArtifactRequest{
		AgentID: "agent", SessionID: "session", TurnID: "untrusted_turn",
	})
	require.NoError(t, err)
	assert.Equal(t, "apub_original", publication.PublicationID)
	assert.Equal(t, []string{"connection", "session"}, store.lookup)
}

func TestArtifactGivenUnresolvedSourceTurnWhenAcceptedThenDoesNotPublish(t *testing.T) {
	wantErr := errors.New("store unavailable")
	for _, tc := range []struct {
		name              string
		listErr, routeErr error
		snapshot          runtime.ActiveTurnsSnapshot
	}{
		{name: "connection lookup fails", listErr: wantErr},
		{name: "session lookup fails", routeErr: wantErr},
		{name: "no active turn"},
		{name: "another session active", snapshot: runtime.ActiveTurnsSnapshot{
			ActiveTurns: []runtime.SessionActiveTurn{{NativeSessionID: "another", TurnID: "turn_other"}},
		}},
		{name: "empty turn", snapshot: runtime.ActiveTurnsSnapshot{
			ActiveTurns: []runtime.SessionActiveTurn{{NativeSessionID: "native"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := artifactControlService{
				store:    &artifactTurnStoreStub{listErr: tc.listErr, routeErr: tc.routeErr},
				runtimes: &artifactTurnRuntimeStub{snapshot: tc.snapshot},
				publications: artifactPublisherFunc(func(context.Context, control.PublishArtifactRequest) (control.ArtifactPublication, error) {
					t.Fatal("an unresolved publication must not be accepted")
					return control.ArtifactPublication{}, nil
				}),
			}
			_, err := service.PublishArtifact(t.Context(), control.PublishArtifactRequest{AgentID: "agent", SessionID: "session"})
			require.Error(t, err)
			if tc.listErr != nil || tc.routeErr != nil {
				assert.ErrorIs(t, err, wantErr)
			} else {
				assert.ErrorContains(t, err, "active source turn")
			}
		})
	}
}

type artifactTurnStoreStub struct {
	listErr, routeErr error
	lookup            []string
}

func (s *artifactTurnStoreStub) ListDesiredAgentConnections(context.Context) ([]daemonstore.AgentConnectionDesiredSpec, error) {
	return []daemonstore.AgentConnectionDesiredSpec{
		{ConnectionID: "other_connection", CloudAgentID: "other_agent"},
		{ConnectionID: "connection", CloudAgentID: "agent"},
	}, s.listErr
}

func (s *artifactTurnStoreStub) GetACPSessionRouteByManagerID(_ context.Context, connection, session string) (daemonstore.ACPSessionRouteView, error) {
	s.lookup = []string{connection, session}
	return daemonstore.ACPSessionRouteView{NativeSessionID: "native"}, s.routeErr
}

type artifactTurnRuntimeStub struct{ snapshot runtime.ActiveTurnsSnapshot }

func (s *artifactTurnRuntimeStub) SessionRuntimeSnapshot(string) (runtime.ActiveTurnsSnapshot, bool) {
	return s.snapshot, true
}

type artifactPublisherFunc func(context.Context, control.PublishArtifactRequest) (control.ArtifactPublication, error)

func (f artifactPublisherFunc) PublishArtifact(ctx context.Context, req control.PublishArtifactRequest) (control.ArtifactPublication, error) {
	return f(ctx, req)
}
