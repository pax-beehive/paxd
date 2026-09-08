package harnessregistry

import (
	"context"
	"errors"
	"fmt"
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

	assert.ElementsMatch(t, []string{"hermes", "codex", "claude-code", "gemini", "kimi", "opencode", "openclaw", "pi", "dsh"}, names)
	assertDefaultDetectorCommand(t, detectors, "dsh", []string{"dsh", "--profile", "acp"}, nil)
	assertDefaultDetectorCommand(t, detectors, "kimi", []string{"kimi", "acp"}, nil)
	assertDefaultDetectorCommand(t, detectors, "opencode", []string{"opencode", "acp"}, nil)
	assertDefaultDetectorCommand(t, detectors, "openclaw", []string{"openclaw", "acp"}, nil)
	assertDefaultDetectorCommand(t, detectors, "pi", []string{"pi-acp"}, []string{"npx", "-y", "pi-acp"})
}

func TestDSHDiscoveryRequiresInstalledCLIAndCachesResult(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprint(installed), func(t *testing.T) {
			var detector CommandDetector
			for _, candidate := range DefaultDetectors() {
				if candidate.Name() == "dsh" {
					detector = candidate.(CommandDetector)
				}
			}
			require.Equal(t, "dsh", detector.Harness)
			paths := map[string]string{"npx": "/bin/npx"}
			if installed {
				paths["dsh"] = "/bin/dsh"
			}
			detector.Lookup = fakeLookup{paths: paths}
			store := &fakeStore{}
			views, err := New(store, detector).Discover(t.Context(), control.DiscoverHarnessesQuery{Names: []string{"dsh"}})
			require.NoError(t, err)
			require.Len(t, views, 1)
			assert.Equal(t, []string{"dsh", "--profile", "acp"}, views[0].Command)
			assert.Equal(t, "DeepSeek Harness", views[0].DisplayName)
			assert.Contains(t, views[0].InstallHint, "DEEPSEEK_API_KEY")
			if installed {
				assert.Equal(t, StateAvailable, views[0].State)
			} else {
				assert.Equal(t, StateMissing, views[0].State)
			}
			assert.Equal(t, views, store.upserted)
		})
	}
}

func assertDefaultDetectorCommand(t *testing.T, detectors []Detector, name string, command []string, fallback []string) {
	t.Helper()
	for _, detector := range detectors {
		if detector.Name() != name {
			continue
		}
		commandDetector, ok := detector.(CommandDetector)
		require.True(t, ok, "%s detector type = %T, want CommandDetector", name, detector)
		assert.Equal(t, command, commandDetector.Command)
		assert.Equal(t, fallback, commandDetector.FallbackCommand)
		return
	}
	t.Fatalf("default detector %q not found", name)
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
