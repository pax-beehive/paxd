package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pax-beehive/paxd/internal/agentregistry"
	"github.com/pax-beehive/paxd/internal/auth"
	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/control"
	paxdaemon "github.com/pax-beehive/paxd/internal/daemon"
	"github.com/pax-beehive/paxd/internal/localapi"
	"github.com/pax-beehive/paxd/internal/remotelogin"
	"github.com/pax-beehive/paxd/internal/remotesecrets"
	"github.com/pax-beehive/paxd/internal/sessionrender"
	"github.com/pax-beehive/paxd/internal/sessionstore"
	"github.com/urfave/cli/v3"
)

const (
	knowledgeKeywordLimit     = 80
	knowledgeTitleLimit       = 120
	knowledgeSummaryLimit     = 1200
	knowledgeContentLimit     = 6000
	knowledgeDeliveryLimit    = 4000
	knowledgeExtractLineLimit = 40
)

var steerSession = agentregistry.SteerSession

type remoteLoginFunc func(context.Context, remotelogin.LoginSpec, remotelogin.Options) (remotelogin.LoginResult, error)

type localControlClient interface {
	GetStatus(context.Context) (control.QueryResult, error)
	ListRemotes(context.Context, bool) (control.QueryResult, error)
	CreateRemote(context.Context, string, control.CreateRemoteCommand) (control.CommandAck, error)
	RestartRemote(context.Context, string, string) (control.CommandAck, error)
	DeleteRemote(context.Context, string, string, bool) (control.CommandAck, error)
	ListAgentConnections(context.Context, bool) (control.QueryResult, error)
	CreateAgentConnection(context.Context, string, control.CreateAgentConnectionCommand) (control.CommandAck, error)
	UpdateAgentConnection(context.Context, string, string, control.UpdateAgentConnectionCommand) (control.CommandAck, error)
	RestartAgentConnection(context.Context, string, string) (control.CommandAck, error)
	DeleteAgentConnection(context.Context, string, string) (control.CommandAck, error)
	ListHarnesses(context.Context, bool) (control.QueryResult, error)
	DiscoverHarnesses(context.Context, control.DiscoverHarnessesQuery) (control.QueryResult, error)
}

var runRemoteLogin remoteLoginFunc = remotelogin.Login
var loadRemoteNodeKey = func(ctx context.Context, remoteID string) (string, error) {
	ref, err := (remotesecrets.Store{}).NodeKeyRef(remoteID)
	if err != nil {
		return "", err
	}
	return auth.NewDefaultResolver().Resolve(ctx, ref)
}
var resolveRemoteSecretRef = func(ctx context.Context, ref string) (string, error) {
	return auth.NewDefaultResolver().Resolve(ctx, ref)
}
var registerCloudAgent = func(ctx context.Context, remote control.RemoteView, nodeKey string, name string, agentType string) (string, error) {
	client, err := cloudClientForRemoteView(ctx, remote, nodeKey)
	if err != nil {
		return "", err
	}
	resp, err := client.RegisterNodeAgent(&cloud.RegisterNodeAgentRequest{
		Agent: cloud.RegisterNodeAgentPayload{Name: name, AgentType: agentType},
	}, "")
	if err != nil {
		return "", err
	}
	if resp == nil || strings.TrimSpace(resp.AgentID) == "" {
		return "", errors.New("register cloud agent returned empty agent id")
	}
	return resp.AgentID, nil
}

func cloudClientForRemoteView(ctx context.Context, remote control.RemoteView, nodeKey string) (*cloud.Client, error) {
	client := cloud.NewClient(remote.Remote.CloudAPIURL, nodeKey)
	if remote.Auth == nil || remote.Auth.Kind == "" || remote.Auth.Kind == control.RemoteAuthNone {
		return client, nil
	}

	switch remote.Auth.Kind {
	case control.RemoteAuthCloudflareAccess:
		clientID := strings.TrimSpace(remote.Auth.ClientID)
		if clientID == "" {
			return nil, fmt.Errorf("remote %q cloudflare access auth is missing client id", remote.Remote.ID)
		}
		secretRef := strings.TrimSpace(remote.Auth.ClientSecretRef)
		if secretRef == "" {
			return nil, fmt.Errorf("remote %q cloudflare access auth is missing client secret ref", remote.Remote.ID)
		}
		clientSecret, err := resolveRemoteSecretRef(ctx, secretRef)
		if err != nil {
			return nil, fmt.Errorf("resolve cloudflare access client secret for remote %q: %w", remote.Remote.ID, err)
		}
		client.WithCloudflareAccess(clientID, clientSecret)
		return client, nil
	default:
		return nil, fmt.Errorf("remote %q has unsupported auth kind %q", remote.Remote.ID, remote.Auth.Kind)
	}
}

var newLocalControlClient = func() localControlClient {
	return localapi.NewUnixClient(paxdaemon.DefaultControlSocketPath())
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return newPaxCommand(stdout, stderr).Run(ctx, append([]string{"paxctl"}, args...))
}

