package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/paxlinstall"
)

type harnessInstallation struct {
	SchemaVersion int    `json:"schema_version"`
	Harness       string `json:"harness"`
	Component     string `json:"component"`
	Path          string `json:"path"`
	ResolvedPath  string `json:"resolved_path"`
	Package       string `json:"package"`
	Version       string `json:"version"`
	Source        string `json:"source"`
}

type harnessInstallResult struct {
	Installation   *harnessInstallation               `json:"installation"`
	TargetVersion  string                             `json:"target_version"`
	Phase          string                             `json:"phase"`
	RollbackID     string                             `json:"rollback_id,omitempty"`
	RuntimeReports []*control.ACPPoolCapabilityReport `json:"runtime_reports,omitempty"`
}

type harnessPaxlClient struct {
	executable string
	harness    string
	component  string
	path       string
	dir        string
	env        map[string]string
}

func newHarnessPaxlClient(command *control.UpgradeHarnessCommand, conn *control.AgentConnectionView) (*harnessPaxlClient, error) {
	argv, err := paxlinstall.ResolveCommand(nil)
	if err != nil || len(argv) != 1 {
		return nil, fmt.Errorf("a directly executable paxl with harness upgrade support is required")
	}
	client := &harnessPaxlClient{executable: argv[0], harness: command.Harness, component: command.Component}
	if client.harness == "claude-code" {
		client.harness = "claude"
	}
	if conn != nil {
		if len(conn.Command) != 1 {
			return nil, fmt.Errorf("unsupported ACP launcher: configure a direct installed adapter executable")
		}
		client.path, client.dir, client.env = conn.Command[0], conn.WorkingDir, conn.Env
	}
	return client, nil
}

func (c *harnessPaxlClient) run(ctx context.Context, action string, result any, args ...string) error {
	argv := []string{"daemon", "harness", action, c.harness, "--component", c.component, "--format", "json"}
	if c.path != "" {
		argv = append(argv, "--path", c.path)
	}
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, c.executable, argv...)
	configureHarnessPaxlProcess(cmd)
	cmd.Dir = c.dir
	cmd.Env = os.Environ()
	for key, value := range c.env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.WaitDelay = 3 * time.Second
	var stdout, stderr harnessCommandOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("paxl harness %s: %w: %s", action, err, strings.TrimSpace(stderr.String()))
	}
	if err := json.Unmarshal(stdout.Bytes(), result); err != nil {
		return fmt.Errorf("invalid paxl harness response; upgrade paxl before retrying: %w", err)
	}
	return nil
}

func (c *harnessPaxlClient) inspect(ctx context.Context) (*harnessInstallation, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var installed harnessInstallation
	if err := c.run(ctx, "inspect", &installed); err != nil {
		return nil, err
	}
	if installed.SchemaVersion != 1 || installed.Path == "" || installed.Version == "" || installed.Component != c.component {
		return nil, fmt.Errorf("unsupported paxl harness response; upgrade paxl before retrying")
	}
	return &installed, nil
}

type harnessCommandOutput struct{ bytes.Buffer }

func (b *harnessCommandOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 16384 - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
