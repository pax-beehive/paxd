package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/pax-beehive/paxd/internal/auth"
	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/urfave/cli/v3"
)

const (
	envPaxAgentID                          = "PAX_AGENT_ID"
	envPaxRepresentativeAgentID            = "PAX_REPRESENTATIVE_AGENT_ID"
	envPaxSessionID                        = "PAX_SESSION_ID"
	conversationTargetKindRepresentative   = "representative"
	conversationTargetKindAgent            = "agent"
	conversationTargetKindActiveInvocation = "active_invocation"
)

func cmdMCPCommand() *cli.Command {
	return &cli.Command{
		Name:  "mcp",
		Usage: "run Pax MCP helper commands",
		Commands: []*cli.Command{
			{
				Name:  "conversation",
				Usage: "send Pax conversation messages",
				Commands: []*cli.Command{
					cmdMCPConversationAskCommand(),
					cmdMCPConversationReplyCommand(),
					cmdMCPConversationServeCommand(),
				},
			},
		},
	}
}

func cmdMCPConversationServeCommand() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "serve Pax conversation tools over stdio MCP",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return serveConversationMCP(ctx, os.Stdin, commandWriter(cmd))
		},
	}
}

func cmdMCPConversationAskCommand() *cli.Command {
	return &cli.Command{
		Name:  "ask",
		Usage: "ask another representative agent",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "to-representative-agent-id", Usage: "target representative agent id"},
			&cli.StringFlag{Name: "input-file", Usage: "read the question from a file"},
			&cli.BoolFlag{Name: "include-message", Usage: "include the latest visible session message in conversation context"},
			&cli.BoolFlag{Name: "json", Usage: "print the full pax-manager response"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			agentID := strings.TrimSpace(os.Getenv(envPaxAgentID))
			representativeAgentID := strings.TrimSpace(os.Getenv(envPaxRepresentativeAgentID))
			sessionID := strings.TrimSpace(os.Getenv(envPaxSessionID))
			toRepresentativeAgentID := strings.TrimSpace(cmd.String("to-representative-agent-id"))
			if agentID == "" {
				return fmt.Errorf("%s is required", envPaxAgentID)
			}
			if sessionID == "" {
				return fmt.Errorf("%s is required", envPaxSessionID)
			}
			if toRepresentativeAgentID == "" {
				return fmt.Errorf("--to-representative-agent-id is required")
			}
			instruction, err := conversationInstruction(cmd.Args().Slice(), cmd.String("input-file"))
			if err != nil {
				return err
			}
			req := cloud.ConversationDeliveryRequest{
				Source: &cloud.ConversationDeliverySource{
					AgentID:               agentID,
					RepresentativeAgentID: representativeAgentID,
					SessionID:             sessionID,
				},
				Target: cloud.ConversationDeliveryTarget{
					Kind:                  conversationTargetKindRepresentative,
					RepresentativeAgentID: toRepresentativeAgentID,
				},
				Context: cloud.ConversationDeliveryContext{
					LatestResponse: cmd.Bool("include-message"),
				},
				Instruction: instruction,
			}
			return sendConversationDelivery(ctx, cmd, agentID, &req)
		},
	}
}

func cmdMCPConversationReplyCommand() *cli.Command {
	return &cli.Command{
		Name:  "reply",
		Usage: "reply to the current Pax conversation invocation",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "input-file", Usage: "read the reply from a file"},
			&cli.BoolFlag{Name: "include-message", Usage: "include the latest visible session message in conversation context"},
			&cli.BoolFlag{Name: "json", Usage: "print the full pax-manager response"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			agentID := strings.TrimSpace(os.Getenv(envPaxAgentID))
			representativeAgentID := strings.TrimSpace(os.Getenv(envPaxRepresentativeAgentID))
			sessionID := strings.TrimSpace(os.Getenv(envPaxSessionID))
			if agentID == "" {
				return fmt.Errorf("%s is required", envPaxAgentID)
			}
			if sessionID == "" {
				return fmt.Errorf("%s is required", envPaxSessionID)
			}
			instruction, err := conversationInstruction(cmd.Args().Slice(), cmd.String("input-file"))
			if err != nil {
				return err
			}
			req := cloud.ConversationDeliveryRequest{
				Source: &cloud.ConversationDeliverySource{
					AgentID:               agentID,
					RepresentativeAgentID: representativeAgentID,
					SessionID:             sessionID,
				},
				Target: cloud.ConversationDeliveryTarget{
					Kind: conversationTargetKindActiveInvocation,
				},
				Context: cloud.ConversationDeliveryContext{
					LatestResponse: cmd.Bool("include-message"),
				},
				Instruction: instruction,
			}
			return sendConversationDelivery(ctx, cmd, agentID, &req)
		},
	}
}

