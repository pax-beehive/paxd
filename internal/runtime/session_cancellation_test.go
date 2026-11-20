package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A runtime slot never restarts a session that reports itself canceled, so a
// session may only say so when its slot canceled it. These tests pin that for
// context errors that come from somewhere else.

// interruptingDialer fails the handshake after the slot stopped the session.
type interruptingDialer struct {
	stop func()
	err  error
}

func (d interruptingDialer) Dial(context.Context, string, http.Header) (WebSocketConn, *http.Response, error) {
	d.stop()
	return nil, nil, d.err
}

func dialOnlySessions(dialer WebSocketDialer) map[string]Session {
	return map[string]Session{
		"node control": NewRemoteControlSession(RemoteControlSessionConfig{
			Spec:    RemoteSpec{RemoteID: "remote_prod", CloudAPIURL: "https://fleet.example.com"},
			Headers: fakeHeaderProvider{header: http.Header{}},
			Dialer:  dialer,
			Runner: NodeControlRunnerFunc(func(context.Context, WebSocketConn, RemoteSpec) Exit {
				return TransientExit("unexpected", "the handshake failed, so nothing should run")
			}),
		}),
		"agent tunnel": NewAgentTunnelSession(validAgentSpec(), AgentTunnelSessionDeps{
			Headers:               fakeHeaderProvider{header: http.Header{}},
			Dialer:                dialer,
			LocalACPProcessRunner: fakeLocalACPProcessRunner{proc: newFakeProcess("")},
			ReliableEngineFactory: ReliableEngineFromStore(newSpyStore()),
		}),
	}
}

func TestSessionsRetryAHandshakeThatTimedOut(t *testing.T) {
	// gorilla bounds the handshake with its own 45 second deadline and
	// returns that context error while the session context is still live.
	handshakeTimeout := fmt.Errorf("dial tcp 203.0.113.7:443: %w", context.DeadlineExceeded)

	for name, session := range dialOnlySessions(&fakeDialer{err: handshakeTimeout}) {
		t.Run(name, func(t *testing.T) {
			exit := session.Run(context.Background())

			assert.Equal(t, ExitTransient, exit.Class)
			assert.Equal(t, "dial_timeout", exit.Code)
			assert.Contains(t, exit.Message, "context deadline exceeded")
			assert.False(t, exit.ResetBackoff, "a failed dial must keep backing off")
		})
	}
}

func TestSessionsStoppedDuringTheHandshakeReportCancellation(t *testing.T) {
	for _, dialErr := range []error{context.Canceled, context.DeadlineExceeded} {
		ctx, stop := context.WithCancel(context.Background())
		for name, session := range dialOnlySessions(interruptingDialer{stop: stop, err: dialErr}) {
			t.Run(name+"/"+dialErr.Error(), func(t *testing.T) {
				assert.Equal(t, CanceledExit(context.Canceled), session.Run(ctx))
			})
		}
	}
}

// connectedTunnel is an agent tunnel that reaches its running state on one of
// the three process models and then fails its websocket read with readErr.
type connectedTunnel struct {
	session *AgentTunnelSession
	proc    *fakeProcess
	running <-chan struct{}
}

