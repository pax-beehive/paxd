package runtime

import (
	"context"
	"io"
	"net/http"
	"time"
)

type ExitClass string

const (
	ExitTransient ExitClass = "transient"
	ExitAuth      ExitClass = "auth"
	ExitConfig    ExitClass = "config"
	ExitTerminal  ExitClass = "terminal"
)

func (c ExitClass) Valid() bool {
	switch c {
	case ExitTransient, ExitAuth, ExitConfig, ExitTerminal:
		return true
	default:
		return false
	}
}

type Exit struct {
	Class        ExitClass
	Code         string
	Message      string
	Details      map[string]string
	ResetBackoff bool
}

func (e Exit) WithDetail(key string, value string) Exit {
	if e.Details == nil {
		e.Details = make(map[string]string, 1)
	}
	e.Details[key] = value
	return e
}

func (e Exit) WithBackoffReset() Exit {
	e.ResetBackoff = true
	return e
}

func TransientExit(code string, message string) Exit {
	return Exit{Class: ExitTransient, Code: code, Message: message}
}

func AuthExit(code string, message string) Exit {
	return Exit{Class: ExitAuth, Code: code, Message: message}
}

func ConfigExit(code string, message string) Exit {
	return Exit{Class: ExitConfig, Code: code, Message: message}
}

func TerminalExit(code string, message string) Exit {
	return Exit{Class: ExitTerminal, Code: code, Message: message}
}

func CanceledExit(err error) Exit {
	msg := "runtime session canceled"
	if err != nil {
		msg = err.Error()
	}
	return TerminalExit("canceled", msg)
}

type Session interface {
	Run(ctx context.Context) Exit
}

type RemoteControlSessionFactory interface {
	NewRemoteControlSession(spec RemoteSpec) Session
}

type AgentTunnelSessionFactory interface {
	NewAgentTunnelSession(spec AgentConnectionSpec) Session
}

type RemoteSpec struct {
	RemoteID        string
	Name            string
	CloudAPIURL     string
	NodeControlPath string
	NodeID          string
	Generation      int64
	RestartNonce    int64
}

type AgentConnectionSpec struct {
	ConnectionID string
	RemoteID     string
	CloudAPIURL  string
	CloudAgentID string
	InstanceID   string
	AgentType    string
	Harness      string
	Command      []string
	WorkingDir   string
	TunnelPath   string
	Env          map[string]string
	Generation   int64
	RestartNonce int64
}

type WebSocketDialer interface {
	Dial(ctx context.Context, url string, header http.Header) (WebSocketConn, *http.Response, error)
}

type WebSocketConn interface {
	ReadMessage() (messageType int, payload []byte, err error)
	WriteMessage(messageType int, payload []byte) error
	Close() error
}

type NodeControlRunner interface {
	RunNodeControl(ctx context.Context, conn WebSocketConn, spec RemoteSpec) Exit
}

type NodeControlRunnerFunc func(ctx context.Context, conn WebSocketConn, spec RemoteSpec) Exit

func (f NodeControlRunnerFunc) RunNodeControl(ctx context.Context, conn WebSocketConn, spec RemoteSpec) Exit {
	if f == nil {
		return ConfigExit("missing_node_control_runner", "node-control runner is required")
	}
	return f(ctx, conn, spec)
}

type LocalACPProcessSpec struct {
	Command    []string
	WorkingDir string
	Env        map[string]string
}

type LocalACPProcessRunner interface {
	Start(ctx context.Context, spec LocalACPProcessSpec) (LocalACPProcess, error)
}

type LocalACPProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() error
	Terminate(ctx context.Context) error
}

type HeartbeatConfig struct {
	PingInterval time.Duration
	ReadTimeout  time.Duration
}

func (c HeartbeatConfig) WithDefaults() HeartbeatConfig {
	if c.PingInterval <= 0 {
		c.PingInterval = 15 * time.Second
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 45 * time.Second
	}
	return c
}
