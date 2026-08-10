package daemon

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/pax-beehive/paxd/internal/e2ee"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2EENodeSeedGivenNoLegacyEnvironmentKeyWhenLoadedThenDerivesPerAgentRoot(t *testing.T) {
	t.Setenv("PAX_E2EE_ROOT_KEY", "")
	databasePath := filepath.Join(t.TempDir(), "paxd.db")
	seed, err := e2ee.LoadOrCreateNodeSeed(
		context.Background(), e2eeNodeSeedPath(databasePath), nil,
	)
	require.NoError(t, err)
	provider := e2ee.DerivedAgentRootKeyProvider{NodeSeed: seed}

	root, err := provider.RootKey(context.Background(), "agent_1", 1)
	require.NoError(t, err)
	assert.Len(t, root, 32)
	info, err := os.Stat(e2eeNodeSeedPath(databasePath))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestLoadE2EERootKeyFromEnvironment(t *testing.T) {
	t.Run("unset disables E2EE", func(t *testing.T) {
		t.Setenv("PAX_E2EE_ROOT_KEY", "")

		rootKey, err := loadE2EERootKeyFromEnvironment()

		require.NoError(t, err)
		assert.Nil(t, rootKey)
	})

	t.Run("valid key is loaded", func(t *testing.T) {
		expected := make([]byte, 32)
		for index := range expected {
			expected[index] = byte(index)
		}
		t.Setenv("PAX_E2EE_ROOT_KEY", base64.StdEncoding.EncodeToString(expected))

		rootKey, err := loadE2EERootKeyFromEnvironment()

		require.NoError(t, err)
		assert.Equal(t, expected, rootKey)
	})

	t.Run("invalid key fails closed", func(t *testing.T) {
		t.Setenv("PAX_E2EE_ROOT_KEY", "not-base64")

		_, err := loadE2EERootKeyFromEnvironment()

		require.ErrorContains(t, err, "PAX_E2EE_ROOT_KEY")
	})
}
