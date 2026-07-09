package model

// ═══════════════════════════════════════════════════════════════════
// Shared Data Types
// ═══════════════════════════════════════════════════════════════════

// SessionInfo is the wire format for session list responses.
// Lightweight pointer — actual message/tool history is read from native stores on demand.
type SessionInfo struct {
	SessionID      string   `json:"sessionId"`
	AgentType      string   `json:"agentType"` // which native store to query: "hermes" | "claude" | "codex"
	NativeID       string   `json:"nativeId"`  // Hermes response_id / Claude UUID / Codex rollout ID
	Name           string   `json:"name,omitempty"`
	ProjectID      string   `json:"projectId,omitempty"`
	LastActive     string   `json:"lastActive"` // ISO 8601
	Preview        string   `json:"preview,omitempty"`
	WorkspaceRoots []string `json:"workspaceRoots,omitempty"`
	Source         string   `json:"source,omitempty"`

	// Live status from Hermes API (not persisted, polled on demand)
	Status      string `json:"status,omitempty"`      // "idle" | "running" | "completed"
	CurrentTask string `json:"currentTask,omitempty"` // description of current tool/task
	TokenUsage  int64  `json:"tokenUsage,omitempty"`  // total tokens consumed
	UpdatedAt   string `json:"updatedAt,omitempty"`   // last activity timestamp

	Messages []SessionMessage `json:"messages,omitempty"`
}

type SessionMessage struct {
	SessionID   string `json:"sessionId"`
	Seq         int64  `json:"seq"`
	Kind        string `json:"kind"`
	Role        string `json:"role,omitempty"`
	Text        string `json:"text,omitempty"`
	StartedAt   string `json:"startedAt,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
}

// HistoryMessage is a single turn in session history.
type HistoryMessage struct {
	Role    string `json:"role"` // "user" | "assistant" | "tool" | "diff"
	Content string `json:"content"`
}

// FileInfo describes a file or directory.
type FileInfo struct {
	IsDir   bool   `json:"isDir"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
}

// Project binds an agent to a workspace directory.
type Project struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RootPath string `json:"rootPath"`
}

// FileChange describes a single file modification from a turn.
type FileChange struct {
	Path       string `json:"path"`
	Tool       string `json:"tool"`       // "write_file" | "patch"
	OldContent string `json:"oldContent"` // empty for write_file new files
	NewContent string `json:"newContent"` // full new content or patched snippet
}

// AgentInfo describes a connected agent for the project list.
type AgentInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Hostname  string    `json:"hostname,omitempty"`
	AgentType string    `json:"agentType"`
	Projects  []Project `json:"projects"`
	Online    bool      `json:"online"`
}

// CapabilityDecl tells the frontend which features this agent backend supports.
// The frontend uses this to show/hide UI controls without hardcoding per-agent logic.
type CapabilityDecl struct {
	Streaming        bool   `json:"streaming"`        // emits message.delta incrementally
	MultiTurn        bool   `json:"multiTurn"`        // supports session.resume
	Cancel           bool   `json:"cancel"`           // supports turn.cancel
	SideBranch       bool   `json:"sideBranch"`       // supports session fork
	Reasoning        string `json:"reasoning"`        // "none" | "plain" | "encrypted"
	ToolApproval     bool   `json:"toolApproval"`     // emits tool.approval_required
	Subagents        bool   `json:"subagents"`        // supports delegate_task
	FileTracking     bool   `json:"fileTracking"`     // emits file.changed at turn end
	StructuredOutput bool   `json:"structuredOutput"` // supports JSON Schema constrained output
	MaxContextTokens int    `json:"maxContextTokens"`
}

// UsageInfo carries token counts and cost for a turn or session.
type UsageInfo struct {
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens,omitempty"`
	CacheCreationTokens int     `json:"cacheCreationTokens,omitempty"`
	ReasoningTokens     int     `json:"reasoningTokens,omitempty"`
	TotalTokens         int     `json:"totalTokens"`
	CostUSD             float64 `json:"costUsd,omitempty"`
}
