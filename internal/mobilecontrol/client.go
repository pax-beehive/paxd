// Package mobilecontrol bridges named operator actions to an owned local mobile runtime.
package mobilecontrol

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	maxPayload      = 16384
	maxJSONResponse = 4 * 1024 * 1024
	maxChunk        = 256 * 1024
	maxArtifact     = 1024 * 1024 * 1024
)

var (
	sessionID      = regexp.MustCompile(`^m-[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	uuid           = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	templateName   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	adminToken     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	errUnavailable = errors.New("mobile runtime unavailable; check node configuration")
	errDenied      = errors.New("mobile operation denied")
	errResponse    = errors.New("invalid mobile runtime response")
)

// Client resolves credentials and the fixed loopback port on the node. Callers
// cannot supply an origin, path, credential, SDK setting or native RTC message.
type Client struct {
	ConfigPath string
}

type endpoint struct {
	port  int
	token string
	actor string
}

type template struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
}

type capabilities struct {
	Screen                 bool   `json:"screen"`
	InternalAudioRecording bool   `json:"internalAudioRecording"`
	RealtimeMedia          bool   `json:"realtimeMedia"`
	RealtimeMediaMode      string `json:"realtimeMediaMode"`
	HostNetworkIsolation   bool   `json:"hostNetworkIsolation"`
	TextInput              string `json:"textInput"`
}

type session struct {
	ID               string       `json:"id"`
	DeviceID         string       `json:"deviceId"`
	Template         string       `json:"template"`
	State            string       `json:"state"`
	Takeover         bool         `json:"takeover"`
	RecoveryRequired bool         `json:"recoveryRequired"`
	CreatedAt        string       `json:"createdAt"`
	Capabilities     capabilities `json:"capabilities"`
}

type recording struct {
	ID          string `json:"id"`
	DeviceID    string `json:"deviceId"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	StartedAt   string `json:"startedAt"`
	EndedAt     string `json:"endedAt,omitempty"`
	DurationSec int    `json:"durationSec"`
	Bytes       int64  `json:"bytes"`
	Validation  string `json:"validation,omitempty"`
	Error       string `json:"error,omitempty"`
}

type state struct {
	Available    bool       `json:"available"`
	Mode         string     `json:"mode"`
	Templates    []template `json:"templates"`
	Sessions     []session  `json:"sessions"`
	RemoteWebRTC bool       `json:"remoteWebRTC"`
}

func marshal(value any) (json.RawMessage, error) { data, err := json.Marshal(value); return data, err }

func (c *Client) Request(ctx context.Context, source, operation string, payload json.RawMessage) (json.RawMessage, error) {
	args, err := arguments(operation, payload)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout(operation))
	defer cancel()
	if operation != "state" && operation != "screenshot" && operation != "recording_read" {
		log.Printf("[mobile-control] operator request source=%q operation=%q", source, operation)
	}
	ep, err := c.resolve()
	if err != nil {
		if operation == "state" {
			return marshal(state{Mode: "screenshots-and-recordings", Templates: []template{}, Sessions: []session{}})
		}
		return nil, errUnavailable
	}
	ep.actor = source
	if operation == "state" {
		return c.state(ctx, ep)
	}
	if operation == "templates" {
		data, err := c.requestJSON(ctx, ep, http.MethodGet, "/v1/admin/templates", nil)
		if err != nil {
			return nil, err
		}
		items, err := templates(data)
		if err != nil {
			return nil, err
		}
		return marshal(items)
	}
	if operation == "create" {
		data, err := c.requestJSON(ctx, ep, http.MethodPost, "/v1/admin/sessions", args)
		if err != nil {
			return nil, err
		}
		return publicSession(data)
	}
	id, _ := stringArg(args, "sessionId")
	delete(args, "sessionId")
	path := "/v1/admin/sessions/" + id
	if operation == "recording_read" {
		return c.readRecording(ctx, ep, path, args)
	}
	if operation == "destroy" {
		data, err := c.requestJSON(ctx, ep, http.MethodDelete, path, nil)
		if err != nil {
			return nil, err
		}
		var value struct {
			Destroyed bool `json:"destroyed"`
		}
		if json.Unmarshal(data, &value) != nil || !value.Destroyed {
			return nil, errResponse
		}
		return marshal(value)
	}
	data, err := c.requestJSON(ctx, ep, http.MethodPost, path+"/action", map[string]any{"action": operation, "args": args})
	if err != nil {
		return nil, err
	}
	switch operation {
	case "start", "stop", "takeover", "release":
		return publicSession(data)
	case "recording_start", "recording_stop":
		item, err := publicRecording(data)
		if err != nil {
			return nil, err
		}
		return marshal(item)
	case "recording_list":
		var raw []json.RawMessage
		if json.Unmarshal(data, &raw) != nil || len(raw) > 4096 {
			return nil, errResponse
		}
		items := make([]recording, 0, len(raw))
		for _, entry := range raw {
			item, err := publicRecording(entry)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		return marshal(items)
	case "screenshot":
		return publicScreenshot(data)
	default:
		// Input acknowledgements have no runtime-dependent data to forward.
		return marshal(map[string]bool{"ok": true})
	}
}

func (c *Client) resolve() (endpoint, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return endpoint{}, errUnavailable
	}
	filename := c.ConfigPath
	if filename == "" {
		filename = os.Getenv("AGENT_MOBILE_CONFIG")
	}
	if filename == "" {
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		filename = filepath.Join(configHome, "agent-mobile-runtime", "config.json")
	}
	if !filepath.IsAbs(filename) {
		return endpoint{}, errUnavailable
	}
	config := struct {
		DataDir     string `json:"dataDir"`
		ControlPort int    `json:"controlPort"`
	}{ControlPort: 7341}
	data, err := readFile(filename, 65536, false)
	if err != nil {
		return endpoint{}, errUnavailable
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(data, &input) != nil || input == nil {
		return endpoint{}, errUnavailable
	}
	if value, present := input["dataDir"]; present {
		if json.Unmarshal(value, &config.DataDir) != nil || config.DataDir == "" {
			return endpoint{}, errUnavailable
		}
	}
	if value, present := input["controlPort"]; present {
		port, err := strconv.Atoi(string(value))
		if err != nil {
			return endpoint{}, errUnavailable
		}
		config.ControlPort = port
	}
	if config.DataDir == "" {
		dataHome := os.Getenv("XDG_DATA_HOME")
		if dataHome == "" {
			dataHome = filepath.Join(home, ".local", "share")
		}
		config.DataDir = filepath.Join(dataHome, "agent-mobile-runtime")
	}
	if !filepath.IsAbs(config.DataDir) || strings.ContainsRune(config.DataDir, '\x00') || config.ControlPort < 1024 || config.ControlPort > 65535 {
		return endpoint{}, errUnavailable
	}
	root, err := filepath.EvalSymlinks(config.DataDir)
	if err != nil {
		return endpoint{}, errUnavailable
	}
	for _, dir := range []string{root, filepath.Join(root, "secrets")} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return endpoint{}, errUnavailable
		}
	}
	token, err := readFile(filepath.Join(root, "secrets", "admin-token"), 256, true)
	value := strings.TrimSpace(string(token))
	if err != nil || !adminToken.MatchString(value) {
		return endpoint{}, errUnavailable
	}
	return endpoint{port: config.ControlPort, token: value}, nil
}

