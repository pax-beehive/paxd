package cloud

import (
	"context"
	"net/http"
	"net/url"
)

type SessionOrigin struct {
	Kind           string `json:"kind"`
	RegistrationID string `json:"registration_id"`
	CronjobID      string `json:"cronjob_id"`
	RunID          string `json:"run_id"`
	Name           string `json:"name"`
}

type RegisterSessionRequest struct {
	NativeID string        `json:"native_id"`
	CWD      string        `json:"cwd"`
	Origin   SessionOrigin `json:"origin"`
}

type RegisteredSession struct {
	SessionID string `json:"session_id"`
}

func (c *Client) RegisterSession(ctx context.Context, agentID string, req *RegisterSessionRequest) (*RegisteredSession, error) {
	resp, err := c.doWithContextAndHeaders(ctx, http.MethodPost, "/api/v1/node/agents/"+url.PathEscape(agentID)+"/sessions/register", req, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err = checkStatus(resp, "register session"); err != nil {
		return nil, err
	}
	return decodeEnvelope[RegisteredSession](resp.Body, "register session")
}
