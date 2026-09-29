package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientCapabilitiesGivenPaxdUpgradeWhenHashedThenPreservesConfigurationIdentity(t *testing.T) {
	before, err := buildACPClientInitProfile("0.1.40")
	require.NoError(t, err)
	after, err := buildACPClientInitProfile("0.1.41")
	require.NoError(t, err)
	assert.NotEqual(t, before.ProfileHash, after.ProfileHash)
	require.NotEmpty(t, before.CapabilitiesHash)
	assert.Equal(t, before.CapabilitiesHash, after.CapabilitiesHash)
	var params map[string]any
	require.NoError(t, json.Unmarshal(after.Params, &params))
	params["clientCapabilities"] = map[string]any{"session": map[string]any{}}
	changed, err := canonicalJSON(params)
	require.NoError(t, err)
	assert.NotEqual(t, after.CapabilitiesHash, clientCapabilitiesHash(changed))
	params["protocolVersion"] = 2
	protocolChanged, err := canonicalJSON(params)
	require.NoError(t, err)
	assert.NotEqual(t, clientCapabilitiesHash(changed), clientCapabilitiesHash(protocolChanged))
	assert.Empty(t, clientCapabilitiesHash(json.RawMessage(`invalid`)))
	assert.Empty(t, clientCapabilitiesHash(json.RawMessage(`{}`)))
}
