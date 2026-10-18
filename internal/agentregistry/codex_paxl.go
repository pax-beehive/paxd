package agentregistry

import (
	"context"
	"fmt"
	"time"

	"github.com/pax-beehive/paxd/internal/paxlclient"
	"github.com/pax-beehive/paxd/internal/paxlinstall"
	"github.com/pax-beehive/paxd/internal/sessionstore"
	"github.com/pax-beehive/paxd/pkg/model"
)

func codexLocalAvailable() bool {
	_, err := paxlinstall.ResolveCommand(nil)
	return err == nil
}

func codexPaxlClient() (*paxlclient.Client, error) {
	command, err := paxlinstall.ResolveCommand(nil)
	if err != nil {
		return nil, fmt.Errorf("resolve paxl for codex sessions: %w", err)
	}
	return &paxlclient.Client{Command: command}, nil
}

func listCodexSessionsViaPaxl(ctx context.Context, timeout time.Duration) ([]model.SessionInfo, error) {
	client, err := codexPaxlClient()
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return client.ListSessions(ctx, "codex", 0)
}

func CodexElements(ctx context.Context, nativeID string) ([]sessionstore.Element, error) {
	client, err := codexPaxlClient()
	if err != nil {
		return nil, err
	}
	return client.GetSessionElements(ctx, "codex:"+nativeID, "codex")
}
