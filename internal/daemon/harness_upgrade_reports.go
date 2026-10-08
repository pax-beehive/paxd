package daemon

import (
	"fmt"
	"github.com/pax-beehive/paxd/internal/control"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
)

func (r *acpCapabilityReports) harnessEpochs(connections []control.AgentConnectionView) map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	previous := make(map[string]bool)
	for _, conn := range connections {
		for _, slot := range r.slots[conn.ID] {
			previous[slot.processEpoch] = true
		}
	}
	return previous
}

func (r *acpCapabilityReports) harnessReady(connections []control.AgentConnectionView, previous map[string]bool, version string) (bool, error) {
	return r.harnessComponentReady(connections, previous, version, "acp")
}

func (r *acpCapabilityReports) harnessComponentReady(connections []control.AgentConnectionView, previous map[string]bool, version, component string) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, conn := range connections {
		ready := 0
		for _, slot := range r.slots[conn.ID] {
			identity := slot.report.Implementation
			if slot.processEpoch == "" || previous[slot.processEpoch] || slot.report.InitPhase != runtimes.ACPPoolInitPhaseReady {
				return false, nil
			}
			matched := identity != nil && identity.ACPAgent != nil && identity.ACPAgent.Version == version
			if component == "cli" {
				matched = identity != nil && identity.Runtime != nil && identity.Runtime.Name == conn.Harness && identity.Runtime.Version == version
			}
			if !matched {
				return false, fmt.Errorf("new ACP process for %s did not report target %s version %s", conn.ID, component, version)
			}
			ready++
		}
		desired := conn.DesiredACPSlots
		if desired < 1 {
			desired = 1
		}
		if ready != desired {
			return false, nil
		}
	}
	return len(connections) > 0, nil
}
