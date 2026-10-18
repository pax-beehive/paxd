package harnessauth

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
)

func (m *Manager) Status(ctx context.Context, source control.Source, req control.HarnessAuthStatusQuery) (control.HarnessAuthView, error) {
	if err := req.Validate(); err != nil {
		return control.HarnessAuthView{}, err
	}
	if req.SessionID != "" || req.CommandID != "" {
		m.mu.Lock()
		defer m.mu.Unlock()
		s := m.sessions[req.SessionID]
		if req.SessionID == "" {
			for _, candidate := range m.sessions {
				if candidate.owner == (ownerScope{source: source, actor: req.ActorID}) && candidate.view.Harness == req.Harness {
					if _, ok := candidate.commands[req.CommandID]; ok {
						s = candidate
						break
					}
				}
			}
		}
		if s == nil || s.owner != (ownerScope{source: source, actor: req.ActorID}) || s.view.Harness != req.Harness {
			return control.HarnessAuthView{}, authError("not_found", "authentication attempt not found")
		}
		return cloneView(s.view), nil
	}
	command, err := m.resolveCommand()
	if err != nil {
		return control.HarnessAuthView{}, authError("unavailable", "paxl is unavailable")
	}
	if req.Operation == "capabilities" {
		return m.capabilities(ctx, req.Harness, command)
	}
	operation := req.Operation
	if operation == "" {
		operation = "status"
	}
	args := append(command[1:], "harness", "auth", operation, "--harness", req.Harness, "--format", "json")
	if req.Provider != "" {
		args = append(args, "--provider", req.Provider)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], args...)
	prepareLoginCommand(cmd)
	defer func() { _ = stopLoginCommand(cmd) }()
	cmd.WaitDelay = time.Second
	out := &boundedMetadata{}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil || out.overflow {
		return control.HarnessAuthView{}, authError("auth_query_failed", "paxl authentication query failed; check the installed version and provider")
	}
	var result control.HarnessAuthResult
	if err := json.Unmarshal(out.data, &result); err != nil || result.Harness != req.Harness || result.State == "" {
		return control.HarnessAuthView{}, authError("auth_query_failed", "invalid paxl authentication metadata")
	}
	return control.HarnessAuthView{ProtocolVersion: 1, Harness: req.Harness, State: result.State, Result: &result}, nil
}

type boundedMetadata struct {
	data     []byte
	overflow bool
}

func (b *boundedMetadata) Write(data []byte) (int, error) {
	n := len(data)
	if len(b.data)+n > 128*1024 {
		b.overflow = true
		return n, nil
	}
	b.data = append(b.data, data...)
	return n, nil
}

func (m *Manager) capabilities(ctx context.Context, harness string, command []string) (control.HarnessAuthView, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := append(command[1:], "harness", "auth", "capabilities", "--format", "json")
	cmd := exec.CommandContext(ctx, command[0], args...)
	cmd.WaitDelay = time.Second
	output := &boundedMetadata{}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	var metadata struct {
		ProtocolVersion int `json:"protocol_version"`
	}
	if err := cmd.Run(); err != nil || output.overflow {
		return control.HarnessAuthView{}, authError("paxl_auth_unavailable", "update paxl to support the Console authentication protocol")
	}
	if json.Unmarshal(output.data, &metadata) != nil || metadata.ProtocolVersion != 1 {
		return control.HarnessAuthView{}, authError("paxl_auth_unavailable", "update paxl to support the Console authentication protocol")
	}
	return control.HarnessAuthView{ProtocolVersion: 1, Harness: harness, State: "available"}, nil
}
