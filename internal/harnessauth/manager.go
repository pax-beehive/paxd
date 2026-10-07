// Package harnessauth owns short-lived native harness login processes. It never
// reads credential files or returns raw subprocess output.
package harnessauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
)

type Options struct {
	Command string
	TTL     time.Duration
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

type loginScope struct {
	owner   control.Source
	harness string
}

type session struct {
	owner       control.Source
	view        control.HarnessAuthView
	expires     time.Time
	cancel      context.CancelFunc
	stdin       io.WriteCloser
	startDigest [32]byte
	commands    map[string][32]byte
	running     bool
}

func New(ctx context.Context, opts Options) *Manager {
	if opts.TTL <= 0 {
		opts.TTL = 5 * time.Minute
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Manager{ctx: ctx, cancel: cancel, command: opts.Command, ttl: opts.TTL, sessions: map[string]*session{}, latest: map[loginScope]*session{}}
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
}

func authError(code, message string) error { return control.ControlError{Code: code, Message: message} }

func (m *Manager) Login(ctx context.Context, owner control.Source, commandID string, req control.HarnessAuthLoginCommand) (control.HarnessAuthView, error) {
	if err := req.Validate(); err != nil {
		return control.HarnessAuthView{}, err
	}
	if err := ctx.Err(); err != nil {
		return control.HarnessAuthView{}, err
	}
	if req.Operation == "start" {
		req.Method = loginMethod(req)
	}
	// Keep only a digest for retry detection, never the submitted code.
	payload, _ := json.Marshal(req)
	digest := sha256.Sum256(payload)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return control.HarnessAuthView{}, authError("unavailable", "login manager stopped")
	}
	for id, s := range m.sessions {
		if !s.running && time.Now().After(s.expires) {
			delete(m.sessions, id)
			scope := loginScope{owner: s.owner, harness: s.view.Harness}
			if m.latest[scope] == s {
				delete(m.latest, scope)
			}
			continue
		}
		if s.owner == owner {
			if previous, ok := s.commands[commandID]; ok {
				if previous != digest {
					return control.HarnessAuthView{}, authError("conflict", "command id already used for another login operation")
				}
				return s.view, nil
			}
		}
	}
	if req.Operation == "start" {
		return m.startLocked(owner, commandID, digest, req)
	}
	s := m.sessions[req.SessionID]
	if req.SessionID == "" {
		s = m.active
	}
	if s == nil || s.owner != owner || s.view.Harness != req.Harness {
		return control.HarnessAuthView{}, authError("not_found", "login session not found")
	}
	if len(s.commands) >= 64 {
		return control.HarnessAuthView{}, authError("conflict", "too many operations for login session")
	}
	if req.Operation == "cancel" {
		if s.running {
			s.view.State = "cancelled"
			s.view.AuthorizationURL = ""
			s.view.UserCode = ""
			s.cancel()
		}
	} else {
		if !s.running || s.view.State != "awaiting_code" || time.Now().After(s.expires) {
			return control.HarnessAuthView{}, authError("conflict", "login is not waiting for a code")
		}
		// A single <=4 KiB write to the child's empty stdin pipe cannot accumulate
		// unbounded input. A session accepts at most one authorization code.
		if _, err := io.WriteString(s.stdin, req.Code+"\n"); err != nil {
			return control.HarnessAuthView{}, authError("login_input_failed", "could not submit authorization code")
		}
		s.view.State = "exchanging"
		s.view.AuthorizationURL = ""
		s.view.UserCode = ""
	}
	s.commands[commandID] = digest
	return s.view, nil
}

func (m *Manager) startLocked(owner control.Source, commandID string, digest [32]byte, req control.HarnessAuthLoginCommand) (control.HarnessAuthView, error) {
	harness, method := req.Harness, loginMethod(req)
	if m.active != nil && m.active.running {
		if m.active.view.State == "cancelled" {
			return control.HarnessAuthView{}, authError("conflict", "previous login is still stopping; retry shortly")
		}
		if m.active.owner == owner && m.active.view.Harness == harness && m.active.view.Method == method && m.active.startDigest == digest {
			if len(m.active.commands) >= 62 {
				return control.HarnessAuthView{}, authError("conflict", "too many repeated login starts")
			}
			m.active.commands[commandID] = digest
			return m.active.view, nil
		}
		return control.HarnessAuthView{}, authError("conflict", "a harness login is already running")
	}
	if len(m.sessions) >= 32 {
		return control.HarnessAuthView{}, authError("conflict", "too many recent login sessions; retry after expiration")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return control.HarnessAuthView{}, authError("internal_error", "could not create login session")
	}
	expires := time.Now().Add(m.ttl)
	ctx, cancel := context.WithDeadline(m.ctx, expires)
	s := &session{owner: owner, startDigest: digest, expires: expires, cancel: cancel, running: true, commands: map[string][32]byte{commandID: digest}, view: control.HarnessAuthView{Harness: harness, Method: method, SessionID: hex.EncodeToString(nonce[:]), State: "starting", ExpiresAt: expires.UTC().Format(time.RFC3339)}}
	args := loginArgs(harness, method)
	cmd := exec.CommandContext(ctx, m.commandFor(harness), args...)
	prepareLoginCommand(cmd)
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return control.HarnessAuthView{}, authError("login_start_failed", "could not open harness login input")
	}
	s.stdin = stdin
	output := &loginOutput{manager: m, session: s}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		stdin.Close()
		cancel()
		return control.HarnessAuthView{}, authError("login_start_failed", "could not start harness login; check installation and daemon PATH")
	}
	if req.Code != "" {
		if _, err := io.WriteString(stdin, req.Code+"\n"); err != nil {
			cancel()
			stdin.Close()
			// Reap without holding up the daemon's next operation.
			m.wg.Add(1)
			go func() { defer m.wg.Done(); _ = cmd.Wait(); _ = stopLoginCommand(cmd) }()
			return control.HarnessAuthView{}, authError("login_input_failed", "could not submit login input")
		}
		stdin.Close()
		s.view.State = "exchanging"
	}
	m.sessions[s.view.SessionID] = s
	m.latest[loginScope{owner: owner, harness: harness}] = s
	m.active = s
	m.wg.Add(1)
	go m.wait(ctx, s, cmd)
	return s.view, nil
}

