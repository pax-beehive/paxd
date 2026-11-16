//go:build unix

package acpforwarder

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForwarderChildProcess(t *testing.T) {
	mode := os.Getenv("PAX_FORWARDER_CHILD_MODE")
	if mode == "" {
		return
	}
	if err := os.WriteFile(os.Getenv("PAX_FORWARDER_CHILD_PID"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(2)
	}
	if mode == "exit" {
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}
	if mode == "invalid" {
		_, _ = fmt.Fprintln(os.Stdout, "not valid JSON")
	}
	if mode == "hold-output-child" {
		child := exec.Command(os.Args[0], "-test.run=^TestForwarderChildProcess$")
		child.Env = append(os.Environ(), "PAX_FORWARDER_CHILD_MODE=wait", "PAX_FORWARDER_CHILD_PID="+os.Getenv("PAX_FORWARDER_CHILD_PID")+".child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(4)
		}
		_ = child.Process.Release()
	}
	if mode == "echo" {
		scanner := bufio.NewScanner(os.Stdin)
		count := 0
		for scanner.Scan() {
			var request struct {
				ID int `json:"id"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil {
				os.Exit(3)
			}
			_, _ = fmt.Fprintf(os.Stdout, "{\"id\":%d,\"result\":{\"text\":%q}}\n", request.ID, strings.Repeat("r", 2048))
			count++
			if count == 1010 {
				_ = os.WriteFile(os.Getenv("PAX_FORWARDER_CHILD_DONE"), []byte("done"), 0o600)
			}
		}
		os.Exit(0)
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestForwarderGivenReconcileFailureThenReapsStartedWorker(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "pid")
	t.Setenv("PAX_FORWARDER_CHILD_PID", pidPath)
	t.Setenv("PAX_FORWARDER_CHILD_MODE", "wait")
	server := forwarderRecoveryServer(t, pidPath, false)
	t.Cleanup(server.Close)
	journal := openTestJournal(t)
	t.Cleanup(func() { _ = journal.Close() })
	svc := New(Config{
		CloudURL: server.URL, APIKey: "test", ConnectionID: "conn_1", AgentID: "agent_1",
		Command: []string{os.Args[0], "-test.run=^TestForwarderChildProcess$"}, Journal: journal,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	connected, err := svc.runOnce(ctx)
	assert.True(t, connected)
	require.ErrorContains(t, err, "reconcile producer")
	assertForwarderChildReaped(t, pidPath)
}

func TestForwarderGivenCancellationDuringQuietReconcileThenInterruptsSocketRead(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "pid")
	t.Setenv("PAX_FORWARDER_CHILD_PID", pidPath)
	t.Setenv("PAX_FORWARDER_CHILD_MODE", "hold-output-child")
	// A launcher can exit while its worker keeps the inherited output pipe open.
	// Cancelling recovery must therefore interrupt the socket independently of EOF.
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidPath + ".child")
		if err == nil {
			if pid, err := strconv.Atoi(string(raw)); err == nil {
				if child, err := os.FindProcess(pid); err == nil {
					_ = child.Kill()
				}
			}
		}
	})
	reconciling := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		reconciling <- conn
		// Remain silent instead of replying to the reconcile request.
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(server.Close)
	journal := openTestJournal(t)
	t.Cleanup(func() { _ = journal.Close() })
	svc := New(Config{
		CloudURL: server.URL, APIKey: "test", ConnectionID: "conn_1", AgentID: "agent_1",
		Command: []string{os.Args[0], "-test.run=^TestForwarderChildProcess$"}, Journal: journal,
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := svc.runOnce(ctx); done <- err }()
	var managerConn *websocket.Conn
	select {
	case managerConn = <-reconciling:
	case <-time.After(5 * time.Second):
		t.Fatal("forwarder did not begin reconciliation")
	}
	t.Cleanup(func() { _ = managerConn.Close() })
	require.Eventually(t, func() bool {
		_, err := os.Stat(pidPath + ".child")
		return err == nil
	}, 5*time.Second, time.Millisecond)

	cancel()
	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(2 * time.Second):
		assert.Fail(t, "cancelled recovery remained blocked on a quiet manager")
		_ = managerConn.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("forwarder did not stop after test socket cleanup")
		}
	}
	assertForwarderChildReaped(t, pidPath)
}

func TestForwarderGivenWorkerExitThenReturnsWithoutSecondWait(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "pid")
	t.Setenv("PAX_FORWARDER_CHILD_PID", pidPath)
	t.Setenv("PAX_FORWARDER_CHILD_MODE", "exit")
	server := forwarderRecoveryServer(t, pidPath, true)
	t.Cleanup(server.Close)
	journal := openTestJournal(t)
	t.Cleanup(func() { _ = journal.Close() })
	svc := New(Config{
		CloudURL: server.URL, APIKey: "test", ConnectionID: "conn_1", AgentID: "agent_1",
		Command: []string{os.Args[0], "-test.run=^TestForwarderChildProcess$"}, Journal: journal,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	started := time.Now()
	connected, err := svc.runOnce(ctx)
	assert.True(t, connected)
	if err != nil {
		assert.NotContains(t, err.Error(), "did not exit after cancellation")
	}
	assert.Less(t, time.Since(started), 2*time.Second)
	assertForwarderChildReaped(t, pidPath)
}

func TestForwarderGivenOutputFailureDuringReconcileThenInterruptsRecovery(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "pid")
	t.Setenv("PAX_FORWARDER_CHILD_PID", pidPath)
	t.Setenv("PAX_FORWARDER_CHILD_MODE", "invalid")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, _ = conn.ReadMessage()
		// A quiet manager must not hide an already failed worker output pump.
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(server.Close)
	journal := openTestJournal(t)
	t.Cleanup(func() { _ = journal.Close() })
	svc := New(Config{
		CloudURL: server.URL, APIKey: "test", ConnectionID: "conn_1", AgentID: "agent_1",
		Command: []string{os.Args[0], "-test.run=^TestForwarderChildProcess$"}, Journal: journal,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	started := time.Now()
	_, err := svc.runOnce(ctx)
	assert.Error(t, err)
	assert.Less(t, time.Since(started), 1500*time.Millisecond)
	assertForwarderChildReaped(t, pidPath)
}

func TestForwarderGivenInconsistentInboundHistoryThenStopsWithoutReconnectOrDeletion(t *testing.T) {
	root := t.TempDir()
	pidPath := filepath.Join(root, "pid")
	t.Setenv("PAX_FORWARDER_CHILD_PID", pidPath)
	t.Setenv("PAX_FORWARDER_CHILD_MODE", "wait")
	inner := forwarderRecoveryServer(t, pidPath, true)
	t.Cleanup(inner.Close)
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	journal := openTestJournal(t)
	t.Cleanup(func() { _ = journal.Close() })
	mq := openReliableStore(t, journal)
	_, _, err := mq.SaveInboundIfAbsent(t.Context(), reliablemq.InboundFrameFromEnvelope(forwarderInput(1)))
	require.NoError(t, err)
	// Simulate the legacy watermark having advanced past an unapplied request.
	_, err = journal.DB().ExecContext(t.Context(), "UPDATE transport_queue_state SET inbound_applied_through=1 WHERE queue_id='conn_1' AND stream='acp'")
	require.NoError(t, err)
	history := openTestHistory(t, filepath.Join(root, "history.db"))
	t.Cleanup(func() { _ = history.Close() })
	svc := New(Config{
		CloudURL: server.URL, APIKey: "test", ConnectionID: "conn_1", AgentID: "agent_1",
		Command: []string{os.Args[0], "-test.run=^TestForwarderChildProcess$"}, Journal: journal,
		History: history, ReconnectInterval: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = svc.Run(ctx)
	assert.ErrorIs(t, err, reliablemq.ErrInboundHistoryInconsistent)
	assert.Equal(t, int32(1), connections.Load())
	frame := mustGetReliableFrame(t, journal, forwarderInputKey(1))
	assert.Equal(t, reliablemq.StatusReceived, frame.Status)
	assert.JSONEq(t, `{"id":1}`, string(frame.Payload))
	state, err := mq.LoadQueueState(t.Context(), "conn_1", reliablemq.StreamACP)
	require.NoError(t, err)
	assert.Equal(t, int64(1), state.InboundAppliedThrough)
	assertForwarderChildReaped(t, pidPath)
}

func TestForwarderGivenLargeInboundReplayThenDrainsWorkerOutputDuringRecovery(t *testing.T) {
	root := t.TempDir()
	pidPath, donePath := filepath.Join(root, "pid"), filepath.Join(root, "done")
	t.Setenv("PAX_FORWARDER_CHILD_PID", pidPath)
	t.Setenv("PAX_FORWARDER_CHILD_DONE", donePath)
	t.Setenv("PAX_FORWARDER_CHILD_MODE", "echo")
	server := forwarderRecoveryServer(t, pidPath, true)
	t.Cleanup(server.Close)
	journal := openTestJournal(t)
	t.Cleanup(func() { _ = journal.Close() })
	mq := openReliableStore(t, journal)
	batch := reliablemq.StoreBatch{}
	for seq := int64(1); seq <= 1010; seq++ {
		batch.Frames = append(batch.Frames, reliablemq.Frame{
			Key: forwarderInputKey(seq), Kind: reliablemq.FrameKindData, Status: reliablemq.StatusReceived,
			Payload: []byte(fmt.Sprintf(`{"id":%d,"params":{"input":%q}}`, seq, strings.Repeat("p", 2048))),
		})
	}
	require.NoError(t, mq.ApplyBatch(t.Context(), batch))
	svc := New(Config{
		CloudURL: server.URL, APIKey: "test", ConnectionID: "conn_1", AgentID: "agent_1",
		Command: []string{os.Args[0], "-test.run=^TestForwarderChildProcess$"}, Journal: journal,
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := svc.runOnce(ctx); done <- err }()
	assert.Eventually(t, func() bool {
		_, err := os.Stat(donePath)
		if err != nil {
			return false
		}
		state, err := mq.LoadQueueState(t.Context(), "conn_1", reliablemq.StreamACP)
		return err == nil && state.InboundAppliedThrough == 1010
	}, 30*time.Second, 20*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker recovery did not stop after cancellation")
	}
	assertForwarderChildReaped(t, pidPath)
}

func forwarderRecoveryServer(t *testing.T, pidPath string, aligned bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(pidPath); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		response := []byte(`{}`)
		if aligned {
			response, err = reliablemq.MarshalEnvelope(reliablemq.Envelope{
				Type: reliablemq.EnvelopeTypeReconcileResponse, QueueID: "conn_1", Stream: reliablemq.StreamACP,
				Action: reliablemq.ReconcileActionAligned,
			})
			if err != nil {
				t.Error(err)
				return
			}
		}
		if err := conn.WriteMessage(websocket.TextMessage, response); err != nil {
			return
		}
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			env, err := reliablemq.UnmarshalEnvelope(data)
			if err == nil && env.Type == reliablemq.EnvelopeTypeData {
				ack, err := reliablemq.MarshalEnvelope(reliablemq.AckEnvelope("conn_1", reliablemq.StreamACP, env.Seq))
				if err != nil || conn.WriteMessage(websocket.TextMessage, ack) != nil {
					return
				}
			}
		}
	}))
}

func assertForwarderChildReaped(t *testing.T, pidPath string) {
	t.Helper()
	raw, err := os.ReadFile(pidPath)
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(raw))
	require.NoError(t, err)
	var waitErr error
	require.Eventually(t, func() bool {
		var status syscall.WaitStatus
		waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		waitErr = err
		return waited == pid || err != nil
	}, time.Second, time.Millisecond)
	assert.ErrorIs(t, waitErr, syscall.ECHILD, "worker must already have been reaped by the forwarder")
}
