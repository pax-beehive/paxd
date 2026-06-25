package localsessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListReturnsCachedSessionsWithoutScanning(t *testing.T) {
	store := &fakeStore{sessions: []control.LocalSessionView{
		{ID: "codex:sess_1", Agent: "codex", NativeID: "sess_1"},
	}}
	scanners := &fakeScannerRegistry{}
	service := New(store, scanners)

	got, err := service.List(context.Background(), control.ListLocalSessionsQuery{Agent: "codex"})

	require.NoError(t, err)
	assert.Equal(t, store.sessions, got)
	assert.Equal(t, 0, scanners.availableCalls)
}

func TestSyncExplicitAgentScansAndCachesSessions(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	scanner := &fakeScanner{sessions: []control.LocalSessionView{
		{
			NativeID: "sess_1",
			Title:    "Debug paxd",
			Metadata: map[string]string{
				"project":      "paxd",
				"access_token": "secret-value",
			},
		},
		{ID: "codex:sess_2", Agent: "codex", NativeID: "sess_2"},
	}}
	service := New(store, &fakeScannerRegistry{scanners: map[string]Scanner{"codex": scanner}}, WithClock(func() time.Time { return now }))

	result, err := service.Sync(context.Background(), control.SyncLocalSessionsQuery{Agent: " codex ", Limit: 5})

	require.NoError(t, err)
	assert.Equal(t, control.LocalSessionSyncResult{Synced: 2}, result)
	require.Len(t, store.upserted, 2)
	assert.Equal(t, "codex:sess_1", store.upserted[0].ID)
	assert.Equal(t, "codex", store.upserted[0].Agent)
	assert.Equal(t, now.Format(time.RFC3339Nano), store.upserted[0].LastListedAt)
	assert.Equal(t, now.Format(time.RFC3339Nano), store.upserted[0].LastSyncedAt)
	assert.Equal(t, map[string]string{"project": "paxd"}, store.upserted[0].Metadata)
	assert.Equal(t, []scannerCall{{agent: "codex", limit: 5}}, scanner.calls)
}

func TestSyncAllAgentsSkipsUnavailableScanners(t *testing.T) {
	store := &fakeStore{}
	codex := &fakeScanner{sessions: []control.LocalSessionView{{NativeID: "sess_1"}}}
	registry := &fakeScannerRegistry{
		available: []string{"codex", "gemini"},
		scanners:  map[string]Scanner{"codex": codex},
	}
	service := New(store, registry)

	result, err := service.Sync(context.Background(), control.SyncLocalSessionsQuery{})

	require.NoError(t, err)
	assert.Equal(t, 1, registry.availableCalls)
	assert.Equal(t, control.LocalSessionSyncResult{Synced: 1}, result)
	require.Len(t, store.upserted, 1)
	assert.Equal(t, "codex:sess_1", store.upserted[0].ID)
}

func TestSyncExplicitUnavailableAgentReturnsError(t *testing.T) {
	store := &fakeStore{}
	service := New(store, &fakeScannerRegistry{})

	result, err := service.Sync(context.Background(), control.SyncLocalSessionsQuery{Agent: "gemini"})

	require.Error(t, err)
	assert.Equal(t, 1, result.Failed)
	require.Len(t, result.Errors, 1)
	assert.Equal(t, "scanner_unavailable", result.Errors[0].Code)
	assert.Empty(t, store.upserted)
}

func TestSyncReportsScannerErrorAndContinues(t *testing.T) {
	store := &fakeStore{}
	registry := &fakeScannerRegistry{
		available: []string{"codex", "claude-code"},
		scanners: map[string]Scanner{
			"codex":       &fakeScanner{err: errors.New("scan failed")},
			"claude-code": &fakeScanner{sessions: []control.LocalSessionView{{NativeID: "sess_ok"}}},
		},
	}
	service := New(store, registry)

	result, err := service.Sync(context.Background(), control.SyncLocalSessionsQuery{})

	require.Error(t, err)
	assert.Equal(t, 1, result.Synced)
	assert.Equal(t, 1, result.Failed)
	require.Len(t, result.Errors, 1)
	assert.Equal(t, "scan_failed", result.Errors[0].Code)
	require.Len(t, store.upserted, 1)
	assert.Equal(t, "claude-code:sess_ok", store.upserted[0].ID)
}

func TestSyncTimeoutIsPassedToScanner(t *testing.T) {
	store := &fakeStore{}
	scanner := &fakeScanner{observeDeadline: true, sessions: []control.LocalSessionView{{NativeID: "sess_1"}}}
	service := New(store, &fakeScannerRegistry{scanners: map[string]Scanner{"codex": scanner}})

	result, err := service.Sync(context.Background(), control.SyncLocalSessionsQuery{Agent: "codex", TimeoutMillis: 500})

	require.NoError(t, err)
	assert.Equal(t, 1, result.Synced)
	assert.True(t, scanner.sawDeadline)
}

func TestGetReturnsCachedSessionDetail(t *testing.T) {
	want := &control.LocalSessionView{
		ID:       "codex:sess_1",
		Agent:    "codex",
		NativeID: "sess_1",
		Elements: []control.LocalSessionElementView{{Seq: 1, Kind: "message", Role: "user", Text: "hello"}},
	}
	service := New(&fakeStore{session: want}, &fakeScannerRegistry{})

	got, err := service.Get(context.Background(), control.GetLocalSessionQuery{SessionID: "codex:sess_1"})

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

type fakeStore struct {
	sessions []control.LocalSessionView
	upserted []control.LocalSessionView
	session  *control.LocalSessionView
}

func (s *fakeStore) ListSessions(context.Context, control.ListLocalSessionsQuery) ([]control.LocalSessionView, error) {
	return append([]control.LocalSessionView(nil), s.sessions...), nil
}

func (s *fakeStore) UpsertSessions(_ context.Context, sessions []control.LocalSessionView) error {
	s.upserted = append(s.upserted, sessions...)
	return nil
}

func (s *fakeStore) GetSession(context.Context, string) (*control.LocalSessionView, error) {
	return s.session, nil
}

type fakeScannerRegistry struct {
	available      []string
	scanners       map[string]Scanner
	availableCalls int
}

func (r *fakeScannerRegistry) ScannerFor(agent string) (Scanner, bool) {
	scanner, ok := r.scanners[agent]
	return scanner, ok
}

func (r *fakeScannerRegistry) AvailableAgents(context.Context) ([]string, error) {
	r.availableCalls++
	return append([]string(nil), r.available...), nil
}

type scannerCall struct {
	agent string
	limit int
}

type fakeScanner struct {
	sessions        []control.LocalSessionView
	err             error
	observeDeadline bool
	sawDeadline     bool
	calls           []scannerCall
}

func (s *fakeScanner) ListSessions(ctx context.Context, agent string, limit int) ([]control.LocalSessionView, error) {
	s.calls = append(s.calls, scannerCall{agent: agent, limit: limit})
	if s.observeDeadline {
		_, s.sawDeadline = ctx.Deadline()
	}
	return append([]control.LocalSessionView(nil), s.sessions...), s.err
}
