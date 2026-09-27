package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/remotelogin"
)

type setupTokenClient interface {
	RegisterNode(*cloud.RegisterNodeRequest, string) (*cloud.RegisterNodeResponse, error)
}

func registerSetupToken(ctx context.Context, cfg *config.Config, spec remotelogin.LoginSpec, client setupTokenClient) (remotelogin.LoginResult, error) {
	token := strings.TrimSpace(os.Getenv("PAX_REGISTRATION_TOKEN"))
	// A one-time credential must not be inherited by the background service.
	_ = os.Unsetenv("PAX_REGISTRATION_TOKEN")
	if token == "" {
		return remotelogin.LoginResult{}, fmt.Errorf("PAX_REGISTRATION_TOKEN is required")
	}
	if err := ctx.Err(); err != nil {
		return remotelogin.LoginResult{}, err
	}
	if strings.TrimSpace(cfg.Cloud.APIKey) != "" {
		return remotelogin.LoginResult{}, fmt.Errorf("this device already has cloud credentials; use its existing connection or explicitly run paxd login to change accounts")
	}
	store, closeStore, err := openDaemonStore(cfg.Daemon.DBPath)
	if err != nil {
		return remotelogin.LoginResult{}, err
	}
	remotes, err := store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	closeStore()
	if err != nil {
		return remotelogin.LoginResult{}, err
	}
	for _, remote := range remotes {
		if remote.Remote.ID == spec.RemoteID {
			return remotelogin.LoginResult{}, fmt.Errorf("remote %s is already configured; use paxd service restart to resume it, or explicitly run paxd login to change accounts", spec.RemoteID)
		}
	}
	registered, err := client.RegisterNode(&cloud.RegisterNodeRequest{
		Name: spec.Node.Name, Hostname: spec.Node.Hostname, MachineType: spec.Node.MachineType,
		OS: spec.Node.OS, Arch: spec.Node.Arch, PaxdVersion: spec.Node.PaxdVersion,
		APIEndpoint: spec.Node.APIEndpoint,
	}, token)
	if err != nil {
		// The remote response is not printed: it could echo the credential.
		return remotelogin.LoginResult{}, fmt.Errorf("device registration failed; check the server connection and generate a fresh command in Console")
	}
	if registered == nil || strings.TrimSpace(registered.NodeID) == "" || strings.TrimSpace(registered.APIKey) == "" {
		return remotelogin.LoginResult{}, fmt.Errorf("device registration did not return node credentials")
	}
	return remotelogin.LoginResult{RemoteID: spec.RemoteID, CloudAPIURL: spec.CloudAPIURL, NodeID: registered.NodeID, NodeAPIKey: registered.APIKey}, nil
}
