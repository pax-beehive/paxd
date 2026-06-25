package runtime

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	DefaultNodeControlPath = "/api/v1/node/control"
	DefaultAgentTunnelPath = "/api/v1/agent/tunnel"
)

func websocketURLFromHTTP(rawBase, path string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimRight(rawBase, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse cloud url: %w", err)
	}
	switch base.Scheme {
	case "https":
		base.Scheme = "wss"
	case "http":
		base.Scheme = "ws"
	case "ws", "wss":
	default:
		return nil, fmt.Errorf("unsupported cloud url scheme %q", base.Scheme)
	}
	if path == "" {
		path = DefaultAgentTunnelPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	return base, nil
}
