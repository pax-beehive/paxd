package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pax-beehive/paxd/internal/auth"
)

type RemoteControlSessionConfig struct {
	Spec             RemoteSpec
	Headers          auth.HeaderProvider
	Dialer           WebSocketDialer
	Runner           NodeControlRunner
	NodeControlPath  string
	Heartbeat        HeartbeatConfig
	SessionEventSink SessionEventSink
}

type RemoteControlSession struct {
	cfg RemoteControlSessionConfig
}

func NewRemoteControlSession(cfg RemoteControlSessionConfig) *RemoteControlSession {
	if cfg.NodeControlPath == "" {
		cfg.NodeControlPath = DefaultNodeControlPath
	}
	if cfg.SessionEventSink == nil {
		cfg.SessionEventSink = NoopSessionEventSink{}
	}
	return &RemoteControlSession{cfg: cfg}
}

func (s *RemoteControlSession) Run(ctx context.Context) Exit {
	if s.cfg.Spec.RemoteID == "" {
		return ConfigExit("missing_remote_id", "remote id is required")
	}
	if s.cfg.Spec.CloudAPIURL == "" {
		return ConfigExit("missing_cloud_url", "cloud api url is required")
	}
	if s.cfg.Headers == nil {
		return ConfigExit("missing_auth_provider", "auth header provider is required")
	}
	if s.cfg.Dialer == nil {
		return ConfigExit("missing_dialer", "websocket dialer is required")
	}
	if s.cfg.Runner == nil {
		return ConfigExit("missing_node_control_runner", "node-control runner is required")
	}

	header, err := s.cfg.Headers.Headers(ctx, s.cfg.Spec.RemoteID)
	if err != nil {
		return AuthExit("auth_headers_failed", err.Error())
	}
	wsURL, err := websocketURLFromHTTP(s.cfg.Spec.CloudAPIURL, s.cfg.NodeControlPath)
	if err != nil {
		return ConfigExit("invalid_cloud_url", err.Error())
	}
	q := wsURL.Query()
	if s.cfg.Spec.NodeID != "" {
		q.Set("node_id", s.cfg.Spec.NodeID)
	}
	wsURL.RawQuery = q.Encode()

	s.emit(PhaseConnecting)
	conn, resp, err := s.cfg.Dialer.Dial(ctx, wsURL.String(), header)
	closeResponse(resp)
	if err != nil {
		return classifyDialExit(err, resp)
	}
	if conn == nil {
		return TransientExit("dial_no_connection", "websocket dialer returned nil connection")
	}

	hbConn := newHeartbeatConn(conn, s.cfg.Heartbeat)
	hbConn.Start()
	defer hbConn.Close()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go closeOnContextDone(runCtx, hbConn)

	s.emit(PhaseConnected)
	exit := s.cfg.Runner.RunNodeControl(runCtx, hbConn, s.cfg.Spec)
	if hbConn.TimedOut() {
		return TransientExit("heartbeat_timeout", "websocket heartbeat timed out")
	}
	if ctx.Err() != nil {
		return CanceledExit(ctx.Err())
	}
	if exit.Class == "" {
		return TransientExit("node_control_closed", "node-control websocket session ended")
	}
	return exit
}

func (s *RemoteControlSession) emit(phase Phase) {
	s.cfg.SessionEventSink.OnSessionEvent(SessionEvent{
		Kind:         SessionRemoteControl,
		Phase:        phase,
		RemoteID:     s.cfg.Spec.RemoteID,
		Generation:   s.cfg.Spec.Generation,
		RestartNonce: s.cfg.Spec.RestartNonce,
		At:           time.Now(),
	})
}

func closeResponse(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func classifyDialExit(err error, resp *http.Response) Exit {
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	switch status {
	case http.StatusUnauthorized:
		return AuthExit("unauthorized", "websocket handshake was unauthorized")
	case http.StatusForbidden:
		return AuthExit("access_denied", "websocket handshake was forbidden")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return CanceledExit(err)
	}
	return TransientExit("dial_failed", fmt.Sprintf("websocket dial failed: %v", err))
}

func closeOnContextDone(ctx context.Context, conn WebSocketConn) {
	<-ctx.Done()
	_ = conn.Close()
}
