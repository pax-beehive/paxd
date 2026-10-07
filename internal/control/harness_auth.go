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
	Harness   string `json:"harness"`
	Operation string `json:"operation"`
	Method    string `json:"method,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Code      string `json:"code,omitempty"`
}

type HarnessAuthStatusQuery struct {
	Harness   string `json:"harness"`
	SessionID string `json:"session_id,omitempty"`
}

type HarnessAuthView struct {
	Harness          string `json:"harness"`
	SessionID        string `json:"session_id,omitempty"`
	State            string `json:"state"`
	AuthorizationURL string `json:"authorization_url,omitempty"`
	Method           string `json:"method,omitempty"`
	UserCode         string `json:"user_code,omitempty"`
	ExpiresAt        string `json:"expires_at,omitempty"`
	LoggedIn         *bool  `json:"logged_in,omitempty"`
	AuthMethod       string `json:"auth_method,omitempty"`
	ErrorCode        string `json:"error_code,omitempty"`
}

func (c HarnessAuthLoginCommand) Validate() error {
	if c.Harness != "claude" && c.Harness != "codex" {
		return invalid("harness", "authentication is supported for claude and codex")
	}
	if len(c.SessionID) > 128 {
		return invalid("session_id", "invalid login session id")
	}
	switch c.Operation {
	case "start":
		if c.SessionID != "" {
			return invalid("session_id", "start accepts no login handle")
		}
		if c.Harness == "claude" {
			if c.Method != "" && c.Method != "subscription" && c.Method != "console" {
				return invalid("method", "Claude supports subscription or console")
			}
			if c.Code != "" {
				return invalid("code", "Claude start does not accept a secret")
			}
		} else {
			switch c.Method {
			case "", "device":
				if c.Code != "" {
					return invalid("code", "device login does not accept a secret")
				}
			case "api-key", "access-token":
				if !validAuthInput(c.Code) {
					return invalid("code", "secret input must be one non-empty line of at most 4096 bytes")
				}
			default:
				return invalid("method", "Codex supports device, api-key, or access-token")
			}
		}
	case "submit":
		if c.Harness != "claude" || c.Method != "" || !validAuthInput(c.Code) {
			return invalid("harness_auth_login", "submit accepts a Claude authorization code, without a method")
		}
	case "cancel":
		if c.SessionID == "" || c.Code != "" || c.Method != "" {
			return invalid("harness_auth_login", "cancel requires a session id and no code or method")
		}
	default:
		return invalid("operation", "expected start, submit, or cancel")
	}
	return nil
}

func (q HarnessAuthStatusQuery) Validate() error {
	if (q.Harness != "claude" && q.Harness != "codex") || len(q.SessionID) > 128 {
		return invalid("harness_auth_status", "expected claude or codex and an optional login session id")
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
