package control

import "context"

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
// Windows and ExtraUsage are set only when State is available; ErrorCode only
// when State is unavailable.
type HarnessUsageView struct {
	Harness    string               `json:"harness"`
	State      string               `json:"state"`
	ObservedAt string               `json:"observed_at"`
	Plan       string               `json:"plan,omitempty"`
	Windows    []HarnessUsageWindow `json:"windows,omitempty"`
	ExtraUsage *HarnessUsageExtra   `json:"extra_usage,omitempty"`
	ErrorCode  string               `json:"error_code,omitempty"`
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
	if q.Harness != "claude" && q.Harness != "codex" {
		return invalid("harness_usage_status", "expected claude or codex")
	}
	return nil
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
