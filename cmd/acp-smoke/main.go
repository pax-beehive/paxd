package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

type repeatedFlag []string

func (r *repeatedFlag) String() string {
	return strings.Join(*r, ",")
}

func (r *repeatedFlag) Set(value string) error {
	*r = append(*r, value)
	return nil
}

type rawMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type outboundMessage struct {
	ID     int64
	Method string
	Raw    []byte
}

type inboundFrame struct {
	At      time.Time
	Payload []byte
}

type userAgent struct {
	AgentID       string `json:"agent_id"`
	NodeID        string `json:"node_id,omitempty"`
	Name          string `json:"name,omitempty"`
	Hostname      string `json:"hostname,omitempty"`
	AgentType     string `json:"agent_type,omitempty"`
	Status        string `json:"status,omitempty"`
	Online        bool   `json:"online,omitempty"`
	LastHeartbeat string `json:"last_heartbeat,omitempty"`
}

type aggregate struct {
	builder strings.Builder
	frames  int
	ui      display
	rawOnly bool
}

type display struct {
	color     bool
	rawOut    *os.File
	humanOut  *os.File
	rawStream string
}

func main() {
	log.SetFlags(0)

	var headers repeatedFlag
	var prompts repeatedFlag
	var files repeatedFlag

	fs := flag.NewFlagSet("acp-smoke", flag.ExitOnError)
	baseURL := fs.String("url", env("PAX_CLOUD_URL", ""), "Pax cloud URL or full user tunnel WebSocket URL")
	agentID := fs.String("agent-id", env("PAX_AGENT_ID", ""), "agent id used with the default user tunnel path")
	agentName := fs.String("agent-name", env("PAX_AGENT_NAME", ""), "agent name used when --agent-id is omitted")
	agentNameAlias := fs.String("agent", env("PAX_AGENT", ""), "deprecated alias for --agent-name")
	listAgents := fs.Bool("list-agents", false, "list user-visible agents and exit")
	includeOffline := fs.Bool("include-offline", false, "include offline agents when listing or auto-selecting")
	agentsPath := fs.String("agents-path", "/api/user/agents", "user agents list path")
	path := fs.String("path", "/api/v1/user/self/agents/{agent_id}/tunnel", "user tunnel path; {agent_id} is replaced")
	cwd := fs.String("cwd", env("PAX_ACP_TEST_CWD", "/tmp"), "cwd for session/new")
	authMethod := fs.String("auth-method", env("PAX_ACP_AUTH_METHOD", "auto"), "authenticate method id, auto, or empty to skip")
	timeout := fs.Duration("timeout", 2*time.Minute, "per-request timeout")
	overallTimeout := fs.Duration("overall-timeout", 10*time.Minute, "overall test timeout")
	rawOnly := fs.Bool("raw-only", false, "only print raw WebSocket frames")
	rawStream := fs.String("raw-stream", env("PAX_ACP_RAW_STREAM", "stderr"), "where to print raw frames: stderr, stdout, or off")
	noColor := fs.Bool("no-color", env("NO_COLOR", "") != "", "disable ANSI colors")
	interactive := fs.Bool("interactive", false, "create a session, then read prompts from stdin")
	noSmoke := fs.Bool("no-smoke", false, "do not send the built-in initialize/session/prompt smoke flow")
	userEmail := fs.String("user-email", env("PAX_USER_EMAIL", ""), "sets X-User-Email for local pax-manager tests")
	cfID := fs.String("cf-client-id", env("CF_ACCESS_CLIENT_ID", ""), "sets CF-Access-Client-Id")
	cfSecret := fs.String("cf-client-secret", env("CF_ACCESS_CLIENT_SECRET", ""), "sets CF-Access-Client-Secret")
	cfJWT := fs.String("cf-access-jwt", firstEnv("CF_ACCESS_JWT", "CF_ACCESS_TOKEN", "CF_AUTHORIZATION"), "sets Cf-Access-Jwt-Assertion")
	cookie := fs.String("cookie", firstEnv("PAX_COOKIE", "CF_ACCESS_COOKIE", "COOKIE"), "sets Cookie; accepts either raw cookie pairs or a copied 'Cookie: ...' header")
	cfAuthorization := fs.String("cf-authorization", env("CF_AUTHORIZATION", ""), "sets CF_Authorization cookie from a token value")
	bearer := fs.String("bearer", env("PAX_BEARER_TOKEN", ""), "sets Authorization: Bearer <token>")
	fs.Var(&headers, "header", "extra header as 'Name: value'; repeatable")
	fs.Var(&prompts, "prompt", "session prompt text; repeatable")
	fs.Var(&files, "messages-file", "JSON array or NDJSON file with raw JSON-RPC messages; repeatable")
	fs.Parse(os.Args[1:])
	if *agentName == "" {
		*agentName = *agentNameAlias
	}

	if *baseURL == "" {
		log.Fatal("--url or PAX_CLOUD_URL is required")
	}
	ui, err := newDisplay(*rawStream, !*noColor)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *overallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *overallTimeout)
		defer cancel()
	}

	header := http.Header{}
	if *userEmail != "" {
		header.Set("X-User-Email", *userEmail)
	}
	if *cfID != "" {
		header.Set("CF-Access-Client-Id", *cfID)
	}
	if *cfSecret != "" {
		header.Set("CF-Access-Client-Secret", *cfSecret)
	}
	if *cfJWT != "" {
		header.Set("Cf-Access-Jwt-Assertion", *cfJWT)
	}
	cookieHeader := normalizeCookieHeader(*cookie)
	if *cfAuthorization != "" {
		cookieHeader = appendCookie(cookieHeader, "CF_Authorization="+strings.TrimSpace(*cfAuthorization))
	}
	if cookieHeader != "" {
		header.Set("Cookie", cookieHeader)
	}
	if *bearer != "" {
		header.Set("Authorization", "Bearer "+*bearer)
	}
	for _, h := range headers {
		name, value, ok := strings.Cut(h, ":")
		if !ok || strings.TrimSpace(name) == "" {
			log.Fatalf("invalid --header %q; want 'Name: value'", h)
		}
		header.Add(strings.TrimSpace(name), strings.TrimSpace(value))
	}

	if *listAgents {
		agents, err := fetchUserAgents(ctx, *baseURL, *agentsPath, header)
		if err != nil {
			log.Fatalf("list agents: %v", err)
		}
		printAgents(ui, agents, *includeOffline)
		return
	}
	if *agentID == "" && !looksLikeTunnelURL(*baseURL) {
		agents, err := fetchUserAgents(ctx, *baseURL, *agentsPath, header)
		if err != nil {
			log.Fatalf("choose agent: %v", err)
		}
		selected, err := chooseAgent(agents, *agentName, *includeOffline)
		if err != nil {
			printAgents(ui, agents, *includeOffline)
			log.Fatalf("choose agent: %v", err)
		}
		*agentID = selected.AgentID
		ui.human("AGENT", "%s name=%q type=%s status=%s", selected.AgentID, selected.Name, selected.AgentType, selected.Status)
	}
	if *agentID == "" && !looksLikeTunnelURL(*baseURL) {
		log.Fatal("--agent-id, --agent-name, or PAX_AGENT_ID is required unless --url is a full tunnel URL")
	}

	tunnelURL, err := userTunnelURL(*baseURL, *path, *agentID)
	if err != nil {
		log.Fatalf("build tunnel URL: %v", err)
	}

	ui.human("CONNECT", "%s", tunnelURL.Redacted())
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, tunnelURL.String(), header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			log.Fatalf("dial WebSocket: HTTP %d%s: %v", resp.StatusCode, responseLocation(resp), err)
		}
		log.Fatalf("dial WebSocket: %v", err)
	}
	defer conn.Close()
	ui.human("CONNECTED", "%s", time.Now().Format(time.RFC3339Nano))

	inbound := make(chan inboundFrame, 256)
	readDone := make(chan error, 1)
	go readLoop(ctx, conn, inbound, readDone)

	id := int64(0)
	nextID := func() int64 { return atomic.AddInt64(&id, 1) }
	var sessionID string

	if !*noSmoke {
		initMsg := buildMessage(nextID(), "initialize", map[string]any{
			"protocolVersion":    1,
			"clientCapabilities": map[string]any{},
			"clientInfo": map[string]any{
				"name":    "paxd-acp-smoke",
				"version": "0.1.0",
			},
		})
		initResp, err := sendAndWait(ctx, conn, inbound, readDone, initMsg, *timeout, ui, *rawOnly, nil)
		if err != nil {
			log.Fatalf("initialize: %v", err)
		}

		methodID := chooseAuthMethod(*authMethod, initResp)
		if methodID != "" {
			authMsg := buildMessage(nextID(), "authenticate", map[string]any{"methodId": methodID})
			if _, err := sendAndWait(ctx, conn, inbound, readDone, authMsg, *timeout, ui, *rawOnly, nil); err != nil {
				log.Fatalf("authenticate: %v", err)
			}
		}

		newMsg := buildMessage(nextID(), "session/new", map[string]any{
			"cwd":        *cwd,
			"mcpServers": []any{},
		})
		newResp, err := sendAndWait(ctx, conn, inbound, readDone, newMsg, *timeout, ui, *rawOnly, nil)
		if err != nil {
			log.Fatalf("session/new: %v", err)
		}
		sessionID = firstStringByKey(newResp.Result, "sessionId")
		if sessionID == "" {
			log.Fatal("session/new response did not include sessionId")
		}
		if !*rawOnly {
			ui.human("SESSION", "sessionId=%s", sessionID)
		}

		if *interactive {
			runInteractive(ctx, conn, inbound, readDone, nextID, sessionID, *timeout, ui, *rawOnly)
			return
		}

		if len(prompts) == 0 {
			prompts = repeatedFlag{
				"Say hello in one short sentence.",
				"Remember the passphrase blue-mango. Reply only OK.",
				"What passphrase did I ask you to remember?",
			}
		}
		for _, prompt := range prompts {
			agg := &aggregate{ui: ui, rawOnly: *rawOnly}
			msg := buildMessage(nextID(), "session/prompt", map[string]any{
				"sessionId": sessionID,
				"prompt": []map[string]any{
					{"type": "text", "text": prompt},
				},
			})
			if !*rawOnly {
				ui.human("PROMPT", "%q", prompt)
			}
			if _, err := sendAndWait(ctx, conn, inbound, readDone, msg, *timeout, ui, *rawOnly, agg); err != nil {
				log.Fatalf("session/prompt: %v", err)
			}
			if !*rawOnly {
				ui.human("ANSWER", "frames=%d text=%q", agg.frames, agg.builder.String())
			}
		}
	}

	custom, err := loadMessages(files, sessionID)
	if err != nil {
		log.Fatalf("load messages: %v", err)
	}
	for _, msg := range custom {
		agg := &aggregate{ui: ui, rawOnly: *rawOnly}
		if _, err := sendAndWait(ctx, conn, inbound, readDone, msg, *timeout, ui, *rawOnly, agg); err != nil {
			log.Fatalf("%s: %v", msg.Method, err)
		}
		if !*rawOnly && agg.builder.Len() > 0 {
			ui.human("ANSWER", "custom method=%s frames=%d text=%q", msg.Method, agg.frames, agg.builder.String())
		}
	}
}

