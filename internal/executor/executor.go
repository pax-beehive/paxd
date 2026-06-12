// Package executor handles execution of incoming messages against Hermes,
// producing structured model.* event batches for Cloud storage.
package executor

import (
	"context"
	"fmt"
	"log"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/hermes"
	"github.com/pax-beehive/paxd/internal/store"
	"github.com/pax-beehive/paxd/pkg/model"
)

// Executor runs messages against Hermes and returns structured turn results.
type Executor struct {
	hermesClient *hermes.Client
	cloudClient  *cloud.Client
	store        *store.Store
	agentID      string
	OnEvent      func(any) // called for each event as it arrives (nil = buffer all)
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

// TurnResult is the structured output of message execution.
type TurnResult struct {
	SessionID   string
	TurnID      string
	ResponseID  string
	Status      string // "completed" | "cancelled" | "error"
	Events      []any  // model.* structs
	FileChanges []*model.FileChange
}

// Execute dispatches a message by type and returns structured results.
func (e *Executor) Execute(ctx context.Context, msg cloud.Message) (*TurnResult, error) {
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
func (e *Executor) executeChat(ctx context.Context, msg cloud.Message) (*TurnResult, error) {
	session, err := e.hermesClient.GetSessionStatus(msg.SessionID)
	if err != nil {
		return nil, fmt.Errorf("get session status: %w", err)
	}

	// nil session = not found → treat as idle and execute
	if session != nil && session.Status == "running" {
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
		return nil, nil // nil result = skipped
	}

	// Idle → execute
	var turn *hermes.Turn
	if e.OnEvent != nil {
		turn, err = e.hermesClient.StreamTurnLive(ctx, "", msg.SessionID, msg.Content, e.OnEvent)
	} else {
		turn, err = e.hermesClient.StreamTurn(ctx, "", msg.SessionID, msg.Content)
	}
	if err != nil && turn == nil {
		return nil, fmt.Errorf("stream turn: %w", err)
	}

	return &TurnResult{
		SessionID:   turn.SessionID,
		TurnID:      turn.TurnID,
		ResponseID:  turn.ResponseID,
		Status:      turn.Status,
		Events:      turn.Events, // nil when streaming via OnEvent
		FileChanges: turn.FileChanges,
	}, err
}

// executeSteer stops the current run then sends the steer message.
func (e *Executor) executeSteer(ctx context.Context, msg cloud.Message) (*TurnResult, error) {
	log.Printf("[executor] steering session %s", msg.SessionID)

	// Stop the current run
	if err := e.hermesClient.StopRun(msg.SessionID); err != nil {
		log.Printf("[executor] stop run error (non-fatal): %v", err)
	}

	var turn *hermes.Turn
	var err error
	if e.OnEvent != nil {
		turn, err = e.hermesClient.StreamTurnLive(ctx, "", msg.SessionID, msg.Content, e.OnEvent)
	} else {
		turn, err = e.hermesClient.StreamTurn(ctx, "", msg.SessionID, msg.Content)
	}
	if err != nil && turn == nil {
		return nil, fmt.Errorf("steer turn: %w", err)
	}

	return &TurnResult{
		SessionID:   turn.SessionID,
		TurnID:      turn.TurnID,
		ResponseID:  turn.ResponseID,
		Status:      turn.Status,
		Events:      turn.Events,
		FileChanges: turn.FileChanges,
	}, err
}

// executeCommand handles local commands like upgrade, reconfig, status.
func (e *Executor) executeCommand(ctx context.Context, msg cloud.Message) (*TurnResult, error) {
	log.Printf("[executor] received command: %s", msg.Content)
	return &TurnResult{
		Status: "completed",
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
