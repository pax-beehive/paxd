// Package harnessauth owns ephemeral paxl authentication attempts. Authentication
// belongs to paxl; this package owns scope, deduplication and process lifetime.
package harnessauth

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/paxlinstall"
)

type Options struct {
	Command string
	TTL     time.Duration
}
type ownerScope struct {
	source control.Source
	actor  string
}
type loginScope struct {
	owner   ownerScope
	harness string
}
type session struct {
	owner      ownerScope
	view       control.HarnessAuthView
	expires    time.Time
	cancel     context.CancelFunc
	input      chan []byte
	done       chan struct{}
	commands   map[string][32]byte
	digest     [32]byte
	running    bool
	result     bool
	eventBytes int
}
type Manager struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	command  string
	ttl      time.Duration
	sessions map[string]*session
	latest   map[loginScope]*session
	active   *session
	closed   bool
	wg       sync.WaitGroup
}

func New(ctx context.Context, opts Options) *Manager {
	if opts.TTL <= 0 {
		opts.TTL = 5 * time.Minute
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Manager{ctx: ctx, cancel: cancel, command: opts.Command, ttl: opts.TTL, sessions: map[string]*session{}, latest: map[loginScope]*session{}}
}
func (m *Manager) Close()                  { m.mu.Lock(); m.closed = true; m.cancel(); m.mu.Unlock(); m.wg.Wait() }
func authError(code, message string) error { return control.ControlError{Code: code, Message: message} }
func (m *Manager) Login(ctx context.Context, source control.Source, commandID string, req control.HarnessAuthLoginCommand) (control.HarnessAuthView, error) {
	if err := req.Validate(); err != nil {
		return control.HarnessAuthView{}, err
	}
	if err := ctx.Err(); err != nil {
		return control.HarnessAuthView{}, err
	}
	owner := ownerScope{source: source, actor: req.ActorID}
	payload, _ := json.Marshal(req)
	digest := sha256.Sum256(payload)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return control.HarnessAuthView{}, authError("unavailable", "authentication manager stopped")
	}
	m.expireLocked()
	for _, s := range m.sessions {
		if s.owner == owner {
			if previous, ok := s.commands[commandID]; ok {
				if previous != digest {
					return control.HarnessAuthView{}, authError("conflict", "command id already used")
				}
				return cloneView(s.view), nil
			}
		}
	}
	if req.Operation == "start" {
		return m.startLocked(owner, commandID, digest, req)
	}
	s := m.sessions[req.SessionID]
	if s == nil || s.owner != owner || s.view.Harness != req.Harness {
		return control.HarnessAuthView{}, authError("not_found", "authentication attempt not found")
	}
	if len(s.commands) >= 64 {
		return control.HarnessAuthView{}, authError("conflict", "too many attempt operations")
	}
	if req.Operation == "cancel" {
		if s.running {
			s.view.State = "cancelled"
			clearChallenges(s)
			s.cancel()
		}
	} else if err := m.submitLocked(s, req); err != nil {
		return control.HarnessAuthView{}, err
	}
	s.commands[commandID] = digest
	return cloneView(s.view), nil
}
func (m *Manager) expireLocked() {
	for id, s := range m.sessions {
		if !s.running && time.Now().After(s.expires) {
			delete(m.sessions, id)
			key := loginScope{owner: s.owner, harness: s.view.Harness}
			if m.latest[key] == s {
				delete(m.latest, key)
			}
		}
	}
}
func (m *Manager) submitLocked(s *session, req control.HarnessAuthLoginCommand) error {
	if !s.running || time.Now().After(s.expires) || s.view.Prompt == nil || s.view.Prompt.ID != req.PromptID {
		return authError("conflict", "attempt is not waiting for this prompt")
	}
	data, _ := json.Marshal(map[string]any{"version": 1, "type": "input", "id": req.PromptID, "value": req.Code})
	select {
	case s.input <- append(data, '\n'):
		s.view.Prompt = nil
		s.view.State = "running"
		return nil
	default:
		return authError("conflict", "previous input is still being consumed")
	}
}
func (m *Manager) startLocked(owner ownerScope, commandID string, digest [32]byte, req control.HarnessAuthLoginCommand) (control.HarnessAuthView, error) {
	if m.active != nil && m.active.running {
		if m.active.owner == owner && m.active.digest == digest {
			if len(m.active.commands) >= 64 {
				return control.HarnessAuthView{}, authError("conflict", "too many repeated starts")
			}
			m.active.commands[commandID] = digest
			return cloneView(m.active.view), nil
		}
		return control.HarnessAuthView{}, authError("conflict", "an authentication attempt is already running")
	}
	if len(m.sessions) >= 32 {
		return control.HarnessAuthView{}, authError("conflict", "too many recent authentication attempts")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return control.HarnessAuthView{}, authError("internal_error", "could not create authentication attempt")
	}
	command, err := m.resolveCommand()
	if err != nil {
		return control.HarnessAuthView{}, authError("login_start_failed", "paxl is unavailable; check daemon PATH")
	}
	expires := time.Now().Add(m.ttl)
	ctx, cancel := context.WithDeadline(m.ctx, expires)
	s := &session{owner: owner, digest: digest, expires: expires, cancel: cancel, running: true, input: make(chan []byte, 1), done: make(chan struct{}), commands: map[string][32]byte{commandID: digest}, view: control.HarnessAuthView{Harness: req.Harness, Method: req.Method, SessionID: hex.EncodeToString(nonce[:]), State: "starting", ExpiresAt: expires.UTC().Format(time.RFC3339)}}
	args := append(command[1:], loginArgs(req)...)
	cmd := exec.CommandContext(ctx, command[0], args...)
	prepareLoginCommand(cmd)
	// Give paxl time to cancel its native child before escalating to group cleanup.
	cmd.Cancel = func() error {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			return stopLoginCommand(cmd)
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return control.HarnessAuthView{}, authError("login_start_failed", "could not open paxl input")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		cancel()
		return control.HarnessAuthView{}, authError("login_start_failed", "could not open paxl event stream")
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		cancel()
		return control.HarnessAuthView{}, authError("login_start_failed", "could not start paxl authentication")
	}
	m.sessions[s.view.SessionID] = s
	m.latest[loginScope{owner: owner, harness: req.Harness}] = s
	m.active = s
	m.wg.Add(2)
	go m.writeInput(ctx, s, stdin)
	go m.run(ctx, s, cmd, stdout)
	return cloneView(s.view), nil
}
func (m *Manager) resolveCommand() ([]string, error) {
	var command []string
	if m.command != "" {
		command = []string{m.command}
	}
	return paxlinstall.ResolveCommand(command)
}
func loginArgs(req control.HarnessAuthLoginCommand) []string {
	action := req.Action
	if action == "" {
		action = "login"
	}
	args := []string{"harness", "auth", action, "--harness", req.Harness, "--protocol", "jsonl"}
	for _, pair := range [][2]string{{"--method", req.Method}, {"--provider", req.Provider}, {"--profile", req.Profile}} {
		if pair[1] != "" {
			args = append(args, pair[0], pair[1])
		}
	}
	if req.SecretStdin {
		args = append(args, "--secret-stdin")
	}
	return args
}
func (m *Manager) writeInput(ctx context.Context, s *session, stdin io.WriteCloser) {
	defer m.wg.Done()
	defer func() { _ = stdin.Close() }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case input := <-s.input:
			_, err := stdin.Write(input)
			clear(input)
			if err != nil {
				s.cancel()
				return
			}
		}
	}
}
func (m *Manager) run(ctx context.Context, s *session, cmd *exec.Cmd, stdout io.ReadCloser) {
	defer m.wg.Done()
	defer s.cancel()
	defer close(s.done)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 128*1024)
	for scanner.Scan() {
		var event control.HarnessAuthEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || !m.applyEvent(s, &event) {
			s.cancel()
			break
		}
	}
	scanErr := scanner.Err()
	err := cmd.Wait()
	_ = stopLoginCommand(cmd)
	m.mu.Lock()
	defer m.mu.Unlock()
	s.running = false
	clearInputBuffer(s)
	clearChallenges(s)
	if s.view.State == "cancelled" {
		return
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		s.view.State = "expired"
	case ctx.Err() != nil:
		s.view.State = "failed"
		s.view.ErrorCode = "login_interrupted"
	case err != nil || scanErr != nil || !s.result:
		s.view.State = "failed"
		s.view.ErrorCode = "login_failed"
	default:
		s.view.State = s.view.Result.State
	}
	if s.view.State == "failed" && s.view.Result == nil {
		s.view.Result = &control.HarnessAuthResult{Harness: s.view.Harness, State: "failed", Message: "Authentication protocol did not complete. Update paxl and verify the harness supports the selected method."}
	}
}
func (m *Manager) applyEvent(s *session, event *control.HarnessAuthEvent) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.view.State == "cancelled" {
		return true
	}
	if event.Version != 1 || !s.running || s.result {
		return false
	}
	switch event.Type {
	case "result":
		if event.Result == nil || event.Result.Harness != s.view.Harness {
			return false
		}
		switch event.Result.State {
		case "succeeded", "configured", "unknown", "failed", "logged_in", "logged_out", "available":
		default:
			return false
		}
		s.result = true
		s.view.Result = event.Result
		s.view.State = "finishing"
		clearChallenges(s)
	case "prompt":
		if event.ID == "" || len(event.ID) > 128 || len(event.Message) > 4096 || len(event.Options) > 128 {
			return false
		}
		switch event.Kind {
		case "text", "secret", "select":
		default:
			return false
		}
		s.view.Prompt = event
		s.view.State = "awaiting_input"
	case "message", "notice":
		if len(event.Message)+len(event.URL)+len(event.Code) > 8192 {
			return false
		}
		data, _ := json.Marshal(event)
		s.eventBytes += len(data)
		if s.eventBytes > 64*1024 {
			return false
		}
		if len(s.view.Events) >= 64 {
			s.view.Events = s.view.Events[1:]
		}
		s.view.Events = append(s.view.Events, *event)
		if event.URL != "" {
			s.view.AuthorizationURL = event.URL
		}
		if event.Code != "" {
			s.view.UserCode = event.Code
		}
	default:
		return false
	}
	return true
}
func clearChallenges(s *session) {
	s.view.Prompt = nil
	s.view.Events = nil
	s.view.AuthorizationURL = ""
	s.view.UserCode = ""
}
func cloneView(view control.HarnessAuthView) control.HarnessAuthView {
	view.ProtocolVersion = 1
	// Copies avoid exposing memory concurrently mutated by the child event reader.
	data, _ := json.Marshal(view)
	var copyView control.HarnessAuthView
	_ = json.Unmarshal(data, &copyView)
	return copyView
}

func clearInputBuffer(s *session) {
	for {
		select {
		case input := <-s.input:
			clear(input)
		default:
			return
		}
	}
}
