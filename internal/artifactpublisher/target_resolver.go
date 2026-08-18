package artifactpublisher

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pax-beehive/paxd/internal/auth"
	"github.com/pax-beehive/paxd/internal/daemonstore"
)

type DesiredConnectionSource interface {
	ListDesiredAgentConnections(
		context.Context,
	) ([]daemonstore.AgentConnectionDesiredSpec, error)
}

type RemoteTargetResolver struct {
	source  DesiredConnectionSource
	headers auth.HeaderProvider
}

func NewRemoteTargetResolver(
	source DesiredConnectionSource,
	headers auth.HeaderProvider,
) *RemoteTargetResolver {
	return &RemoteTargetResolver{source: source, headers: headers}
}

func (r *RemoteTargetResolver) Resolve(
	ctx context.Context,
	remoteID string,
	agentID string,
) (ManagerTarget, error) {
	if r == nil || r.source == nil || r.headers == nil {
		return ManagerTarget{}, errors.New("artifact manager target resolver is not configured")
	}
	remoteID = strings.TrimSpace(remoteID)
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return ManagerTarget{}, errors.New("artifact manager agent id is required")
	}
	connections, err := r.source.ListDesiredAgentConnections(ctx)
	if err != nil {
		return ManagerTarget{}, err
	}
	var match *daemonstore.AgentConnectionDesiredSpec
	for _, connection := range connections {
		if strings.TrimSpace(connection.CloudAgentID) != agentID ||
			(remoteID != "" && strings.TrimSpace(connection.RemoteID) != remoteID) {
			continue
		}
		if match != nil {
			return ManagerTarget{}, fmt.Errorf(
				"multiple enabled manager connections found for agent %q; remote identity is ambiguous",
				agentID,
			)
		}
		matched := connection
		match = &matched
	}
	if match == nil {
		if remoteID != "" {
			return ManagerTarget{}, fmt.Errorf(
				"no enabled manager connection for agent %q on remote %q",
				agentID,
				remoteID,
			)
		}
		return ManagerTarget{}, fmt.Errorf("no enabled manager connection for agent %q", agentID)
	}
	resolvedRemoteID := strings.TrimSpace(match.RemoteID)
	if resolvedRemoteID == "" {
		return ManagerTarget{}, fmt.Errorf("manager connection for agent %q has no remote id", agentID)
	}
	headers, err := r.headers.Headers(ctx, resolvedRemoteID)
	if err != nil {
		return ManagerTarget{}, err
	}
	return ManagerTarget{
		RemoteID: resolvedRemoteID,
		BaseURL:  match.CloudAPIURL,
		Headers:  headers,
	}, nil
}
