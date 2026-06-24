package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	paxdaemon "github.com/pax-beehive/paxd/internal/daemon"
	"github.com/pax-beehive/paxd/internal/localapi"
	"github.com/urfave/cli/v3"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	app := newApp(stdout, stderr)
	return app.Run(ctx, append([]string{"paxctl"}, args...))
}

type controlClient interface {
	GetStatus(ctx context.Context) (control.QueryResult, error)
	ListRemotes(ctx context.Context, includeDisabled bool) (control.QueryResult, error)
	CreateRemote(ctx context.Context, commandID string, cmd control.CreateRemoteCommand) (control.CommandAck, error)
	UpdateRemote(ctx context.Context, commandID string, remoteID string, cmd control.UpdateRemoteCommand) (control.CommandAck, error)
	RestartRemote(ctx context.Context, commandID string, remoteID string) (control.CommandAck, error)
	DeleteRemote(ctx context.Context, commandID string, remoteID string, cascadeAgentConnections bool) (control.CommandAck, error)
	ListAgentConnections(ctx context.Context, includeDisabled bool) (control.QueryResult, error)
	ListHarnesses(ctx context.Context, includeMissing bool) (control.QueryResult, error)
	DiscoverHarnesses(ctx context.Context, query control.DiscoverHarnessesQuery) (control.QueryResult, error)
	ListLocalSessions(ctx context.Context, query control.ListLocalSessionsQuery) (control.QueryResult, error)
	SyncLocalSessions(ctx context.Context, query control.SyncLocalSessionsQuery) (control.QueryResult, error)
	CreateAgentConnection(ctx context.Context, commandID string, cmd control.CreateAgentConnectionCommand) (control.CommandAck, error)
	RestartAgentConnection(ctx context.Context, commandID string, connectionID string) (control.CommandAck, error)
	DeleteAgentConnection(ctx context.Context, commandID string, connectionID string) (control.CommandAck, error)
}

var newControlClient = localClient

func newApp(stdout io.Writer, stderr io.Writer) *cli.Command {
	socket := paxdaemon.DefaultControlSocket
	debugHTTP := ""
	client := func() controlClient {
		return newControlClient(socket, debugHTTP)
	}
	return &cli.Command{
		Name:      "paxctl",
		Usage:     "control a local paxd daemon",
		Writer:    stdout,
		ErrWriter: stderr,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "socket", Usage: "paxd Unix socket path", Value: paxdaemon.DefaultControlSocket, Destination: &socket, Local: true},
			&cli.StringFlag{Name: "debug-http", Usage: "debug HTTP base URL, for example http://127.0.0.1:8765", Destination: &debugHTTP, Local: true},
		},
		Action: func(context.Context, *cli.Command) error {
			return fmt.Errorf("command is required: status, remotes, agent-connections, harnesses, local-sessions")
		},
		Commands: []*cli.Command{
			statusCommand(stdout, client),
			remotesCommand(stdout, client),
			agentConnectionsCommand(stdout, client),
			harnessesCommand(stdout, client),
			localSessionsCommand(stdout, client),
		},
	}
}

func statusCommand(stdout io.Writer, client func() controlClient) *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "show daemon status",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			result, err := client().GetStatus(ctx)
			return writeResult(stdout, result, err)
		},
	}
}