func newPaxCommand(stdout, stderr io.Writer) *cli.Command {
	return &cli.Command{
		Name:  "paxctl",
		Usage: "Local-first agent session tools",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "db", Usage: "SQLite database path"},
		},
		Writer:    stdout,
		ErrWriter: stderr,
		Commands: []*cli.Command{
			{
				Name:  "status",
				Usage: "Show local paxd status",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return statusCommand(ctx, newLocalControlClient(), stdout)
				},
			},
			remotesCommand(stdout),
			{
				Name:  "agents",
				Usage: "Manage local agent connections",
				Commands: []*cli.Command{
					{
						Name:  "list",
						Usage: "List local agent connections",
						Flags: []cli.Flag{
							&cli.BoolFlag{Name: "all", Usage: "Include disabled/deleted connections"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return agentConnectionsList(ctx, newLocalControlClient(), stdout, cmd.Bool("all"))
						},
					},
					{
						Name:  "create",
						Usage: "Create a local agent connection",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "remote", Usage: "Remote id"},
							&cli.StringFlag{Name: "harness", Usage: "Harness to run"},
							&cli.StringFlag{Name: "name", Usage: "Agent connection name"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return agentConnectionCreate(ctx, newLocalControlClient(), stdout, cmd)
						},
					},
					{
						Name:      "restart",
						Usage:     "Restart a local agent connection",
						ArgsUsage: "<name-or-id>",
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return agentConnectionRestart(ctx, newLocalControlClient(), stdout, cmd.Args().First())
						},
					},
					{
						Name:      "stop",
						Usage:     "Stop a local agent connection",
						ArgsUsage: "<name-or-id>",
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return agentConnectionStop(ctx, newLocalControlClient(), stdout, cmd.Args().First())
						},
					},
					{
						Name:      "remove",
						Usage:     "Remove a local agent connection",
						ArgsUsage: "<name-or-id>",
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return agentConnectionRemove(ctx, newLocalControlClient(), stdout, cmd.Args().First())
						},
					},
				},
			},
			harnessesCommand(stdout),
			{
				Name:  "sessions",
				Usage: "List, sync, and render local agent sessions",
				Commands: []*cli.Command{
					{
						Name:  "list",
						Usage: "List session metadata",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "agents", Usage: "Comma-separated agents to scan"},
							&cli.StringFlag{Name: "updated-since", Usage: "Only show sessions updated since a duration like 24h or 7d"},
							&cli.IntFlag{Name: "limit", Usage: "Maximum sessions to show"},
							&cli.StringFlag{Name: "format", Value: "table", Usage: "Output format: table, jsonl, or html"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return sessionsList(ctx, cmd, stdout, stderr)
						},
					},
					{
						Name:  "sync",
						Usage: "Force sync session metadata and available timeline data",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "agents", Usage: "Comma-separated agents to sync"},
							&cli.StringFlag{Name: "updated-since", Usage: "Only sync sessions updated since a duration like 24h or 7d"},
							&cli.IntFlag{Name: "limit", Usage: "Maximum sessions to sync"},
							&cli.StringFlag{Name: "timeout", Value: "10s", Usage: "Per-agent ACP list timeout, for example 5s or 1m"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return sessionsSync(ctx, cmd, stdout, stderr)
						},
					},
					{
						Name:      "get",
						Usage:     "Render a session timeline",
						ArgsUsage: "<session-id>",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "agent", Usage: "Agent for bare native session IDs"},
							&cli.StringFlag{Name: "format", Value: "transcript", Usage: "Output format: transcript, jsonl, or html"},
							&cli.StringFlag{Name: "output", Usage: "Output path"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return sessionsGet(ctx, cmd, stdout)
						},
					},
				},
			},
			{
				Name:  "capsules",
				Usage: "Create, list, and render local knowledge capsules",
				Commands: []*cli.Command{
					{
						Name:      "create",
						Usage:     "Create a knowledge capsule from a synced source session",
						ArgsUsage: "<source-session-id>",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "agent", Usage: "Agent for bare native session IDs"},
							&cli.StringFlag{Name: "keyword", Usage: "Keyword to extract from the source session"},
							&cli.StringFlag{Name: "format", Value: "table", Usage: "Output format: table or jsonl"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return capsulesCreate(ctx, cmd, stdout)
						},
					},
					{
						Name:  "list",
						Usage: "List local knowledge capsules",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "status", Value: "active", Usage: "Capsule status filter"},
							&cli.StringFlag{Name: "keyword", Usage: "Keyword filter"},
							&cli.StringFlag{Name: "source-session", Usage: "Source session filter"},
							&cli.IntFlag{Name: "limit", Usage: "Maximum capsules to show"},
							&cli.StringFlag{Name: "format", Value: "table", Usage: "Output format: table or jsonl"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return capsulesList(ctx, cmd, stdout)
						},
					},
					{
						Name:      "get",
						Usage:     "Render a local knowledge capsule",
						ArgsUsage: "<capsule-id>",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "format", Value: "text", Usage: "Output format: text or jsonl"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return capsulesGet(ctx, cmd, stdout)
						},
					},
					{
						Name:      "archive",
						Usage:     "Archive a local knowledge capsule",
						ArgsUsage: "<capsule-id>",
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return capsulesArchive(ctx, cmd, stdout)
						},
					},
					{
						Name:      "inject",
						Usage:     "Render a system_handoff message for a target session",
						ArgsUsage: "<capsule-id> <target-session-id>",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "agent", Usage: "Agent for bare native target session IDs"},
							&cli.StringFlag{Name: "timeout", Value: "30s", Usage: "ACP steer timeout, for example 10s or 1m"},
							&cli.StringFlag{Name: "output", Usage: "Also write the sent system_handoff message to this path"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return capsulesInject(ctx, cmd, stdout)
						},
					},
					{
						Name:  "injections",
						Usage: "List local capsule injection records",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "target-session", Usage: "Target session filter"},
							&cli.IntFlag{Name: "limit", Usage: "Maximum injections to show"},
							&cli.StringFlag{Name: "format", Value: "table", Usage: "Output format: table or jsonl"},
						},
						Action: func(ctx context.Context, cmd *cli.Command) error {
							return capsulesInjections(ctx, cmd, stdout)
						},
					},
				},
			},
		},
	}
}

func remotesCommand(stdout io.Writer) *cli.Command {
	return &cli.Command{
		Name:  "remotes",
		Usage: "Manage Pax remotes",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "List configured remotes",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "all", Usage: "Include disabled remotes"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return remotesList(ctx, newLocalControlClient(), stdout, cmd.Bool("all"))
				},
			},
			{
				Name:      "login",
				Usage:     "Login another remote through the running daemon",
				ArgsUsage: "<remote>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "cloud-url", Value: config.DefaultCloudAPIURL, Usage: "Pax cloud API URL"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return remotesLogin(ctx, newLocalControlClient(), stdout, cmd.Args().First(), cmd.String("cloud-url"))
				},
			},
			{
				Name:      "restart",
				Usage:     "Restart a remote control connection",
				ArgsUsage: "<remote>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return remotesRestart(ctx, newLocalControlClient(), stdout, cmd.Args().First())
				},
			},
			{
				Name:      "disconnect",
				Usage:     "Disable a remote",
				ArgsUsage: "<remote>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return remotesDisconnect(ctx, newLocalControlClient(), stdout, cmd.Args().First())
				},
			},
			{
				Name:      "remove",
				Usage:     "Remove a remote and its local agent connections",
				ArgsUsage: "<remote>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return remotesRemove(ctx, newLocalControlClient(), stdout, cmd.Args().First())
				},
			},
		},
	}
}

func harnessesCommand(stdout io.Writer) *cli.Command {
	return &cli.Command{
		Name:  "harnesses",
		Usage: "List and discover local harnesses",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "List known harnesses",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "all", Usage: "Include missing harnesses"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return harnessesList(ctx, newLocalControlClient(), stdout, cmd.Bool("all"))
				},
			},
			{
				Name:      "discover",
				Usage:     "Discover harness availability",
				ArgsUsage: "[harness...]",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "probe", Usage: "Probe live reachability where supported"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return harnessesDiscover(ctx, newLocalControlClient(), stdout, control.DiscoverHarnessesQuery{
						Probe: cmd.Bool("probe"),
						Names: cmd.Args().Slice(),
					})
				},
			},
		},
	}
}

