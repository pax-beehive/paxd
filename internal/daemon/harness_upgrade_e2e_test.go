package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/controlws"
	"github.com/pax-beehive/paxd/internal/paxlinstall"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/stretchr/testify/require"
)

// The browser test provides isolated package executables; orchestration, ACP
// initialization, subprocess restarts, command persistence and transport are real.
func TestHarnessBrowserE2ENode(t *testing.T) {
	endpoint := os.Getenv("PAXL_E2E_MANAGER")
	if endpoint == "" {
		t.Skip("started by the manager harness browser test")
	}
	root := os.Getenv("PAXL_E2E_STATE")
	require.NotEmpty(t, root)
	t.Setenv("HOME", root)
	cfg := config.DefaultConfig()
	cfg.Daemon.DBPath = filepath.Join(root, "daemon.db")
	rt, err := Bootstrap(t.Context(), Options{Config: &cfg})
	require.NoError(t, err)
	require.NoError(t, rt.Store.WithTx(t.Context(), func(tx control.TxStore) error {
		enabled := true
		_, err := tx.CreateRemote(t.Context(), control.CreateRemoteCommand{Remote: control.Remote{ID: "e2e", Name: "Fixture", CloudAPIURL: endpoint, Enabled: &enabled}})
		if err != nil {
			return err
		}
		for _, item := range []struct{ harness, bin string }{{"claude-code", "claude-agent-acp"}, {"codex", "codex-acp"}, {"pi", "pi-acp"}} {
			slots := 2
			_, err = tx.CreateAgentConnection(t.Context(), control.CreateAgentConnectionCommand{
				ID: "fixture-" + item.harness, RemoteID: "e2e", Name: item.harness + " fixture", InstanceID: item.harness,
				AgentType: item.harness, Harness: item.harness, Command: []string{filepath.Join(root, "initial", "bin", item.bin)},
				DesiredState: control.DesiredStateRunning, DesiredSlots: &slots,
			})
			if err != nil {
				return err
			}
		}
		return nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = rt.supervisors.acpSlots.Start(ctx) }()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(endpoint, "http")+"/api/v1/node/control?node_id="+os.Getenv("PAXL_E2E_NODE"), http.Header{"X-Pax-Key": []string{os.Getenv("PAXL_E2E_KEY")}})
	require.NoError(t, err)
	defer conn.Close()
	runner := controlws.NewRunner(rt.Control)
	runner.Reports = controlws.ReportOptions{HeartbeatInterval: time.Second, SendInitialHeartbeat: true, Heartbeat: func() control.HeartbeatReport {
		got := paxlinstall.Probe(context.Background())
		return control.HeartbeatReport{BootID: "fixture-boot", PaxdVersion: "1.0.0", DaemonPhase: "running", Paxl: &got}
	}}
	runner.RunNodeControl(ctx, conn, runtimes.RemoteSpec{RemoteID: "e2e", NodeID: os.Getenv("PAXL_E2E_NODE")})
}
