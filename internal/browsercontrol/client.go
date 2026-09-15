// Package browsercontrol exposes a fixed set of browser operator actions.
// Credentials and loopback addresses are resolved on the node, never by callers.
package browsercontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Client struct {
	DataDir     string
	mu          sync.Mutex
	previewMu   sync.Mutex
	vncSessions map[string]*vncSession
}

func (c *Client) Request(ctx context.Context, source, operation string, payload json.RawMessage) (json.RawMessage, error) {
	if len(payload) > 16384 {
		return nil, errors.New("browser payload is too large")
	}
	if operation != "vnc_exchange" && operation != "state" {
		log.Printf("[browser-control] operator request source=%q operation=%q", source, operation)
	}
	if operation == "vnc_open" || operation == "vnc_exchange" || operation == "vnc_close" {
		return c.vnc(ctx, source, operation, payload)
	}
	if operation == "view" {
		var view struct {
			Source string `json:"source"`
			Action struct {
				Type string `json:"type"`
			} `json:"action"`
		}
		if json.Unmarshal(payload, &view) != nil {
			return nil, errors.New("invalid browser view")
		}
		if view.Source == "docker" {
			if view.Action.Type != "screenshot" {
				return nil, errors.New("unsupported Docker preview action")
			}
			return c.dockerPreview(ctx)
		}
		if view.Source != "" && view.Source != "native" {
			return nil, errors.New("unsupported browser source")
		}
	}
	routes := map[string]string{"view": "view", "state": "state", "policy": "policy", "decide": "decide", "revoke": "revoke", "secret": "secret", "resume_sensitive": "resume-sensitive"}
	route, ok := routes[operation]
	if !ok || len(payload) > 16384 {
		return nil, errors.New("browser operation denied")
	}
	dir := c.DataDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, errors.New("browser control unavailable")
		}
		b, err := os.ReadFile(filepath.Join(home, ".config", "agent-browser-native", "data-dir"))
		if err != nil {
			return nil, errors.New("browser control is not installed")
		}
		dir = strings.TrimSpace(string(b))
	}
	token, err := os.ReadFile(filepath.Join(dir, "secrets", "control-admin-token"))
	if err != nil {
		return nil, errors.New("browser control unavailable")
	}
	method := http.MethodPost
	if operation == "state" {
		method = http.MethodGet
		payload = nil
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:7331/admin/"+route, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("invalid browser request")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("X-Pax-Browser-Actor", source)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("browser control unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(body) > 1024*1024 || !json.Valid(body) {
		return nil, errors.New("invalid browser response")
	}
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("browser operation rejected; refresh state before retrying")
	}
	return body, nil
}
