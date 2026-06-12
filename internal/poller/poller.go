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
	executor    *executor.Executor
	store       *store.Store
}

// New creates a new Poller.
func New(cloudClient *cloud.Client, exec *executor.Executor, s *store.Store) *Poller {
	return &Poller{
		cloudClient: cloudClient,
		executor:    exec,
		store:       s,
	}
}

// ProcessMessage executes a single Cloud message and creates an outbound result.
func (p *Poller) ProcessMessage(ctx context.Context, msg cloud.Message) error {
	// Mark as delivered
	if err := p.cloudClient.MarkDelivered(msg.MessageID); err != nil {
		log.Printf("[poller] mark delivered error: %v", err)
	}

	result, err := p.executor.Execute(ctx, msg)
	if err != nil {
		log.Printf("[poller] execute error for %s: %v", msg.MessageID, err)
		p.cloudClient.ReportFailure(msg.MessageID, err.Error())
		return err
	}

	// nil result = message was orphaned (session running)
	if result == nil {
		return nil
	}

	// Create structured outbound message with turn events
	outbound := &cloud.OutboundMessage{
		AgentID:     msg.AgentID,
		SessionID:   msg.SessionID,
		Type:        "turn_result",
		ParentMsgID: msg.MessageID,
		TurnID:      result.TurnID,
		ResponseID:  result.ResponseID,
		Status:      result.Status,
		Events:      result.Events,
		FileChanges: result.FileChanges,
	}
	if err := p.cloudClient.CreateOutbound(outbound); err != nil {
		log.Printf("[poller] create outbound error: %v", err)
	}

	if err := p.cloudClient.ReportCompleted(msg.MessageID, result.ResponseID); err != nil {
		log.Printf("[poller] report completed error: %v", err)
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
		if !p.executor.IsSessionIdle(ctx, orphan.SessionID) {
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

		result, err := p.executor.Execute(ctx, msg)
		if err != nil {
			log.Printf("[poller] orphan retry failed for %s: %v", orphan.MessageID, err)
			p.store.IncrementOrphanedRetry(orphan.MessageID)
			continue
		}

		if result != nil {
			outbound := &cloud.OutboundMessage{
				AgentID:     orphan.AgentID,
				SessionID:   orphan.SessionID,
				Type:        "turn_result",
				ParentMsgID: orphan.MessageID,
				TurnID:      result.TurnID,
				ResponseID:  result.ResponseID,
				Status:      result.Status,
				Events:      result.Events,
				FileChanges: result.FileChanges,
			}
			p.cloudClient.CreateOutbound(outbound)
		}

		p.store.DeleteOrphaned(orphan.MessageID)
	}

	return nil
}
