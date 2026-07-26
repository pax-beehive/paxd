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
	agentID string,
) (ManagerTarget, error) {
	if r == nil || r.source == nil || r.headers == nil {
		return ManagerTarget{}, errors.New("artifact manager target resolver is not configured")
	}
	connections, err := r.source.ListDesiredAgentConnections(ctx)
	if err != nil {
		return ManagerTarget{}, err
	}
	agentID = strings.TrimSpace(agentID)
	for _, connection := range connections {
		if connection.CloudAgentID != agentID {
			continue
		}
		headers, err := r.headers.Headers(ctx, connection.RemoteID)
		if err != nil {
			return ManagerTarget{}, err
		}
		return ManagerTarget{
			RemoteID: connection.RemoteID,
			BaseURL:  connection.CloudAPIURL,
			Headers:  headers,
		}, nil
	}
	return ManagerTarget{}, fmt.Errorf("no enabled manager connection for agent %q", agentID)
}
