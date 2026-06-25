package remotelogin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/cloud"
)

var (
	ErrDenied   = errors.New("node login denied")
	ErrExpired  = errors.New("node login expired")
	ErrCanceled = errors.New("node login canceled")
)

type CloudClient interface {
	StartNodeRegistration(req *cloud.StartNodeRegistrationRequest) (*cloud.StartNodeRegistrationResponse, error)
	PollNodeRegistration(req *cloud.PollNodeRegistrationRequest) (*cloud.PollNodeRegistrationResponse, error)
}

type NodeRegistrationInfo struct {
	Name        string
	Hostname    string
	MachineType string
	OS          string
	Arch        string
	PaxdVersion string
	APIEndpoint string
}

type LoginSpec struct {
	RemoteID    string
	CloudAPIURL string
	Node        NodeRegistrationInfo
}

type LoginResult struct {
	RemoteID    string
	CloudAPIURL string
	NodeID      string
	NodeAPIKey  string
}

type BrowserOpener func(ctx context.Context, url string) error

type Sleeper func(ctx context.Context, d time.Duration) error

type Options struct {
	Client      CloudClient
	Stdout      io.Writer
	OpenBrowser BrowserOpener
	Sleep       Sleeper
}

func Login(ctx context.Context, spec LoginSpec, opts Options) (LoginResult, error) {
	if opts.Client == nil {
		return LoginResult{}, errors.New("remote login cloud client is required")
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}

	start, err := opts.Client.StartNodeRegistration(&cloud.StartNodeRegistrationRequest{
		Name:        spec.Node.Name,
		Hostname:    spec.Node.Hostname,
		MachineType: spec.Node.MachineType,
		OS:          stringDefault(spec.Node.OS, runtime.GOOS),
		Arch:        stringDefault(spec.Node.Arch, runtime.GOARCH),
		PaxdVersion: spec.Node.PaxdVersion,
		APIEndpoint: spec.Node.APIEndpoint,
	})
	if err != nil {
		return LoginResult{}, fmt.Errorf("start node registration: %w", err)
	}
	if start == nil {
		return LoginResult{}, errors.New("start node registration returned empty response")
	}

	printPairing(stdout, start)
	if url := verificationURL(start); url != "" {
		open := opts.OpenBrowser
		if open == nil {
			open = OpenBrowser
		}
		_ = open(ctx, url)
	}

	sleep := opts.Sleep
	if sleep == nil {
		sleep = Sleep
	}
	interval := pollInterval(start.Interval)
	for {
		if err := ctx.Err(); err != nil {
			return LoginResult{}, err
		}
		poll, err := opts.Client.PollNodeRegistration(&cloud.PollNodeRegistrationRequest{
			RegistrationID: start.RegistrationID,
			PollToken:      start.PollToken,
		})
		if err != nil {
			return LoginResult{}, fmt.Errorf("poll node registration: %w", err)
		}
		if poll == nil {
			return LoginResult{}, errors.New("poll node registration returned empty response")
		}
		switch strings.ToLower(strings.TrimSpace(poll.Status)) {
		case "approved", "complete", "completed", "connected":
			if strings.TrimSpace(poll.NodeID) == "" || strings.TrimSpace(poll.APIKey) == "" {
				return LoginResult{}, errors.New("approved node registration did not return node credentials")
			}
			return LoginResult{
				RemoteID:    spec.RemoteID,
				CloudAPIURL: spec.CloudAPIURL,
				NodeID:      poll.NodeID,
				NodeAPIKey:  poll.APIKey,
			}, nil
		case "denied", "rejected":
			return LoginResult{}, ErrDenied
		case "expired":
			return LoginResult{}, ErrExpired
		case "canceled", "cancelled":
			return LoginResult{}, ErrCanceled
		default:
			if err := sleep(ctx, interval); err != nil {
				return LoginResult{}, err
			}
		}
	}
}

func printPairing(stdout io.Writer, start *cloud.StartNodeRegistrationResponse) {
	fmt.Fprintln(stdout, "Opening browser for Pax login...")
	if url := verificationURL(start); url != "" {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "If the browser did not open:")
		fmt.Fprintln(stdout, url)
	}
	if start.PairCode != "" {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Code: %s\n", start.PairCode)
	}
	fmt.Fprintln(stdout, "Waiting for approval...")
}

func verificationURL(start *cloud.StartNodeRegistrationResponse) string {
	if start.VerificationURIComplete != "" {
		return start.VerificationURIComplete
	}
	return start.VerificationURI
}

func pollInterval(seconds int64) time.Duration {
	if seconds <= 0 {
		return 2 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func OpenBrowser(ctx context.Context, url string) error {
	if strings.TrimSpace(url) == "" {
		return nil
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", url)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

func stringDefault(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