func readFile(filename string, limit int64, secret bool) ([]byte, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > limit || secret && info.Mode().Perm() != 0600 {
		return nil, errUnavailable
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, errUnavailable
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil || !os.SameFile(info, current) || !current.Mode().IsRegular() || secret && current.Mode().Perm() != 0600 {
		return nil, errUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errUnavailable
	}
	return data, nil
}

func (c *Client) request(ctx context.Context, ep endpoint, method, path string, payload any, headers map[string]string) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, errDenied
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", ep.port, path), body)
	if err != nil {
		return nil, errDenied
	}
	req.Header.Set("Authorization", "Bearer "+ep.token)
	req.Header.Set("X-Pax-Mobile-Actor", ep.actor)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, errUnavailable
	}
	return response, nil
}

func (c *Client) requestJSON(ctx context.Context, ep endpoint, method, path string, payload any) (json.RawMessage, error) {
	response, err := c.request(ctx, ep, method, path, payload, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, errors.New("mobile operation rejected; refresh state before retrying")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxJSONResponse+1))
	if err != nil || len(data) > maxJSONResponse || !json.Valid(data) {
		return nil, errResponse
	}
	return data, nil
}

func (c *Client) state(ctx context.Context, ep endpoint) (json.RawMessage, error) {
	data, err := c.requestJSON(ctx, ep, http.MethodGet, "/v1/admin/templates", nil)
	if err != nil {
		return marshal(state{Mode: "screenshots-and-recordings", Templates: []template{}, Sessions: []session{}})
	}
	availableTemplates, err := templates(data)
	if err != nil {
		return nil, err
	}
	data, err = c.requestJSON(ctx, ep, http.MethodGet, "/v1/admin/sessions", nil)
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	if json.Unmarshal(data, &raw) != nil || len(raw) > 8 {
		return nil, errResponse
	}
	items := make([]session, 0, len(raw))
	for _, entry := range raw {
		item, err := decodeSession(entry)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return marshal(state{Available: true, Mode: "screenshots-and-recordings", Templates: availableTemplates, Sessions: items})
}

func templates(data []byte) ([]template, error) {
	var items []template
	if json.Unmarshal(data, &items) != nil || len(items) > 256 {
		return nil, errResponse
	}
	for _, item := range items {
		if !templateName.MatchString(item.Name) {
			return nil, errResponse
		}
	}
	if items == nil {
		items = []template{}
	}
	return items, nil
}

func decodeSession(data []byte) (session, error) {
	var item session
	if json.Unmarshal(data, &item) != nil || !sessionID.MatchString(item.ID) || item.DeviceID != item.ID || !templateName.MatchString(item.Template) {
		return item, errResponse
	}
	switch item.State {
	case "stopped", "booting", "running", "stopping", "interrupted", "failed":
	default:
		return item, errResponse
	}
	item.Capabilities.RealtimeMedia = false
	item.Capabilities.RealtimeMediaMode = "local-media-only"
	item.Capabilities.TextInput = "printable-ascii"
	return item, nil
}

func publicSession(data []byte) (json.RawMessage, error) {
	item, err := decodeSession(data)
	if err != nil {
		return nil, err
	}
	return marshal(item)
}

func publicRecording(data []byte) (recording, error) {
	var item recording
	if json.Unmarshal(data, &item) != nil || !uuid.MatchString(item.ID) || !sessionID.MatchString(item.DeviceID) || item.Bytes < 0 || item.Bytes > maxArtifact {
		return item, errResponse
	}
	if item.Kind != "screen" && item.Kind != "audio" {
		return item, errResponse
	}
	switch item.State {
	case "starting", "recording", "stopping", "completed", "failed":
	default:
		return item, errResponse
	}
	if item.Error != "" {
		item.Error = "Recording failed; inspect the runtime on this node"
	}
	return item, nil
}

func publicScreenshot(data []byte) (json.RawMessage, error) {
	var item struct {
		Mime    string `json:"mime"`
		Data    string `json:"data"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
		FrameID string `json:"frameId"`
	}
	if json.Unmarshal(data, &item) != nil || item.Mime != "image/png" || !uuid.MatchString(item.FrameID) || item.Width < 1 || item.Width > 16384 || item.Height < 1 || item.Height > 16384 {
		return nil, errResponse
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(item.Data)
	if err != nil || len(decoded) < 24 || !bytes.Equal(decoded[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return nil, errResponse
	}
	return marshal(item)
}

func (c *Client) readRecording(ctx context.Context, ep endpoint, path string, args map[string]json.RawMessage) (json.RawMessage, error) {
	id, _ := stringArg(args, "recordingId")
	offset := integer(args, "offset", 0)
	length := integer(args, "length", maxChunk)
	response, err := c.request(ctx, ep, http.MethodGet, path+"/artifacts/"+id, nil, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)})
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		return nil, errors.New("mobile recording unavailable; only finalized ranged artifacts are supported")
	}
	var start, end, total int64
	if _, err := fmt.Sscanf(response.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil || start != offset || end < start || end-start+1 > length || total <= end || total > maxArtifact {
		return nil, errResponse
	}
	mime := response.Header.Get("Content-Type")
	if mime != "audio/ogg" && mime != "video/x-matroska" {
		return nil, errResponse
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxChunk+1))
	if err != nil || int64(len(data)) != end-start+1 || len(data) > maxChunk {
		return nil, errResponse
	}
	return marshal(struct {
		ID         string `json:"id"`
		Mime       string `json:"mime"`
		Data       string `json:"data"`
		Offset     int64  `json:"offset"`
		Length     int    `json:"length"`
		TotalBytes int64  `json:"totalBytes"`
		EOF        bool   `json:"eof"`
	}{id, mime, base64.StdEncoding.EncodeToString(data), offset, len(data), total, end+1 == total})
}

func stringArg(args map[string]json.RawMessage, key string) (string, bool) {
	var value string
	data, ok := args[key]
	if !ok || json.Unmarshal(data, &value) != nil {
		return "", false
	}
	return value, true
}

func integer(args map[string]json.RawMessage, key string, fallback int64) int64 {
	data, ok := args[key]
	if !ok {
		return fallback
	}
	value, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return -1
	}
	return value
}

func operationTimeout(operation string) time.Duration {
	switch operation {
	case "start":
		return 12 * time.Minute
	case "create":
		return 2 * time.Minute
	case "stop", "destroy":
		return 90 * time.Second
	case "takeover", "release", "recording_stop":
		return time.Minute
	default:
		return 15 * time.Second
	}
}

func arguments(operation string, payload json.RawMessage) (map[string]json.RawMessage, error) {
	if len(payload) > maxPayload {
		return nil, errDenied
	}
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(payload, &args) != nil || args == nil {
		return nil, errDenied
	}
	fields := map[string]bool{}
	allow := func(keys ...string) {
		for _, key := range keys {
			fields[key] = true
		}
	}
	switch operation {
	case "state", "templates":
	case "create":
		allow("template")
	case "start", "stop", "destroy", "takeover", "release", "screenshot", "recording_list":
		allow("sessionId")
	case "tap":
		allow("sessionId", "frameId", "x", "y")
	case "swipe":
		allow("sessionId", "frameId", "x1", "y1", "x2", "y2", "durationMs")
	case "text":
		allow("sessionId", "frameId", "text")
	case "key":
		allow("sessionId", "frameId", "key")
	case "recording_start":
		allow("sessionId", "kind", "durationSec")
	case "recording_stop":
		allow("sessionId", "recordingId")
	case "recording_read":
		allow("sessionId", "recordingId", "offset", "length")
	default:
		return nil, errDenied
	}
	for key := range args {
		if !fields[key] {
			return nil, errDenied
		}
	}
	if operation == "create" {
		value, ok := stringArg(args, "template")
		if !ok || !templateName.MatchString(value) {
			return nil, errDenied
		}
	}
	if fields["sessionId"] {
		value, ok := stringArg(args, "sessionId")
		if !ok || !sessionID.MatchString(value) {
			return nil, errDenied
		}
	}
	if fields["frameId"] {
		value, ok := stringArg(args, "frameId")
		if !ok || !uuid.MatchString(value) {
			return nil, errDenied
		}
	}
	if fields["recordingId"] {
		value, ok := stringArg(args, "recordingId")
		if !ok || !uuid.MatchString(value) {
			return nil, errDenied
		}
	}
	for _, key := range []string{"x", "y", "x1", "y1", "x2", "y2"} {
		if fields[key] {
			value := integer(args, key, -1)
			if value < 0 || value > 16383 {
				return nil, errDenied
			}
		}
	}
	if _, ok := args["durationMs"]; ok {
		value := integer(args, "durationMs", -1)
		if value < 50 || value > 5000 {
			return nil, errDenied
		}
	}
	if fields["text"] {
		value, ok := stringArg(args, "text")
		if !ok || len(value) < 1 || len(value) > 2000 || strings.Contains(value, "%s") {
			return nil, errDenied
		}
		for _, char := range value {
			if char < 32 || char > 126 {
				return nil, errDenied
			}
		}
	}
	if fields["key"] {
		value, _ := stringArg(args, "key")
		switch value {
		case "HOME", "BACK", "ENTER", "APP_SWITCH", "DELETE":
		default:
			return nil, errDenied
		}
	}
	if value, ok := args["kind"]; ok {
		var kind string
		if json.Unmarshal(value, &kind) != nil || kind != "screen" && kind != "audio" {
			return nil, errDenied
		}
	}
	if _, ok := args["durationSec"]; ok {
		value := integer(args, "durationSec", -1)
		if value < 1 || value > 3600 {
			return nil, errDenied
		}
	}
	if operation == "recording_read" {
		offset, length := integer(args, "offset", 0), integer(args, "length", maxChunk)
		if offset < 0 || offset >= maxArtifact || length < 1 || length > maxChunk || offset+length > maxArtifact {
			return nil, errDenied
		}
	}
	return args, nil
}
