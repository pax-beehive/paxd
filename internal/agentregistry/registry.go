package agentregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/acpclient"
	"github.com/pax-beehive/paxd/pkg/model"
)

// Agent describes a local ACP session source.
type Agent struct {
	Name            string
	Aliases         []string
	Kind            string
	Command         []string
	FallbackCommand []string
	InstallCommands [][]string
	AppNames        []string
	Source          string
	InstallHint     string
}

// Status describes whether an agent source can be used on this machine.
type Status struct {
	Agent      Agent
	Available  bool
	Command    []string
	State      string
	Capability string
	Reason     string
}

// Registry contains built-in local ACP sources.
type Registry struct {
	agents []Agent
}

// Default returns the built-in ACP source registry.
func Default() Registry {
	return Registry{agents: []Agent{
		{
			Name:            "codex",
			Kind:            "local",
			Command:         []string{"~/.codex/sessions"},
			FallbackCommand: []string{"npx", "-y", "@zed-industries/codex-acp"},
			InstallCommands: [][]string{{"npm", "install", "-g", "@zed-industries/codex-acp"}},
			Source:          "codex",
			InstallHint:     "run Codex locally so ~/.codex/sessions exists",
		},
		{
			Name:            "claude",
			Kind:            "acp",
			Command:         []string{"claude-agent-acp"},
			FallbackCommand: []string{"npx", "-y", "@agentclientprotocol/claude-agent-acp"},
			InstallCommands: [][]string{{"npm", "install", "-g", "@agentclientprotocol/claude-agent-acp"}},
			Source:          "official",
			InstallHint:     "install claude-agent-acp; npx fallback is not run automatically by sync",
		},
		{
			Name:        "gemini",
			Kind:        "acp",
			Command:     []string{"gemini", "--acp"},
			Source:      "native",
			InstallHint: "install gemini with ACP support",
		},
		{
			Name:            "pi",
			Kind:            "acp",
			Command:         []string{"pi-acp"},
			FallbackCommand: []string{"npx", "-y", "pi-acp"},
			InstallCommands: [][]string{{"npm", "install", "-g", "pi-acp", "@earendil-works/pi-coding-agent"}},
			Source:          "community",
			InstallHint:     "install pi-acp and @earendil-works/pi-coding-agent; npx fallback is not run automatically by sync",
		},
		{
			Name:            "qwen",
			Aliases:         []string{"qwen-code", "qwen_code"},
			Kind:            "local",
			Command:         []string{"~/.qwen/projects"},
			FallbackCommand: []string{"npx", "-y", "@qwen-code/qwen-code", "--acp"},
			InstallCommands: [][]string{{"npm", "install", "-g", "@qwen-code/qwen-code"}},
			Source:          "official",
			InstallHint:     "run Qwen Code locally so ~/.qwen/projects contains chat logs",
		},
		{
			Name:        "zcode",
			Aliases:     []string{"zai-code", "zai_code", "z-ai-code", "z.ai-code"},
			Kind:        "app",
			AppNames:    []string{"ZCode.app", "Z.ai Code.app", "Zai Code.app"},
			Source:      "app",
			InstallHint: "install the Z.ai Code desktop app; local session sync needs an ACP or log adapter",
		},
		{
			Name:        "openclaw",
			Kind:        "gateway",
			Command:     []string{"openclaw"},
			Source:      "gateway",
			InstallHint: "install openclaw and run openclaw gateway status --require-rpc",
		},
		{
			Name:        "hermes",
			Kind:        "acp",
			Command:     []string{"hermes", "acp"},
			Source:      "native",
			InstallHint: "install hermes with ACP support",
		},
	}}
}

func (r Registry) Agents(names []string) ([]Agent, error) {
	if len(names) == 0 {
		return append([]Agent(nil), r.agents...), nil
	}
	byName := make(map[string]Agent, len(r.agents))
	for _, agent := range r.agents {
		byName[agent.Name] = agent
		for _, alias := range agent.Aliases {
			byName[alias] = agent
		}
	}
	out := make([]Agent, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		agent, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unsupported agent %q", name)
		}
		out = append(out, agent)
	}
	return out, nil
}

func (r Registry) Statuses(names []string) ([]Status, error) {
	return r.StatusesWithProbe(names, true)
}

func (r Registry) StatusesWithProbe(names []string, probe bool) ([]Status, error) {
	agents, err := r.Agents(names)
	if err != nil {
		return nil, err
	}
	statuses := make([]Status, 0, len(agents))
	for _, agent := range agents {
		statuses = append(statuses, DetectWithProbe(agent, probe))
	}
	return statuses, nil
}

// Detect resolves the command used for an ACP source.
func Detect(agent Agent) Status {
	return DetectWithProbe(agent, true)
}

