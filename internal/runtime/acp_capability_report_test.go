package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestACPImplementationIdentityFromInitializeResult(t *testing.T) {
	t.Run("Given a Codex initialize result when identity is projected then only allowlisted adapter and runtime fields are reported", func(t *testing.T) {
		result := json.RawMessage(`{
			"protocolVersion": 1,
			"agentInfo": {
				"name": "@agentclientprotocol/codex-acp",
				"title": "Codex",
				"version": "1.1.7",
				"privateInstallPath": "/secret/codex-acp"
			},
			"agentCapabilities": {"prompt": {"credential": "do-not-report"}},
			"_meta": {
				"pax": {
					"runtime": {
						"name": "codex",
						"version": "0.58.0",
						"build": "abc123",
						"channel": "stable",
						"binaryPath": "/secret/codex"
					}
				},
				"accessToken": "do-not-report"
			}
		}`)

		identity := implementationIdentityFromResult(result)

		require.NotNil(t, identity)
		require.NotNil(t, identity.ACPAgent)
		assert.Equal(t, "@agentclientprotocol/codex-acp", identity.ACPAgent.Name)
		assert.Equal(t, "Codex", identity.ACPAgent.Title)
		assert.Equal(t, "1.1.7", identity.ACPAgent.Version)
		require.NotNil(t, identity.Runtime)
		assert.Equal(t, "codex", identity.Runtime.Name)
		assert.Equal(t, "0.58.0", identity.Runtime.Version)
		assert.Equal(t, "abc123", identity.Runtime.Build)
		assert.Equal(t, "stable", identity.Runtime.Channel)
		assert.NotEmpty(t, identity.IdentityFingerprint)

		report := ACPPoolCapabilityReport{
			Implementation:       identity,
			WorkerCapabilityKeys: capabilityKeys(result, "agentCapabilities"),
			InitPhase:            ACPPoolInitPhaseReady,
		}.WithDefaults()
		reported, err := json.Marshal(report)
		require.NoError(t, err)
		assert.NotContains(t, string(reported), "privateInstallPath")
		assert.NotContains(t, string(reported), "binaryPath")
		assert.NotContains(t, string(reported), "accessToken")
		assert.NotContains(t, string(reported), "do-not-report")
		assert.NotContains(t, string(reported), "agentCapabilities")
	})

	t.Run("Given a Hermes initialize result when identity is projected then agent info is available without invented runtime data", func(t *testing.T) {
		result := json.RawMessage(`{
			"protocolVersion": 1,
			"agentInfo": {"name": "hermes-agent", "version": "0.17.0"},
			"authMethods": [{"id": "deepseek", "description": "private provider details"}],
			"_meta": {"runtime": {"name": "not-allowlisted", "version": "secret"}}
		}`)

		identity := implementationIdentityFromResult(result)

		require.NotNil(t, identity)
		require.NotNil(t, identity.ACPAgent)
		assert.Equal(t, "hermes-agent", identity.ACPAgent.Name)
		assert.Empty(t, identity.ACPAgent.Title)
		assert.Equal(t, "0.17.0", identity.ACPAgent.Version)
		assert.Nil(t, identity.Runtime)
		assert.NotEmpty(t, identity.IdentityFingerprint)
	})

	t.Run("Given flat pax runtime metadata when identity is projected then the compatibility path is supported", func(t *testing.T) {
		result := json.RawMessage(`{
			"agentInfo": {"name": "@agentclientprotocol/codex-acp", "version": "1.1.7"},
			"_meta": {"pax.runtime": {"name": "codex", "version": "0.58.0"}}
		}`)

		identity := implementationIdentityFromResult(result)

		require.NotNil(t, identity)
		require.NotNil(t, identity.Runtime)
		assert.Equal(t, "codex", identity.Runtime.Name)
		assert.Equal(t, "0.58.0", identity.Runtime.Version)
	})
}

func TestACPImplementationIdentityFingerprintIsStable(t *testing.T) {
	t.Run("Given equivalent allowlisted identity fields when unrelated fields and JSON ordering differ then fingerprints match", func(t *testing.T) {
		first := implementationIdentityFromResult(json.RawMessage(`{
			"agentInfo":{"name":"codex-acp","title":"Codex","version":"1.1.7"},
			"_meta":{"pax":{"runtime":{"name":"codex","version":"0.58.0","channel":"stable"}},"secret":"first"}
		}`))
		second := implementationIdentityFromResult(json.RawMessage(`{
			"_meta":{"ignored":{"secret":"second"},"pax.runtime":{"channel":"stable","version":"0.58.0","name":"codex"}},
			"agentInfo":{"version":"1.1.7","unsupported":"ignored","title":"Codex","name":"codex-acp"}
		}`))

		require.NotNil(t, first)
		require.NotNil(t, second)
		assert.Equal(t, first.IdentityFingerprint, second.IdentityFingerprint)
		assert.Equal(t, first.ACPAgent, second.ACPAgent)
		assert.Equal(t, first.Runtime, second.Runtime)
	})
}
