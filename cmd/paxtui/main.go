package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pax-beehive/paxd/internal/control"
	paxdaemon "github.com/pax-beehive/paxd/internal/daemon"
	"github.com/pax-beehive/paxd/internal/localapi"
	"github.com/urfave/cli/v3"
)

type overviewClient interface {
	GetLocalOverview(ctx context.Context) (control.QueryResult, error)
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	socket := paxdaemon.DefaultControlSocket
	debugHTTP := ""
	cmd := &cli.Command{
		Name:      "paxtui",
		Usage:     "show local paxd overview",
		Writer:    stdout,
		ErrWriter: io.Discard,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "socket", Usage: "paxd Unix socket path", Value: paxdaemon.DefaultControlSocket, Destination: &socket},
			&cli.StringFlag{Name: "debug-http", Usage: "debug HTTP base URL, for example http://127.0.0.1:8765", Destination: &debugHTTP},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client := newOverviewClient(socket, debugHTTP)
			result, err := client.GetLocalOverview(ctx)
			if err != nil {
				return err
			}
			return renderOverview(stdout, result.LocalOverview)
		},
	}
	return cmd.Run(ctx, append([]string{"paxtui"}, args...))
}

type localAPIOverviewClient struct {
	client *localapi.Client
}

func (c localAPIOverviewClient) GetLocalOverview(ctx context.Context) (control.QueryResult, error) {
	return c.client.GetLocalOverview(ctx)
}

var newOverviewClient = func(socket string, debugHTTP string) overviewClient {
	if strings.TrimSpace(debugHTTP) != "" {
		return localAPIOverviewClient{client: localapi.NewHTTPClient(debugHTTP)}
	}
	return localAPIOverviewClient{client: localapi.NewUnixClient(expandHome(socket))}
}

func renderOverview(stdout io.Writer, overview *control.LocalOverview) error {
	if overview == nil {
		_, err := fmt.Fprintln(stdout, "No local overview available")
		return err
	}
	_, err := fmt.Fprintf(stdout, "Pax Local Overview\n%+v\n", overview)
	return err
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}
