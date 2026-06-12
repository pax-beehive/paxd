// Package poller fetches messages from the Fleet Cloud API and dispatches
// them to the executor. Also handles orphan reconciliation.
package poller

import (
	"context"
	"fmt"
	"log"

	"github.com/toddzheng/paxd/internal/cloud"
	"github.com/toddzheng/paxd/internal/executor"
	"github.com/toddzheng/paxd/internal/store"
)

// Poller pulls messages from Cloud and dispatches them for execution.
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

// PollAndExecute fetches new messages, dispatches each, and reconciles orphans.
func (p *Poller) PollAndExecute(ctx context.Context) error {
	state, err := p.store.GetAgentState()
	if err != nil {
		return fmt.Errorf("get agent state: %w", err)
	}
	if state == nil {
		return fmt.Errorf("agent not registered")
	}

	// Fetch messages from the last offset
	msgs, err := p.cloudClient.FetchMessages(state.LastOffset)
	if err != nil {
		return fmt.Errorf("fetch messages: %w", err)
	}

	if len(msgs) > 0 {
		log.Printf("[poller] fetched %d new messages (offset=%d)", len(msgs), state.LastOffset)
	}

	var maxOffset int64
	for _, msg := range msgs {
		// Mark as delivered
		if err := p.cloudClient.MarkDelivered(msg.MessageID); err != nil {
			log.Printf("[poller] mark delivered error: %v", err)
		}

		result, err := p.executor.Execute(ctx, msg)
		if err != nil {
			log.Printf("[poller] execute error for %s: %v", msg.MessageID, err)
			p.cloudClient.ReportFailure(msg.MessageID, err.Error())
			continue
		}

		// nil result means the message was skipped (e.g., orphaned)
		if result == nil {
			maxOffset = msg.ID
			continue
		}

		// Create outbound message
		outbound := &cloud.OutboundMessage{
			AgentID:     msg.AgentID,
			SessionID:   msg.SessionID,
			Type:        "chat_response",
			Content:     result.Response,
			ParentMsgID: msg.MessageID,
		}
		if err := p.cloudClient.CreateOutbound(outbound); err != nil {
			log.Printf("[poller] create outbound error: %v", err)
		}
		if err := p.cloudClient.ReportCompleted(msg.MessageID, ""); err != nil {
			log.Printf("[poller] report completed error: %v", err)
		}

		maxOffset = msg.ID
	}

	// Update the local offset
	if maxOffset > 0 {
		if err := p.store.UpdateOffset(maxOffset); err != nil {
			return fmt.Errorf("update offset: %w", err)
		}
		if err := p.cloudClient.UpdateOffset(maxOffset); err != nil {
			log.Printf("[poller] cloud update offset error: %v", err)
		}
	}

	// Reconcile orphaned messages
	if err := p.reconcileOrphans(ctx); err != nil {
		log.Printf("[poller] orphan reconcile error: %v", err)
	}

	return nil
}

// reconcileOrphans replays skipped messages when their sessions become idle.
func (p *Poller) reconcileOrphans(ctx context.Context) error {
	orphans, err := p.store.ListOrphaned()
	if err != nil {
		return fmt.Errorf("list orphaned: %w", err)
	}

	if len(orphans) == 0 {
		return nil
	}

	log.Printf("[poller] reconciling %d orphaned messages", len(orphans))

	for _, orphan := range orphans {
		// Check if session is now idle
		if !p.executor.IsSessionIdle(ctx, orphan.SessionID) {
			continue
		}

		// Rebuild a Message from the orphaned record
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
			if err := p.store.IncrementOrphanedRetry(orphan.MessageID); err != nil {
				log.Printf("[poller] increment retry error: %v", err)
			}
			continue
		}

		// Success — create outbound and delete orphan
		if result != nil {
			outbound := &cloud.OutboundMessage{
				AgentID:     orphan.AgentID,
				SessionID:   orphan.SessionID,
				Type:        "chat_response",
				Content:     result.Response,
				ParentMsgID: orphan.MessageID,
			}
			if err := p.cloudClient.CreateOutbound(outbound); err != nil {
				log.Printf("[poller] orphan outbound error: %v", err)
			}
		}

		if err := p.store.DeleteOrphaned(orphan.MessageID); err != nil {
			log.Printf("[poller] delete orphan error: %v", err)
		}
	}

	return nil
}
