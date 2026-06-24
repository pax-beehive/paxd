package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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
		log.Printf("[paxd] remote session validation failed: missing remote id")
		return ConfigExit("missing_remote_id", "remote id is required")
	}
	if s.cfg.Spec.CloudAPIURL == "" {
		log.Printf("[paxd] remote session id=%s validation failed: missing cloud api url", s.cfg.Spec.RemoteID)
		return ConfigExit("missing_cloud_url", "cloud api url is required")
	}
	if s.cfg.Headers == nil {
		log.Printf("[paxd] remote session id=%s validation failed: missing auth provider", s.cfg.Spec.RemoteID)
		return ConfigExit("missing_auth_provider", "auth header provider is required")
	}
	if s.cfg.Dialer == nil {
		log.Printf("[paxd] remote session id=%s validation failed: missing websocket dialer", s.cfg.Spec.RemoteID)
		return ConfigExit("missing_dialer", "websocket dialer is required")
	}
	if s.cfg.Runner == nil {
		log.Printf("[paxd] remote session id=%s validation failed: missing node-control runner", s.cfg.Spec.RemoteID)
		return ConfigExit("missing_node_control_runner", "node-control runner is required")
	}

	header, err := s.cfg.Headers.Headers(ctx, s.cfg.Spec.RemoteID)
	if err != nil {
		log.Printf("[paxd] remote session id=%s auth headers failed: %v", s.cfg.Spec.RemoteID, err)
		return AuthExit("auth_headers_failed", err.Error())
	}
	wsURL, err := websocketURLFromHTTP(s.cfg.Spec.CloudAPIURL, s.cfg.NodeControlPath)
	if err != nil {
		log.Printf("[paxd] remote session id=%s invalid cloud url %q: %v", s.cfg.Spec.RemoteID, s.cfg.Spec.CloudAPIURL, err)
		return ConfigExit("invalid_cloud_url", err.Error())
	}
	q := wsURL.Query()
	if s.cfg.Spec.NodeID != "" {
		q.Set("node_id", s.cfg.Spec.NodeID)
	}
	wsURL.RawQuery = q.Encode()

	s.emit(PhaseConnecting)
	log.Printf("[paxd] remote session id=%s dialing %s", s.cfg.Spec.RemoteID, wsURL.Redacted())
	conn, resp, err := s.cfg.Dialer.Dial(ctx, wsURL.String(), header)
	closeResponse(resp)
	if err != nil {
		log.Printf("[paxd] remote session id=%s dial failed status=%d err=%v", s.cfg.Spec.RemoteID, responseStatus(resp), err)
		return classifyDialExit(err, resp)
	}
	if conn == nil {
		log.Printf("[paxd] remote session id=%s dialer returned nil connection", s.cfg.Spec.RemoteID)
		return TransientExit("dial_no_connection", "websocket dialer returned nil connection")
	}

	hbConn := newHeartbeatConn(conn, s.cfg.Heartbeat)
	hbConn.Start()
	defer hbConn.Close()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go closeOnContextDone(runCtx, hbConn)

	s.emit(PhaseConnected)
	log.Printf("[paxd] remote session id=%s connected", s.cfg.Spec.RemoteID)
	exit := s.cfg.Runner.RunNodeControl(runCtx, hbConn, s.cfg.Spec)
	if hbConn.TimedOut() {
		log.Printf("[paxd] remote session id=%s heartbeat timed out", s.cfg.Spec.RemoteID)
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

func responseStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
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