// DetectWithProbe resolves a source. Gateway probes may start external CLIs, so
// callers that only need inventory can disable them for a fast path.
func DetectWithProbe(agent Agent, probe bool) Status {
	if agent.Kind == "gateway" && agent.Name == "openclaw" {
		return detectOpenClaw(agent, probe)
	}
	if agent.Kind == "app" {
		return detectApp(agent)
	}
	if agent.Kind == "local" && agent.Name == "codex" {
		if codexLocalAvailable() {
			return Status{Agent: agent, Available: true, Command: agent.Command, State: "available", Capability: "local-log"}
		}
		return Status{Agent: agent, State: "missing", Reason: agent.InstallHint}
	}
	if agent.Kind == "local" && agent.Name == "qwen" {
		if qwenLocalAvailable() {
			return Status{Agent: agent, Available: true, Command: agent.Command, State: "available", Capability: "local-log"}
		}
		return Status{Agent: agent, State: "missing", Reason: agent.InstallHint}
	}
	if commandAvailable(agent.Command) {
		return Status{Agent: agent, Available: true, Command: agent.Command, State: "available", Capability: "acp"}
	}
	if len(agent.FallbackCommand) > 0 && commandAvailable(agent.FallbackCommand) {
		return Status{
			Agent:      agent,
			Command:    agent.FallbackCommand,
			State:      "installable",
			Capability: "acp",
			Reason:     firstNonEmpty(agent.InstallHint, "adapter command unavailable"),
		}
	}
	reason := agent.InstallHint
	if reason == "" {
		reason = "command unavailable"
	}
	return Status{Agent: agent, State: "missing", Reason: reason}
}

func commandAvailable(command []string) bool {
	if len(command) == 0 || command[0] == "" {
		return false
	}
	_, err := exec.LookPath(command[0])
	return err == nil
}

func detectApp(agent Agent) Status {
	path := findMacApp(agent.AppNames)
	if path == "" {
		return Status{Agent: agent, State: "missing", Capability: "app", Reason: agent.InstallHint}
	}
	return Status{
		Agent:      agent,
		Available:  true,
		Command:    []string{path},
		State:      "installed",
		Capability: "app",
		Reason:     "app is installed, but no ACP or local-log session adapter is available yet",
	}
}

func findMacApp(names []string) string {
	for _, name := range names {
		if filepath.IsAbs(name) {
			info, err := os.Stat(name)
			if err == nil && info.IsDir() {
				return name
			}
			continue
		}
		for _, root := range appSearchRoots() {
			path := filepath.Join(root, name)
			info, err := os.Stat(path)
			if err == nil && info.IsDir() {
				return path
			}
		}
	}
	return ""
}

func appSearchRoots() []string {
	roots := []string{"/Applications"}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, "Applications"))
	}
	return roots
}

// ListSessions asks an available ACP source for lightweight session metadata.
func ListSessions(ctx context.Context, status Status, timeout time.Duration) ([]model.SessionInfo, error) {
	if !status.Available {
		return nil, errors.New(status.Reason)
	}
	if status.Agent.Kind == "local" && status.Agent.Name == "codex" {
		return listCodexLocalSessions()
	}
	if status.Agent.Kind == "local" && status.Agent.Name == "qwen" {
		return listQwenLocalSessions()
	}
	if status.Agent.Kind == "gateway" && status.Agent.Name == "openclaw" {
		return listOpenClawSessions(ctx, status, timeout)
	}
	if status.Agent.Kind == "app" {
		return nil, fmt.Errorf("%s is app-only: no ACP or local-log session adapter is available", status.Agent.Name)
	}
	lister := acpclient.SessionLister{Command: status.Command, Timeout: timeout}
	sessions, err := lister.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		if sessions[i].AgentType == "" {
			sessions[i].AgentType = status.Agent.Name
		}
		if sessions[i].NativeID == "" {
			sessions[i].NativeID = sessions[i].SessionID
		}
		sessions[i].SessionID = CanonicalSessionID(status.Agent.Name, sessions[i].NativeID)
	}
	return sessions, nil
}

// SteerSession sends a system handoff prompt to an existing local ACP session.
func SteerSession(ctx context.Context, agentName string, nativeSessionID string, text string, timeout time.Duration) error {
	agents, err := Default().Agents([]string{agentName})
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		return fmt.Errorf("unsupported agent %q", agentName)
	}
	agent := agents[0]
	command, err := steerCommand(agent)
	if err != nil {
		return err
	}
	prompter := acpclient.SessionPrompter{Command: command, Timeout: timeout}
	if err := prompter.Prompt(ctx, nativeSessionID, text); err != nil {
		return fmt.Errorf("steer %s session %s: %w", agent.Name, nativeSessionID, err)
	}
	return nil
}

func steerCommand(agent Agent) ([]string, error) {
	if agent.Kind == "acp" && commandAvailable(agent.Command) {
		return agent.Command, nil
	}
	if len(agent.FallbackCommand) > 0 && commandAvailable(agent.FallbackCommand) {
		return agent.FallbackCommand, nil
	}
	if agent.Kind == "local" {
		return nil, fmt.Errorf("%s has local session logs but no ACP adapter for injection: %s", agent.Name, agent.InstallHint)
	}
	if agent.Kind == "app" {
		return nil, fmt.Errorf("%s is app-only: no ACP adapter is available for injection", agent.Name)
	}
	if agent.Kind == "gateway" {
		return nil, fmt.Errorf("%s gateway injection is not implemented yet", agent.Name)
	}
	return nil, fmt.Errorf("%s ACP command is unavailable: %s", agent.Name, firstNonEmpty(agent.InstallHint, "install adapter command"))
}

