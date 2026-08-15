package runtime

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

const (
	ACPPoolCapabilityReportSchemaVersion = 2

	ACPPoolInitPhaseInitializing = "initializing"
	ACPPoolInitPhaseReady        = "ready"
	ACPPoolInitPhaseFailed       = "failed"

	ACPPoolConsistencyUnknown    = "unknown"
	ACPPoolConsistencyConsistent = "consistent"
	ACPPoolConsistencyMixed      = "mixed"

	maxACPIdentityFieldRunes = 256
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
	SchemaVersion        int                        `json:"schema_version"`
	ConnectionID         string                     `json:"connection_id"`
	ReportGeneration     int64                      `json:"report_generation"`
	PaxdVersion          string                     `json:"paxd_version"`
	CommandFingerprint   string                     `json:"command_fingerprint"`
	ClientProfileHash    string                     `json:"client_profile_hash"`
	WorkerResultHash     string                     `json:"worker_result_hash"`
	ProtocolVersion      int                        `json:"protocol_version,omitempty"`
	ClientCapabilityKeys []string                   `json:"client_capability_keys,omitempty"`
	WorkerCapabilityKeys []string                   `json:"worker_capability_keys,omitempty"`
	Implementation       *ACPImplementationIdentity `json:"implementation,omitempty"`
	PoolConsistency      string                     `json:"pool_consistency"`
	InitPhase            string                     `json:"init_phase"`
	InitializedAt        time.Time                  `json:"initialized_at,omitempty"`
	LastErrorCode        string                     `json:"last_error_code,omitempty"`
	LastErrorMessage     string                     `json:"last_error_message,omitempty"`
}

// ACPImplementationIdentity is the deliberately narrow, public projection of
// an ACP worker's initialize result. Arbitrary initialize fields and metadata
// must never be added to this type or carried by ACPPoolCapabilityReport.
type ACPImplementationIdentity struct {
	ACPAgent            *ACPAgentImplementation   `json:"acp_agent,omitempty"`
	Runtime             *ACPRuntimeImplementation `json:"runtime,omitempty"`
	IdentityFingerprint string                    `json:"identity_fingerprint"`
}

type ACPAgentImplementation struct {
	Name    string `json:"name,omitempty"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

type ACPRuntimeImplementation struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	Build   string `json:"build,omitempty"`
	Channel string `json:"channel,omitempty"`
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
	if strings.TrimSpace(r.PoolConsistency) == "" {
		if r.InitPhase == ACPPoolInitPhaseReady && r.Implementation != nil &&
			strings.TrimSpace(r.Implementation.IdentityFingerprint) != "" {
			r.PoolConsistency = ACPPoolConsistencyConsistent
		} else {
			r.PoolConsistency = ACPPoolConsistencyUnknown
		}
	}
	r.ClientCapabilityKeys = sortedStrings(r.ClientCapabilityKeys)
	r.WorkerCapabilityKeys = sortedStrings(r.WorkerCapabilityKeys)
	return r
}

func implementationIdentityFromResult(raw json.RawMessage) *ACPImplementationIdentity {
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil
	}

	agent := agentImplementationFromRaw(result["agentInfo"])
	runtime := runtimeImplementationFromMeta(result["_meta"])
	if agent == nil && runtime == nil {
		return nil
	}
	return &ACPImplementationIdentity{
		ACPAgent:            agent,
		Runtime:             runtime,
		IdentityFingerprint: implementationIdentityFingerprint(agent, runtime),
	}
}

func agentImplementationFromRaw(raw json.RawMessage) *ACPAgentImplementation {
	fields := rawJSONObject(raw)
	if fields == nil {
		return nil
	}
	agent := &ACPAgentImplementation{
		Name:    allowlistedIdentityString(fields["name"]),
		Title:   allowlistedIdentityString(fields["title"]),
		Version: allowlistedIdentityString(fields["version"]),
	}
	if agent.Name == "" && agent.Title == "" && agent.Version == "" {
		return nil
	}
	return agent
}

func runtimeImplementationFromMeta(raw json.RawMessage) *ACPRuntimeImplementation {
	meta := rawJSONObject(raw)
	if meta == nil {
		return nil
	}

	var candidates []json.RawMessage
	if pax := rawJSONObject(meta["pax"]); pax != nil {
		candidates = append(candidates, pax["runtime"])
	}
	// Some ACP implementations cannot conveniently produce nested extension
	// metadata and use the namespaced flat key instead.
	candidates = append(candidates, meta["pax.runtime"])
	for _, candidate := range candidates {
		fields := rawJSONObject(candidate)
		if fields == nil {
			continue
		}
		runtime := &ACPRuntimeImplementation{
			Name:    allowlistedIdentityString(fields["name"]),
			Version: allowlistedIdentityString(fields["version"]),
			Build:   allowlistedIdentityString(fields["build"]),
			Channel: allowlistedIdentityString(fields["channel"]),
		}
		if runtime.Name != "" || runtime.Version != "" || runtime.Build != "" || runtime.Channel != "" {
			return runtime
		}
	}
	return nil
}

func implementationIdentityFingerprint(agent *ACPAgentImplementation, runtime *ACPRuntimeImplementation) string {
	// The schema discriminator makes future fingerprint inputs explicit while
	// canonical struct JSON keeps the value stable across source JSON ordering.
	payload, err := json.Marshal(struct {
		SchemaVersion int                       `json:"schema_version"`
		ACPAgent      *ACPAgentImplementation   `json:"acp_agent,omitempty"`
		Runtime       *ACPRuntimeImplementation `json:"runtime,omitempty"`
	}{
		SchemaVersion: 1,
		ACPAgent:      agent,
		Runtime:       runtime,
	})
	if err != nil {
		return ""
	}
	return hashBytes(payload)
}

func rawJSONObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil
	}
	return object
}

func allowlistedIdentityString(raw json.RawMessage) string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	value = strings.TrimSpace(value)
	if len([]rune(value)) > maxACPIdentityFieldRunes {
		return ""
	}
	return value
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