func connectedTunnels(t *testing.T, readErr error) map[string]connectedTunnel {
	t.Helper()
	newConn := func() *fakeWebSocketConn {
		conn := newFakeWebSocketConn()
		enqueueAlignedReconcile(t, conn, validAgentSpec().TransportQueueID, 0)
		conn.readErrWhenDrained = readErr
		return conn
	}
	// The process stays alive and silent, so the tunnel read decides the exit.
	newProc := func() (*fakeProcess, *io.PipeWriter) {
		stdout, stdoutWriter := io.Pipe()
		t.Cleanup(func() { _ = stdoutWriter.Close() })
		return &fakeProcess{
			stdin:  &bufferWriteCloser{},
			stdout: stdout,
			stderr: strings.NewReader(""),
			waitCh: make(chan error),
		}, stdoutWriter
	}
	build := func(proc *fakeProcess, deps AgentTunnelSessionDeps) connectedTunnel {
		running := make(chan struct{}, 1)
		deps.Headers = fakeHeaderProvider{header: http.Header{}}
		deps.Dialer = &fakeDialer{conn: newConn()}
		deps.Heartbeat = HeartbeatConfig{PingInterval: time.Hour, ReadTimeout: time.Hour}
		deps.SessionEventSink = SessionEventSinkFunc(func(event SessionEvent) {
			if event.Phase == PhaseRunning {
				select {
				case running <- struct{}{}:
				default:
				}
			}
		})
		return connectedTunnel{
			session: NewAgentTunnelSession(validAgentSpec(), deps),
			proc:    proc,
			running: running,
		}
	}

	tunnels := make(map[string]connectedTunnel, 3)

	proc, _ := newProc()
	tunnels["process per tunnel"] = build(proc, AgentTunnelSessionDeps{
		LocalACPProcessRunner: fakeLocalACPProcessRunner{proc: proc},
		ReliableEngineFactory: ReliableEngineFromStore(newSpyStore()),
	})

	proc, stdout := newProc()
	store := newSpyStore()
	processPool := NewPersistentACPProcessPool(fakeLocalACPProcessRunner{proc: proc}, store)
	t.Cleanup(func() { processPool.Stop(validAgentSpec()) })
	go writePersistentInitializeResponse(t, stdout, `{"protocolVersion":1,"agentCapabilities":{}}`)
	tunnels["persistent process"] = build(proc, AgentTunnelSessionDeps{
		LocalACPProcessRunner: fakeLocalACPProcessRunner{proc: proc},
		ACPProcessPool:        processPool,
		ReliableEngineFactory: ReliableEngineFromStore(store),
	})

	routes := newFakeACPRouteStore(validAgentSpec().ConnectionID)
	factory := ReliableEngineFromStore(newSpyStore())
	t.Cleanup(func() {
		require.NoError(t, factory.(*reliableEngineFactory).Close(context.Background()))
	})
	tunnels["slot pool"] = build(nil, AgentTunnelSessionDeps{
		ACPPoolRegistry: NewACPPoolRegistry(
			ACPRouteStoreFactoryFunc(func(string) ACPRouteStore { return routes }),
		),
		ACPSessionBindings:    newFakeACPSessionBindingStore(),
		ReliableEngineFactory: factory,
	})
	return tunnels
}

func runTunnel(t *testing.T, ctx context.Context, tunnel connectedTunnel) <-chan Exit {
	t.Helper()
	exited := make(chan Exit, 1)
	go func() { exited <- tunnel.session.Run(ctx) }()
	return exited
}

func awaitExit(t *testing.T, exited <-chan Exit) Exit {
	t.Helper()
	select {
	case exit := <-exited:
		return exit
	case <-time.After(3 * time.Second):
		t.Fatal("agent tunnel did not exit")
		return Exit{}
	}
}

func TestConnectedAgentTunnelRetriesContextErrorsItsSlotDidNotCause(t *testing.T) {
	for _, internal := range []error{context.DeadlineExceeded, context.Canceled} {
		readErr := fmt.Errorf("read tunnel: %w", internal)
		for name, tunnel := range connectedTunnels(t, readErr) {
			t.Run(name+"/"+internal.Error(), func(t *testing.T) {
				exit := awaitExit(t, runTunnel(t, context.Background(), tunnel))

				assert.Equal(t, ExitTransient, exit.Class)
				assert.Equal(t, "session_error", exit.Code)
				assert.Contains(t, exit.Message, internal.Error())
				assert.True(t, exit.ResetBackoff, "the tunnel was connected, so it reconnects at once")
				if name == "persistent process" {
					assert.False(t, tunnel.proc.terminated, "the process outlives a tunnel that reconnects")
				}
			})
		}
	}
}

func TestConnectedAgentTunnelStoppedByItsSlotReportsCancellation(t *testing.T) {
	// The read blocks, so only the slot can end these tunnels.
	for name, tunnel := range connectedTunnels(t, nil) {
		t.Run(name, func(t *testing.T) {
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			exited := runTunnel(t, ctx, tunnel)
			select {
			case <-tunnel.running:
			case exit := <-exited:
				t.Fatalf("agent tunnel exited before running: %+v", exit)
			case <-time.After(3 * time.Second):
				t.Fatal("agent tunnel did not start")
			}

			stop()

			exit := awaitExit(t, exited)
			assert.Equal(t, ExitTerminal, exit.Class)
			assert.Equal(t, "canceled", exit.Code)
		})
	}
}
