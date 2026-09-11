package browsercontrol

import (
	"context"
	"crypto/des" // RFB 3.8 VNC authentication requires DES.
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/bits"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RFB bytes use transient control queries; they never enter ACP history.
// The only upstream is the node's loopback Docker display, not a supplied URL.
type vncSession struct {
	source  string
	mu      sync.Mutex
	conn    net.Conn
	pending []byte
	input   []byte
	stage   int
	seq     uint64
	closed  bool
	timer   *time.Timer
	created time.Time
}

type vncRequest struct {
	ID   string `json:"id"`
	Seq  uint64 `json:"seq"`
	Data string `json:"data"`
}
type vncResult struct {
	ID   string `json:"id,omitempty"`
	Data string `json:"data,omitempty"`
}

func (c *Client) vnc(ctx context.Context, source, operation string, payload json.RawMessage) (json.RawMessage, error) {
	if operation == "vnc_open" {
		return c.openVNC(ctx, source)
	}
	var req vncRequest
	if json.Unmarshal(payload, &req) != nil {
		return nil, errors.New("invalid VNC request")
	}
	c.mu.Lock()
	session := c.vncSessions[req.ID]
	c.mu.Unlock()
	if session == nil || session.source != source {
		return nil, errors.New("VNC session unavailable")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if operation == "vnc_close" {
		session.close()
		return json.Marshal(vncResult{})
	}
	if session.closed || time.Since(session.created) > 30*time.Minute {
		session.close()
		return nil, errors.New("VNC session expired")
	}
	if req.Seq != session.seq {
		session.close()
		return nil, errors.New("VNC sequence mismatch; reconnect")
	}
	session.seq++
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil || len(data) > 8192 {
		session.close()
		return nil, errors.New("invalid VNC input")
	}
	session.timer.Reset(30 * time.Second)
	if err := session.write(data); err != nil {
		session.close()
		return nil, errors.New("VNC input failed; reconnect")
	}
	size := min(len(session.pending), 128*1024)
	output := append([]byte(nil), session.pending[:size]...)
	session.pending = session.pending[size:]
	return json.Marshal(vncResult{Data: base64.StdEncoding.EncodeToString(output)})
}

func (c *Client) openVNC(ctx context.Context, source string) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, s := range c.vncSessions {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			delete(c.vncSessions, id)
		}
	}
	if len(c.vncSessions) >= 2 {
		return nil, errors.New("too many VNC viewers")
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:5900")
	if err != nil {
		return nil, errors.New("Docker VNC is not available")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		conn.Close()
		return nil, errors.New("VNC credential unavailable")
	}
	password, err := os.ReadFile(filepath.Join(home, ".local/share/agent-browser/secrets/vnc-password"))
	if err != nil {
		conn.Close()
		return nil, errors.New("VNC credential unavailable")
	}
	err = authenticateVNC(conn, []byte(strings.TrimSpace(string(password))))
	clear(password)
	if err != nil {
		conn.Close()
		return nil, errors.New("VNC authentication failed")
	}
	idBytes := make([]byte, 24)
	if _, err := rand.Read(idBytes); err != nil {
		conn.Close()
		return nil, errors.New("VNC session creation failed")
	}
	id := hex.EncodeToString(idBytes)
	s := &vncSession{source: source, conn: conn, created: time.Now()}
	s.timer = time.AfterFunc(30*time.Second, func() { s.mu.Lock(); defer s.mu.Unlock(); s.close() })
	if c.vncSessions == nil {
		c.vncSessions = make(map[string]*vncSession)
	}
	c.vncSessions[id] = s
	go s.read()
	return json.Marshal(vncResult{ID: id, Data: base64.StdEncoding.EncodeToString([]byte("RFB 003.008\n"))})
}

func authenticateVNC(conn net.Conn, password []byte) error {
	defer clear(password)
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	version := make([]byte, 12)
	if _, err := io.ReadFull(conn, version); err != nil {
		return err
	}
	if string(version) != "RFB 003.008\n" {
		return errors.New("unsupported RFB version")
	}
	if _, err := conn.Write(version); err != nil {
		return err
	}
	count := []byte{0}
	if _, err := io.ReadFull(conn, count); err != nil {
		return err
	}
	security := make([]byte, int(count[0]))
	if _, err := io.ReadFull(conn, security); err != nil {
		return err
	}
	found := false
	for _, typ := range security {
		if typ == 2 {
			found = true
		}
	}
	if !found {
		return errors.New("VNC password authentication required")
	}
	if _, err := conn.Write([]byte{2}); err != nil {
		return err
	}
	challenge := make([]byte, 16)
	if _, err := io.ReadFull(conn, challenge); err != nil {
		return err
	}
	key := make([]byte, 8)
	copy(key, password)
	for i := range key {
		key[i] = bits.Reverse8(key[i])
	}
	cipher, err := des.NewCipher(key)
	clear(key)
	if err != nil {
		return err
	}
	cipher.Encrypt(challenge[:8], challenge[:8])
	cipher.Encrypt(challenge[8:], challenge[8:])
	if _, err := conn.Write(challenge); err != nil {
		return err
	}
	status := make([]byte, 4)
	if _, err := io.ReadFull(conn, status); err != nil {
		return err
	}
	if binary.BigEndian.Uint32(status) != 0 {
		return errors.New("VNC authentication rejected")
	}
	return conn.SetDeadline(time.Time{})
}

// The browser receives a private, already-authenticated RFB connection.
// Terminate its version/security handshake here; forward ClientInit onwards.
func (s *vncSession) write(data []byte) error {
	s.input = append(s.input, data...)
	for s.stage < 2 {
		need := 1
		if s.stage == 0 {
			need = 12
		}
		if len(s.input) < need {
			return nil
		}
		part := s.input[:need]
		s.input = s.input[need:]
		if s.stage == 0 {
			if string(part) != "RFB 003.008\n" {
				return errors.New("invalid RFB version")
			}
			s.pending = append(s.pending, 1, 1)
		} else {
			if part[0] != 1 {
				return errors.New("invalid RFB security")
			}
			s.pending = append(s.pending, 0, 0, 0, 0)
		}
		s.stage++
	}
	if len(s.input) > 0 {
		if err := s.conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return err
		}
		if _, err := s.conn.Write(s.input); err != nil {
			return err
		}
		s.input = nil
	}
	return nil
}
func (s *vncSession) read() {
	buf := make([]byte, 64*1024)
	for {
		n, err := s.conn.Read(buf)
		s.mu.Lock()
		if err != nil || s.closed || len(s.pending)+n > 4*1024*1024 {
			s.close()
			s.mu.Unlock()
			return
		}
		s.pending = append(s.pending, buf[:n]...)
		s.mu.Unlock()
	}
}
func (s *vncSession) close() {
	if !s.closed {
		s.closed = true
		s.timer.Stop()
		s.conn.Close()
		s.pending = nil
		s.input = nil
	}
}
