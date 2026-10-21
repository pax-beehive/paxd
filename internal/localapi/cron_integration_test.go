//go:build !windows

package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/cronjob"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/stretchr/testify/require"
)

// Build paxl separately and provide its absolute path to exercise both projects
// without introducing a Go module dependency between them.
func TestPaxlCronRoundTrip(t *testing.T) {
	binary := os.Getenv("PAXL_CRON_TEST_BINARY")
	if binary == "" {
		t.Skip("Set PAXL_CRON_TEST_BINARY to a newly built paxl binary")
	}
	dir := t.TempDir()
	db, err := daemonstore.OpenSQLite(filepath.Join(dir, "paxd.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc, err := cronjob.New(db.DB(), cronjob.Options{Runner: cronAPITestRunner{}, LockPath: filepath.Join(dir, "cron.lock")})
	require.NoError(t, err)
	require.NoError(t, svc.Start(context.Background()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, svc.Shutdown(ctx))
	})
	socket := filepath.Join(dir, "paxd.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	server := &http.Server{Handler: NewHandlerWithCron(docsRouteService{}, nil, svc)}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	invoke := func(input string, result any, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		argv := append([]string{"cron", "--socket", socket}, args...)
		cmd := exec.CommandContext(ctx, binary, argv...)
		cmd.Stdin = strings.NewReader(input)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		require.NoError(t, cmd.Run(), stderr.String())
		if result != nil {
			require.NoError(t, json.Unmarshal(stdout.Bytes(), result), stdout.String())
		}
	}
	var status map[string]bool
	invoke("", &status, "status")
	require.True(t, status["running"])
	var job cronjob.Job
	invoke("literal $(not-a-command)\n", &job, "create", "--name", "Review", "--connection", "conn_test", "--session-mode", "reuse", "--schedule", "0 9 * * *", "--cwd", dir, "--prompt-file", "-", "--disabled")
	require.False(t, job.Enabled)
	invoke("", &job, "update", "--name", "Renamed", job.ID)
	require.Equal(t, "Renamed", job.Name)
	require.Equal(t, "literal $(not-a-command)\n", job.Prompt)
	var run cronjob.Run
	invoke("", &run, "run", job.ID)
	require.Equal(t, "queued", run.Status)
	require.Eventually(t, func() bool {
		runs, err := svc.Runs(context.Background(), job.ID, 1)
		return err == nil && len(runs) == 1 && runs[0].Status == "succeeded"
	}, 10*time.Second, 20*time.Millisecond)
	var runs []cronjob.Run
	invoke("", &runs, "runs", job.ID)
	require.Len(t, runs, 1)
	require.Equal(t, "Review complete: literal $(not-a-command)\n", runs[0].Output)
	invoke("", &job, "resume", job.ID)
	require.True(t, job.Enabled)
	invoke("", &job, "pause", job.ID)
	require.False(t, job.Enabled)
	invoke("", nil, "delete", job.ID)
	var jobs []cronjob.Job
	invoke("", &jobs, "list")
	require.Empty(t, jobs)
}

type cronAPITestRunner struct{}

func (cronAPITestRunner) Run(_ context.Context, job *cronjob.Job) (string, error) {
	return "Review complete: " + job.Prompt, nil
}