func (m *Manager) wait(ctx context.Context, s *session, cmd *exec.Cmd) {
	defer m.wg.Done()
	defer s.cancel()
	err := cmd.Wait()
	_ = stopLoginCommand(cmd)
	s.stdin.Close()
	var status control.HarnessAuthView
	var statusErr error
	if err == nil {
		status, statusErr = m.authStatus(ctx, s.view.Harness)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s.running = false
	s.view.AuthorizationURL = ""
	s.view.UserCode = ""
	if s.view.State == "cancelled" {
		return
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		s.view.State = "expired"
	case ctx.Err() != nil:
		s.view.State = "cancelled"
	case err != nil:
		s.view.State = "failed"
		s.view.ErrorCode = "login_failed"
	case statusErr != nil || status.LoggedIn == nil || !*status.LoggedIn || !loginStatusMatches(s.view, status):
		s.view.State = "failed"
		s.view.ErrorCode = "login_not_verified"
	default:
		s.view.State = "succeeded"
		s.view.LoggedIn = status.LoggedIn
		s.view.AuthMethod = status.AuthMethod
	}
}

func (m *Manager) Status(ctx context.Context, owner control.Source, q control.HarnessAuthStatusQuery) (control.HarnessAuthView, error) {
	if err := q.Validate(); err != nil {
		return control.HarnessAuthView{}, err
	}
	if q.SessionID == "" {
		m.mu.Lock()
		if recent := m.latest[loginScope{owner: owner, harness: q.Harness}]; recent != nil && (recent.running || time.Now().Before(recent.expires)) {
			view := recent.view
			m.mu.Unlock()
			return view, nil
		}
		m.mu.Unlock()
		return m.authStatus(ctx, q.Harness)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[q.SessionID]
	if s == nil || s.owner != owner || s.view.Harness != q.Harness {
		return control.HarnessAuthView{}, authError("not_found", "login session not found")
	}
	return s.view, nil
}

func (m *Manager) authStatus(ctx context.Context, harness string) (control.HarnessAuthView, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if harness == "codex" {
		return m.codexStatus(ctx)
	}
	cmd := exec.CommandContext(ctx, m.commandFor(harness), "auth", "status")
	prepareLoginCommand(cmd)
	cmd.WaitDelay = time.Second
	output := &limitedOutput{}
	cmd.Stdout = output
	err := cmd.Run()
	_ = stopLoginCommand(cmd)
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		return control.HarnessAuthView{}, authError("status_failed", "could not check Claude authentication")
	}
	var raw struct {
		LoggedIn   *bool  `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if json.Unmarshal(output.data, &raw) != nil || raw.LoggedIn == nil {
		return control.HarnessAuthView{}, authError("status_failed", "invalid Claude authentication status")
	}
	state := "logged_out"
	if *raw.LoggedIn {
		state = "logged_in"
	}
	return control.HarnessAuthView{Harness: "claude", State: state, LoggedIn: raw.LoggedIn, AuthMethod: raw.AuthMethod}, nil
}

type limitedOutput struct{ data []byte }

func (w *limitedOutput) Write(p []byte) (int, error) {
	if len(w.data)+len(p) > 64*1024 {
		return 0, errors.New("output limit exceeded")
	}
	w.data = append(w.data, p...)
	return len(p), nil
}

// Parse complete URLs, including terminal OSC hyperlinks, across write chunks.
// Only the authorize URL is retained; all other CLI output is discarded.
var urlPattern = regexp.MustCompile(`https://[^\s\x00-\x20\x7f]+`)

type loginOutput struct {
	manager *Manager
	session *session
	tail    string
}

func (w *loginOutput) Write(p []byte) (int, error) {
	m := w.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.session.view.State != "starting" {
		return len(p), nil
	}
	w.tail += string(p)
	if w.session.view.Harness == "codex" {
		w.parseCodexDevice()
		if len(w.tail) > 16384 {
			w.tail = w.tail[len(w.tail)-16384:]
		}
		return len(p), nil
	}
	for _, loc := range urlPattern.FindAllStringIndex(w.tail, -1) {
		if loc[1] == len(w.tail) {
			continue
		}
		candidate := w.tail[loc[0]:loc[1]]
		u, err := url.Parse(candidate)
		if err != nil || u.User != nil || u.Port() != "" {
			continue
		}
		if u.Host != "claude.com" && u.Host != "claude.ai" && u.Host != "console.anthropic.com" && u.Host != "platform.claude.com" {
			continue
		}
		if u.Path != "/cai/oauth/authorize" && u.Path != "/oauth/authorize" {
			continue
		}
		q := u.Query()
		if q.Get("state") == "" || q.Get("code_challenge") == "" || q.Get("response_type") != "code" {
			continue
		}
		w.session.view.AuthorizationURL = candidate
		// The URL can arrive before the stdin prompt. Preserve it until the CLI
		// explicitly reports it can accept a code.
	}
	if w.session.view.AuthorizationURL != "" && strings.Contains(w.tail, "Paste code here") {
		w.session.view.State = "awaiting_code"
		w.tail = ""
	}
	if len(w.tail) > 16384 {
		w.tail = w.tail[len(w.tail)-16384:]
	}
	return len(p), nil
}
