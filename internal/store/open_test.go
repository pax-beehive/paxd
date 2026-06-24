package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCreatesMissingParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "nested", "paxd.db")

	store, err := Open(path)

	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close()) })
	assert.FileExists(t, path)
}