func readLoop(ctx context.Context, conn *websocket.Conn, inbound chan<- inboundFrame, done chan<- error) {
	defer close(inbound)
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			done <- err
			return
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}
		cp := append([]byte(nil), payload...)
		select {
		case inbound <- inboundFrame{At: time.Now(), Payload: cp}:
		case <-ctx.Done():
			done <- ctx.Err()
			return
		}
	}
}

func sendAndWait(
	ctx context.Context,
	conn *websocket.Conn,
	inbound <-chan inboundFrame,
	readDone <-chan error,
	msg outboundMessage,
	timeout time.Duration,
	ui display,
	rawOnly bool,
	agg *aggregate,
) (*rawMessage, error) {
	ui.raw("SEND", time.Now(), msg.Raw)
	if err := conn.WriteMessage(websocket.TextMessage, msg.Raw); err != nil {
		return nil, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		select {
		case frame, ok := <-inbound:
			if !ok {
				return nil, errors.New("websocket reader stopped")
			}
			ui.raw("RECV", frame.At, frame.Payload)
			parsed, err := parseRaw(frame.Payload)
			if err != nil {
				if !rawOnly {
					ui.human("PARSE", "error=%q", err.Error())
				}
				continue
			}
			if agg != nil {
				agg.ingest(parsed)
			}
			if rawIDEqual(parsed.ID, msg.ID) {
				if parsed.Error != nil {
					return parsed, fmt.Errorf("json-rpc error: %s", string(parsed.Error))
				}
				return parsed, nil
			}
		case err := <-readDone:
			return nil, err
		case <-waitCtx.Done():
			return nil, waitCtx.Err()
		}
	}
}

