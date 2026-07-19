package harnessregistry

import (
	"context"
	"errors"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListCachedReturnsStoreInventoryWithoutRunningDetectors(t *testing.T) {
	store := &fakeStore{harnesses: []control.HarnessView{
		{Harness: "codex", DisplayName: "Codex", State: StateAvailable},
		{Harness: "gemini", DisplayName: "Gemini", State: StateMissing},
	}}
	detector := &fakeDetector{name: "codex"}
	registry := New(store, detector)

	got, err := registry.ListCached(context.Background())

	require.NoError(t, err)
	assert.Equal(t, store.harnesses, got)
	assert.Equal(t, 0, detector.calls)
}

func TestDiscoverRunsRegisteredDetectorsAndRefreshesCache(t *testing.T) {
	store := &fakeStore{}
	codex := &fakeDetector{name: "codex", view: control.HarnessView{Harness: "codex", DisplayName: "Codex", State: StateAvailable}}
	gemini := &fakeDetector{name: "gemini", view: control.HarnessView{Harness: "gemini", DisplayName: "Gemini", State: StateMissing}}
	registry := New(store, gemini, codex)

	got, err := registry.Discover(context.Background(), control.DiscoverHarnessesQuery{})

	require.NoError(t, err)
	assert.Equal(t, 1, codex.calls)
	assert.Equal(t, 1, gemini.calls)
	assert.Equal(t, []control.HarnessView{
		{Harness: "codex", DisplayName: "Codex", State: StateAvailable},
		{Harness: "gemini", DisplayName: "Gemini", State: StateMissing},
	}, got)
	assert.Equal(t, got, store.upserted)
}

func TestDiscoverFiltersDetectorNames(t *testing.T) {
	store := &fakeStore{}
	codex := &fakeDetector{name: "codex", view: control.HarnessView{DisplayName: "Codex"}}
	gemini := &fakeDetector{name: "gemini", view: control.HarnessView{DisplayName: "Gemini"}}
	registry := New(store, codex, gemini)

	got, err := registry.Discover(context.Background(), control.DiscoverHarnessesQuery{Names: []string{" codex "}})

	require.NoError(t, err)
	assert.Equal(t, 1, codex.calls)
	assert.Equal(t, 0, gemini.calls)
	require.Len(t, got, 1)
	assert.Equal(t, "codex", got[0].Harness)
	assert.Equal(t, StateAvailable, got[0].State)
}

func TestDetectorErrorBecomesDegradedStatusAndDiscoveryContinues(t *testing.T) {
	store := &fakeStore{}
	codex := &fakeDetector{name: "codex", err: errors.New("probe failed")}
	gemini := &fakeDetector{name: "gemini", view: control.HarnessView{Harness: "gemini", State: StateAvailable}}
	registry := New(store, codex, gemini)

	got, err := registry.Discover(context.Background(), control.DiscoverHarnessesQuery{Probe: true})

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "codex", got[0].Harness)
	assert.Equal(t, StateDegraded, got[0].State)
	assert.Equal(t, "probe failed", got[0].LastError)
	assert.Equal(t, "gemini", got[1].Harness)
	assert.Equal(t, StateAvailable, got[1].State)
}

func TestCommandDetectorReportsAvailableAndMissingCommands(t *testing.T) {
	lookup := fakeLookup{
		paths: map[string]string{
			"codex": "/usr/local/bin/codex",
			"npx":   "/usr/local/bin/npx",
		},
	}
	detector := CommandDetector{
		Harness:     "codex",
		DisplayName: "Codex",
		Command:     []string{"codex", "--acp"},
		Source:      "native",
		InstallHint: "install codex",
		Lookup:      lookup,
	}

	got, err := detector.Detect(context.Background(), control.DiscoverHarnessesQuery{})

	require.NoError(t, err)
	assert.Equal(t, control.HarnessView{
		Harness:     "codex",
		DisplayName: "Codex",
		State:       StateAvailable,
		Capability:  CapabilityACP,
		Command:     []string{"codex", "--acp"},
		Source:      "native",
		InstallHint: "install codex",
	}, got)

	missing := CommandDetector{Harness: "gemini", DisplayName: "Gemini", Command: []string{"gemini"}, Lookup: lookup}
	got, err = missing.Detect(context.Background(), control.DiscoverHarnessesQuery{})

	require.NoError(t, err)
	assert.Equal(t, StateMissing, got.State)
	assert.Equal(t, []string{"gemini"}, got.Command)

	fallback := CommandDetector{
		Harness:         "claude-code",
		DisplayName:     "Claude Code",
		Command:         []string{"claude-agent-acp"},
		FallbackCommand: []string{"npx", "-y", "@agentclientprotocol/claude-agent-acp"},
		Lookup:          lookup,
	}
	got, err = fallback.Detect(context.Background(), control.DiscoverHarnessesQuery{})

	require.NoError(t, err)
	assert.Equal(t, StateAvailable, got.State)
	assert.Equal(t, []string{"npx", "-y", "@agentclientprotocol/claude-agent-acp"}, got.Command)
}

func TestDefaultDetectorsIncludeBuiltInHarnesses(t *testing.T) {
	detectors := DefaultDetectors()
	names := make([]string, 0, len(detectors))
	for _, detector := range detectors {
		names = append(names, detector.Name())
	}

	assert.ElementsMatch(t, []string{"hermes", "codex", "claude-code", "gemini", "kimi", "pi"}, names)
}

type fakeStore struct {
	harnesses []control.HarnessView
	upserted  []control.HarnessView
}

func (s *fakeStore) ListHarnesses(context.Context) ([]control.HarnessView, error) {
	return append([]control.HarnessView(nil), s.harnesses...), nil
}

func (s *fakeStore) UpsertHarnesses(_ context.Context, harnesses []control.HarnessView) error {
	s.upserted = append([]control.HarnessView(nil), harnesses...)
	return nil
}

type fakeDetector struct {
	name  string
	view  control.HarnessView
	err   error
	calls int
}

func (d *fakeDetector) Name() string {
	return d.name
}

func (d *fakeDetector) Detect(context.Context, control.DiscoverHarnessesQuery) (control.HarnessView, error) {
	d.calls++
	return d.view, d.err
}

type fakeLookup struct {
	paths map[string]string
}

func (l fakeLookup) LookPath(file string) (string, error) {
	path, ok := l.paths[file]
	if !ok {
		return "", errors.New("not found")
	}
	return path, nil
}
