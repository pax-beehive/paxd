package harnessregistry

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/pax-beehive/paxd/internal/control"
)

const (
	StateAvailable = "available"
	StateMissing   = "missing"
	StateDegraded  = "degraded"

	CapabilityACP      = "acp"
	CapabilityLocalLog = "local-log"
	CapabilityGateway  = "gateway"
)

type Store interface {
	ListHarnesses(ctx context.Context) ([]control.HarnessView, error)
	UpsertHarnesses(ctx context.Context, harnesses []control.HarnessView) error
}

type Detector interface {
	Name() string
	Detect(ctx context.Context, req control.DiscoverHarnessesQuery) (control.HarnessView, error)
}

type Registry struct {
	store     Store
	detectors []Detector
}

func New(store Store, detectors ...Detector) *Registry {
	return &Registry{store: store, detectors: detectors}
}

func (r *Registry) ListCached(ctx context.Context) ([]control.HarnessView, error) {
	if r.store == nil {
		return nil, errors.New("harness registry store is not configured")
	}
	return r.store.ListHarnesses(ctx)
}

func (r *Registry) Discover(ctx context.Context, req control.DiscoverHarnessesQuery) ([]control.HarnessView, error) {
	selected := selectedNames(req.Names)
	results := make([]control.HarnessView, 0, len(r.detectors))
	for _, detector := range r.detectors {
		name := normalizeName(detector.Name())
		if len(selected) > 0 && !selected[name] {
			continue
		}
		view, err := detector.Detect(ctx, req)
		view = normalizeView(name, view)
		if err != nil {
			view.State = StateDegraded
			view.LastError = safeError(err)
		}
		results = append(results, view)
	}
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Harness < results[j].Harness
	})
	if r.store != nil {
		if err := r.store.UpsertHarnesses(ctx, results); err != nil {
			return nil, err
		}
	}
	return results, nil
}

type CommandLookup interface {
	LookPath(file string) (string, error)
}

type CommandDetector struct {
	Harness         string
	DisplayName     string
	Capability      string
	Command         []string
	FallbackCommand []string
	Source          string
	InstallHint     string
	Lookup          CommandLookup
}

func (d CommandDetector) Name() string {
	return d.Harness
}

func (d CommandDetector) Detect(ctx context.Context, req control.DiscoverHarnessesQuery) (control.HarnessView, error) {
	_ = ctx
	_ = req
	view := control.HarnessView{
		Harness:     normalizeName(d.Harness),
		DisplayName: d.DisplayName,
		Capability:  d.Capability,
		Source:      d.Source,
		InstallHint: d.InstallHint,
	}
	if view.Capability == "" {
		view.Capability = CapabilityACP
	}
	if len(d.Command) == 0 || strings.TrimSpace(d.Command[0]) == "" {
		view.State = StateMissing
		view.LastError = "command is not configured"
		return view, nil
	}
	lookup := d.Lookup
	if lookup == nil {
		lookup = osLookup{}
	}
	_, err := lookup.LookPath(d.Command[0])
	if err != nil {
		if len(d.FallbackCommand) > 0 && strings.TrimSpace(d.FallbackCommand[0]) != "" {
			if _, fallbackErr := lookup.LookPath(d.FallbackCommand[0]); fallbackErr == nil {
				view.State = StateAvailable
				view.Command = append([]string(nil), d.FallbackCommand...)
				return view, nil
			}
		}
		view.State = StateMissing
		view.Command = append([]string(nil), d.Command...)
		return view, nil
	}
	view.State = StateAvailable
	view.Command = append([]string(nil), d.Command...)
	return view, nil
}

func DefaultDetectors() []Detector {
	return []Detector{
		CommandDetector{
			Harness:     "hermes",
			DisplayName: "Hermes",
			Command:     []string{"hermes", "acp"},
			Source:      "native",
			InstallHint: "install hermes with ACP support",
		},
		CommandDetector{
			Harness:     "codex",
			DisplayName: "Codex",
			Command:     []string{"codex-acp"},
			FallbackCommand: []string{
				"npx",
				"-y",
				"@agentclientprotocol/codex-acp",
			},
			Source:      "official",
			InstallHint: "install codex-acp or use fallback: npx -y @agentclientprotocol/codex-acp",
		},
		CommandDetector{
			Harness:     "claude-code",
			DisplayName: "Claude Code",
			Command:     []string{"claude-agent-acp"},
			FallbackCommand: []string{
				"npx",
				"-y",
				"@agentclientprotocol/claude-agent-acp",
			},
			Source:      "official",
			InstallHint: "install claude-agent-acp or use fallback: npx -y @agentclientprotocol/claude-agent-acp",
		},
		CommandDetector{
			Harness:     "gemini",
			DisplayName: "Gemini",
			Command:     []string{"gemini", "--acp"},
			Source:      "native",
			InstallHint: "install gemini with ACP support",
		},
		CommandDetector{
			Harness:     "opencode",
			DisplayName: "OpenCode",
			Command:     []string{"opencode", "acp"},
			Source:      "first-party",
			InstallHint: "install OpenCode with ACP support; run opencode acp",
		},
		CommandDetector{
			Harness:     "openclaw",
			DisplayName: "OpenClaw",
			Command:     []string{"openclaw", "acp"},
			Source:      "gateway-bridge",
			InstallHint: "install OpenClaw and configure a reachable Gateway; run openclaw acp",
		},
		CommandDetector{
			Harness:         "pi",
			DisplayName:     "Pi Agent",
			Command:         []string{"pi-acp"},
			FallbackCommand: []string{"npx", "-y", "pi-acp"},
			Source:          "community",
			InstallHint:     "install pi-acp or use fallback: npx -y pi-acp; requires the Pi CLI to be configured",
		},
	}
}

type osLookup struct{}

func (osLookup) LookPath(file string) (string, error) {
	return exec.LookPath(file)
}

func selectedNames(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	selected := make(map[string]bool, len(names))
	for _, name := range names {
		selected[normalizeName(name)] = true
	}
	return selected
}

func normalizeView(name string, view control.HarnessView) control.HarnessView {
	if view.Harness == "" {
		view.Harness = name
	}
	view.Harness = normalizeName(view.Harness)
	if view.DisplayName == "" {
		view.DisplayName = view.Harness
	}
	if view.State == "" {
		view.State = StateAvailable
	}
	return view
}

func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%v", err)
}