func statusCommand(ctx context.Context, client localControlClient, stdout io.Writer) error {
	result, err := client.GetStatus(ctx)
	if err != nil {
		return localAPIGuidance(err)
	}
	if err := queryOK(result); err != nil {
		return err
	}
	status := result.Status
	if status == nil {
		return errors.New("local API returned no status")
	}
	fmt.Fprintf(stdout, "DAEMON\t%s\n", firstNonEmpty(status.Phase, "unknown"))
	if len(status.Remotes) > 0 {
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "REMOTE\tPHASE\tERROR")
		for _, item := range status.Remotes {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", item.RemoteID, firstNonEmpty(item.Phase, "-"), firstNonEmpty(item.LastErrorMessage, "-"))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(status.AgentConnections) > 0 {
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "AGENT\tPHASE\tERROR")
		for _, item := range status.AgentConnections {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", item.ConnectionID, firstNonEmpty(item.Phase, "-"), firstNonEmpty(item.LastErrorMessage, "-"))
		}
		return tw.Flush()
	}
	return nil
}

func remotesList(ctx context.Context, client localControlClient, stdout io.Writer, includeDisabled bool) error {
	result, err := client.ListRemotes(ctx, includeDisabled)
	if err != nil {
		return localAPIGuidance(err)
	}
	if err := queryOK(result); err != nil {
		return err
	}
	items := remoteItems(result)
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "REMOTE\tNAME\tURL\tENABLED\tPHASE")
	for _, item := range items {
		enabled := true
		if item.Remote.Enabled != nil {
			enabled = *item.Remote.Enabled
		}
		phase := "-"
		if item.Status != nil {
			phase = firstNonEmpty(item.Status.Phase, "-")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n", item.Remote.ID, item.Remote.Name, item.Remote.CloudAPIURL, enabled, phase)
	}
	return tw.Flush()
}

func remotesLogin(ctx context.Context, client localControlClient, stdout io.Writer, remoteID string, cloudURL string) error {
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		return errors.New("usage: paxctl remotes login <remote>")
	}
	cloudURL = strings.TrimRight(firstNonEmpty(cloudURL, config.DefaultCloudAPIURL), "/")
	cloudClient, err := paxctlLoginCloudClient(cloudURL)
	if err != nil {
		return err
	}
	result, err := runRemoteLogin(ctx, remotelogin.LoginSpec{
		RemoteID:    remoteID,
		CloudAPIURL: cloudURL,
		Node: remotelogin.NodeRegistrationInfo{
			OS:          runtime.GOOS,
			Arch:        runtime.GOARCH,
			PaxdVersion: "paxctl",
		},
	}, remotelogin.Options{
		Client: cloudClient,
		Stdout: stdout,
	})
	if err != nil {
		return err
	}
	secretRef, err := (remotesecrets.Store{}).StoreNodeKey(ctx, result.RemoteID, result.NodeAPIKey)
	if err != nil {
		return err
	}
	enabled := true
	ack, err := client.CreateRemote(ctx, newCommandID(), control.CreateRemoteCommand{
		Remote: control.Remote{
			ID:          result.RemoteID,
			Name:        result.RemoteID,
			CloudAPIURL: result.CloudAPIURL,
			NodeID:      result.NodeID,
			Enabled:     &enabled,
		},
		CloudAPIKeyRef: secretRef,
	})
	if err != nil {
		return localAPIGuidance(err)
	}
	if err := ackOK(ack); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Remote %s login committed.\n", result.RemoteID)
	return nil
}

func paxctlLoginCloudClient(cloudURL string) (*cloud.Client, error) {
	cfg, err := config.Load("")
	if err != nil {
		return nil, err
	}
	client := cloud.NewClient(cloudURL, "")
	clientID := strings.TrimSpace(cfg.Cloud.CFClientID)
	clientSecret := strings.TrimSpace(cfg.Cloud.CFClientSecret)
	if clientID == "" && clientSecret == "" {
		return client, nil
	}
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("cloudflare access login requires both PAX_CLOUD_CF_CLIENT_ID and PAX_CLOUD_CF_CLIENT_SECRET")
	}
	client.WithCloudflareAccess(clientID, clientSecret)
	return client, nil
}

func remotesRestart(ctx context.Context, client localControlClient, stdout io.Writer, remoteID string) error {
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		return errors.New("usage: paxctl remotes restart <remote>")
	}
	ack, err := client.RestartRemote(ctx, newCommandID(), remoteID)
	if err != nil {
		return localAPIGuidance(err)
	}
	if err := ackOK(ack); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Restart requested for remote %s.\n", remoteID)
	return nil
}

func remotesDisconnect(ctx context.Context, client localControlClient, stdout io.Writer, remoteID string) error {
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		return errors.New("usage: paxctl remotes disconnect <remote>")
	}
	ack, err := client.DeleteRemote(ctx, newCommandID(), remoteID, false)
	if err != nil {
		return localAPIGuidance(err)
	}
	if err := ackOK(ack); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Disconnected remote %s.\n", remoteID)
	return nil
}

func remotesRemove(ctx context.Context, client localControlClient, stdout io.Writer, remoteID string) error {
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		return errors.New("usage: paxctl remotes remove <remote>")
	}
	ack, err := client.DeleteRemote(ctx, newCommandID(), remoteID, true)
	if err != nil {
		return localAPIGuidance(err)
	}
	if err := ackOK(ack); err != nil {
		return err
	}
	if err := (remotesecrets.Store{}).DeleteRemote(ctx, remoteID); err != nil && !errors.Is(err, remotesecrets.ErrInvalidRemoteID) {
		return err
	}
	fmt.Fprintf(stdout, "Removed remote %s.\n", remoteID)
	return nil
}

func selectRemote(ctx context.Context, client localControlClient, explicit string) (string, error) {
	remote, err := selectRemoteView(ctx, client, explicit)
	if err != nil {
		return "", err
	}
	return remote.Remote.ID, nil
}

func selectRemoteView(ctx context.Context, client localControlClient, explicit string) (control.RemoteView, error) {
	explicit = strings.TrimSpace(explicit)
	result, err := client.ListRemotes(ctx, false)
	if err != nil {
		return control.RemoteView{}, localAPIGuidance(err)
	}
	if err := queryOK(result); err != nil {
		return control.RemoteView{}, err
	}
	items := remoteItems(result)
	if explicit != "" {
		for _, item := range items {
			if item.Remote.ID == explicit {
				return item, nil
			}
		}
		return control.RemoteView{}, fmt.Errorf("remote %q is not configured or is disabled", explicit)
	}
	switch len(items) {
	case 0:
		return control.RemoteView{}, errors.New("no remotes configured; run paxd login first or paxctl remotes login <remote>")
	case 1:
		return items[0], nil
	default:
		return control.RemoteView{}, errors.New("multiple remotes configured; pass --remote")
	}
}

