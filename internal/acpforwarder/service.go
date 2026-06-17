// Package acpforwarder tunnels ACP JSON-RPC between pax-manager and a local
// agent CLI without interpreting protocol payloads.
package acpforwarder

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Config controls the stateless ACP forwarder service.
type Config struct {
	CloudURL          string
	APIKey            string
	CFClientID        string
	CFClientSecret    string
	AgentID           string
	InstanceID        string
	Command           []string
	WorkingDir        string
	TunnelPath        string
	ReconnectInterval time.Duration
}

// Service maintains one ACP tunnel session at a time.
type Service struct {
	cfg    Config
	dialer *websocket.Dialer
}

// New creates a forwarder service.
func New(cfg Config) *Service {
	if cfg.TunnelPath == "" {
		cfg.TunnelPath = "/api/v1/agent/tunnel"
	}
	if cfg.ReconnectInterval <= 0 {
		cfg.ReconnectInterval = 2 * time.Second
	}
	return &Service{
		cfg: cfg,
		dialer: &websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
		},
	}
}

// Run reconnects forever until ctx is canceled.
func (s *Service) Run(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}

	backoff := s.cfg.ReconnectInterval
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := s.runOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[acp-forwarder] tunnel ended: %v; reconnecting in %s", err, backoff)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (s *Service) validate() error {
	switch {
	case s.cfg.CloudURL == "":
		return fmt.Errorf("cloud url is required")
	case s.cfg.APIKey == "":
		return fmt.Errorf("api key is required")
	case len(s.cfg.Command) == 0:
		return fmt.Errorf("acp command is required")
	}
	if !strings.ContainsAny(s.cfg.Command[0], `/\`) {
		if _, err := exec.LookPath(s.cfg.Command[0]); err != nil {
			return fmt.Errorf(
				"acp command executable %q not found in PATH; install it or set agents[].acp_forwarder.command",
				s.cfg.Command[0],
			)
		}
	}
	return nil
}

func (s *Service) runOnce(ctx context.Context) error {
	tunnelURL, err := tunnelURLFromHTTP(s.cfg.CloudURL, s.cfg.TunnelPath)
	if err != nil {
		return err
	}

	q := tunnelURL.Query()
	if s.cfg.AgentID != "" {
		q.Set("agent_id", s.cfg.AgentID)
	}
	if s.cfg.InstanceID != "" {
		q.Set("instance_id", s.cfg.InstanceID)
	}
	tunnelURL.RawQuery = q.Encode()

	headers := http.Header{}
	headers.Set("X-Pax-Key", s.cfg.APIKey)
	if s.cfg.CFClientID != "" {
		headers.Set("CF-Access-Client-Id", s.cfg.CFClientID)
	}
	if s.cfg.CFClientSecret != "" {
		headers.Set("CF-Access-Client-Secret", s.cfg.CFClientSecret)
	}

	conn, resp, err := s.dialer.DialContext(ctx, tunnelURL.String(), headers)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("dial tunnel: %w", err)
	}
	defer conn.Close()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(runCtx, s.cfg.Command[0], s.cfg.Command[1:]...)
	cmd.Dir = s.cfg.WorkingDir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start acp command: %w", err)
	}

	log.Printf("[acp-forwarder] connected %s -> %s", tunnelURL.Redacted(), s.cfg.Command[0])

	errCh := make(chan error, 3)
	go func() { errCh <- copyWSToStdin(conn, stdin) }()
	go func() { errCh <- copyStdoutToWS(stdout, conn) }()
	go logStderr(stderr)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var result error
	select {
	case err := <-errCh:
		result = err
	case err := <-waitCh:
		result = err
	case <-ctx.Done():
		result = ctx.Err()
	}

	cancel()
	_ = conn.Close()
	_ = stdin.Close()

	select {
	case err := <-waitCh:
		if result == nil {
			result = err
		}
	case <-time.After(5 * time.Second):
		if result == nil {
			result = fmt.Errorf("acp command did not exit after cancellation")
		}
	}

	if result != nil && strings.Contains(result.Error(), "use of closed network connection") {
		return nil
	}
	return result
}

func copyWSToStdin(conn *websocket.Conn, stdin io.WriteCloser) error {
	defer stdin.Close()
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read tunnel: %w", err)
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}
		if _, err := stdin.Write(payload); err != nil {
			return fmt.Errorf("write acp stdin: %w", err)
		}
		if !bytes.HasSuffix(payload, []byte("\n")) {
			if _, err := stdin.Write([]byte("\n")); err != nil {
				return fmt.Errorf("write acp stdin delimiter: %w", err)
			}
		}
	}
}

func copyStdoutToWS(stdout io.Reader, conn *websocket.Conn) error {
	reader := bufio.NewReader(stdout)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = trimLineDelimiter(line)
			if len(line) > 0 {
				if writeErr := conn.WriteMessage(websocket.TextMessage, line); writeErr != nil {
					return fmt.Errorf("write tunnel: %w", writeErr)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read acp stdout: %w", err)
		}
	}
}

func trimLineDelimiter(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return line
}

func logStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		log.Printf("[acp-forwarder stderr] %s", scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[acp-forwarder] stderr read error: %v", err)
	}
}

func tunnelURLFromHTTP(rawBase, tunnelPath string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimRight(rawBase, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse cloud url: %w", err)
	}
	switch base.Scheme {
	case "https":
		base.Scheme = "wss"
	case "http":
		base.Scheme = "ws"
	case "ws", "wss":
	default:
		return nil, fmt.Errorf("unsupported cloud url scheme %q", base.Scheme)
	}
	if tunnelPath == "" {
		tunnelPath = "/api/v1/agent/tunnel"
	}
	if !strings.HasPrefix(tunnelPath, "/") {
		tunnelPath = "/" + tunnelPath
	}
	base.Path = strings.TrimRight(base.Path, "/") + tunnelPath
	return base, nil
}
