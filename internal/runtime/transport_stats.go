package runtime

import (
	"context"
	"sync"

	"github.com/pax-beehive/paxkit/reliablemq"
)

// TransportStatsTracker wraps a ReliableEngineFactory and remembers every
// producer it hands out, so diagnostics can report per-queue transport stats.
type TransportStatsTracker struct {
	inner ReliableEngineFactory

	mu        sync.Mutex
	producers map[string]*reliablemq.Producer
}

func NewTransportStatsTracker(inner ReliableEngineFactory) *TransportStatsTracker {
	return &TransportStatsTracker{
		inner:     inner,
		producers: make(map[string]*reliablemq.Producer),
	}
}

func (t *TransportStatsTracker) Producer(
	ctx context.Context,
	queueID string,
	stream reliablemq.Stream,
) (*reliablemq.Producer, error) {
	producer, err := t.inner.Producer(ctx, queueID, stream)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.producers[queueID] = producer
	t.mu.Unlock()
	return producer, nil
}

func (t *TransportStatsTracker) NewReliableEngine(
	producer *reliablemq.Producer,
	dispatcher reliablemq.Dispatcher,
) ReliableEngine {
	return t.inner.NewReliableEngine(producer, dispatcher)
}

func (t *TransportStatsTracker) Close(ctx context.Context) error {
	if closer, ok := t.inner.(interface{ Close(context.Context) error }); ok {
		return closer.Close(ctx)
	}
	return nil
}

func (t *TransportStatsTracker) Stats() map[string]reliablemq.ProducerStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]reliablemq.ProducerStats, len(t.producers))
	for queueID, producer := range t.producers {
		out[queueID] = producer.Stats()
	}
	return out
}
