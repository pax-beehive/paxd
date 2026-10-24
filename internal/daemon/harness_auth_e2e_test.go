package daemon

import (
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

func TestHarnessAuthBrowserE2ENode(t *testing.T) {
	endpoint := os.Getenv("PAXL_E2E_MANAGER")
	if endpoint == "" {
		t.Skip("started by the Manager auth browser test")
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
		_, err := tx.CreateRemote(t.Context(), control.CreateRemoteCommand{Remote: control.Remote{ID: "e2e", Name: "Auth fixture", CloudAPIURL: endpoint, Enabled: &enabled}})
		return err
	}))
	for t.Context().Err() == nil {
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(endpoint, "http")+"/api/v1/node/control?node_id="+os.Getenv("PAXL_E2E_NODE"), http.Header{"X-Pax-Key": []string{os.Getenv("PAXL_E2E_KEY")}})
		require.NoError(t, err)

		runner := controlws.NewRunner(rt.Control)
		runner.Reports = controlws.ReportOptions{HeartbeatInterval: time.Second, SendInitialHeartbeat: true, Heartbeat: func() control.HeartbeatReport {
			probe := paxlinstall.Probe(t.Context())
			return control.HeartbeatReport{Paxl: &probe, BootID: "auth-fixture", PaxdVersion: "1.0.0", DaemonPhase: "running"}
		}}
		runner.RunNodeControl(t.Context(), conn, runtimes.RemoteSpec{RemoteID: "e2e", NodeID: os.Getenv("PAXL_E2E_NODE")})
		_ = conn.Close()
		select {
		case <-t.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}

}
