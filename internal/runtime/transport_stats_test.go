package runtime

import (
	"context"
	"testing"

	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/pax-beehive/paxkit/reliablemq/memory"
)

func TestTransportStatsTrackerRecordsProducers(t *testing.T) {
	inner := ReliableEngineFromStore(memory.New())
	tracker := NewTransportStatsTracker(inner)

	if got := tracker.Stats(); len(got) != 0 {
		t.Fatalf("Stats() before Producer = %v, want empty", got)
	}
	if _, err := tracker.Producer(context.Background(), "queue-1", reliablemq.StreamACP); err != nil {
		t.Fatalf("Producer() error = %v", err)
	}
	stats := tracker.Stats()
	if _, ok := stats["queue-1"]; !ok {
		t.Fatalf("Stats() = %v, want entry for queue-1", stats)
	}
}

func TestTransportStatsTrackerDelegatesClose(t *testing.T) {
	inner := ReliableEngineFromStore(memory.New())
	tracker := NewTransportStatsTracker(inner)
	if _, err := tracker.Producer(context.Background(), "queue-1", reliablemq.StreamACP); err != nil {
		t.Fatalf("Producer() error = %v", err)
	}
	if err := tracker.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}
