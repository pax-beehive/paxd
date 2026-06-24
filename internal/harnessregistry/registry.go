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
	Harness     string
	DisplayName string
	Capability  string
	Command     []string
	Source      string
	InstallHint string
	Lookup      CommandLookup
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
	path, err := lookup.LookPath(d.Command[0])
	if err != nil {
		view.State = StateMissing
		view.Command = append([]string(nil), d.Command...)
		return view, nil
	}
	view.State = StateAvailable
	view.Command = append([]string{path}, d.Command[1:]...)
	return view, nil
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
