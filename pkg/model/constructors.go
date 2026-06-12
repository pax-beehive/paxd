package model

// ═══════════════════════════════════════════════════════════════════
// Constructor helpers — ensure EntityType/EventType are always set.
// ═══════════════════════════════════════════════════════════════════

func NewTurnStarted(turnID string) *TurnStarted {
	return &TurnStarted{
		TurnBase: TurnBase{
			SessionBase: SessionBase{
				Envelope: Envelope{EntityType: "turn", EventType: "started"},
			},
			TurnID: turnID,
		},
	}
}

func NewTurnDone(turnID, responseID, status string) *TurnDone {
	return &TurnDone{
		TurnBase: TurnBase{
			SessionBase: SessionBase{
				Envelope: Envelope{EntityType: "turn", EventType: "done"},
			},
			TurnID: turnID,
		},
		ResponseID: responseID,
		Status:     status,
	}
}

func NewAgentStatus(turnID, status, label, icon, detail string) *AgentStatus {
	return &AgentStatus{
		SessionBase: SessionBase{
			Envelope: Envelope{EntityType: "agent", EventType: "status"},
		},
		TurnID: turnID,
		Status: status,
		Label:  label,
		Icon:   icon,
		Detail: detail,
	}
}

func NewMessageDelta(turnID, role, content string) *MessageDelta {
	return &MessageDelta{
		TurnBase: TurnBase{
			SessionBase: SessionBase{
				Envelope: Envelope{EntityType: "message", EventType: "delta"},
			},
			TurnID: turnID,
		},
		Role:    role,
		Content: content,
	}
}

func NewToolCall(turnID, callID, name, args string) *ToolCall {
	return &ToolCall{
		TurnBase: TurnBase{
			SessionBase: SessionBase{
				Envelope: Envelope{EntityType: "tool", EventType: "call"},
			},
			TurnID: turnID,
		},
		CallID:    callID,
		Name:      name,
		Arguments: args,
	}
}

func NewToolResult(turnID, callID, output string) *ToolResult {
	return &ToolResult{
		TurnBase: TurnBase{
			SessionBase: SessionBase{
				Envelope: Envelope{EntityType: "tool", EventType: "result"},
			},
			TurnID: turnID,
		},
		CallID: callID,
		Output: output,
	}
}

func NewFileChanged(turnID string, changes []*FileChange) *FileChanged {
	return &FileChanged{
		TurnBase: TurnBase{
			SessionBase: SessionBase{
				Envelope: Envelope{EntityType: "file", EventType: "changed"},
			},
			TurnID: turnID,
		},
		Changes: changes,
	}
}

func NewSessionCreated(sessionID, agentType string, caps *CapabilityDecl, cwd, model string, roots []string) *SessionCreated {
	return &SessionCreated{
		SessionBase: SessionBase{
			Envelope:   Envelope{EntityType: "session", EventType: "created"},
			SessionID:  sessionID,
		},
		AgentType:    agentType,
		Capabilities: caps,
		CWD:          cwd,
		Model:        model,
		Roots:        roots,
	}
}
