package control

import (
	"context"
	"strings"
)

// Login commands are ephemeral: authorization codes must never enter the
// durable command journal. Retries are deduplicated by the process owner.
type HarnessAuth interface {
	Login(context.Context, Source, string, HarnessAuthLoginCommand) (HarnessAuthView, error)
	Status(context.Context, Source, HarnessAuthStatusQuery) (HarnessAuthView, error)
}

type HarnessAuthLoginCommand struct {
	Harness     string `json:"harness"`
	Operation   string `json:"operation"`
	Action      string `json:"action,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Profile     string `json:"profile,omitempty"`
	Method      string `json:"method,omitempty"`
	SecretStdin bool   `json:"secret_stdin,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	PromptID    string `json:"prompt_id,omitempty"`
	Code        string `json:"code,omitempty"`
	ActorID     string `json:"actor_id,omitempty"`
}
type HarnessAuthStatusQuery struct {
	CommandID string `json:"command_id,omitempty"`
	Harness   string `json:"harness"`
	SessionID string `json:"session_id,omitempty"`
	Operation string `json:"operation,omitempty"`
	Provider  string `json:"provider,omitempty"`
	ActorID   string `json:"actor_id,omitempty"`
}
type HarnessAuthChoice struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}
type HarnessAuthEvent struct {
	Version int                 `json:"version"`
	Type    string              `json:"type"`
	ID      string              `json:"id,omitempty"`
	Kind    string              `json:"kind,omitempty"`
	Message string              `json:"message,omitempty"`
	URL     string              `json:"url,omitempty"`
	Code    string              `json:"code,omitempty"`
	Options []HarnessAuthChoice `json:"options,omitempty"`
	Result  *HarnessAuthResult  `json:"result,omitempty"`
}
type HarnessAuthProvider struct {
	ID             string   `json:"id"`
	Label          string   `json:"label,omitempty"`
	Methods        []string `json:"methods,omitempty"`
	ProfileID      string   `json:"profile_id,omitempty"`
	CredentialType string   `json:"credential_type,omitempty"`
	Source         string   `json:"source,omitempty"`
}
type HarnessAuthResult struct {
	Harness        string                `json:"harness"`
	State          string                `json:"state"`
	Provider       string                `json:"provider,omitempty"`
	Method         string                `json:"method,omitempty"`
	Providers      []HarnessAuthProvider `json:"providers,omitempty"`
	Message        string                `json:"message,omitempty"`
	RuntimeVersion string                `json:"runtime_version,omitempty"`
	LoggedIn       *bool                 `json:"logged_in,omitempty"`
	AuthMethod     string                `json:"auth_method,omitempty"`
}
type HarnessAuthView struct {
	ProtocolVersion int                `json:"protocol_version"`
	Harness         string             `json:"harness"`
	SessionID       string             `json:"session_id,omitempty"`
	State           string             `json:"state"`
	Method          string             `json:"method,omitempty"`
	ExpiresAt       string             `json:"expires_at,omitempty"`
	ErrorCode       string             `json:"error_code,omitempty"`
	Prompt          *HarnessAuthEvent  `json:"prompt,omitempty"`
	Events          []HarnessAuthEvent `json:"events,omitempty"`
	Result          *HarnessAuthResult `json:"result,omitempty"`
	// Native notice fields remain useful for clients that show a single challenge.
	AuthorizationURL string `json:"authorization_url,omitempty"`
	UserCode         string `json:"user_code,omitempty"`
	LoggedIn         *bool  `json:"logged_in,omitempty"`
	AuthMethod       string `json:"auth_method,omitempty"`
}

func validAuthHarness(h string) bool {
	switch h {
	case "claude", "codex", "kimi", "pi", "gemini", "dsh", "dscode", "opencode", "kiro", "hermes", "openclaw", "antigravity":
		return true
	}
	return false
}
func validAuthSelector(value string) bool {
	return len(value) <= 128 && !strings.HasPrefix(value, "-") && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}
