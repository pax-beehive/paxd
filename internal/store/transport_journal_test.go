package store

import (
	"path/filepath"
	"testing"
)

func TestTransportJournalSavesAndListsFrames(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()

	next, err := s.NextTransportSeq("agent_1", TransportStreamPaxdToManager, TransportDirectionOutbound)
	if err != nil {
		t.Fatalf("NextTransportSeq() error = %v", err)
	}
	if next != 1 {
		t.Fatalf("first seq = %d, want 1", next)
	}

	frame := &TransportFrame{
		AgentID:        "agent_1",
		Stream:         TransportStreamPaxdToManager,
		Seq:            next,
		LocalDirection: TransportDirectionOutbound,
		PayloadJSON:    `{"jsonrpc":"2.0","id":1,"result":{}}`,
	}
	if err := s.SaveTransportFrame(frame); err != nil {
		t.Fatalf("SaveTransportFrame() error = %v", err)
	}
	if frame.ID == 0 {
		t.Fatal("SaveTransportFrame() did not populate ID")
	}
	if frame.Status != TransportStatusPending {
		t.Fatalf("default outbound status = %q, want pending", frame.Status)
	}

	frames, err := s.ListTransportFrames(
		"agent_1",
		TransportStreamPaxdToManager,
		TransportDirectionOutbound,
		[]string{TransportStatusPending},
		10,
	)
	if err != nil {
		t.Fatalf("ListTransportFrames() error = %v", err)
	}
	if len(frames) != 1 || frames[0].Seq != 1 || frames[0].PayloadJSON != frame.PayloadJSON {
		t.Fatalf("frames = %+v", frames)
	}
}

func TestTransportJournalIgnoresDuplicateInboundFrame(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()

	frame := &TransportFrame{
		AgentID:        "agent_1",
		Stream:         TransportStreamManagerToPaxd,
		Seq:            42,
		LocalDirection: TransportDirectionInbound,
		PayloadJSON:    `{"jsonrpc":"2.0","id":42,"method":"session/prompt"}`,
	}
	inserted, err := s.SaveTransportFrameIfAbsent(frame)
	if err != nil {
		t.Fatalf("first SaveTransportFrameIfAbsent() error = %v", err)
	}
	if !inserted {
		t.Fatal("first insert inserted = false")
	}
	if frame.Status != TransportStatusReceived {
		t.Fatalf("default inbound status = %q, want received", frame.Status)
	}

	dupe := *frame
	dupe.PayloadJSON = `{"jsonrpc":"2.0","id":42,"method":"different"}`
	inserted, err = s.SaveTransportFrameIfAbsent(&dupe)
	if err != nil {
		t.Fatalf("duplicate SaveTransportFrameIfAbsent() error = %v", err)
	}
	if inserted {
		t.Fatal("duplicate insert inserted = true")
	}

	got, err := s.GetTransportFrame(
		"agent_1",
		TransportStreamManagerToPaxd,
		42,
		TransportDirectionInbound,
	)
	if err != nil {
		t.Fatalf("GetTransportFrame() error = %v", err)
	}
	if got == nil || got.PayloadJSON != frame.PayloadJSON {
		t.Fatalf("stored frame = %+v", got)
	}
}

func TestTransportJournalAcksOutboundFramesThroughSeq(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()

	for seq := int64(1); seq <= 3; seq++ {
		if err := s.SaveTransportFrame(&TransportFrame{
			AgentID:        "agent_1",
			Stream:         TransportStreamPaxdToManager,
			Seq:            seq,
			LocalDirection: TransportDirectionOutbound,
			PayloadJSON:    `{"jsonrpc":"2.0","method":"session/update"}`,
		}); err != nil {
			t.Fatalf("SaveTransportFrame(%d) error = %v", seq, err)
		}
	}

	if err := s.AckOutboundTransportFrames("agent_1", TransportStreamPaxdToManager, 2); err != nil {
		t.Fatalf("AckOutboundTransportFrames() error = %v", err)
	}

	acked, err := s.ListTransportFrames(
		"agent_1",
		TransportStreamPaxdToManager,
		TransportDirectionOutbound,
		[]string{TransportStatusAcked},
		10,
	)
	if err != nil {
		t.Fatalf("List acked error = %v", err)
	}
	if len(acked) != 2 || acked[0].Seq != 1 || acked[1].Seq != 2 {
		t.Fatalf("acked frames = %+v", acked)
	}

	pending, err := s.ListTransportFrames(
		"agent_1",
		TransportStreamPaxdToManager,
		TransportDirectionOutbound,
		[]string{TransportStatusPending},
		10,
	)
	if err != nil {
		t.Fatalf("List pending error = %v", err)
	}
	if len(pending) != 1 || pending[0].Seq != 3 {
		t.Fatalf("pending frames = %+v", pending)
	}
}