func runInteractive(
	ctx context.Context,
	conn *websocket.Conn,
	inbound <-chan inboundFrame,
	readDone <-chan error,
	nextID func() int64,
	sessionID string,
	timeout time.Duration,
	ui display,
	rawOnly bool,
) {
	ui.human("INPUT", "type a prompt, or /quit to exit")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Fprint(ui.humanOut, colorize(ui.color, colorCyan, "pax> "))
		if !scanner.Scan() {
			break
		}
		prompt := strings.TrimSpace(scanner.Text())
		if prompt == "" {
			continue
		}
		if prompt == "/quit" || prompt == "/exit" {
			return
		}
		agg := &aggregate{ui: ui, rawOnly: rawOnly}
		msg := buildMessage(nextID(), "session/prompt", map[string]any{
			"sessionId": sessionID,
			"prompt": []map[string]any{
				{"type": "text", "text": prompt},
			},
		})
		if _, err := sendAndWait(ctx, conn, inbound, readDone, msg, timeout, ui, rawOnly, agg); err != nil {
			ui.human("ERROR", "%v", err)
			continue
		}
		if !rawOnly {
			ui.human("ANSWER", "frames=%d text=%q", agg.frames, agg.builder.String())
		}
	}
	if err := scanner.Err(); err != nil {
		ui.human("ERROR", "stdin: %v", err)
	}
}

