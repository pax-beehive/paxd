package sessionreporter

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/acpclient"
	"github.com/pax-beehive/paxd/internal/agentregistry"
	"github.com/pax-beehive/paxd/internal/paxlclient"
	"github.com/pax-beehive/paxd/internal/paxlinstall"
	"github.com/pax-beehive/paxd/pkg/model"
)

type DefaultScanner struct {
	Timeout     time.Duration
	PaxlCommand []string
}

func (s DefaultScanner) ListSessions(
	ctx context.Context,
	spec SessionScannerSpec,
) ([]model.SessionInfo, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = defaultScanTimeout
	}
	if sessions, err := s.listPaxlSessions(ctx, spec); err == nil {
		return sessions, nil
	}
	var errs []error
	if isCodexSpec(spec) {
		// Codex sessions are local rollout logs. Starting codex-acp from the
		// daemon can trigger browser OAuth with a short-lived localhost callback.
		return listCodexLocalSessions(ctx, timeout)
	}
	if isGeminiSpec(spec) {
		sessions, err := agentregistry.ListGeminiLocalSessions(ctx, spec.Limit)
		if err == nil && len(sessions) > 0 {
			return sessions, nil
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(spec.Command) > 0 {
		sessions, err := acpclient.SessionLister{
			Command:    spec.Command,
			WorkingDir: spec.WorkingDir,
			Env:        spec.Env,
			Timeout:    timeout,
		}.List(ctx)
		if err == nil {
			if harness := strings.ToLower(firstNonEmpty(spec.Harness, spec.AgentType)); harness == "dsh" || harness == "dscode" {
				for i := range sessions {
					sessions[i].AgentType = harness
					if sessions[i].NativeID == "" {
						sessions[i].NativeID = strings.TrimPrefix(sessions[i].SessionID, harness+":")
					}
					sessions[i].SessionID = agentregistry.CanonicalSessionID(harness, sessions[i].NativeID)
				}
				return limitSessions(sessions, spec.Limit), nil
			}
			if isHermesSpec(spec) {
				return mergeHermesLocalSessions(ctx, spec, sessions)
			}
			if isKimiSpec(spec) {
				return limitSessions(normalizeKimiSessions(sessions), spec.Limit), nil
			}
			if isPiSpec(spec) {
				return limitSessions(normalizePiSessions(sessions), spec.Limit), nil
			}
			return limitSessions(sessions, spec.Limit), nil
		}
		errs = append(errs, err)
	}
	if isHermesSpec(spec) {
		sessions, err := agentregistry.ListHermesLocalSessions(ctx, spec.Command, spec.Limit)
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
		return limitSessions(sessions, spec.Limit), nil
	}
	errs = append(errs, err)
	return nil, joinErrors(errs)
}

func isCodexSpec(spec SessionScannerSpec) bool {
	name := strings.ToLower(strings.TrimSpace(firstNonEmpty(spec.Harness, spec.AgentType)))
	if name == "codex" {
		return true
	}
	if len(spec.Command) == 0 {
		return false
	}
	commandName := filepath.Base(spec.Command[0])
	if commandName == "codex" || commandName == "codex-acp" {
		return true
	}
	return strings.Contains(strings.Join(spec.Command, " "), "@agentclientprotocol/codex-acp")
}

func listCodexLocalSessions(ctx context.Context, timeout time.Duration) ([]model.SessionInfo, error) {
	status, err := fallbackStatus(SessionScannerSpec{Harness: "codex"})
	if err != nil {
		return nil, err
	}
	return agentregistry.ListSessions(ctx, status, timeout)
}

func (s DefaultScanner) listPaxlSessions(
	ctx context.Context,
	spec SessionScannerSpec,
) ([]model.SessionInfo, error) {
	command, ok := resolvePaxlCommand(s.PaxlCommand)
	if !ok {
		return nil, fmt.Errorf("paxl command is unavailable")
	}
	client := paxlclient.Client{Command: command}
	agent := strings.ToLower(strings.TrimSpace(firstNonEmpty(spec.Harness, spec.AgentType)))
	if agent == "dsh" || agent == "dscode" {
		client.WorkingDir = spec.WorkingDir
		client.Env = map[string]string{}
		// Forward only storage location overrides, never model credentials.
		keys := []string{"DSH_HOME", "PAXL_DSH_SESSIONS_DIR"}
		if agent == "dscode" {
			keys = []string{"DSCODE_HOME", "PAXL_DSCODE_SESSIONS_DIR"}
		}
		for _, key := range keys {
			if value, ok := spec.Env[key]; ok {
				client.Env[key] = value
			}
		}
	}
	sessions, err := client.ListSessions(ctx, agent, spec.Limit)
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		messages, err := client.GetSessionMessages(ctx, sessions[i].SessionID, sessions[i].AgentType)
		if err != nil {
			continue
		}
		sessions[i].Messages = messages
	}
	return sessions, nil
}

