package daemon

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