func agentConnectionsList(ctx context.Context, client localControlClient, stdout io.Writer, includeDisabled bool) error {
	result, err := client.ListAgentConnections(ctx, includeDisabled)
	if err != nil {
		return localAPIGuidance(err)
	}
	if err := queryOK(result); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tREMOTE\tNAME\tHARNESS\tDESIRED\tPHASE")
	for _, item := range agentItems(result) {
		phase := "-"
		if item.Status != nil {
			phase = firstNonEmpty(item.Status.Phase, "-")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", item.ID, item.RemoteID, item.Name, item.Harness, item.DesiredState, phase)
	}
	return tw.Flush()
}

func agentConnectionCreate(ctx context.Context, client localControlClient, stdout io.Writer, cmd *cli.Command) error {
	harness := strings.TrimSpace(cmd.String("harness"))
	name := strings.TrimSpace(cmd.String("name"))
	if harness == "" || name == "" {
		return errors.New("usage: paxctl agents create [--remote <remote>] --harness <harness> --name <name>")
	}
	remote, err := selectRemoteView(ctx, client, cmd.String("remote"))
	if err != nil {
		return err
	}
	command, err := resolveHarnessCommand(ctx, client, harness)
	if err != nil {
		return err
	}
	nodeKey, err := loadRemoteNodeKey(ctx, remote.Remote.ID)
	if err != nil {
		return err
	}
	cloudAgentID, err := registerCloudAgent(ctx, remote, nodeKey, name, harness)
	if err != nil {
		return err
	}
	ack, err := client.CreateAgentConnection(ctx, newCommandID(), control.CreateAgentConnectionCommand{
		RemoteID:     remote.Remote.ID,
		Name:         name,
		CloudAgentID: cloudAgentID,
		InstanceID:   name,
		AgentType:    harness,
		Harness:      harness,
		Command:      command,
		DesiredState: control.DesiredStateRunning,
	})
	if err != nil {
		return localAPIGuidance(err)
	}
	if err := ackOK(ack); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Agent create requested for %s on remote %s as %s.\n", name, remote.Remote.ID, cloudAgentID)
	return nil
}

func resolveHarnessCommand(ctx context.Context, client localControlClient, harness string) ([]string, error) {
	result, err := client.ListHarnesses(ctx, true)
	if err != nil {
		return nil, localAPIGuidance(err)
	}
	if err := queryOK(result); err != nil {
		return nil, err
	}
	command, cached, cachedErr := commandForHarness(harnessItems(result), harness)
	if cached && cachedErr == nil {
		return command, nil
	}

	result, err = client.DiscoverHarnesses(ctx, control.DiscoverHarnessesQuery{Names: []string{harness}})
	if err != nil {
		return nil, localAPIGuidance(err)
	}
	if err := queryOK(result); err != nil {
		return nil, err
	}
	if command, ok, err := commandForHarness(harnessItems(result), harness); ok || err != nil {
		return command, err
	}
	if cachedErr != nil {
		return nil, cachedErr
	}
	return nil, fmt.Errorf("harness %q is not known; run `paxctl harnesses discover %s` and choose an available harness", harness, harness)
}

func commandForHarness(items []control.HarnessView, harness string) ([]string, bool, error) {
	for _, item := range items {
		if !strings.EqualFold(item.Harness, harness) {
			continue
		}
		state := strings.TrimSpace(item.State)
		if state != "" && state != "available" {
			reason := firstNonEmpty(item.LastError, item.InstallHint, "harness is not available")
			return nil, true, fmt.Errorf("harness %q is %s: %s", harness, state, reason)
		}
		if len(item.Command) == 0 {
			return nil, true, fmt.Errorf("harness %q has no command configured; run `paxctl harnesses discover %s`", harness, harness)
		}
		return append([]string(nil), item.Command...), true, nil
	}
	return nil, false, nil
}

func agentConnectionRestart(ctx context.Context, client localControlClient, stdout io.Writer, connectionID string) error {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return errors.New("usage: paxctl agents restart <name-or-id>")
	}
	connectionID, err := resolveAgentConnectionID(ctx, client, connectionID)
	if err != nil {
		return err
	}
	ack, err := client.RestartAgentConnection(ctx, newCommandID(), connectionID)
	if err != nil {
		return localAPIGuidance(err)
	}
	if err := ackOK(ack); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Restart requested for agent %s.\n", connectionID)
	return nil
}

func agentConnectionStop(ctx context.Context, client localControlClient, stdout io.Writer, connectionID string) error {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return errors.New("usage: paxctl agents stop <name-or-id>")
	}
	connectionID, err := resolveAgentConnectionID(ctx, client, connectionID)
	if err != nil {
		return err
	}
	state := control.DesiredStateStopped
	ack, err := client.UpdateAgentConnection(ctx, newCommandID(), connectionID, control.UpdateAgentConnectionCommand{DesiredState: &state})
	if err != nil {
		return localAPIGuidance(err)
	}
	if err := ackOK(ack); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Stop requested for agent %s.\n", connectionID)
	return nil
}

func agentConnectionRemove(ctx context.Context, client localControlClient, stdout io.Writer, connectionID string) error {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return errors.New("usage: paxctl agents remove <name-or-id>")
	}
	connectionID, err := resolveAgentConnectionID(ctx, client, connectionID)
	if err != nil {
		return err
	}
	ack, err := client.DeleteAgentConnection(ctx, newCommandID(), connectionID)
	if err != nil {
		return localAPIGuidance(err)
	}
	if err := ackOK(ack); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Remove requested for agent %s.\n", connectionID)
	return nil
}

func resolveAgentConnectionID(ctx context.Context, client localControlClient, nameOrID string) (string, error) {
	result, err := client.ListAgentConnections(ctx, true)
	if err != nil {
		return "", localAPIGuidance(err)
	}
	if err := queryOK(result); err != nil {
		return "", err
	}
	var matches []control.AgentConnectionView
	for _, item := range agentItems(result) {
		if item.ID == nameOrID || item.Name == nameOrID {
			matches = append(matches, item)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("agent connection %q not found", nameOrID)
	case 1:
		return matches[0].ID, nil
	default:
		ids := make([]string, 0, len(matches))
		for _, item := range matches {
			ids = append(ids, item.ID)
		}
		return "", fmt.Errorf("agent connection name %q is ambiguous; use one of: %s", nameOrID, strings.Join(ids, ", "))
	}
}

func harnessesList(ctx context.Context, client localControlClient, stdout io.Writer, includeMissing bool) error {
	result, err := client.ListHarnesses(ctx, includeMissing)
	if err != nil {
		return localAPIGuidance(err)
	}
	if err := queryOK(result); err != nil {
		return err
	}
	return renderHarnesses(stdout, harnessItems(result))
}

func harnessesDiscover(ctx context.Context, client localControlClient, stdout io.Writer, query control.DiscoverHarnessesQuery) error {
	result, err := client.DiscoverHarnesses(ctx, query)
	if err != nil {
		return localAPIGuidance(err)
	}
	if err := queryOK(result); err != nil {
		return err
	}
	return renderHarnesses(stdout, harnessItems(result))
}

func renderHarnesses(stdout io.Writer, items []control.HarnessView) error {
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "HARNESS\tSTATE\tCOMMAND\tSOURCE\tNOTE")
	for _, item := range items {
		command := "-"
		if len(item.Command) > 0 {
			command = strings.Join(item.Command, " ")
		}
		note := firstNonEmpty(item.LastError, item.InstallHint, "-")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", item.Harness, firstNonEmpty(item.State, "-"), command, firstNonEmpty(item.Source, "-"), note)
	}
	return tw.Flush()
}