func TestTransportJournalAckDecisionTable(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()

	cases := []struct {
		name      string
		agentID   string
		stream    string
		direction string
		seq       int64
		status    string
		want      string
	}{
		{
			name:      "same agent same stream outbound through acked seq",
			agentID:   "agent_1",
			stream:    TransportStreamPaxdToManager,
			direction: TransportDirectionOutbound,
			seq:       1,
			status:    TransportStatusSent,
			want:      TransportStatusAcked,
		},
		{
			name:      "same agent same stream outbound above acked seq",
			agentID:   "agent_1",
			stream:    TransportStreamPaxdToManager,
			direction: TransportDirectionOutbound,
			seq:       3,
			status:    TransportStatusSent,
			want:      TransportStatusSent,
		},
		{
			name:      "same agent different stream not acked",
			agentID:   "agent_1",
			stream:    TransportStreamManagerToPaxd,
			direction: TransportDirectionOutbound,
			seq:       1,
			status:    TransportStatusSent,
			want:      TransportStatusSent,
		},
		{
			name:      "same stream inbound not acked",
			agentID:   "agent_1",
			stream:    TransportStreamPaxdToManager,
			direction: TransportDirectionInbound,
			seq:       1,
			status:    TransportStatusReceived,
			want:      TransportStatusReceived,
		},
		{
			name:      "different agent not acked",
			agentID:   "agent_2",
			stream:    TransportStreamPaxdToManager,
			direction: TransportDirectionOutbound,
			seq:       1,
			status:    TransportStatusSent,
			want:      TransportStatusSent,
		},
	}

	for _, tc := range cases {
		if err := s.SaveTransportFrame(&TransportFrame{
			AgentID:        tc.agentID,
			Stream:         tc.stream,
			Seq:            tc.seq,
			LocalDirection: tc.direction,
			PayloadJSON:    `{"jsonrpc":"2.0"}`,
			Status:         tc.status,
		}); err != nil {
			t.Fatalf("%s SaveTransportFrame() error = %v", tc.name, err)
		}
	}

	if err := s.AckOutboundTransportFrames("agent_1", TransportStreamPaxdToManager, 2); err != nil {
		t.Fatalf("AckOutboundTransportFrames() error = %v", err)
	}

	for _, tc := range cases {
		got, err := s.GetTransportFrame(tc.agentID, tc.stream, tc.seq, tc.direction)
		if err != nil {
			t.Fatalf("%s GetTransportFrame() error = %v", tc.name, err)
		}
		if got == nil || got.Status != tc.want {
			t.Fatalf("%s status = %+v, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTransportJournalUpdatesStatusAndCleansCompletedFrames(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()

	frame := &TransportFrame{
		AgentID:        "agent_1",
		Stream:         TransportStreamManagerToPaxd,
		Seq:            1,
		LocalDirection: TransportDirectionInbound,
		PayloadJSON:    `{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
	}
	if err := s.SaveTransportFrame(frame); err != nil {
		t.Fatalf("SaveTransportFrame() error = %v", err)
	}
	if err := s.UpdateTransportFrameStatus(
		"agent_1",
		TransportStreamManagerToPaxd,
		1,
		TransportDirectionInbound,
		TransportStatusApplied,
		"",
	); err != nil {
		t.Fatalf("UpdateTransportFrameStatus() error = %v", err)
	}

	applied, err := s.GetTransportFrame("agent_1", TransportStreamManagerToPaxd, 1, TransportDirectionInbound)
	if err != nil {
		t.Fatalf("GetTransportFrame() error = %v", err)
	}
	if applied == nil || applied.Status != TransportStatusApplied || applied.AppliedAt == "" {
		t.Fatalf("applied frame = %+v", applied)
	}

	deleted, err := s.DeleteCompletedTransportFrames("9999-01-01T00:00:00Z", 100)
	if err != nil {
		t.Fatalf("DeleteCompletedTransportFrames() error = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}

	got, err := s.GetTransportFrame("agent_1", TransportStreamManagerToPaxd, 1, TransportDirectionInbound)
	if err != nil {
		t.Fatalf("GetTransportFrame() after delete error = %v", err)
	}
	if got != nil {
		t.Fatalf("frame after delete = %+v", got)
	}
}

func TestTransportJournalCleanupDecisionTable(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()

	cases := []struct {
		status string
		seq    int64
		delete bool
	}{
		{status: TransportStatusPending, seq: 1},
		{status: TransportStatusSent, seq: 2},
		{status: TransportStatusReceived, seq: 3},
		{status: TransportStatusFailed, seq: 4},
		{status: TransportStatusAcked, seq: 5, delete: true},
		{status: TransportStatusApplied, seq: 6, delete: true},
	}

	for _, tc := range cases {
		if err := s.SaveTransportFrame(&TransportFrame{
			AgentID:        "agent_1",
			Stream:         TransportStreamPaxdToManager,
			Seq:            tc.seq,
			LocalDirection: TransportDirectionOutbound,
			PayloadJSON:    `{"jsonrpc":"2.0"}`,
			Status:         tc.status,
			UpdatedAt:      "2000-01-01T00:00:00Z",
		}); err != nil {
			t.Fatalf("SaveTransportFrame(%s) error = %v", tc.status, err)
		}
	}

	deleted, err := s.DeleteCompletedTransportFrames("2001-01-01T00:00:00Z", 100)
	if err != nil {
		t.Fatalf("DeleteCompletedTransportFrames() error = %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}

	for _, tc := range cases {
		got, err := s.GetTransportFrame("agent_1", TransportStreamPaxdToManager, tc.seq, TransportDirectionOutbound)
		if err != nil {
			t.Fatalf("GetTransportFrame(%s) error = %v", tc.status, err)
		}
		if tc.delete && got != nil {
			t.Fatalf("%s frame still exists: %+v", tc.status, got)
		}
		if !tc.delete && got == nil {
			t.Fatalf("%s frame was deleted", tc.status)
		}
	}
}

func TestTransportJournalSeqIsScopedByAgentStreamAndDirection(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()

	frames := []TransportFrame{
		{
			AgentID:        "agent_1",
			Stream:         TransportStreamPaxdToManager,
			Seq:            1,
			LocalDirection: TransportDirectionOutbound,
			PayloadJSON:    `{"jsonrpc":"2.0"}`,
		},
		{
			AgentID:        "agent_1",
			Stream:         TransportStreamManagerToPaxd,
			Seq:            10,
			LocalDirection: TransportDirectionOutbound,
			PayloadJSON:    `{"jsonrpc":"2.0"}`,
		},
		{
			AgentID:        "agent_1",
			Stream:         TransportStreamPaxdToManager,
			Seq:            20,
			LocalDirection: TransportDirectionInbound,
			PayloadJSON:    `{"jsonrpc":"2.0"}`,
		},
		{
			AgentID:        "agent_2",
			Stream:         TransportStreamPaxdToManager,
			Seq:            30,
			LocalDirection: TransportDirectionOutbound,
			PayloadJSON:    `{"jsonrpc":"2.0"}`,
		},
	}
	for i := range frames {
		if err := s.SaveTransportFrame(&frames[i]); err != nil {
			t.Fatalf("SaveTransportFrame(%d) error = %v", i, err)
		}
	}

	next, err := s.NextTransportSeq("agent_1", TransportStreamPaxdToManager, TransportDirectionOutbound)
	if err != nil {
		t.Fatalf("NextTransportSeq() error = %v", err)
	}
	if next != 2 {
		t.Fatalf("next seq = %d, want 2", next)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "paxd.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return s
}
