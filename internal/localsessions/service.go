package localsessions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
)

type Store interface {
	ListSessions(ctx context.Context, query control.ListLocalSessionsQuery) ([]control.LocalSessionView, error)
	UpsertSessions(ctx context.Context, sessions []control.LocalSessionView) error
	GetSession(ctx context.Context, sessionID string) (*control.LocalSessionView, error)
}

type Scanner interface {
	ListSessions(ctx context.Context, agent string, limit int) ([]control.LocalSessionView, error)
}

type ScannerRegistry interface {
	ScannerFor(agent string) (Scanner, bool)
	AvailableAgents(ctx context.Context) ([]string, error)
}

type Service struct {
	store    Store
	scanners ScannerRegistry
	now      func() time.Time
}

type Option func(*Service)

func WithClock(now func() time.Time) Option {
	return func(service *Service) {
		service.now = now
	}
}

func New(store Store, scanners ScannerRegistry, opts ...Option) *Service {
	service := &Service{
		store:    store,
		scanners: scanners,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(service)
	}
	return service
}

func (s *Service) List(ctx context.Context, query control.ListLocalSessionsQuery) ([]control.LocalSessionView, error) {
	if s.store == nil {
		return nil, errors.New("local session store is not configured")
	}
	return s.store.ListSessions(ctx, query)
}

func (s *Service) Sync(ctx context.Context, query control.SyncLocalSessionsQuery) (control.LocalSessionSyncResult, error) {
	if s.store == nil {
		return control.LocalSessionSyncResult{}, errors.New("local session store is not configured")
	}
	if s.scanners == nil {
		return control.LocalSessionSyncResult{}, errors.New("local session scanners are not configured")
	}
	agents, explicit, err := s.syncAgents(ctx, query.Agent)
	if err != nil {
		return control.LocalSessionSyncResult{Failed: 1, Errors: []control.ControlError{controlErr("scanner_unavailable", err.Error(), "agent")}}, err
	}
	result := control.LocalSessionSyncResult{}
	var syncErr error
	for _, agent := range agents {
		scanner, ok := s.scanners.ScannerFor(agent)
		if !ok {
			if explicit {
				err := fmt.Errorf("local session scanner %q is not available", agent)
				return control.LocalSessionSyncResult{Failed: 1, Errors: []control.ControlError{controlErr("scanner_unavailable", err.Error(), "agent")}}, err
			}
			continue
		}
		scanCtx, cancel := s.scannerContext(ctx, query.TimeoutMillis)
		sessions, err := scanner.ListSessions(scanCtx, agent, query.Limit)
		cancel()
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, controlErr("scan_failed", err.Error(), agent))
			syncErr = err
			continue
		}
		stamped := s.stampSessions(agent, sessions)
		if err := s.store.UpsertSessions(ctx, stamped); err != nil {
			result.Failed++
			result.Errors = append(result.Errors, controlErr("cache_write_failed", err.Error(), agent))
			syncErr = err
			continue
		}
		result.Synced += len(stamped)
	}
	return result, syncErr
}

func (s *Service) Get(ctx context.Context, query control.GetLocalSessionQuery) (*control.LocalSessionView, error) {
	if s.store == nil {
		return nil, errors.New("local session store is not configured")
	}
	return s.store.GetSession(ctx, query.SessionID)
}

func (s *Service) syncAgents(ctx context.Context, agent string) ([]string, bool, error) {
	agent = normalizeAgent(agent)
	if agent != "" {
		return []string{agent}, true, nil
	}
	agents, err := s.scanners.AvailableAgents(ctx)
	if err != nil {
		return nil, false, err
	}
	out := make([]string, 0, len(agents))
	for _, item := range agents {
		item = normalizeAgent(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out, false, nil
}

func (s *Service) scannerContext(ctx context.Context, timeoutMillis int64) (context.Context, context.CancelFunc) {
	if timeoutMillis <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, time.Duration(timeoutMillis)*time.Millisecond)
}

func (s *Service) stampSessions(agent string, sessions []control.LocalSessionView) []control.LocalSessionView {
	now := s.now().UTC().Format(time.RFC3339Nano)
	out := make([]control.LocalSessionView, 0, len(sessions))
	for _, session := range sessions {
		session.Agent = normalizeAgent(firstNonEmpty(session.Agent, agent))
		if session.ID == "" && session.Agent != "" && session.NativeID != "" {
			session.ID = session.Agent + ":" + session.NativeID
		}
		if session.LastListedAt == "" {
			session.LastListedAt = now
		}
		session.LastSyncedAt = now
		session.Metadata = safeMetadata(session.Metadata)
		out = append(out, session)
	}
	return out
}

func safeMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	safe := make(map[string]string, len(metadata))
	for key, value := range metadata {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "password") {
			continue
		}
		safe[key] = value
	}
	return safe
}

func normalizeAgent(agent string) string {
	return strings.ToLower(strings.TrimSpace(agent))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func controlErr(code string, message string, target string) control.ControlError {
	return control.ControlError{Code: code, Message: message, Target: target}
}