func queryOK(result control.QueryResult) error {
	if result.Error != nil {
		return result.Error
	}
	return nil
}

func ackOK(ack control.CommandAck) error {
	if ack.OK || ack.Status == control.CommandStatusReceived || ack.Status == control.CommandStatusApplied {
		return nil
	}
	if ack.Error != nil {
		return ack.Error
	}
	return fmt.Errorf("local API command failed: %s", firstNonEmpty(string(ack.Status), "unknown"))
}

func localAPIGuidance(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w; is paxd running? try `paxd run` or `paxd setup`", err)
}

func remoteItems(result control.QueryResult) []control.RemoteView {
	if result.Remotes == nil {
		return nil
	}
	return result.Remotes.Items
}

func agentItems(result control.QueryResult) []control.AgentConnectionView {
	if result.AgentConnections == nil {
		return nil
	}
	return result.AgentConnections.Items
}

func harnessItems(result control.QueryResult) []control.HarnessView {
	if result.Harnesses == nil {
		return nil
	}
	return result.Harnesses.Items
}

func newCommandID() string {
	id, err := newLocalID("cmd")
	if err != nil {
		return "cmd_local_fallback"
	}
	return id
}

func agentsList(cmd *cli.Command, stdout io.Writer) error {
	statuses, err := agentregistry.Default().StatusesWithProbe(nil, cmd.Bool("probe"))
	if err != nil {
		return err
	}
	switch cmd.String("format") {
	case "table":
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "AGENT\tSTATUS\tCAPABILITY\tCOMMAND\tSOURCE")
		for _, status := range statuses {
			state := firstNonEmpty(status.State, "missing")
			command := "-"
			if len(status.Command) > 0 {
				command = strings.Join(status.Command, " ")
			}
			capability := firstNonEmpty(status.Capability, "-")
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", status.Agent.Name, state, capability, command, status.Agent.Source)
		}
		return tw.Flush()
	case "jsonl":
		encoder := json.NewEncoder(stdout)
		for _, status := range statuses {
			if err := encoder.Encode(map[string]any{
				"agent":      status.Agent.Name,
				"kind":       status.Agent.Kind,
				"available":  status.Available,
				"state":      firstNonEmpty(status.State, "missing"),
				"capability": status.Capability,
				"command":    status.Command,
				"source":     status.Agent.Source,
				"reason":     status.Reason,
			}); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported format %q", cmd.String("format"))
	}
}

func agentsSetup(ctx context.Context, cmd *cli.Command, stdout, stderr io.Writer) error {
	agents := parseCSV(cmd.String("agents"))
	explicit := len(agents) > 0
	statuses, err := agentregistry.Default().StatusesWithProbe(agents, false)
	if err != nil {
		return err
	}
	selected := 0
	for _, status := range statuses {
		if status.Available {
			if explicit {
				fmt.Fprintf(stdout, "%s already available via %s\n", status.Agent.Name, strings.Join(status.Command, " "))
			}
			continue
		}
		if len(status.Agent.InstallCommands) == 0 {
			if explicit {
				return fmt.Errorf("agent %s has no setup command: %s", status.Agent.Name, firstNonEmpty(status.Reason, status.Agent.InstallHint, "unsupported setup"))
			}
			continue
		}
		if !explicit && status.State != "installable" {
			continue
		}
		selected++
		if err := runAgentSetupCommands(ctx, stdout, stderr, status.Agent.Name, status.Agent.InstallCommands, cmd.Bool("dry-run")); err != nil {
			return err
		}
	}
	if selected == 0 && !explicit {
		fmt.Fprintln(stdout, "No installable agents found.")
	}
	return nil
}

func runAgentSetupCommands(ctx context.Context, stdout, stderr io.Writer, agent string, commands [][]string, dryRun bool) error {
	fmt.Fprintf(stdout, "Setting up %s\n", agent)
	for _, command := range commands {
		if len(command) == 0 {
			continue
		}
		fmt.Fprintf(stdout, "$ %s\n", strings.Join(command, " "))
		if dryRun {
			continue
		}
		proc := exec.CommandContext(ctx, command[0], command[1:]...)
		proc.Stdout = stderr
		proc.Stderr = stderr
		if err := proc.Run(); err != nil {
			return fmt.Errorf("setup %s: %s: %w", agent, strings.Join(command, " "), err)
		}
	}
	if !dryRun {
		fmt.Fprintf(stdout, "Setup complete for %s\n", agent)
	}
	return nil
}

func openSessionStore(cmd *cli.Command) (*sessionstore.Store, error) {
	return sessionstore.Open(cmd.String("db"))
}

func sessionsList(ctx context.Context, cmd *cli.Command, stdout, stderr io.Writer) error {
	store, err := openSessionStore(cmd)
	if err != nil {
		return err
	}
	defer store.Close()

	agents := parseCSV(cmd.String("agents"))
	limit := cmd.Int("limit")
	cutoff, err := parseUpdatedSince(cmd.String("updated-since"))
	if err != nil {
		return err
	}
	sessions, err := store.ListSessions(ctx, agents, limit)
	if err != nil {
		return err
	}
	sessions = filterSessions(sessions, cutoff, limit)
	return renderSessionList(stdout, sessions, cmd.String("format"))
}

func sessionsSync(ctx context.Context, cmd *cli.Command, stdout, stderr io.Writer) error {
	store, err := openSessionStore(cmd)
	if err != nil {
		return err
	}
	defer store.Close()

	agents := parseCSV(cmd.String("agents"))
	limit := cmd.Int("limit")
	cutoff, err := parseUpdatedSince(cmd.String("updated-since"))
	if err != nil {
		return err
	}
	timeout, err := parseDuration(cmd.String("timeout"))
	if err != nil {
		return fmt.Errorf("invalid timeout: %w", err)
	}
	if err := scanMetadata(ctx, store, agents, limit, timeout, stderr); err != nil {
		return err
	}
	sessions, err := store.ListSessions(ctx, agents, limit)
	if err != nil {
		return err
	}
	sessions = filterSessions(sessions, cutoff, limit)
	for _, session := range sessions {
		if err := syncSession(ctx, store, session); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "Synced %d session metadata records and available timelines.\n", len(sessions))
	return nil
}

