package sessionreporter

import (
	"context"
	"errors"

	"github.com/pax-beehive/paxd/internal/auth"
	"github.com/pax-beehive/paxd/internal/cloud"
)

type CloudReporter struct {
	Headers auth.HeaderProvider
}

func (r CloudReporter) ReportAgentSessions(
	ctx context.Context,
	target ReportTarget,
	agentID string,
	sessions []cloud.SessionStatus,
) error {
	if r.Headers == nil {
		return errors.New("sessionreporter: auth header provider is required")
	}
	headers, err := r.Headers.Headers(ctx, target.RemoteID)
	if err != nil {
		return err
	}
	client := cloud.NewClient(target.CloudAPIURL, headers.Get(auth.HeaderPaxKey)).
		WithCloudflareAccess(
			headers.Get(auth.HeaderCloudflareAccessID),
			headers.Get(auth.HeaderCloudflareAccessSecret),
		)
	return client.PostAgentSessionsContext(ctx, agentID, &cloud.AgentSessionsReport{Sessions: sessions})
}