func buildMessage(id int64, method string, params map[string]any) outboundMessage {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return outboundMessage{ID: id, Method: method, Raw: raw}
}

func parseRaw(payload []byte) (*rawMessage, error) {
	var msg rawMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func rawIDEqual(raw json.RawMessage, id int64) bool {
	if len(raw) == 0 {
		return false
	}
	var number json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&number); err == nil {
		got, err := number.Int64()
		return err == nil && got == id
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text == strconv.FormatInt(id, 10)
	}
	return false
}

func chooseAuthMethod(setting string, msg *rawMessage) string {
	switch strings.TrimSpace(setting) {
	case "", "none", "skip":
		return ""
	case "auto":
		return firstAuthMethodID(msg.Result)
	default:
		return setting
	}
}

func firstAuthMethodID(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return firstAuthMethodIDValue(v)
}

func firstAuthMethodIDValue(v any) string {
	switch x := v.(type) {
	case map[string]any:
		if methods, ok := x["authMethods"].([]any); ok {
			for _, method := range methods {
				if m, ok := method.(map[string]any); ok {
					if id, ok := m["id"].(string); ok && id != "" {
						return id
					}
				}
			}
		}
		for _, child := range x {
			if id := firstAuthMethodIDValue(child); id != "" {
				return id
			}
		}
	case []any:
		for _, child := range x {
			if id := firstAuthMethodIDValue(child); id != "" {
				return id
			}
		}
	}
	return ""
}

func firstStringByKey(raw json.RawMessage, key string) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return firstStringByKeyValue(v, key)
}

