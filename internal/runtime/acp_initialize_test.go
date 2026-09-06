package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildACPClientInitProfileAdvertisesBooleanSessionConfig(t *testing.T) {
	profile, err := buildACPClientInitProfile("v1.2.3")
	require.NoError(t, err)

	var params map[string]any
	require.NoError(t, json.Unmarshal(profile.Params, &params))
	capabilities := params["clientCapabilities"].(map[string]any)
	session := capabilities["session"].(map[string]any)
	configOptions := session["configOptions"].(map[string]any)
	boolean := configOptions["boolean"].(map[string]any)
	assert.Empty(t, boolean)
}