func sessionsGet(ctx context.Context, cmd *cli.Command, stdout io.Writer) error {
	sessionID := cmd.Args().First()
	if sessionID == "" {
		return errors.New("usage: paxctl sessions get <session-id>")
	}
	store, err := openSessionStore(cmd)
	if err != nil {
		return err
	}
	defer store.Close()

	session, err := store.FindSession(ctx, sessionID, cmd.String("agent"))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("session %q not found; run paxctl sessions list first", sessionID)
	}
	if err != nil {
		return err
	}
	if session.CurrentSyncVersion == 0 {
		if err := syncSession(ctx, store, session); err != nil {
			return err
		}
		session, err = store.FindSession(ctx, session.ID, "")
		if err != nil {
			return err
		}
	}
	elements, err := store.Elements(ctx, session)
	if err != nil {
		return err
	}

	format := cmd.String("format")
	output := cmd.String("output")
	var writer io.Writer = stdout
	var file *os.File
	if format == "html" {
		if output == "" {
			output = defaultHTMLPath(session)
		}
		file, err = os.Create(output)
		if err != nil {
			return err
		}
		defer file.Close()
		writer = file
	} else if output != "" {
		file, err = os.Create(output)
		if err != nil {
			return err
		}
		defer file.Close()
		writer = file
	}

	switch format {
	case "transcript":
		err = sessionrender.Transcript(writer, session, elements)
	case "jsonl":
		err = sessionrender.JSONL(writer, session, elements)
	case "html":
		err = sessionrender.HTML(writer, session, elements)
	default:
		return fmt.Errorf("unsupported format %q", format)
	}
	if err != nil {
		return err
	}
	if format == "html" {
		fmt.Fprintf(stdout, "Wrote %s\n", output)
	}
	return nil
}

func capsulesCreate(ctx context.Context, cmd *cli.Command, stdout io.Writer) error {
	sourceID := cmd.Args().First()
	if sourceID == "" {
		return errors.New("usage: paxctl capsules create <source-session-id> --keyword <keyword>")
	}
	keyword := strings.TrimSpace(cmd.String("keyword"))
	if keyword == "" {
		return errors.New("keyword is required")
	}
	if len(keyword) > knowledgeKeywordLimit {
		return fmt.Errorf("keyword is too long: max %d chars", knowledgeKeywordLimit)
	}
	store, err := openSessionStore(cmd)
	if err != nil {
		return err
	}
	defer store.Close()

	session, err := store.FindSession(ctx, sourceID, cmd.String("agent"))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("source session %q not found; run paxctl sessions sync first", sourceID)
	}
	if err != nil {
		return err
	}
	if session.CurrentSyncVersion == 0 {
		if err := syncSession(ctx, store, session); err != nil {
			return err
		}
		session, err = store.FindSession(ctx, session.ID, "")
		if err != nil {
			return err
		}
	}
	elements, err := store.Elements(ctx, session)
	if err != nil {
		return err
	}
	capsule, err := buildLocalKnowledgeCapsule(session, keyword, elements)
	if err != nil {
		return err
	}
	created, err := store.CreateKnowledgeCapsule(ctx, capsule)
	if err != nil {
		return err
	}
	return renderCapsuleList(stdout, []sessionstore.KnowledgeCapsule{created}, cmd.String("format"))
}

func capsulesList(ctx context.Context, cmd *cli.Command, stdout io.Writer) error {
	store, err := openSessionStore(cmd)
	if err != nil {
		return err
	}
	defer store.Close()

	capsules, err := store.ListKnowledgeCapsules(ctx, cmd.String("status"), cmd.String("keyword"),
		cmd.String("source-session"), cmd.Int("limit"))
	if err != nil {
		return err
	}
	return renderCapsuleList(stdout, capsules, cmd.String("format"))
}

func capsulesGet(ctx context.Context, cmd *cli.Command, stdout io.Writer) error {
	capsuleID := cmd.Args().First()
	if capsuleID == "" {
		return errors.New("usage: paxctl capsules get <capsule-id>")
	}
	store, err := openSessionStore(cmd)
	if err != nil {
		return err
	}
	defer store.Close()

	capsule, err := store.GetKnowledgeCapsule(ctx, capsuleID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("capsule %q not found", capsuleID)
	}
	if err != nil {
		return err
	}
	switch cmd.String("format") {
	case "text":
		_, err = fmt.Fprintln(stdout, renderCapsuleText(capsule))
		return err
	case "jsonl":
		return encodeCapsuleJSONL(stdout, capsule)
	default:
		return fmt.Errorf("unsupported format %q", cmd.String("format"))
	}
}

func capsulesArchive(ctx context.Context, cmd *cli.Command, stdout io.Writer) error {
	capsuleID := cmd.Args().First()
	if capsuleID == "" {
		return errors.New("usage: paxctl capsules archive <capsule-id>")
	}
	store, err := openSessionStore(cmd)
	if err != nil {
		return err
	}
	defer store.Close()

	capsule, err := store.ArchiveKnowledgeCapsule(ctx, capsuleID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("capsule %q not found", capsuleID)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Archived %s\n", capsule.CapsuleID)
	return nil
}

func capsulesInject(ctx context.Context, cmd *cli.Command, stdout io.Writer) error {
	if cmd.Args().Len() < 2 {
		return errors.New("usage: paxctl capsules inject <capsule-id> <target-session-id>")
	}
	capsuleID := cmd.Args().Get(0)
	targetID := cmd.Args().Get(1)
	store, err := openSessionStore(cmd)
	if err != nil {
		return err
	}
	defer store.Close()

	capsule, err := store.GetKnowledgeCapsule(ctx, capsuleID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("capsule %q not found", capsuleID)
	}
	if err != nil {
		return err
	}
	if capsule.Status != "active" {
		return fmt.Errorf("capsule %q is not active", capsuleID)
	}
	target, err := store.FindSession(ctx, targetID, cmd.String("agent"))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("target session %q not found; run paxctl sessions sync first", targetID)
	}
	if err != nil {
		return err
	}

	injectionID, err := newLocalID("kci")
	if err != nil {
		return err
	}
	injection := sessionstore.KnowledgeInjection{
		InjectionID:         injectionID,
		CapsuleID:           capsule.CapsuleID,
		TargetSessionID:     target.ID,
		TargetAgent:         target.Agent,
		DeliveryMethod:      "acp_steer",
		DeliveryMessageType: "system_handoff",
		Status:              "delivered",
	}
	message := renderKnowledgeHandoff(capsule, injection, knowledgeDeliveryLimit)
	timeout, err := parseDuration(cmd.String("timeout"))
	if err != nil {
		return fmt.Errorf("invalid timeout: %w", err)
	}
	if err := steerSession(ctx, target.Agent, target.NativeID, message, timeout); err != nil {
		return err
	}
	injection, err = store.CreateKnowledgeInjection(ctx, sessionstore.KnowledgeInjection{
		InjectionID:         injection.InjectionID,
		CapsuleID:           injection.CapsuleID,
		TargetSessionID:     injection.TargetSessionID,
		TargetAgent:         injection.TargetAgent,
		DeliveryMethod:      injection.DeliveryMethod,
		DeliveryMessageType: injection.DeliveryMessageType,
		Status:              injection.Status,
	})
	if err != nil {
		return err
	}
	if output := cmd.String("output"); output != "" {
		if err := os.WriteFile(output, []byte(message+"\n"), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Injected %s into %s and wrote %s\n", injection.InjectionID, target.ID, output)
		return nil
	}
	fmt.Fprintf(stdout, "Injected %s into %s\n", injection.InjectionID, target.ID)
	return nil
}

func capsulesInjections(ctx context.Context, cmd *cli.Command, stdout io.Writer) error {
	store, err := openSessionStore(cmd)
	if err != nil {
		return err
	}
	defer store.Close()

	injections, err := store.ListKnowledgeInjections(ctx, cmd.String("target-session"), cmd.Int("limit"))
	if err != nil {
		return err
	}
	return renderInjectionList(stdout, injections, cmd.String("format"))
}

func scanMetadata(ctx context.Context, store *sessionstore.Store, agents []string, limit int, timeout time.Duration, stderr io.Writer) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	explicit := len(agents) > 0
	statuses, err := agentregistry.Default().Statuses(agents)
	if err != nil {
		return err
	}
	available := 0
	for _, status := range statuses {
		if !status.Available {
			if explicit {
				return fmt.Errorf("agent %s is unavailable: %s", status.Agent.Name, status.Reason)
			}
			continue
		}
		available++
		fmt.Fprintf(stderr, "syncing %s via %s (timeout %s)...\n", status.Agent.Name, strings.Join(status.Command, " "), timeout)
		sessions, err := agentregistry.ListSessions(ctx, status, timeout)
		if err != nil {
			if explicit {
				return fmt.Errorf("agent %s list sessions: %w", status.Agent.Name, err)
			}
			fmt.Fprintf(stderr, "warning: agent %s list failed: %v\n", status.Agent.Name, err)
			continue
		}
		if limit > 0 && len(sessions) > limit {
			sessions = sessions[:limit]
		}
		if err := store.UpsertSessions(ctx, status.Agent.Name, sessions); err != nil {
			return err
		}
	}
	if available == 0 {
		return errors.New("no supported local agents are available; run paxctl agents list")
	}
	return nil
}