func firstStringByKeyValue(v any, key string) string {
	switch x := v.(type) {
	case map[string]any:
		if s, ok := x[key].(string); ok && s != "" {
			return s
		}
		for _, child := range x {
			if s := firstStringByKeyValue(child, key); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range x {
			if s := firstStringByKeyValue(child, key); s != "" {
				return s
			}
		}
	}
	return ""
}

func (a *aggregate) ingest(msg *rawMessage) {
	if msg == nil || msg.Method != "session/update" || len(msg.Params) == 0 {
		return
	}
	var v any
	if err := json.Unmarshal(msg.Params, &v); err != nil {
		return
	}
	chunks := textChunks(v, "")
	if len(chunks) == 0 {
		return
	}
	a.frames++
	for _, chunk := range chunks {
		a.builder.WriteString(chunk)
	}
	if !a.rawOnly {
		a.ui.human("DELTA", "%q total=%q", strings.Join(chunks, ""), a.builder.String())
	}
}

func textChunks(v any, key string) []string {
	switch x := v.(type) {
	case map[string]any:
		var out []string
		for childKey, child := range x {
			out = append(out, textChunks(child, childKey)...)
		}
		return out
	case []any:
		var out []string
		for _, child := range x {
			out = append(out, textChunks(child, key)...)
		}
		return out
	case string:
		switch key {
		case "delta", "text", "content", "newText", "newContent":
			return []string{x}
		default:
			return nil
		}
	default:
		return nil
	}
}

func fetchUserAgents(ctx context.Context, rawBase, agentsPath string, header http.Header) ([]userAgent, error) {
	agentsURL, err := userAgentsURL(rawBase, agentsPath)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, agentsURL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header = header.Clone()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if readErr != nil {
			return nil, fmt.Errorf("GET %s: status %d; read body: %w", agentsURL.Redacted(), resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("GET %s: status %d: %s", agentsURL.Redacted(), resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if readErr != nil {
		return nil, readErr
	}
	return parseUserAgents(body)
}

func parseUserAgents(body []byte) ([]userAgent, error) {
	var enveloped struct {
		Data struct {
			Agents []userAgent `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &enveloped); err == nil && enveloped.Data.Agents != nil {
		return enveloped.Data.Agents, nil
	}

	var direct struct {
		Agents []userAgent `json:"agents"`
	}
	if err := json.Unmarshal(body, &direct); err == nil && direct.Agents != nil {
		return direct.Agents, nil
	}
	return nil, fmt.Errorf("decode agents response: expected data.agents or agents")
}

func chooseAgent(agents []userAgent, name string, includeOffline bool) (userAgent, error) {
	candidates := filterAgents(agents, includeOffline)
	if name != "" {
		candidates = matchAgentsByName(candidates, name)
	}
	switch len(candidates) {
	case 0:
		if name != "" {
			return userAgent{}, fmt.Errorf("no online agent named %q", name)
		}
		return userAgent{}, fmt.Errorf("no online agents found")
	case 1:
		return candidates[0], nil
	default:
		if name != "" {
			return userAgent{}, fmt.Errorf("multiple online agents named %q; pass --agent-id", name)
		}
		return userAgent{}, fmt.Errorf("multiple online agents found; pass --agent-id or --agent-name")
	}
}

func filterAgents(agents []userAgent, includeOffline bool) []userAgent {
	out := make([]userAgent, 0, len(agents))
	for _, agent := range agents {
		if includeOffline || agent.Online || strings.EqualFold(agent.Status, "online") {
			out = append(out, agent)
		}
	}
	return out
}

func matchAgentsByName(agents []userAgent, name string) []userAgent {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return agents
	}
	out := make([]userAgent, 0, len(agents))
	for _, agent := range agents {
		if strings.ToLower(strings.TrimSpace(agent.Name)) == name {
			out = append(out, agent)
		}
	}
	return out
}

func printAgents(ui display, agents []userAgent, includeOffline bool) {
	shown := filterAgents(agents, includeOffline)
	ui.human("AGENTS", "showing=%d total=%d", len(shown), len(agents))
	for _, agent := range shown {
		ui.human(
			"AGENT",
			"%s name=%q type=%s status=%s online=%t node=%s host=%s",
			agent.AgentID,
			agent.Name,
			agent.AgentType,
			agent.Status,
			agent.Online,
			agent.NodeID,
			agent.Hostname,
		)
	}
}

func userAgentsURL(rawBase, agentsPath string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimSpace(rawBase))
	if err != nil {
		return nil, err
	}
	if base.Scheme == "" {
		return nil, fmt.Errorf("missing URL scheme in %q", rawBase)
	}
	switch base.Scheme {
	case "https", "http":
	case "wss":
		base.Scheme = "https"
	case "ws":
		base.Scheme = "http"
	default:
		return nil, fmt.Errorf("unsupported scheme %q", base.Scheme)
	}
	if looksLikeTunnelURL(rawBase) {
		return nil, fmt.Errorf("--url is a tunnel URL; pass a cloud base URL to list agents")
	}
	if !strings.HasPrefix(agentsPath, "/") {
		agentsPath = "/" + agentsPath
	}
	base.Path = strings.TrimRight(base.Path, "/") + agentsPath
	base.RawQuery = ""
	base.Fragment = ""
	return base, nil
}

func userTunnelURL(rawBase, tunnelPath, agentID string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimSpace(rawBase))
	if err != nil {
		return nil, err
	}
	if base.Scheme == "" {
		return nil, fmt.Errorf("missing URL scheme in %q", rawBase)
	}
	switch base.Scheme {
	case "https":
		base.Scheme = "wss"
	case "http":
		base.Scheme = "ws"
	case "ws", "wss":
	default:
		return nil, fmt.Errorf("unsupported scheme %q", base.Scheme)
	}
	if !looksLikeTunnelURL(rawBase) {
		p := strings.ReplaceAll(tunnelPath, "{agent_id}", url.PathEscape(agentID))
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		base.Path = strings.TrimRight(base.Path, "/") + p
	}
	return base, nil
}

func looksLikeTunnelURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/tunnel")
}

func loadMessages(files []string, sessionID string) ([]outboundMessage, error) {
	var out []outboundMessage
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		data = []byte(strings.ReplaceAll(string(data), "{{sessionId}}", sessionID))
		messages, err := parseMessagesFile(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		out = append(out, messages...)
	}
	return out, nil
}

func parseMessagesFile(data []byte) ([]outboundMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var raws []json.RawMessage
		if err := json.Unmarshal(trimmed, &raws); err != nil {
			return nil, err
		}
		return outboundFromRaw(raws)
	}
	lines := bytes.Split(trimmed, []byte("\n"))
	raws := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || bytes.HasPrefix(line, []byte("#")) {
			continue
		}
		raws = append(raws, append([]byte(nil), line...))
	}
	return outboundFromRaw(raws)
}

func outboundFromRaw(raws []json.RawMessage) ([]outboundMessage, error) {
	out := make([]outboundMessage, 0, len(raws))
	for _, raw := range raws {
		msg, err := parseRaw(raw)
		if err != nil {
			return nil, err
		}
		id, err := idFromRaw(msg.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, outboundMessage{ID: id, Method: msg.Method, Raw: append([]byte(nil), raw...)})
	}
	return out, nil
}

func idFromRaw(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, errors.New("custom JSON-RPC messages must include numeric id")
	}
	var number json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&number); err == nil {
		return number.Int64()
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strconv.ParseInt(text, 10, 64)
	}
	return 0, fmt.Errorf("unsupported id %s", string(raw))
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func normalizeCookieHeader(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if name, rest, ok := strings.Cut(value, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Cookie") {
		value = strings.TrimSpace(rest)
	}
	return strings.Trim(value, "\"'")
}

func appendCookie(existing, addition string) string {
	addition = normalizeCookieHeader(addition)
	if addition == "" {
		return existing
	}
	if existing == "" {
		return addition
	}
	return strings.TrimRight(existing, "; ") + "; " + addition
}

func responseLocation(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return ""
	}
	return " location=" + strconv.Quote(location)
}

const (
	colorReset  = "\033[0m"
	colorDim    = "\033[2m"
	colorCyan   = "\033[36m"
	colorGreen  = "\033[32m"
	colorBlue   = "\033[34m"
	colorYellow = "\033[33m"
)

func newDisplay(rawStream string, color bool) (display, error) {
	ui := display{
		color:     color,
		humanOut:  os.Stdout,
		rawStream: rawStream,
	}
	switch rawStream {
	case "stderr", "":
		ui.rawOut = os.Stderr
	case "stdout":
		ui.rawOut = os.Stdout
	case "off", "none":
		ui.rawOut = nil
	default:
		return display{}, fmt.Errorf("--raw-stream must be stderr, stdout, or off")
	}
	return ui, nil
}

func (d display) raw(direction string, at time.Time, payload []byte) {
	if d.rawOut == nil {
		return
	}
	labelColor := colorBlue
	if direction == "RECV" {
		labelColor = colorGreen
	}
	fmt.Fprintf(
		d.rawOut,
		"%s %s %s\n",
		colorize(d.color, labelColor, direction),
		colorize(d.color, colorDim, at.Format(time.RFC3339Nano)),
		string(payload),
	)
}

func (d display) human(label, format string, args ...any) {
	fmt.Fprintf(
		d.humanOut,
		"%s %s\n",
		colorize(d.color, colorYellow, label),
		fmt.Sprintf(format, args...),
	)
}

func colorize(enabled bool, color string, text string) string {
	if !enabled {
		return text
	}
	return color + text + colorReset
}
