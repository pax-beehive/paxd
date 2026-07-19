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
	if isGeminiSpec(spec) {
		sessions, err := agentregistry.ListGeminiLocalSessions(ctx, 0)
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
			if isHermesSpec(spec) {
				return mergeHermesLocalSessions(ctx, spec, sessions)
			}
			if isCodexSpec(spec) {
				return normalizeCodexSessions(sessions), nil
			}
			if isKimiSpec(spec) {
				return normalizeKimiSessions(sessions), nil
			}
			if isPiSpec(spec) {
				return normalizePiSessions(sessions), nil
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

func isCodexSpec(spec SessionScannerSpec) bool {
	name := strings.ToLower(strings.TrimSpace(firstNonEmpty(spec.Harness, spec.AgentType)))
	if name == "codex" {
		return true
	}
	if len(spec.Command) == 0 {
		return false
	}
	base := filepath.Base(spec.Command[0])
	return base == "codex-acp" || base == "codex"
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

// normalizeCodexSessions rewrites ACP-listed codex sessions onto stable
// conversation identities. Codex mints a new thread ID when it forks a
// conversation (for example when resuming after an auto compact); without a
// stable native ID the cloud stores each fork as a separate session with the
// same title. Sessions sharing a conversation root collapse into the most
// recently updated entry, named by the root thread's own entry when present.
func normalizeCodexSessions(sessions []model.SessionInfo) []model.SessionInfo {
	roots, err := agentregistry.CodexSessionRoots()
	if err != nil {
		roots = nil
	}
	type codexSessionEntry struct {
		session  model.SessionInfo
		selfRoot bool
	}
	byRoot := make(map[string]int, len(sessions))
	out := make([]codexSessionEntry, 0, len(sessions))
	for _, session := range sessions {
		if session.SessionID == "" && session.NativeID == "" {
			continue
		}
		session.AgentType = firstNonEmpty(session.AgentType, "codex")
		nativeID := session.NativeID
		if nativeID == "" {
			nativeID = strings.TrimPrefix(session.SessionID, "codex:")
		}
		root := nativeID
		if mapped := roots[nativeID]; mapped != "" {
			root = mapped
		}
		selfRoot := root == nativeID
		session.NativeID = root
		session.SessionID = agentregistry.CanonicalSessionID("codex", root)
		index, ok := byRoot[root]
		if !ok {
			byRoot[root] = len(out)
			out = append(out, codexSessionEntry{session: session, selfRoot: selfRoot})
			continue
		}
		entry := out[index]
		// ACP results arrive sorted by recency, so the first entry for a root
		// is the most recently updated one; older duplicates only fill gaps.
		base := entry.session
		base.Name = firstNonEmpty(base.Name, session.Name)
		base.ProjectID = firstNonEmpty(base.ProjectID, session.ProjectID)
		base.LastActive = firstNonEmpty(base.LastActive, session.LastActive)
		base.Preview = firstNonEmpty(base.Preview, session.Preview)
		if len(base.WorkspaceRoots) == 0 {
			base.WorkspaceRoots = append([]string(nil), session.WorkspaceRoots...)
		}
		base.Source = firstNonEmpty(base.Source, session.Source)
		base.Status = firstNonEmpty(base.Status, session.Status)
		base.CurrentTask = firstNonEmpty(base.CurrentTask, session.CurrentTask)
		if base.TokenUsage == 0 {
			base.TokenUsage = session.TokenUsage
		}
		base.UpdatedAt = firstNonEmpty(base.UpdatedAt, session.UpdatedAt)
		// The conversation's own thread provides the display title.
		if session.Name != "" && selfRoot && !entry.selfRoot {
			base.Name = session.Name
			entry.selfRoot = true
		}
		entry.session = base
		out[index] = entry
	}
	result := make([]model.SessionInfo, 0, len(out))
	for _, entry := range out {
		result = append(result, entry.session)
	}
	return result
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