func (c HarnessAuthLoginCommand) Validate() error {
	if !validAuthHarness(c.Harness) {
		return invalid("harness", "unknown authentication harness")
	}
	if !validAuthSelector(c.SessionID) || !validAuthSelector(c.ActorID) {
		return invalid("session_id", "invalid authentication scope")
	}
	switch c.Operation {
	case "start":
		if c.SessionID != "" || c.Code != "" || c.PromptID != "" {
			return invalid("harness_auth_login", "start accepts no handle or input; submit only after a prompt")
		}
		if c.Action != "" && c.Action != "login" && c.Action != "logout" {
			return invalid("action", "expected login or logout")
		}
		if !validAuthSelector(c.Method) || !validAuthSelector(c.Provider) || !validAuthSelector(c.Profile) {
			return invalid("harness_auth_login", "invalid authentication selector")
		}
	case "submit":
		if c.SessionID == "" || !validAuthSelector(c.PromptID) || c.PromptID == "" || !validAuthInput(c.Code) || c.Method != "" || c.Provider != "" || c.Profile != "" || c.SecretStdin || c.Action != "" {
			return invalid("harness_auth_login", "submit requires a session, current prompt id and one input line")
		}
	case "cancel":
		if c.SessionID == "" || c.Code != "" || c.Method != "" || c.PromptID != "" || c.Provider != "" || c.Profile != "" || c.SecretStdin || c.Action != "" {
			return invalid("harness_auth_login", "cancel requires only a session id")
		}
	default:
		return invalid("operation", "expected start, submit or cancel")
	}
	return nil
}
func (q HarnessAuthStatusQuery) Validate() error {
	if !validAuthHarness(q.Harness) || !validAuthSelector(q.SessionID) || !validAuthSelector(q.CommandID) || !validAuthSelector(q.Provider) || !validAuthSelector(q.ActorID) {
		return invalid("harness_auth_status", "invalid harness or authentication selector")
	}
	if q.Operation != "" && q.Operation != "status" && q.Operation != "methods" && q.Operation != "capabilities" {
		return invalid("operation", "expected status, methods or capabilities")
	}
	if (q.SessionID != "" || q.CommandID != "") && (q.Operation == "methods" || q.Operation == "capabilities" || q.Provider != "") {
		return invalid("harness_auth_status", "attempt lookup accepts only its session id")
	}
	return nil
}

func validHarnessAuthSource(src Source) bool {
	return src.Kind == SourceLocal || src.Kind == SourceRemote && strings.TrimSpace(src.RemoteID) != ""
}

func (s *ControlService) handleHarnessAuthLogin(ctx context.Context, src Source, cmd Command) (CommandAck, error) {
	if !validHarnessAuthSource(src) {
		return rejectedAck(cmd.CommandID, "harness_auth", "", invalid("source", "authenticated control source required")), nil
	}
	if s.harnessAuth == nil {
		return failedAck(cmd.CommandID, "harness_auth", "", ControlError{Code: ErrCodeInternal, Message: "harness authentication unavailable"}), nil
	}
	view, err := s.harnessAuth.Login(ctx, src, cmd.CommandID, *cmd.HarnessAuthLogin)
	if err != nil {
		return rejectedAck(cmd.CommandID, "harness_auth", "", controlErr(err)), nil
	}
	return CommandAck{CommandID: cmd.CommandID, OK: true, Status: CommandStatusApplied, TargetType: "harness_auth", TargetID: view.SessionID, Result: &CommandResult{HarnessAuth: &view}}, nil
}

func (s *ControlService) handleHarnessAuthStatus(ctx context.Context, src Source, q HarnessAuthStatusQuery) (QueryResult, error) {
	result := QueryResult{Type: QueryHarnessAuthStatus}
	if !validHarnessAuthSource(src) {
		result.Error = ptr(invalid("source", "authenticated control source required"))
		return result, nil
	}
	if s.harnessAuth == nil {
		result.Error = ptr(ControlError{Code: ErrCodeInternal, Message: "harness authentication unavailable"})
		return result, nil
	}
	view, err := s.harnessAuth.Status(ctx, src, q)
	if err != nil {
		result.Error = ptr(controlErr(err))
	} else {
		result.HarnessAuth = &view
	}
	return result, nil
}

func validAuthInput(value string) bool {
	return value != "" && len(value) <= 4096 && !strings.ContainsAny(value, "\r\n\x00") && strings.TrimSpace(value) == value
}
