package sessionreporter

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/supervisor"
	"github.com/pax-beehive/paxd/pkg/model"
)

const (
	defaultInterval      = 30 * time.Second
	defaultScanTimeout   = 5 * time.Second
	defaultReportTimeout = 5 * time.Second
)

type RuntimeSource interface {
	ObservedAgentRuntimes() []supervisor.ObservedAgentRuntime
}

type Scanner interface {
	ListSessions(ctx context.Context, spec SessionScannerSpec) ([]model.SessionInfo, error)
}

type Reporter interface {
	ReportAgentSessions(
		ctx context.Context,
		target ReportTarget,
		agentID string,
		sessions []cloud.SessionStatus,
	) error
}

type Options struct {
	RuntimeSource RuntimeSource
	Scanner       Scanner
	Reporter      Reporter
	Interval      time.Duration
	ScanTimeout   time.Duration
	ReportTimeout time.Duration
}

type Service struct {
	source        RuntimeSource
	scanner       Scanner
	reporter      Reporter
	interval      time.Duration
	scanTimeout   time.Duration
	reportTimeout time.Duration

	mu       sync.Mutex
	inFlight map[string]bool
}

type SessionScannerSpec struct {
	ConnectionID  string
	RemoteID      string
	CloudAPIURL   string
	CloudAgentID  string
	InstanceID    string
	AgentType     string
	Harness       string
	Command       []string
	WorkingDir    string
	Env           map[string]string
	Generation    int64
	RestartNonce  int64
	ObservedPhase string
}

type ReportTarget struct {
	RemoteID    string
	CloudAPIURL string
}

func New(opts Options) *Service {
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	scanTimeout := opts.ScanTimeout
	if scanTimeout <= 0 {
		scanTimeout = defaultScanTimeout
	}
	reportTimeout := opts.ReportTimeout
	if reportTimeout <= 0 {
		reportTimeout = defaultReportTimeout
	}
	return &Service{
		source:        opts.RuntimeSource,
		scanner:       opts.Scanner,
		reporter:      opts.Reporter,
		interval:      interval,
		scanTimeout:   scanTimeout,
		reportTimeout: reportTimeout,
		inFlight:      make(map[string]bool),
	}
}

func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	go func() {
		s.scheduleObserved(ctx)
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.scheduleObserved(ctx)
			}
		}
	}()
}

func (s *Service) RunOnce(ctx context.Context) error {
	if s == nil || s.source == nil || s.scanner == nil || s.reporter == nil {
		return nil
	}
	groups := groupByRemote(s.source.ObservedAgentRuntimes())
	for _, runtimes := range groups {
		s.runRemote(ctx, runtimes)
	}
	return nil
}

func (s *Service) scheduleObserved(ctx context.Context) {
	if s.source == nil || s.scanner == nil || s.reporter == nil {
		return
	}
	for key, runtimes := range groupByRemote(s.source.ObservedAgentRuntimes()) {
		if !s.tryStartRemote(key) {
			log.Printf("[sessionreporter] skipping remote %q because previous run is still active", key)
			continue
		}
		go func(remoteKey string, remoteRuntimes []supervisor.ObservedAgentRuntime) {
			defer s.finishRemote(remoteKey)
			s.runRemote(ctx, remoteRuntimes)
		}(key, runtimes)
	}
}

func (s *Service) runRemote(ctx context.Context, runtimes []supervisor.ObservedAgentRuntime) {
	for _, runtime := range runtimes {
		if ctx.Err() != nil {
			return
		}
		spec := scannerSpec(runtime)
		if spec.CloudAgentID == "" {
			log.Printf("[sessionreporter] skipping connection %q because cloud agent id is empty", spec.ConnectionID)
			continue
		}
		scanCtx, cancel := context.WithTimeout(ctx, s.scanTimeout)
		sessions, err := s.scanner.ListSessions(scanCtx, spec)
		cancel()
		if err != nil {
			log.Printf("[sessionreporter] scan failed remote=%q connection=%q agent=%q: %v", spec.RemoteID, spec.ConnectionID, spec.CloudAgentID, err)
			continue
		}
		if len(sessions) == 0 {
			continue
		}
		reportCtx, cancel := context.WithTimeout(ctx, s.reportTimeout)
		err = s.reporter.ReportAgentSessions(
			reportCtx,
			ReportTarget{RemoteID: spec.RemoteID, CloudAPIURL: spec.CloudAPIURL},
			spec.CloudAgentID,
			sessionStatuses(sessions),
		)
		cancel()
		if err != nil {
			log.Printf("[sessionreporter] report failed remote=%q agent=%q: %v", spec.RemoteID, spec.CloudAgentID, err)
		}
	}
}

func (s *Service) tryStartRemote(remoteKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[remoteKey] {
		return false
	}
	s.inFlight[remoteKey] = true
	return true
}

func (s *Service) finishRemote(remoteKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, remoteKey)
}

func groupByRemote(observed []supervisor.ObservedAgentRuntime) map[string][]supervisor.ObservedAgentRuntime {
	groups := make(map[string][]supervisor.ObservedAgentRuntime)
	for _, runtime := range observed {
		key := runtime.Spec.RemoteID
		if key == "" {
			key = runtime.Spec.CloudAPIURL
		}
		groups[key] = append(groups[key], runtime)
	}
	return groups
}

func scannerSpec(observed supervisor.ObservedAgentRuntime) SessionScannerSpec {
	spec := observed.Spec
	return SessionScannerSpec{
		ConnectionID:  spec.ConnectionID,
		RemoteID:      spec.RemoteID,
		CloudAPIURL:   spec.CloudAPIURL,
		CloudAgentID:  spec.CloudAgentID,
		InstanceID:    spec.InstanceID,
		AgentType:     spec.AgentType,
		Harness:       spec.Harness,
		Command:       append([]string(nil), spec.Command...),
		WorkingDir:    spec.WorkingDir,
		Env:           cloneStringMap(spec.Env),
		Generation:    spec.Generation,
		RestartNonce:  spec.RestartNonce,
		ObservedPhase: observed.Phase,
	}
}

func sessionStatuses(sessions []model.SessionInfo) []cloud.SessionStatus {
	out := make([]cloud.SessionStatus, 0, len(sessions))
	for _, session := range sessions {
		if session.SessionID == "" {
			continue
		}
		out = append(out, cloud.SessionStatus{
			SessionID:      session.SessionID,
			AgentType:      session.AgentType,
			NativeID:       session.NativeID,
			Name:           session.Name,
			ProjectID:      session.ProjectID,
			Preview:        session.Preview,
			WorkspaceRoots: append([]string(nil), session.WorkspaceRoots...),
			Source:         session.Source,
			Status:         session.Status,
			CurrentTask:    session.CurrentTask,
			TokenUsage:     cloud.TokenUsage{TotalTokens: session.TokenUsage},
			LastMessageAt:  firstNonEmpty(session.UpdatedAt, session.LastActive),
			Messages:       sessionMessages(session.Messages),
		})
	}
	return out
}

func sessionMessages(messages []model.SessionMessage) []cloud.SessionMessage {
	if len(messages) == 0 {
		return nil
	}
	out := make([]cloud.SessionMessage, 0, len(messages))
	for _, message := range messages {
		if message.Text == "" {
			continue
		}
		out = append(out, cloud.SessionMessage{
			SessionID:   message.SessionID,
			Seq:         message.Seq,
			Kind:        message.Kind,
			Role:        message.Role,
			Text:        message.Text,
			StartedAt:   message.StartedAt,
			CompletedAt: message.CompletedAt,
		})
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
