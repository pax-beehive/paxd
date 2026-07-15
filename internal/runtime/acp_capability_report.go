package runtime

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

const (
	ACPPoolCapabilityReportSchemaVersion = 1

	ACPPoolInitPhaseInitializing = "initializing"
	ACPPoolInitPhaseReady        = "ready"
	ACPPoolInitPhaseFailed       = "failed"
)

// PaxdVersionProvider supplies the daemon binary version used in sanitized ACP
// capability reports and paxd-owned worker initialize descriptors.
type PaxdVersionProvider interface {
	PaxdVersion() string
}

type StaticPaxdVersionProvider string

func (p StaticPaxdVersionProvider) PaxdVersion() string {
	version := strings.TrimSpace(string(p))
	if version == "" {
		return "dev"
	}
	return version
}

type NoopPaxdVersionProvider struct{}

func (NoopPaxdVersionProvider) PaxdVersion() string {
	return "dev"
}

// ACPPoolCapabilityReporter is the narrow port used by the ACP process owner to
// publish sanitized worker initialization state. Raw initialize params/results
// stay local and must not cross this interface.
type ACPPoolCapabilityReporter interface {
	ReportACPPoolCapability(context.Context, ACPPoolCapabilityReport) error
}

type NoopACPPoolCapabilityReporter struct{}

func (NoopACPPoolCapabilityReporter) ReportACPPoolCapability(context.Context, ACPPoolCapabilityReport) error {
	return nil
}

type ACPPoolCapabilityReport struct {
	SchemaVersion        int       `json:"schema_version"`
	ConnectionID         string    `json:"connection_id"`
	ReportGeneration     int64     `json:"report_generation"`
	PaxdVersion          string    `json:"paxd_version"`
	CommandFingerprint   string    `json:"command_fingerprint"`
	ClientProfileHash    string    `json:"client_profile_hash"`
	WorkerResultHash     string    `json:"worker_result_hash"`
	ProtocolVersion      int       `json:"protocol_version,omitempty"`
	ClientCapabilityKeys []string  `json:"client_capability_keys,omitempty"`
	WorkerCapabilityKeys []string  `json:"worker_capability_keys,omitempty"`
	InitPhase            string    `json:"init_phase"`
	InitializedAt        time.Time `json:"initialized_at,omitempty"`
	LastErrorCode        string    `json:"last_error_code,omitempty"`
	LastErrorMessage     string    `json:"last_error_message,omitempty"`
}

func (r ACPPoolCapabilityReport) WithDefaults() ACPPoolCapabilityReport {
	if r.SchemaVersion == 0 {
		r.SchemaVersion = ACPPoolCapabilityReportSchemaVersion
	}
	if strings.TrimSpace(r.PaxdVersion) == "" {
		r.PaxdVersion = "dev"
	}
	if strings.TrimSpace(r.InitPhase) == "" {
		r.InitPhase = ACPPoolInitPhaseInitializing
	}
	r.ClientCapabilityKeys = sortedStrings(r.ClientCapabilityKeys)
	r.WorkerCapabilityKeys = sortedStrings(r.WorkerCapabilityKeys)
	return r
}

func capabilityKeys(raw json.RawMessage, fieldNames ...string) []string {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	keys := make(map[string]struct{})
	for _, fieldName := range fieldNames {
		var caps map[string]json.RawMessage
		if err := json.Unmarshal(root[fieldName], &caps); err != nil {
			continue
		}
		for key := range caps {
			if strings.TrimSpace(key) != "" {
				keys[key] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	return sortedStrings(out)
}

func protocolVersionFromResult(raw json.RawMessage) int {
	var result struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	_ = json.Unmarshal(raw, &result)
	return result.ProtocolVersion
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
