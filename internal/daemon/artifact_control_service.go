package daemon

import (
	"context"
	"errors"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/runtime"
)

type artifactTurnStore interface {
	ListDesiredAgentConnections(context.Context) ([]daemonstore.AgentConnectionDesiredSpec, error)
	GetACPSessionRouteByManagerID(context.Context, string, string) (daemonstore.ACPSessionRouteView, error)
}

type artifactTurnRuntime interface {
	SessionRuntimeSnapshot(string) (runtime.ActiveTurnsSnapshot, bool)
}

type artifactControlService struct {
	control.Service
	publications control.ArtifactPublicationService
	store        artifactTurnStore
	runtimes     artifactTurnRuntime
}

func (s artifactControlService) PublishArtifact(
	ctx context.Context,
	req control.PublishArtifactRequest,
) (control.ArtifactPublication, error) {
	turnID, err := s.sourceTurnID(ctx, req.AgentID, req.SessionID)
	if err != nil {
		return control.ArtifactPublication{}, err
	}
	req.TurnID = turnID
	return s.publications.PublishArtifact(ctx, req)
}

func (s artifactControlService) sourceTurnID(ctx context.Context, agentID, sessionID string) (string, error) {
	connections, err := s.store.ListDesiredAgentConnections(ctx)
	if err != nil {
		return "", err
	}
	for _, connection := range connections {
		if connection.CloudAgentID != agentID {
			continue
		}
		route, err := s.store.GetACPSessionRouteByManagerID(ctx, connection.ConnectionID, sessionID)
		if err != nil {
			return "", err
		}
		snapshot, _ := s.runtimes.SessionRuntimeSnapshot(connection.ConnectionID)
		for _, turn := range snapshot.ActiveTurns {
			if turn.NativeSessionID == route.NativeSessionID && turn.TurnID != "" {
				return turn.TurnID, nil
			}
		}
	}
	return "", errors.New("artifact publication requires an active source turn")
}
