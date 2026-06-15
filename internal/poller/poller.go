// Package poller processes incoming messages from Cloud and reconciles orphans.
//
// ProcessMessage is called for each message received via WebSocket push.
// ReconcileOrphans is called on a ticker to retry skipped messages.
package poller

import (
	"context"
	"log"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/executor"
	"github.com/pax-beehive/paxd/internal/store"
)

// Poller processes Cloud messages arriving via WebSocket push.
type Poller struct {
	cloudClient *cloud.Client
	wsClient    *cloud.WSClient
	executors   map[string]*executor.Executor
	store       *store.Store
}

// New creates a new Poller.
func New(
	cloudClient *cloud.Client,
	wsClient *cloud.WSClient,
	executors map[string]*executor.Executor,
	s *store.Store,
) *Poller {
	return &Poller{
		cloudClient: cloudClient,
		wsClient:    wsClient,
		executors:   executors,
		store:       s,
	}
}

// PollMailbox pulls node and agent mailboxes over the v1 node HTTP API.
func (p *Poller) PollMailbox(ctx context.Context, offset int64, limit int) (int64, error) {
	maxOffset := offset
	nodeMessages, nextOffset, _, err := p.cloudClient.FetchNodeMessages(offset, limit)
	if err != nil {
		return maxOffset, err
	}
	if nextOffset > maxOffset {
		maxOffset = nextOffset
	}
	for _, msg := range nodeMessages {
		if err := p.ProcessMessage(ctx, msg); err != nil {
			log.Printf("[poller] node message error: %v", err)
		}
	}

	for agentID := range p.executors {
		messages, nextOffset, _, err := p.cloudClient.FetchAgentMessages(agentID, offset, limit)
		if err != nil {
			log.Printf("[poller] fetch agent %s messages error: %v", agentID, err)
			continue
		}
		if nextOffset > maxOffset {
			maxOffset = nextOffset
		}
		for _, msg := range messages {
			if err := p.ProcessMessage(ctx, msg); err != nil {
				log.Printf("[poller] agent %s message error: %v", agentID, err)
			}
		}
	}

	if maxOffset > offset {
		if err := p.cloudClient.UpdateOffset(maxOffset); err != nil {
			log.Printf("[poller] update cloud offset error: %v", err)
		}
		if err := p.store.UpdateNodeOffset(maxOffset); err != nil {
			log.Printf("[poller] update local offset error: %v", err)
		}
	}
	return maxOffset, nil
}

// ProcessMessage executes a single Cloud message and creates an outbound result.
func (p *Poller) ProcessMessage(ctx context.Context, msg cloud.Message) error {
	// Mark as delivered
	if err := p.cloudClient.MarkDelivered(msg.MessageID); err != nil {
		log.Printf("[poller] mark delivered error: %v", err)
	}

	exec := p.executors[msg.AgentID]
	if exec == nil {
		err := p.handleUnknownAgent(msg)
		if err != nil {
			p.cloudClient.ReportFailure(msg.MessageID, err.Error())
		}
		return err
	}

	result, err := exec.Execute(ctx, msg)
	if err != nil {
		log.Printf("[poller] execute error for %s: %v", msg.MessageID, err)
		p.cloudClient.ReportFailure(msg.MessageID, err.Error())
		return err
	}

	// nil result = message was orphaned (session running)
	if result == nil {
		return nil
	}

	// Send each event directly via WebSocket as raw model.* JSON
	// Skip if events were already sent via streaming (OnEvent callback)
	if p.wsClient != nil {
		for _, ev := range result.Events {
			if err := p.wsClient.SendJSON(ev); err != nil {
				log.Printf("[poller] ws send event error: %v", err)
			}
		}
	}

	outbound := outboundFromResult(msg, result)
	if err := p.cloudClient.ReportCompleted(msg.MessageID, outbound); err != nil {
		log.Printf("[poller] report completed error: %v", err)
	}
	if err := p.cloudClient.CreateOutbound(outbound); err != nil {
		log.Printf("[poller] create outbound error: %v", err)
	}

	return nil
}

// ReconcileOrphans replays skipped messages when their sessions become idle.
func (p *Poller) ReconcileOrphans(ctx context.Context) error {
	orphans, err := p.store.ListOrphaned()
	if err != nil {
		return err
	}

	if len(orphans) == 0 {
		return nil
	}

	log.Printf("[poller] reconciling %d orphaned messages", len(orphans))

	for _, orphan := range orphans {
		exec := p.executors[orphan.AgentID]
		if exec == nil || !exec.IsSessionIdle(ctx, orphan.SessionID) {
			continue
		}

		msg := cloud.Message{
			MessageID: orphan.MessageID,
			AgentID:   orphan.AgentID,
			SessionID: orphan.SessionID,
			Type:      "chat",
			Content:   orphan.Content,
			CreatedAt: orphan.CreatedAt,
		}

		result, err := exec.Execute(ctx, msg)
		if err != nil {
			log.Printf("[poller] orphan retry failed for %s: %v", orphan.MessageID, err)
			p.store.IncrementOrphanedRetry(orphan.MessageID)
			continue
		}

		if result != nil {
			outbound := &cloud.OutboundMessage{
				AgentID:     orphan.AgentID,
				SessionID:   orphan.SessionID,
				MessageType: "turn_result",
				Content:     firstNonEmpty(result.Content, result.Status, "completed"),
				ParentMsgID: orphan.MessageID,
				TurnID:      result.TurnID,
				ResponseID:  result.ResponseID,
				Status:      result.Status,
				Events:      result.Events,
				FileChanges: result.FileChanges,
				TokenUsage:  result.TokenUsage,
			}
			p.cloudClient.CreateOutbound(outbound)
		}

		p.store.DeleteOrphaned(orphan.MessageID)
	}

	return nil
}

func (p *Poller) handleUnknownAgent(msg cloud.Message) error {
	if msg.Type == "command" && msg.AgentID != "" {
		return nil
	}
	return &UnknownAgentError{AgentID: msg.AgentID}
}

// UnknownAgentError reports a mailbox item for an agent this process does not host.
type UnknownAgentError struct {
	AgentID string
}

func (e *UnknownAgentError) Error() string {
	if e.AgentID == "" {
		return "message has no agent_id"
	}
	return "unknown agent_id: " + e.AgentID
}

func outboundFromResult(msg cloud.Message, result *executor.TurnResult) *cloud.OutboundMessage {
	return &cloud.OutboundMessage{
		AgentID:     msg.AgentID,
		SessionID:   firstNonEmpty(result.SessionID, msg.SessionID),
		MessageType: "turn_result",
		Content:     firstNonEmpty(result.Content, result.Status, "completed"),
		ParentMsgID: msg.MessageID,
		TurnID:      result.TurnID,
		ResponseID:  result.ResponseID,
		Status:      result.Status,
		Events:      result.Events,
		FileChanges: result.FileChanges,
		TokenUsage:  result.TokenUsage,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
