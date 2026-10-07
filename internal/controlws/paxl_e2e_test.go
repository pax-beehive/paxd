package controlws_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/controlws"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/paxlinstall"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/pax-beehive/paxd/internal/updater"
	"github.com/stretchr/testify/require"
)

// TestPaxlBrowserE2ENode is a subprocess fixture using the production node-control
// runner, durable store and installer. The manager browser test owns its lifetime.
func TestPaxlBrowserE2ENode(t *testing.T) {
	endpoint := os.Getenv("PAXL_E2E_MANAGER")
	if endpoint == "" {
		t.Skip("started by the manager browser E2E test")
	}
	root := os.Getenv("PAXL_E2E_STATE")
	require.NotEmpty(t, root)
	store, err := daemonstore.OpenSQLite(filepath.Join(root, "daemon.db"))
	require.NoError(t, err)
	require.NoError(t, store.Migrate(t.Context()))
	service := control.NewService(control.ServiceOptions{Store: store, PaxlInstaller: &paxlinstall.Installer{Options: updater.Options{ResolverURL: endpoint + "/api/v1/public/artifacts/download", StateDir: filepath.Join(root, "updates")}}})
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(endpoint, "http")+"/api/v1/node/control?node_id="+os.Getenv("PAXL_E2E_NODE"), http.Header{"X-Pax-Key": []string{os.Getenv("PAXL_E2E_KEY")}})
	require.NoError(t, err)
	defer conn.Close()
	runner := controlws.NewRunner(service)
	runner.Reports = controlws.ReportOptions{HeartbeatInterval: time.Second, SendInitialHeartbeat: true, Heartbeat: func() control.HeartbeatReport {
		got := paxlinstall.Probe(context.Background())
		return control.HeartbeatReport{BootID: "fixture-boot", PaxdVersion: "1.0.0", DaemonPhase: "running", Paxl: &got}
	}}
	runner.RunNodeControl(t.Context(), conn, runtimes.RemoteSpec{RemoteID: "e2e", NodeID: os.Getenv("PAXL_E2E_NODE")})
}