func syncSession(ctx context.Context, store *sessionstore.Store, session sessionstore.Session) error {
	version, err := store.BeginSync(ctx, session.ID, session.Agent)
	if err != nil {
		return err
	}
	var elements []sessionstore.Element
	if session.Agent == "codex" {
		elements, err = agentregistry.CodexLocalElements(session.NativeID)
		if err != nil {
			_ = store.FailSync(ctx, version, err)
			return err
		}
	} else if session.Agent == "qwen" {
		elements, err = agentregistry.QwenLocalElements(session.NativeID)
		if err != nil {
			_ = store.FailSync(ctx, version, err)
			return err
		}
	}
	if err := store.CompleteSync(ctx, session.ID, version, elements); err != nil {
		_ = store.FailSync(ctx, version, err)
		return err
	}
	return nil
}

func renderSessionList(w io.Writer, sessions []sessionstore.Session, format string) error {
	switch format {
	case "table":
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tAGENT\tUPDATED\tTITLE")
		for _, session := range sessions {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", session.ID, session.Agent, shortTime(session.UpdatedAt), firstNonEmpty(session.Title, session.Preview, "-"))
		}
		return tw.Flush()
	case "jsonl":
		encoder := json.NewEncoder(w)
		for _, session := range sessions {
			if err := encoder.Encode(map[string]any{
				"schemaVersion": "pax.session.metadata.v1",
				"id":            session.ID,
				"agent":         session.Agent,
				"nativeId":      session.NativeID,
				"title":         session.Title,
				"status":        session.Status,
				"preview":       session.Preview,
				"projectId":     session.ProjectID,
				"updatedAt":     session.UpdatedAt,
				"lastSyncedAt":  session.LastSyncedAt,
			}); err != nil {
				return err
			}
		}
		return nil
	case "html":
		return renderSessionListHTML(w, sessions)
	default:
		return fmt.Errorf("unsupported format %q", format)
	}
}

