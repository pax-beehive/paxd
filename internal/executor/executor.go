// Package executor handles execution of incoming messages against Hermes.
//
// Message types:
//   - chat: send to Hermes chat/stream; skip if session is running (orphan)
//   - steer: stop current run then send chat/stream
//   - command: local processing (upgrade, reconfig, status)
package executor

import (
	"context"
	"fmt"
	"log"

	"github.com/toddzheng/paxd/internal/cloud"
	"github.com/toddzheng/paxd/internal/hermes"
	"github.com/toddzheng/paxd/internal/store"
)

// Executor runs messages against Hermes.
type Executor struct {
	hermesClient *hermes.Client
	cloudClient  *cloud.Client
	store        *store.Store
	agentID      string
}

// New creates a new Executor.
func New(hermesClient *hermes.Client, cloudClient *cloud.Client, s *store.Store, agentID string) *Executor {
	return &Executor{
		hermesClient: hermesClient,
		cloudClient:  cloudClient,
		store:        s,
		agentID:      agentID,
	}
}

// ExecuteResult holds the outcome of message execution.
type ExecuteResult struct {
	OutboundMessageID string
	Response          string
}

// Execute dispatches a message by type and returns the result.
func (e *Executor) Execute(ctx context.Context, msg cloud.Message) (*ExecuteResult, error) {
	switch msg.Type {
	case "chat":
		return e.executeChat(ctx, msg)
	case "steer":
		return e.executeSteer(ctx, msg)
	case "command":
		return e.executeCommand(ctx, msg)
	default:
		return nil, fmt.Errorf("unknown message type: %s", msg.Type)
	}
}

// executeChat sends a message to Hermes. If the session is running,
// the message is skipped and saved as orphaned.
func (e *Executor) executeChat(ctx context.Context, msg cloud.Message) (*ExecuteResult, error) {
	session, err := e.hermesClient.GetSessionStatus(msg.SessionID)
	if err != nil {
		return nil, fmt.Errorf("get session status: %w", err)
	}

	// If session is running, skip and orphan this message
	if session.Status == "running" {
		log.Printf("[executor] session %s is running, orphaning chat message %s", msg.SessionID, msg.MessageID)
		orphan := &store.OrphanedMessage{
			MessageID: msg.MessageID,
			AgentID:   e.agentID,
			SessionID: msg.SessionID,
			Content:   msg.Content,
			CreatedAt: msg.CreatedAt,
		}
		if err := e.store.SaveOrphaned(orphan); err != nil {
			return nil, fmt.Errorf("save orphaned: %w", err)
		}
		return nil, nil // nil result = skipped, don't create outbound
	}

	// Session is idle — execute
	response, err := e.hermesClient.ChatStream(msg.SessionID, msg.Content, nil)
	if err != nil {
		return nil, fmt.Errorf("chat stream: %w", err)
	}

	return &ExecuteResult{Response: response}, nil
}

// executeSteer stops the current run then sends the steer message.
func (e *Executor) executeSteer(ctx context.Context, msg cloud.Message) (*ExecuteResult, error) {
	log.Printf("[executor] steering session %s", msg.SessionID)

	// Stop the current run
	if err := e.hermesClient.StopRun(msg.SessionID); err != nil {
		log.Printf("[executor] stop run error (non-fatal): %v", err)
		// Continue anyway — the session might already be stopped
	}

	// Send the steer message
	response, err := e.hermesClient.ChatStream(msg.SessionID, msg.Content, nil)
	if err != nil {
		return nil, fmt.Errorf("steer chat stream: %w", err)
	}

	return &ExecuteResult{Response: response}, nil
}

// executeCommand handles local commands like upgrade, reconfig, status.
func (e *Executor) executeCommand(ctx context.Context, msg cloud.Message) (*ExecuteResult, error) {
	log.Printf("[executor] received command: %s", msg.Content)

	// Command format: "command_name [args...]"
	// Full implementation in cmd/paxd handles the actual command dispatch.
	// For now, acknowledge the command.
	return &ExecuteResult{
		Response: fmt.Sprintf("command acknowledged: %s", msg.Content),
	}, nil
}

// IsSessionIdle checks if a session is idle (not running).
func (e *Executor) IsSessionIdle(ctx context.Context, sessionID string) bool {
	session, err := e.hermesClient.GetSessionStatus(sessionID)
	if err != nil {
		return false
	}
	return session.Status != "running"
}