func resolvePaxlCommand(command []string) ([]string, bool) {
	resolved, err := paxlinstall.ResolveCommand(command)
	return resolved, err == nil
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

func isGeminiSpec(spec SessionScannerSpec) bool {
	name := strings.ToLower(strings.TrimSpace(firstNonEmpty(spec.Harness, spec.AgentType)))
	if name == "gemini" {
		return true
	}
	if len(spec.Command) == 0 {
		return false
	}
	return filepath.Base(spec.Command[0]) == "gemini"
}

func isKimiSpec(spec SessionScannerSpec) bool {
	name := strings.ToLower(strings.TrimSpace(firstNonEmpty(spec.Harness, spec.AgentType)))
	if name == "kimi" || name == "kimi-code" || name == "kimi_code" {
		return true
	}
	if len(spec.Command) == 0 {
		return false
	}
	return filepath.Base(spec.Command[0]) == "kimi"
}

func isPiSpec(spec SessionScannerSpec) bool {
	name := strings.ToLower(strings.TrimSpace(firstNonEmpty(spec.Harness, spec.AgentType)))
	if name == "pi" || name == "pi-agent" || name == "pi_agent" {
		return true
	}
	if len(spec.Command) == 0 {
		return false
	}
	return filepath.Base(spec.Command[0]) == "pi-acp"
}

// normalizePiSessions canonicalizes ACP-listed pi sessions so they report the
// same identity as other pi session sources.
func normalizePiSessions(sessions []model.SessionInfo) []model.SessionInfo {
	out := make([]model.SessionInfo, 0, len(sessions))
	for _, session := range sessions {
		if session.SessionID == "" && session.NativeID == "" {
			continue
		}
		session.AgentType = firstNonEmpty(session.AgentType, "pi")
		if session.NativeID == "" {
			session.NativeID = strings.TrimPrefix(session.SessionID, "pi:")
		}
		session.SessionID = agentregistry.CanonicalSessionID("pi", session.NativeID)
		out = append(out, session)
	}
	return out
}

// normalizeKimiSessions canonicalizes ACP-listed Kimi Code sessions so they
// report the same identity as other kimi session sources. Kimi session IDs
// are stable per conversation, so no lineage handling is needed.
func normalizeKimiSessions(sessions []model.SessionInfo) []model.SessionInfo {
	out := make([]model.SessionInfo, 0, len(sessions))
	for _, session := range sessions {
		if session.SessionID == "" && session.NativeID == "" {
			continue
		}
		session.AgentType = firstNonEmpty(session.AgentType, "kimi")
		if session.NativeID == "" {
			session.NativeID = strings.TrimPrefix(session.SessionID, "kimi:")
		}
		session.SessionID = agentregistry.CanonicalSessionID("kimi", session.NativeID)
		out = append(out, session)
	}
	return out
}

func mergeHermesLocalSessions(
	ctx context.Context,
	spec SessionScannerSpec,
	acpSessions []model.SessionInfo,
) ([]model.SessionInfo, error) {
	localSessions, err := agentregistry.ListHermesLocalSessions(ctx, spec.Command, spec.Limit)
	if err != nil {
		return limitSessions(normalizeHermesSessions(acpSessions), spec.Limit), nil
	}
	return limitSessions(mergeSessionInfos(localSessions, normalizeHermesSessions(acpSessions)), spec.Limit), nil
}

func limitSessions(sessions []model.SessionInfo, limit int) []model.SessionInfo {
	if limit <= 0 || len(sessions) <= limit {
		return sessions
	}
	return sessions[:limit]
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