func remotesCommand(stdout io.Writer, client func() controlClient) *cli.Command {
	return &cli.Command{
		Name:  "remotes",
		Usage: "manage remote Pax manager endpoints",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			result, err := client().ListRemotes(ctx, true)
			return writeResult(stdout, result, err)
		},
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list remotes",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					result, err := client().ListRemotes(ctx, true)
					return writeResult(stdout, result, err)
				},
			},
			{
				Name:  "create",
				Usage: "create a remote",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "id", Usage: "remote id"},
					&cli.StringFlag{Name: "name", Usage: "remote name"},
					&cli.StringFlag{Name: "api-url", Usage: "Pax manager API URL"},
					&cli.StringFlag{Name: "node-id", Usage: "node id"},
					&cli.StringFlag{Name: "api-key-ref", Usage: "Pax node key secret ref"},
					&cli.BoolFlag{Name: "enabled", Usage: "enable remote", Value: true},
					&cli.BoolFlag{Name: "default", Usage: "mark as default remote"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					enabled := cmd.Bool("enabled")
					defaultRemote := cmd.Bool("default")
					create := control.CreateRemoteCommand{
						Remote: control.Remote{
							ID:          cmd.String("id"),
							Name:        cmd.String("name"),
							CloudAPIURL: cmd.String("api-url"),
							NodeID:      cmd.String("node-id"),
							Enabled:     &enabled,
							IsDefault:   &defaultRemote,
						},
						CloudAPIKeyRef: cmd.String("api-key-ref"),
					}
					ack, err := client().CreateRemote(ctx, commandID("remote_create"), create)
					return writeAck(stdout, ack, err)
				},
			},
			{
				Name:      "update",
				Usage:     "update a remote",
				ArgsUsage: "<remote_id>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "name", Usage: "remote name"},
					&cli.StringFlag{Name: "api-url", Usage: "Pax manager API URL"},
					&cli.StringFlag{Name: "node-id", Usage: "node id"},
					&cli.StringFlag{Name: "api-key-ref", Usage: "Pax node key secret ref"},
					&cli.BoolFlag{Name: "clear-api-key", Usage: "clear Pax node key secret ref"},
					&cli.BoolFlag{Name: "enabled", Usage: "enabled value used when --set-enabled is present", Value: true},
					&cli.BoolFlag{Name: "set-enabled", Usage: "update enabled state"},
					&cli.BoolFlag{Name: "default", Usage: "default value used when --set-default is present"},
					&cli.BoolFlag{Name: "set-default", Usage: "update default state"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					remoteID, err := requireOneArg(cmd, "usage: paxctl remotes update <remote_id> [flags]")
					if err != nil {
						return err
					}
					update := control.UpdateRemoteCommand{RemoteID: remoteID}
					if value := cmd.String("name"); value != "" {
						update.Remote.Name = &value
					}
					if value := cmd.String("api-url"); value != "" {
						update.Remote.CloudAPIURL = &value
					}
					if value := cmd.String("node-id"); value != "" {
						update.Remote.NodeID = &value
					}
					if cmd.Bool("set-enabled") {
						value := cmd.Bool("enabled")
						update.Remote.Enabled = &value
					}
					if cmd.Bool("set-default") {
						value := cmd.Bool("default")
						update.Remote.IsDefault = &value
					}
					if value := cmd.String("api-key-ref"); value != "" {
						update.CloudAPIKeyRef = &value
					}
					if cmd.Bool("clear-api-key") {
						empty := ""
						update.CloudAPIKeyRef = &empty
					}
					ack, err := client().UpdateRemote(ctx, commandID("remote_update"), remoteID, update)
					return writeAck(stdout, ack, err)
				},
			},
			{
				Name:      "restart",
				Usage:     "restart a remote runtime",
				ArgsUsage: "<remote_id>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					remoteID, err := requireOneArg(cmd, "usage: paxctl remotes restart <remote_id>")
					if err != nil {
						return err
					}
					ack, err := client().RestartRemote(ctx, commandID("remote_restart"), remoteID)
					return writeAck(stdout, ack, err)
				},
			},
			{
				Name:      "delete",
				Usage:     "delete a remote",
				ArgsUsage: "<remote_id>",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "cascade-agent-connections", Usage: "also delete remote agent connections"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					remoteID, err := requireOneArg(cmd, "usage: paxctl remotes delete [--cascade-agent-connections] <remote_id>")
					if err != nil {
						return err
					}
					ack, err := client().DeleteRemote(ctx, commandID("remote_delete"), remoteID, cmd.Bool("cascade-agent-connections"))
					return writeAck(stdout, ack, err)
				},
			},
		},
	}
}

func agentConnectionsCommand(stdout io.Writer, client func() controlClient) *cli.Command {
	return &cli.Command{
		Name:  "agent-connections",
		Usage: "manage desired agent connections",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			result, err := client().ListAgentConnections(ctx, true)
			return writeResult(stdout, result, err)
		},
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list agent connections",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					result, err := client().ListAgentConnections(ctx, true)
					return writeResult(stdout, result, err)
				},
			},
			{
				Name:      "restart",
				Usage:     "restart an agent connection",
				ArgsUsage: "<connection_id>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					connectionID, err := requireOneArg(cmd, "usage: paxctl agent-connections restart <connection_id>")
					if err != nil {
						return err
					}
					ack, err := client().RestartAgentConnection(ctx, commandID("restart"), connectionID)
					return writeAck(stdout, ack, err)
				},
			},
			{
				Name:      "delete",
				Usage:     "delete an agent connection",
				ArgsUsage: "<connection_id>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					connectionID, err := requireOneArg(cmd, "usage: paxctl agent-connections delete <connection_id>")
					if err != nil {
						return err
					}
					ack, err := client().DeleteAgentConnection(ctx, commandID("delete"), connectionID)
					return writeAck(stdout, ack, err)
				},
			},
			{
				Name:  "create",
				Usage: "create an agent connection",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "remote-id", Usage: "remote id", Value: "default"},
					&cli.StringFlag{Name: "id", Usage: "connection id"},
					&cli.StringFlag{Name: "name", Usage: "connection name"},
					&cli.StringFlag{Name: "cloud-agent-id", Usage: "cloud agent id"},
					&cli.StringFlag{Name: "instance-id", Usage: "agent instance id"},
					&cli.StringFlag{Name: "agent-type", Usage: "agent type"},
					&cli.StringFlag{Name: "harness", Usage: "harness name"},
					&cli.StringFlag{Name: "command", Usage: "command words separated by spaces"},
					&cli.StringFlag{Name: "working-dir", Usage: "working directory"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					enabled := true
					create := control.CreateAgentConnectionCommand{
						ID:           cmd.String("id"),
						RemoteID:     cmd.String("remote-id"),
						Name:         cmd.String("name"),
						CloudAgentID: cmd.String("cloud-agent-id"),
						InstanceID:   firstNonEmpty(cmd.String("instance-id"), cmd.String("id"), cmd.String("name")),
						AgentType:    firstNonEmpty(cmd.String("agent-type"), cmd.String("harness")),
						Harness:      cmd.String("harness"),
						Command:      strings.Fields(cmd.String("command")),
						WorkingDir:   cmd.String("working-dir"),
						Enabled:      &enabled,
						DesiredState: control.DesiredStateRunning,
					}
					ack, err := client().CreateAgentConnection(ctx, commandID("create"), create)
					return writeAck(stdout, ack, err)
				},
			},
		},
	}
}