func CanonicalSessionID(agent, nativeID string) string {
	if strings.Contains(nativeID, ":") {
		return nativeID
	}
	return agent + ":" + nativeID
}

type openClawStatus struct {
	OK         bool   `json:"ok"`
	Capability string `json:"capability"`
	Warnings   []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"warnings"`
}

func detectOpenClaw(agent Agent, probe bool) Status {
	if !commandAvailable(agent.Command) {
		return Status{Agent: agent, State: "missing", Reason: agent.InstallHint}
	}
	if !probe {
		return Status{
			Agent:      agent,
			Command:    agent.Command,
			State:      "installed",
			Capability: "gateway",
			Reason:     "run paxctl agents list --probe to check gateway reachability",
		}
	}
	if status, err := openClawGatewayStatus(true); err == nil && status.OK {
		return Status{
			Agent:      agent,
			Available:  true,
			Command:    agent.Command,
			State:      "available",
			Capability: firstNonEmpty(status.Capability, "read_rpc"),
		}
	}
	status, err := openClawGatewayStatus(false)
	if err == nil && status.OK {
		return Status{
			Agent:      agent,
			Command:    agent.Command,
			State:      "degraded",
			Capability: firstNonEmpty(status.Capability, "connect"),
			Reason:     openClawWarningReason(status, "gateway reachable but read RPC is unavailable"),
		}
	}
	reason := "openclaw is installed but no readable gateway is reachable"
	if err != nil {
		reason = err.Error()
	}
	return Status{Agent: agent, Command: agent.Command, State: "installed", Reason: reason}
}

func openClawGatewayStatus(requireRPC bool) (openClawStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	args := []string{"gateway", "status", "--json", "--timeout", "3000"}
	if requireRPC {
		args = append(args, "--require-rpc")
	}
	out, err := exec.CommandContext(ctx, "openclaw", args...).Output()
	if err != nil {
		return openClawStatus{}, err
	}
	var status openClawStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return openClawStatus{}, err
	}
	return status, nil
}

func openClawWarningReason(status openClawStatus, fallback string) string {
	for _, warning := range status.Warnings {
		if warning.Message != "" {
			return warning.Message
		}
	}
	return fallback
}

type openClawSessionsResult struct {
	Sessions []json.RawMessage `json:"sessions"`
}

func listOpenClawSessions(ctx context.Context, status Status, timeout time.Duration) ([]model.SessionInfo, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, status.Command[0], "sessions", "--all-agents", "--json").Output()
	if err != nil {
		return nil, err
	}
	var result openClawSessionsResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, err
	}
	sessions := make([]model.SessionInfo, 0, len(result.Sessions))
	for _, raw := range result.Sessions {
		session := decodeOpenClawSession(raw)
		if session.SessionID != "" {
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

func decodeOpenClawSession(raw json.RawMessage) model.SessionInfo {
	var typed struct {
		Key            string   `json:"key"`
		SessionKey     string   `json:"sessionKey"`
		ID             string   `json:"id"`
		AgentID        string   `json:"agentId"`
		Label          string   `json:"label"`
		Title          string   `json:"title"`
		Name           string   `json:"name"`
		Model          string   `json:"model"`
		Status         string   `json:"status"`
		Preview        string   `json:"preview"`
		Cwd            string   `json:"cwd"`
		Workspace      string   `json:"workspace"`
		WorkspaceRoots []string `json:"workspaceRoots"`
		UpdatedAt      string   `json:"updatedAt"`
		LastActive     string   `json:"lastActive"`
		CreatedAt      string   `json:"createdAt"`
	}
	_ = json.Unmarshal(raw, &typed)
	nativeID := firstNonEmpty(typed.Key, typed.SessionKey, typed.ID)
	if nativeID == "" {
		return model.SessionInfo{}
	}
	roots := typed.WorkspaceRoots
	if len(roots) == 0 && firstNonEmpty(typed.Cwd, typed.Workspace) != "" {
		roots = []string{firstNonEmpty(typed.Cwd, typed.Workspace)}
	}
	return model.SessionInfo{
		SessionID:      "openclaw:" + strings.TrimPrefix(nativeID, "openclaw:"),
		AgentType:      "openclaw",
		NativeID:       nativeID,
		Name:           firstNonEmpty(typed.Title, typed.Label, typed.Name, nativeID),
		ProjectID:      firstNonEmpty(typed.Cwd, typed.Workspace, typed.AgentID),
		LastActive:     firstNonEmpty(typed.LastActive, typed.UpdatedAt, typed.CreatedAt),
		Preview:        firstNonEmpty(typed.Preview, typed.Model),
		WorkspaceRoots: roots,
		Status:         typed.Status,
		UpdatedAt:      firstNonEmpty(typed.UpdatedAt, typed.LastActive, typed.CreatedAt),
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
