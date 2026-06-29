package sessionreporter

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/acpclient"
	"github.com/pax-beehive/paxd/internal/agentregistry"
	"github.com/pax-beehive/paxd/pkg/model"
)

type DefaultScanner struct {
	Timeout time.Duration
}

func (s DefaultScanner) ListSessions(
	ctx context.Context,
	spec SessionScannerSpec,
) ([]model.SessionInfo, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = defaultScanTimeout
	}
	var errs []error
	if len(spec.Command) > 0 {
		sessions, err := acpclient.SessionLister{
			Command:    spec.Command,
			WorkingDir: spec.WorkingDir,
			Env:        spec.Env,
			Timeout:    timeout,
		}.List(ctx)
		if err == nil {
			if isHermesSpec(spec) {
				return mergeHermesLocalSessions(ctx, spec, sessions)
			}
			return sessions, nil
		}
		errs = append(errs, err)
	}
	if isHermesSpec(spec) {
		sessions, err := agentregistry.ListHermesLocalSessions(ctx, spec.Command, 0)
		if err == nil {
			return sessions, nil
		}
		errs = append(errs, err)
	}

	status, err := fallbackStatus(spec)
	if err != nil {
		errs = append(errs, err)
		return nil, joinErrors(errs)
	}
	sessions, err := agentregistry.ListSessions(ctx, status, timeout)
	if err == nil {
		return sessions, nil
	}
	errs = append(errs, err)
	return nil, joinErrors(errs)
}

func isHermesSpec(spec SessionScannerSpec) bool {
	name := strings.ToLower(strings.TrimSpace(firstNonEmpty(spec.Harness, spec.AgentType)))
	if name == "hermes" {
		return true
	}
	if len(spec.Command) == 0 {
		return false
	}
	commandName := filepath.Base(spec.Command[0])
	if commandName != "hermes" {
		return false
	}
	for _, arg := range spec.Command[1:] {
		if arg == "acp" {
			return true
		}
	}
	return false
}

func mergeHermesLocalSessions(
	ctx context.Context,
	spec SessionScannerSpec,
	acpSessions []model.SessionInfo,
) ([]model.SessionInfo, error) {
	localSessions, err := agentregistry.ListHermesLocalSessions(ctx, spec.Command, 0)
	if err != nil {
		return normalizeHermesSessions(acpSessions), nil
	}
	return mergeSessionInfos(localSessions, normalizeHermesSessions(acpSessions)), nil
}

func normalizeHermesSessions(sessions []model.SessionInfo) []model.SessionInfo {
	out := make([]model.SessionInfo, 0, len(sessions))
	for _, session := range sessions {
		if session.SessionID == "" && session.NativeID == "" {
			continue
		}
		session.AgentType = firstNonEmpty(session.AgentType, "hermes")
		if session.NativeID == "" {
			session.NativeID = strings.TrimPrefix(session.SessionID, "hermes:")
		}
		session.SessionID = agentregistry.CanonicalSessionID("hermes", session.NativeID)
		out = append(out, session)
	}
	return out
}

func mergeSessionInfos(base []model.SessionInfo, overlays []model.SessionInfo) []model.SessionInfo {
	byKey := make(map[string]int, len(base)+len(overlays))
	out := make([]model.SessionInfo, 0, len(base)+len(overlays))
	for _, session := range base {
		key := sessionMergeKey(session)
		if key == "" {
			continue
		}
		byKey[key] = len(out)
		out = append(out, session)
	}
	for _, session := range overlays {
		key := sessionMergeKey(session)
		if key == "" {
			continue
		}
		if index, ok := byKey[key]; ok {
			out[index] = mergeSessionInfo(out[index], session)
			continue
		}
		byKey[key] = len(out)
		out = append(out, session)
	}
	return out
}

func sessionMergeKey(session model.SessionInfo) string {
	if session.NativeID != "" {
		return strings.ToLower(session.AgentType + ":" + strings.TrimPrefix(session.NativeID, session.AgentType+":"))
	}
	if session.SessionID != "" {
		return strings.ToLower(session.SessionID)
	}
	return ""
}

func mergeSessionInfo(base model.SessionInfo, overlay model.SessionInfo) model.SessionInfo {
	base.SessionID = firstNonEmpty(overlay.SessionID, base.SessionID)
	base.AgentType = firstNonEmpty(overlay.AgentType, base.AgentType)
	base.NativeID = firstNonEmpty(overlay.NativeID, base.NativeID)
	base.Name = firstNonEmpty(overlay.Name, base.Name)
	base.ProjectID = firstNonEmpty(overlay.ProjectID, base.ProjectID)
	base.LastActive = firstNonEmpty(overlay.LastActive, base.LastActive)
	base.Preview = firstNonEmpty(overlay.Preview, base.Preview)
	if len(base.WorkspaceRoots) == 0 {
		base.WorkspaceRoots = append([]string(nil), overlay.WorkspaceRoots...)
	}
	base.Source = firstNonEmpty(overlay.Source, base.Source)
	base.Status = firstNonEmpty(overlay.Status, base.Status)
	base.CurrentTask = firstNonEmpty(overlay.CurrentTask, base.CurrentTask)
	if overlay.TokenUsage > 0 {
		base.TokenUsage = overlay.TokenUsage
	}
	base.UpdatedAt = firstNonEmpty(overlay.UpdatedAt, base.UpdatedAt)
	return base
}

func fallbackStatus(spec SessionScannerSpec) (agentregistry.Status, error) {
	name := firstNonEmpty(spec.Harness, spec.AgentType)
	if strings.TrimSpace(name) == "" {
		return agentregistry.Status{}, fmt.Errorf("missing harness or agent type")
	}
	agents, err := agentregistry.Default().Agents([]string{name})
	if err != nil {
		return agentregistry.Status{}, err
	}
	if len(agents) == 0 {
		return agentregistry.Status{}, fmt.Errorf("unsupported agent %q", name)
	}
	agent := agents[0]
	command := append([]string(nil), spec.Command...)
	if len(command) == 0 {
		command = append([]string(nil), agent.Command...)
	}
	return agentregistry.Status{
		Agent:     agent,
		Available: true,
		Command:   command,
	}, nil
}

func joinErrors(errs []error) error {
	var out error
	for _, err := range errs {
		if err == nil {
			continue
		}
		if out == nil {
			out = err
			continue
		}
		out = fmt.Errorf("%v; %w", out, err)
	}
	return out
}