func harnessesCommand(stdout io.Writer, client func() controlClient) *cli.Command {
	return &cli.Command{
		Name:  "harnesses",
		Usage: "inspect local harnesses",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			result, err := client().ListHarnesses(ctx, true)
			return writeResult(stdout, result, err)
		},
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list harness inventory",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					result, err := client().ListHarnesses(ctx, true)
					return writeResult(stdout, result, err)
				},
			},
			{
				Name:  "discover",
				Usage: "refresh harness discovery",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "probe", Usage: "run probe checks"},
					&cli.StringFlag{Name: "names", Usage: "comma-separated harness names"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					result, err := client().DiscoverHarnesses(ctx, control.DiscoverHarnessesQuery{
						Probe: cmd.Bool("probe"),
						Names: splitCSV(cmd.String("names")),
					})
					return writeResult(stdout, result, err)
				},
			},
		},
	}
}

func localSessionsCommand(stdout io.Writer, client func() controlClient) *cli.Command {
	sessionFlags := []cli.Flag{
		&cli.StringFlag{Name: "agent", Usage: "agent name filter"},
		&cli.IntFlag{Name: "limit", Usage: "maximum sessions"},
	}
	return &cli.Command{
		Name:  "local-sessions",
		Usage: "inspect local session cache",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			result, err := client().ListLocalSessions(ctx, control.ListLocalSessionsQuery{})
			return writeResult(stdout, result, err)
		},
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list local sessions",
				Flags: sessionFlags,
				Action: func(ctx context.Context, cmd *cli.Command) error {
					result, err := client().ListLocalSessions(ctx, control.ListLocalSessionsQuery{
						Agent: cmd.String("agent"),
						Limit: cmd.Int("limit"),
					})
					return writeResult(stdout, result, err)
				},
			},
			{
				Name:  "sync",
				Usage: "sync local sessions",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "agent", Usage: "agent name filter"},
					&cli.IntFlag{Name: "limit", Usage: "maximum sessions"},
					&cli.Int64Flag{Name: "timeout-ms", Usage: "scanner timeout in milliseconds"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					result, err := client().SyncLocalSessions(ctx, control.SyncLocalSessionsQuery{
						Agent:         cmd.String("agent"),
						Limit:         cmd.Int("limit"),
						TimeoutMillis: cmd.Int64("timeout-ms"),
					})
					return writeResult(stdout, result, err)
				},
			},
		},
	}
}

func requireOneArg(cmd *cli.Command, usage string) (string, error) {
	if cmd.Args().Len() != 1 {
		return "", fmt.Errorf("%s", usage)
	}
	return cmd.Args().First(), nil
}

func localClient(socket string, debugHTTP string) controlClient {
	if strings.TrimSpace(debugHTTP) != "" {
		return localapi.NewHTTPClient(debugHTTP)
	}
	return localapi.NewUnixClient(expandHome(socket))
}

func writeResult(stdout io.Writer, result control.QueryResult, err error) error {
	if err != nil {
		if result.Error != nil {
			_ = json.NewEncoder(stdout).Encode(result)
		}
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func writeAck(stdout io.Writer, ack control.CommandAck, err error) error {
	if err != nil {
		if ack.CommandID != "" {
			_ = json.NewEncoder(stdout).Encode(ack)
		}
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(ack)
}

func commandID(action string) string {
	return fmt.Sprintf("paxctl_%s_%d", action, time.Now().UnixNano())
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}
