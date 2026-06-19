package sessionstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pax-beehive/paxd/pkg/model"
)

func TestStoreUpsertFindAndVersionedElements(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "sessions.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	err = store.UpsertSessions(ctx, "codex", []model.SessionInfo{{
		SessionID: "codex:sess-1",
		NativeID:  "sess-1",
		Name:      "First session",
		UpdatedAt: "2026-06-18T12:00:00Z",
	}})
	if err != nil {
		t.Fatalf("UpsertSessions() error = %v", err)
	}
	session, err := store.FindSession(ctx, "sess-1", "codex")
	if err != nil {
		t.Fatalf("FindSession() error = %v", err)
	}
	if session.ID != "codex:sess-1" {
		t.Fatalf("session.ID = %q, want codex:sess-1", session.ID)
	}

	v1, err := store.BeginSync(ctx, session.ID, session.Agent)
	if err != nil {
		t.Fatalf("BeginSync(v1) error = %v", err)
	}
	if err := store.CompleteSync(ctx, session.ID, v1, []Element{{
		Seq:         1,
		Type:        "message",
		Role:        "user",
		ContentText: "hello",
	}}); err != nil {
		t.Fatalf("CompleteSync(v1) error = %v", err)
	}
	v2, err := store.BeginSync(ctx, session.ID, session.Agent)
	if err != nil {
		t.Fatalf("BeginSync(v2) error = %v", err)
	}
	if err := store.CompleteSync(ctx, session.ID, v2, []Element{{
		Seq:         1,
		Type:        "message",
		Role:        "assistant",
		ContentText: "hi",
	}}); err != nil {
		t.Fatalf("CompleteSync(v2) error = %v", err)
	}

	session, err = store.FindSession(ctx, session.ID, "")
	if err != nil {
		t.Fatalf("FindSession(current) error = %v", err)
	}
	elements, err := store.Elements(ctx, session)
	if err != nil {
		t.Fatalf("Elements() error = %v", err)
	}
	if len(elements) != 1 || elements[0].Role != "assistant" || elements[0].ContentText != "hi" {
		t.Fatalf("Elements() = %+v, want v2 assistant element", elements)
	}
}