func renderSessionListHTML(w io.Writer, sessions []sessionstore.Session) error {
	if _, err := fmt.Fprintln(w, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>paxctl sessions</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:32px;color:#202124;background:#fafafa}
h1{font-size:24px;margin:0 0 20px}
table{width:100%;border-collapse:collapse;background:#fff;border:1px solid #ddd}
th,td{padding:10px 12px;border-bottom:1px solid #eee;text-align:left;vertical-align:top}
th{font-size:12px;text-transform:uppercase;color:#5f6368;background:#f5f5f5}
code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.empty{color:#5f6368}
</style>
</head>
<body>
<h1>paxctl sessions</h1>`); err != nil {
		return err
	}
	if len(sessions) == 0 {
		_, err := fmt.Fprintln(w, `<p class="empty">No local session metadata found. Run <code>paxctl sessions sync</code> to scan supported agents.</p></body></html>`)
		return err
	}
	if _, err := fmt.Fprintln(w, `<table><thead><tr><th>ID</th><th>Agent</th><th>Updated</th><th>Title</th></tr></thead><tbody>`); err != nil {
		return err
	}
	for _, session := range sessions {
		title := firstNonEmpty(session.Title, session.Preview, "-")
		if _, err := fmt.Fprintf(w, "<tr><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			html.EscapeString(session.ID),
			html.EscapeString(session.Agent),
			html.EscapeString(shortTime(session.UpdatedAt)),
			html.EscapeString(title),
		); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, `</tbody></table></body></html>`)
	return err
}

func buildLocalKnowledgeCapsule(
	session sessionstore.Session,
	keyword string,
	elements []sessionstore.Element,
) (sessionstore.KnowledgeCapsule, error) {
	capsuleID, err := newLocalID("kcap")
	if err != nil {
		return sessionstore.KnowledgeCapsule{}, err
	}
	content, originalChars, truncated := extractKnowledgeContent(keyword, elements)
	if strings.TrimSpace(content) == "" {
		content = "No matching session history was found for this keyword."
	}
	title := truncateString("Knowledge capsule: "+keyword, knowledgeTitleLimit)
	summary := truncateString(
		fmt.Sprintf("Extracted knowledge related to %q from local session %s. Review source context before relying on this handoff.", keyword, session.ID),
		knowledgeSummaryLimit,
	)
	return sessionstore.KnowledgeCapsule{
		CapsuleID:              capsuleID,
		SourceSessionID:        session.ID,
		SourceAgent:            session.Agent,
		Keyword:                keyword,
		Title:                  title,
		Summary:                summary,
		Content:                content,
		Status:                 "active",
		Truncated:              truncated,
		OriginalEstimatedChars: int64(originalChars),
		CreatedAt:              time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func extractKnowledgeContent(keyword string, elements []sessionstore.Element) (string, int, bool) {
	needle := strings.ToLower(keyword)
	var builder strings.Builder
	originalChars := 0
	lines := 0
	for _, element := range elements {
		text := elementSearchText(element)
		if text == "" || !strings.Contains(strings.ToLower(text), needle) {
			continue
		}
		if lines >= knowledgeExtractLineLimit {
			break
		}
		role := firstNonEmpty(element.Role, element.Type, "event")
		stamp := firstNonEmpty(element.CompletedAt, element.StartedAt)
		line := fmt.Sprintf("- [%s %s] %s", role, stamp, redactKnowledgeSecrets(strings.TrimSpace(text)))
		originalChars += len(line)
		if builder.Len()+len(line)+1 > knowledgeContentLimit {
			return builder.String(), originalChars, true
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(line)
		lines++
	}
	return builder.String(), originalChars, lines >= knowledgeExtractLineLimit
}

func elementSearchText(element sessionstore.Element) string {
	parts := []string{}
	if element.ContentText != "" {
		parts = append(parts, element.ContentText)
	}
	if len(element.NormalizedRaw) > 0 {
		normalized, err := json.Marshal(element.NormalizedRaw)
		if err == nil {
			parts = append(parts, string(normalized))
		}
	}
	if element.RawJSON != "" {
		parts = append(parts, element.RawJSON)
	}
	return strings.Join(parts, " ")
}

func renderCapsuleList(w io.Writer, capsules []sessionstore.KnowledgeCapsule, format string) error {
	switch format {
	case "table":
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATUS\tSOURCE\tKEYWORD\tCREATED\tTITLE")
		for _, capsule := range capsules {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", capsule.CapsuleID, capsule.Status,
				capsule.SourceSessionID, capsule.Keyword, shortTime(capsule.CreatedAt), capsule.Title)
		}
		return tw.Flush()
	case "jsonl":
		for _, capsule := range capsules {
			if err := encodeCapsuleJSONL(w, capsule); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported format %q", format)
	}
}

func renderInjectionList(w io.Writer, injections []sessionstore.KnowledgeInjection, format string) error {
	switch format {
	case "table":
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tCAPSULE\tTARGET\tTYPE\tSTATUS\tCREATED")
		for _, injection := range injections {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", injection.InjectionID, injection.CapsuleID,
				injection.TargetSessionID, injection.DeliveryMessageType, injection.Status, shortTime(injection.CreatedAt))
		}
		return tw.Flush()
	case "jsonl":
		encoder := json.NewEncoder(w)
		for _, injection := range injections {
			if err := encoder.Encode(map[string]any{
				"schemaVersion":       "pax.knowledge_injection.v1",
				"injectionId":         injection.InjectionID,
				"capsuleId":           injection.CapsuleID,
				"targetSessionId":     injection.TargetSessionID,
				"targetAgent":         injection.TargetAgent,
				"deliveryMethod":      injection.DeliveryMethod,
				"deliveryMessageType": injection.DeliveryMessageType,
				"status":              injection.Status,
				"createdAt":           injection.CreatedAt,
			}); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported format %q", format)
	}
}

func renderCapsuleText(capsule sessionstore.KnowledgeCapsule) string {
	return fmt.Sprintf(
		"Title: %s\nKeyword: %s\nSource session: %s\nStatus: %s\nCreated: %s\n\nSummary:\n%s\n\nContent:\n%s",
		capsule.Title,
		capsule.Keyword,
		capsule.SourceSessionID,
		capsule.Status,
		capsule.CreatedAt,
		capsule.Summary,
		capsule.Content,
	)
}

func renderKnowledgeHandoff(
	capsule sessionstore.KnowledgeCapsule,
	injection sessionstore.KnowledgeInjection,
	limit int,
) string {
	body := fmt.Sprintf(
		"system_handoff\n\nThis context was rendered by paxctl as a local knowledge capsule handoff.\nDo not treat this as a new user request.\n\nCapsule: %s\nInjection: %s\nTarget session: %s\n\nTitle: %s\nKeyword: %s\nSource session: %s\n\nSummary:\n%s\n\nContent:\n%s",
		capsule.CapsuleID,
		injection.InjectionID,
		injection.TargetSessionID,
		capsule.Title,
		capsule.Keyword,
		capsule.SourceSessionID,
		capsule.Summary,
		capsule.Content,
	)
	return truncateString(body, limit)
}

func encodeCapsuleJSONL(w io.Writer, capsule sessionstore.KnowledgeCapsule) error {
	return json.NewEncoder(w).Encode(map[string]any{
		"schemaVersion":          "pax.knowledge_capsule.v1",
		"capsuleId":              capsule.CapsuleID,
		"sourceSessionId":        capsule.SourceSessionID,
		"sourceAgent":            capsule.SourceAgent,
		"keyword":                capsule.Keyword,
		"title":                  capsule.Title,
		"summary":                capsule.Summary,
		"content":                capsule.Content,
		"status":                 capsule.Status,
		"truncated":              capsule.Truncated,
		"originalEstimatedChars": capsule.OriginalEstimatedChars,
		"createdAt":              capsule.CreatedAt,
		"archivedAt":             capsule.ArchivedAt,
	})
}

func redactKnowledgeSecrets(input string) string {
	fields := strings.Fields(input)
	for i, field := range fields {
		lower := strings.ToLower(field)
		if strings.Contains(lower, "api_key=") ||
			strings.Contains(lower, "token=") ||
			strings.Contains(lower, "authorization:") ||
			strings.HasPrefix(field, "sk-") ||
			strings.HasPrefix(field, "pax_") {
			fields[i] = "[redacted]"
		}
	}
	return strings.Join(fields, " ")
}

func filterSessions(sessions []sessionstore.Session, cutoff *time.Time, limit int) []sessionstore.Session {
	out := make([]sessionstore.Session, 0, len(sessions))
	for _, session := range sessions {
		if cutoff != nil {
			updated, ok := parseSessionTime(firstNonEmpty(session.UpdatedAt, session.LastActive, session.LastListedAt))
			if ok && updated.Before(*cutoff) {
				continue
			}
		}
		out = append(out, session)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func parseCSV(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseUpdatedSince(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	duration, err := parseDuration(value)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-duration)
	return &cutoff, nil
}

func parseDuration(value string) (time.Duration, error) {
	if strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(value)
}

func parseSessionTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func shortTime(value string) string {
	parsed, ok := parseSessionTime(value)
	if !ok {
		return value
	}
	return parsed.Local().Format("2006-01-02 15:04")
}

func defaultHTMLPath(session sessionstore.Session) string {
	safeID := strings.NewReplacer(":", "-", "/", "-", "\\", "-").Replace(session.ID)
	stamp := time.Now().Format("20060102-1504")
	return filepath.Join(".", "pax-session-"+safeID+"-"+stamp+".html")
}

func newLocalID(prefix string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(buf[:]), nil
}

func truncateString(input string, limit int) string {
	if limit <= 0 || len(input) <= limit {
		return input
	}
	if limit <= 3 {
		return input[:limit]
	}
	return input[:limit-3] + "..."
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
