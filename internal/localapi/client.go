package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/pax-beehive/paxd/internal/control"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewUnixClient(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{baseURL: "http://paxd", http: &http.Client{Transport: transport}}
}

func NewHTTPClient(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: http.DefaultClient}
}

func (c *Client) GetStatus(ctx context.Context) (control.QueryResult, error) {
	return c.get(ctx, "/v1/status")
}

func (c *Client) ListRemotes(ctx context.Context, includeDisabled bool) (control.QueryResult, error) {
	path := "/v1/remotes"
	if includeDisabled {
		path += "?include_disabled=true"
	}
	return c.get(ctx, path)
}

func (c *Client) CreateRemote(ctx context.Context, commandID string, cmd control.CreateRemoteCommand) (control.CommandAck, error) {
	return c.postCommand(ctx, "/v1/remotes", cmd, commandID)
}

func (c *Client) UpdateRemote(ctx context.Context, commandID string, remoteID string, cmd control.UpdateRemoteCommand) (control.CommandAck, error) {
	cmd.RemoteID = remoteID
	return c.patchCommand(ctx, "/v1/remotes/"+url.PathEscape(remoteID), cmd, commandID)
}

func (c *Client) RestartRemote(ctx context.Context, commandID string, remoteID string) (control.CommandAck, error) {
	return c.postCommand(ctx, "/v1/remotes/"+url.PathEscape(remoteID)+"/restart", nil, commandID)
}

func (c *Client) DeleteRemote(ctx context.Context, commandID string, remoteID string, cascadeAgentConnections bool) (control.CommandAck, error) {
	path := "/v1/remotes/" + url.PathEscape(remoteID)
	if cascadeAgentConnections {
		path += "?cascade_agent_connections=true"
	}
	return c.deleteCommand(ctx, path, commandID)
}

func (c *Client) ListAgentConnections(ctx context.Context, includeDisabled bool) (control.QueryResult, error) {
	path := "/v1/agent-connections"
	if includeDisabled {
		path += "?include_disabled=true"
	}
	return c.get(ctx, path)
}

func (c *Client) ListHarnesses(ctx context.Context, includeMissing bool) (control.QueryResult, error) {
	path := "/v1/harnesses"
	if includeMissing {
		path += "?include_missing=true"
	}
	return c.get(ctx, path)
}

func (c *Client) DiscoverHarnesses(ctx context.Context, query control.DiscoverHarnessesQuery) (control.QueryResult, error) {
	return c.post(ctx, "/v1/harnesses/discover", query, "")
}

func (c *Client) ListLocalSessions(ctx context.Context, query control.ListLocalSessionsQuery) (control.QueryResult, error) {
	path := "/v1/local/sessions"
	values := url.Values{}
	if query.Agent != "" {
		values.Set("agent", query.Agent)
	}
	if query.Limit > 0 {
		values.Set("limit", fmt.Sprintf("%d", query.Limit))
	}
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return c.get(ctx, path)
}

func (c *Client) GetLocalOverview(ctx context.Context) (control.QueryResult, error) {
	return c.get(ctx, "/v1/local/overview")
}

func (c *Client) SyncLocalSessions(ctx context.Context, query control.SyncLocalSessionsQuery) (control.QueryResult, error) {
	return c.post(ctx, "/v1/local/sessions/sync", query, "")
}

func (c *Client) CreateAgentConnection(ctx context.Context, commandID string, cmd control.CreateAgentConnectionCommand) (control.CommandAck, error) {
	return c.postCommand(ctx, "/v1/agent-connections", cmd, commandID)
}

func (c *Client) RestartAgentConnection(ctx context.Context, commandID string, connectionID string) (control.CommandAck, error) {
	return c.postCommand(ctx, "/v1/agent-connections/"+url.PathEscape(connectionID)+"/restart", nil, commandID)
}

func (c *Client) DeleteAgentConnection(ctx context.Context, commandID string, connectionID string) (control.CommandAck, error) {
	return c.deleteCommand(ctx, "/v1/agent-connections/"+url.PathEscape(connectionID), commandID)
}

func (c *Client) get(ctx context.Context, path string) (control.QueryResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return control.QueryResult{}, err
	}
	return c.do(req)
}

func (c *Client) post(ctx context.Context, path string, body any, commandID string) (control.QueryResult, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return control.QueryResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return control.QueryResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if commandID != "" {
		req.Header.Set(commandIDHeader, commandID)
	}
	return c.do(req)
}

func (c *Client) postCommand(ctx context.Context, path string, body any, commandID string) (control.CommandAck, error) {
	return c.commandWithBody(ctx, http.MethodPost, path, body, commandID)
}

func (c *Client) patchCommand(ctx context.Context, path string, body any, commandID string) (control.CommandAck, error) {
	return c.commandWithBody(ctx, http.MethodPatch, path, body, commandID)
}

func (c *Client) deleteCommand(ctx context.Context, path string, commandID string) (control.CommandAck, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return control.CommandAck{}, err
	}
	req.Header.Set(commandIDHeader, commandID)
	return c.doCommand(req)
}

func (c *Client) commandWithBody(ctx context.Context, method string, path string, body any, commandID string) (control.CommandAck, error) {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			return control.CommandAck{}, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return control.CommandAck{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(commandIDHeader, commandID)
	return c.doCommand(req)
}

func (c *Client) do(req *http.Request) (control.QueryResult, error) {
	httpClient := c.http
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return control.QueryResult{}, err
	}
	defer resp.Body.Close()
	var result control.QueryResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return control.QueryResult{}, err
	}
	if resp.StatusCode >= 400 {
		return result, fmt.Errorf("local API returned %s", resp.Status)
	}
	return result, nil
}

func (c *Client) doCommand(req *http.Request) (control.CommandAck, error) {
	httpClient := c.http
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return control.CommandAck{}, err
	}
	defer resp.Body.Close()
	var ack control.CommandAck
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return control.CommandAck{}, err
	}
	if resp.StatusCode >= 400 {
		return ack, fmt.Errorf("local API returned %s", resp.Status)
	}
	return ack, nil
}
