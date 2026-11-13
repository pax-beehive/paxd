package control

import (
	"context"
	"regexp"
)

// HarnessUsage reads native plan usage for the daemon's credential context.
// A read can reach the provider through the native CLI, so it is a separate
// query from harness_auth.status and never changes authentication state.
type HarnessUsage interface {
	Usage(context.Context, HarnessUsageStatusQuery) (HarnessUsageView, error)
}

type HarnessUsageStatusQuery struct {
	Harness string `json:"harness"`
}

const (
	HarnessUsageAvailable   = "available"
	HarnessUsageUnsupported = "unsupported"
	HarnessUsageUnavailable = "unavailable"
)

// HarnessUsageView is a bounded, allowlisted projection of native plan usage.
// Items (and the deprecated Windows and ExtraUsage) are set only when State is
// available; ErrorCode only when State is unavailable. HarnessLabel is the
// display name of the harness when known.
type HarnessUsageView struct {
	Harness      string               `json:"harness"`
	HarnessLabel string               `json:"harness_label,omitempty"`
	State        string               `json:"state"`
	ObservedAt   string               `json:"observed_at"`
	Plan         string               `json:"plan,omitempty"`
	Items        []HarnessUsageItem   `json:"items,omitempty"`
	Windows      []HarnessUsageWindow `json:"windows,omitempty"`
	ExtraUsage   *HarnessUsageExtra   `json:"extra_usage,omitempty"`
	ErrorCode    string               `json:"error_code,omitempty"`
}

const (
	HarnessUsageKindPercent = "percent"
	HarnessUsageKindAmount  = "amount"
	HarnessUsageKindStatus  = "status"
)

// HarnessUsageItem is one display-ready usage entry. Clients render it by Kind
// alone: percent uses UsedPercent, amount uses Used/Limit/Unit, status uses
// Value. Label is English display text chosen by the harness adapter.
type HarnessUsageItem struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Kind          string   `json:"kind"`
	Severity      string   `json:"severity,omitempty"`
	UsedPercent   *float64 `json:"used_percent,omitempty"`
	Used          *float64 `json:"used,omitempty"`
	Limit         *float64 `json:"limit,omitempty"`
	Unit          string   `json:"unit,omitempty"`
	Value         string   `json:"value,omitempty"`
	WindowMinutes int      `json:"window_minutes,omitempty"`
	ResetsAt      string   `json:"resets_at,omitempty"`
}

type HarnessUsageWindow struct {
	ID            string  `json:"id"`
	WindowMinutes int     `json:"window_minutes"`
	Scope         string  `json:"scope,omitempty"`
	UsedPercent   float64 `json:"used_percent"`
	ResetsAt      string  `json:"resets_at,omitempty"`
}

type HarnessUsageExtra struct {
	Enabled bool `json:"enabled"`
}

func (q HarnessUsageStatusQuery) Validate() error {
	if !ValidHarnessID(q.Harness) {
		return invalid("harness_usage_status", "expected a harness id such as claude-code or codex")
	}
	return nil
}

var harnessIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidHarnessID reports whether id has the shape of a PAX agent type such as
// claude-code, codex, or kimi. It does not imply native support.
func ValidHarnessID(id string) bool {
	return harnessIDPattern.MatchString(id)
}

// NativeHarness maps a PAX agent type to the native login/usage adapter name.
// claude-code and the legacy claude both use the claude adapter.
func NativeHarness(id string) string {
	if id == "claude-code" {
		return "claude"
	}
	return id
}

func (s *ControlService) handleHarnessUsageStatus(ctx context.Context, src Source, q HarnessUsageStatusQuery) (QueryResult, error) {
	result := QueryResult{Type: QueryHarnessUsageStatus}
	if !validHarnessAuthSource(src) {
		result.Error = ptr(invalid("source", "authenticated control source required"))
		return result, nil
	}
	if s.harnessUsage == nil {
		result.Error = ptr(ControlError{Code: ErrCodeInternal, Message: "harness usage unavailable"})
		return result, nil
	}
	view, err := s.harnessUsage.Usage(ctx, q)
	if err != nil {
		result.Error = ptr(controlErr(err))
	} else {
		result.HarnessUsage = &view
	}
	return result, nil
}