func conversationInstruction(args []string, inputFile string) (string, error) {
	inputFile = strings.TrimSpace(inputFile)
	text := strings.TrimSpace(strings.Join(args, " "))
	if inputFile != "" && text != "" {
		return "", fmt.Errorf("provide either text or --input-file, not both")
	}
	if inputFile != "" {
		data, err := os.ReadFile(inputFile)
		if err != nil {
			return "", fmt.Errorf("read input file: %w", err)
		}
		if strings.TrimSpace(string(data)) == "" {
			return "", fmt.Errorf("input file is empty")
		}
		return string(data), nil
	}
	if text == "" {
		return "", fmt.Errorf("text or --input-file is required")
	}
	return text, nil
}

func sendConversationDelivery(ctx context.Context, cmd *cli.Command, agentID string, req *cloud.ConversationDeliveryRequest) error {
	resp, err := postConversationDelivery(ctx, agentID, req)
	if err != nil {
		return err
	}
	if !cmd.Bool("json") {
		fmt.Fprintln(commandWriter(cmd), conversationDeliverySummary(resp))
		return nil
	}
	encoder := json.NewEncoder(commandWriter(cmd))
	encoder.SetIndent("", "  ")
	return encoder.Encode(resp)
}

func postConversationDelivery(ctx context.Context, agentID string, req *cloud.ConversationDeliveryRequest) (*cloud.ConversationDeliveryResponse, error) {
	cfg, err := loadRuntimeConfig()
	if err != nil {
		return nil, err
	}
	client, err := conversationCloudClient(ctx, cfg, agentID)
	if err != nil {
		return nil, err
	}
	resp, err := client.PostConversationDelivery(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func conversationDeliverySummary(resp *cloud.ConversationDeliveryResponse) string {
	summary := []string{"sent"}
	if resp == nil {
		return strings.Join(summary, " ")
	}
	addSummaryField(&summary, "receipt", resp.ReceiptToken)
	addSummaryField(&summary, "target_session", conversationDeliveryTargetSessionID(resp.Delivery))
	return strings.Join(summary, " ")
}

func conversationDeliveryTargetSessionID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var delivery struct {
		TargetSession struct {
			SessionID string `json:"session_id"`
		} `json:"target_session"`
		Invocation struct {
			TargetSessionID string `json:"target_session_id"`
		} `json:"invocation"`
	}
	if err := json.Unmarshal(raw, &delivery); err != nil {
		return ""
	}
	return firstNonEmpty(delivery.TargetSession.SessionID, delivery.Invocation.TargetSessionID)
}

type mcpRPCRequest struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type mcpToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolCallResult struct {
	Content []mcpToolContent `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

func serveConversationMCP(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req mcpRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			if err := writeMCPResponse(out, mcpRPCResponse{
				JSONRPC: "2.0",
				Error:   &mcpRPCError{Code: -32700, Message: "parse error"},
			}); err != nil {
				return err
			}
			continue
		}
		resp, ok := handleConversationMCPRequest(ctx, req)
		if !ok {
			continue
		}
		if err := writeMCPResponse(out, resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func handleConversationMCPRequest(ctx context.Context, req mcpRPCRequest) (mcpRPCResponse, bool) {
	if len(req.ID) == 0 {
		return mcpRPCResponse{}, false
	}
	resp := mcpRPCResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "pax-conversation",
				"version": version,
			},
		}
	case "tools/list":
		resp.Result = map[string]any{"tools": conversationMCPTools()}
	case "tools/call":
		result, err := callConversationMCPTool(ctx, req.Params)
		if err != nil {
			resp.Error = &mcpRPCError{Code: -32602, Message: err.Error()}
			return resp, true
		}
		resp.Result = result
	default:
		resp.Error = &mcpRPCError{Code: -32601, Message: "method not found"}
	}
	return resp, true
}

func writeMCPResponse(out io.Writer, resp mcpRPCResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if _, err := out.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func conversationMCPTools() []map[string]any {
	return []map[string]any{
		{
			"name": "list_agents",
			"description": "List the agents you own so you can pick one to message. " +
				"Returns each agent's agent_id -- pass it to ask as to_agent_id. Each entry " +
				"includes a display name, alias, type, a short description, and current " +
				"reachability (status). Filter with a free-text query (matches name and alias) " +
				"and by status.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":    map[string]any{"type": "string"},
					"status":   map[string]any{"type": "string", "enum": []string{"online", "offline", "any"}},
					"order_by": map[string]any{"type": "string", "enum": []string{"relevance", "last_active", "name"}},
					"limit":    map[string]any{"type": "integer"},
				},
			},
		},
		{
			"name": "ask",
			"description": "Ask one of your agents (by to_agent_id from list_agents) or a Pax " +
				"representative agent (by to_representative_agent_id) and return a receipt token. " +
				"Provide exactly one of to_agent_id or to_representative_agent_id. With to_agent_id " +
				"you may set to_session_id to target a specific session of that agent; omit it to " +
				"open a new session.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"to_agent_id":                map[string]any{"type": "string"},
					"to_representative_agent_id": map[string]any{"type": "string"},
					"to_session_id":              map[string]any{"type": "string"},
					"text":                       map[string]any{"type": "string"},
					"input_file":                 map[string]any{"type": "string"},
					"include_message":            map[string]any{"type": "boolean"},
				},
			},
		},
		{
			"name":        "reply",
			"description": "Reply to the current session's active Pax conversation invocation.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text":            map[string]any{"type": "string"},
					"input_file":      map[string]any{"type": "string"},
					"include_message": map[string]any{"type": "boolean"},
				},
			},
		},
		{
			"name":        "publish_artifact",
			"description": "Deliver a completed file that the user asked to read, review, or download. Local paths are not visible to the user, so you MUST call this tool for user-facing deliverables such as plans, reports, documents, images, archives, and exported data, even when saved inside a repository. Do not publish ordinary code, test, config, or documentation changes, or temporary or internal working files. If the file itself is the requested outcome, publish it; if it is only part of changing the codebase, do not.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"path"},
				"properties": map[string]any{
					"path":  map[string]any{"type": "string"},
					"title": map[string]any{"type": "string"},
				},
			},
		},
	}
}

func callConversationMCPTool(ctx context.Context, rawParams json.RawMessage) (mcpToolCallResult, error) {
	var params mcpToolCallParams
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return mcpToolCallResult{}, fmt.Errorf("invalid tools/call params")
	}
	args := map[string]any{}
	if len(params.Arguments) > 0 {
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return mcpToolCallResult{}, fmt.Errorf("invalid tool arguments")
		}
	}
	switch params.Name {
	case "list_agents":
		return callConversationMCPListAgents(ctx, args), nil
	case "ask":
		return callConversationMCPAsk(ctx, args), nil
	case "reply":
		return callConversationMCPReply(ctx, args), nil
	case "publish_artifact":
		return callConversationMCPPublishArtifact(ctx, args), nil
	default:
		return mcpToolCallResult{}, fmt.Errorf("unknown conversation tool %q", params.Name)
	}
}

func callConversationMCPAsk(ctx context.Context, args map[string]any) mcpToolCallResult {
	agentID, representativeAgentID, sessionID, err := conversationMCPIdentity()
	if err != nil {
		return mcpTextResult(err.Error(), true)
	}
	toAgentID := strings.TrimSpace(mcpStringArg(args, "to_agent_id"))
	toRepresentativeAgentID := strings.TrimSpace(mcpStringArg(args, "to_representative_agent_id"))
	toSessionID := strings.TrimSpace(mcpStringArg(args, "to_session_id"))
	target, err := conversationAskTarget(toAgentID, toRepresentativeAgentID, toSessionID)
	if err != nil {
		return mcpTextResult(err.Error(), true)
	}
	instruction, err := conversationInstructionFromToolArgs(args)
	if err != nil {
		return mcpTextResult(err.Error(), true)
	}
	req := cloud.ConversationDeliveryRequest{
		Source: &cloud.ConversationDeliverySource{
			AgentID:               agentID,
			RepresentativeAgentID: representativeAgentID,
			SessionID:             sessionID,
		},
		Target: target,
		Context: cloud.ConversationDeliveryContext{
			LatestResponse: mcpBoolArg(args, "include_message"),
		},
		Instruction: instruction,
	}
	resp, err := postConversationDelivery(ctx, agentID, &req)
	if err != nil {
		return mcpTextResult(err.Error(), true)
	}
	return mcpTextResult(conversationDeliverySummary(resp), false)
}

// conversationAskTarget builds the delivery target for ask. Exactly one of
// to_agent_id or to_representative_agent_id must be provided: agent_id addresses
// a runtime agent directly (the manager maps it to its canonical
// representative), while representative_agent_id keeps the original addressing.
// to_session_id applies only to the agent path: given, it targets that session;
// absent, the manager opens a new session.
func conversationAskTarget(
	toAgentID, toRepresentativeAgentID, toSessionID string,
) (cloud.ConversationDeliveryTarget, error) {
	switch {
	case toAgentID != "" && toRepresentativeAgentID != "":
		return cloud.ConversationDeliveryTarget{}, fmt.Errorf(
			"provide either to_agent_id or to_representative_agent_id, not both")
	case toAgentID != "":
		return cloud.ConversationDeliveryTarget{
			Kind:      conversationTargetKindAgent,
			AgentID:   toAgentID,
			SessionID: toSessionID,
		}, nil
	case toRepresentativeAgentID != "":
		return cloud.ConversationDeliveryTarget{
			Kind:                  conversationTargetKindRepresentative,
			RepresentativeAgentID: toRepresentativeAgentID,
		}, nil
	default:
		return cloud.ConversationDeliveryTarget{}, fmt.Errorf(
			"to_agent_id or to_representative_agent_id is required")
	}
}

func callConversationMCPListAgents(ctx context.Context, args map[string]any) mcpToolCallResult {
	agentID := strings.TrimSpace(os.Getenv(envPaxAgentID))
	if agentID == "" {
		return mcpTextResult(envPaxAgentID+" is required", true)
	}
	agents, err := listOwnerAgents(ctx, cloud.ListOwnerAgentsParams{
		FromAgentID: agentID,
		Query:       mcpStringArg(args, "query"),
		Status:      mcpStringArg(args, "status"),
		OrderBy:     mcpStringArg(args, "order_by"),
		Limit:       mcpIntArg(args, "limit"),
	})
	if err != nil {
		return mcpTextResult(err.Error(), true)
	}
	raw, err := json.Marshal(agents)
	if err != nil {
		return mcpTextResult("failed to encode agents", true)
	}
	return mcpTextResult(string(raw), false)
}

var listOwnerAgents = func(
	ctx context.Context,
	params cloud.ListOwnerAgentsParams,
) ([]cloud.OwnerAgentView, error) {
	cfg, err := loadRuntimeConfig()
	if err != nil {
		return nil, err
	}
	client, err := conversationCloudClient(ctx, cfg, params.FromAgentID)
	if err != nil {
		return nil, err
	}
	return client.ListOwnerAgents(params)
}

func callConversationMCPReply(ctx context.Context, args map[string]any) mcpToolCallResult {
	agentID, representativeAgentID, sessionID, err := conversationMCPIdentity()
	if err != nil {
		return mcpTextResult(err.Error(), true)
	}
	instruction, err := conversationInstructionFromToolArgs(args)
	if err != nil {
		return mcpTextResult(err.Error(), true)
	}
	req := cloud.ConversationDeliveryRequest{
		Source: &cloud.ConversationDeliverySource{
			AgentID:               agentID,
			RepresentativeAgentID: representativeAgentID,
			SessionID:             sessionID,
		},
		Target: cloud.ConversationDeliveryTarget{
			Kind: conversationTargetKindActiveInvocation,
		},
		Context: cloud.ConversationDeliveryContext{
			LatestResponse: mcpBoolArg(args, "include_message"),
		},
		Instruction: instruction,
	}
	resp, err := postConversationDelivery(ctx, agentID, &req)
	if err != nil {
		return mcpTextResult(err.Error(), true)
	}
	return mcpTextResult(conversationDeliverySummary(resp), false)
}

func conversationMCPIdentity() (string, string, string, error) {
	agentID := strings.TrimSpace(os.Getenv(envPaxAgentID))
	representativeAgentID := strings.TrimSpace(os.Getenv(envPaxRepresentativeAgentID))
	sessionID := strings.TrimSpace(os.Getenv(envPaxSessionID))
	if agentID == "" {
		return "", "", "", fmt.Errorf("%s is required", envPaxAgentID)
	}
	if sessionID == "" {
		return "", "", "", fmt.Errorf("%s is required", envPaxSessionID)
	}
	return agentID, representativeAgentID, sessionID, nil
}

func conversationInstructionFromToolArgs(args map[string]any) (string, error) {
	return conversationInstruction(
		[]string{mcpStringArg(args, "text")},
		mcpStringArg(args, "input_file"),
	)
}

func mcpTextResult(text string, isError bool) mcpToolCallResult {
	return mcpToolCallResult{
		Content: []mcpToolContent{{Type: "text", Text: text}},
		IsError: isError,
	}
}

func mcpStringArg(args map[string]any, name string) string {
	value, ok := args[name]
	if !ok || value == nil {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func mcpBoolArg(args map[string]any, name string) bool {
	value, ok := args[name]
	if !ok || value == nil {
		return false
	}
	v, ok := value.(bool)
	return ok && v
}

func mcpIntArg(args map[string]any, name string) int {
	value, ok := args[name]
	if !ok || value == nil {
		return 0
	}
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	default:
		return 0
	}
}

func addSummaryField(summary *[]string, name string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	*summary = append(*summary, name+"="+value)
}

func conversationCloudClient(ctx context.Context, cfg *config.Config, agentID string) (*cloud.Client, error) {
	store, closeStore, err := openDaemonStore(cfg.Daemon.DBPath)
	if err != nil {
		return nil, err
	}
	defer closeStore()

	conn, err := findAgentConnectionForCloudAgent(ctx, store, agentID)
	if err != nil {
		return nil, err
	}
	material, err := store.GetRemoteAuthMaterial(ctx, conn.RemoteID)
	if err != nil {
		return nil, fmt.Errorf("get remote auth for agent %q: %w", agentID, err)
	}
	if strings.TrimSpace(material.CloudAPIURL) == "" {
		return nil, fmt.Errorf("remote %q has no cloud API URL", conn.RemoteID)
	}
	resolver := auth.NewDefaultResolver()
	nodeKey, err := resolver.Resolve(ctx, material.CloudAPIKeyRef)
	if err != nil {
		return nil, fmt.Errorf("resolve node key for remote %q: %w", conn.RemoteID, err)
	}
	if strings.TrimSpace(nodeKey) == "" {
		return nil, fmt.Errorf("node key for remote %q is empty", conn.RemoteID)
	}
	client := cloud.NewClient(material.CloudAPIURL, nodeKey)
	return client, nil
}

type agentConnectionLister interface {
	ListAgentConnections(context.Context, control.ListAgentConnectionsQuery) ([]control.AgentConnectionView, error)
}

func findAgentConnectionForCloudAgent(ctx context.Context, store agentConnectionLister, agentID string) (control.AgentConnectionView, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return control.AgentConnectionView{}, fmt.Errorf("%s is required", envPaxAgentID)
	}
	conns, err := store.ListAgentConnections(ctx, control.ListAgentConnectionsQuery{IncludeDisabled: true})
	if err != nil {
		return control.AgentConnectionView{}, err
	}
	var found *control.AgentConnectionView
	for i := range conns {
		if strings.TrimSpace(conns[i].CloudAgentID) != agentID {
			continue
		}
		if found != nil {
			return control.AgentConnectionView{}, fmt.Errorf("multiple agent connections use %s=%q", envPaxAgentID, agentID)
		}
		copy := conns[i]
		found = &copy
	}
	if found == nil {
		return control.AgentConnectionView{}, fmt.Errorf("no agent connection found for %s=%q", envPaxAgentID, agentID)
	}
	return *found, nil
}
