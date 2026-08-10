package e2ee

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNodeSeedGivenFirstStartWhenLoadedThenCreatesProtectedStableSeed(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "secrets", "e2ee_node_seed")
	random := bytes.NewReader(bytes.Repeat([]byte{9}, 32))

	first, err := LoadOrCreateNodeSeed(context.Background(), path, random)
	require.NoError(t, err)
	second, err := LoadOrCreateNodeSeed(context.Background(), path, bytes.NewReader(nil))
	require.NoError(t, err)

	assert.Equal(t, bytes.Repeat([]byte{9}, 32), first)
	assert.Equal(t, first, second)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dirInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
}

func TestNodeSeedGivenCorruptSeedWhenLoadedThenFailsClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "e2ee_node_seed")
	require.NoError(t, os.WriteFile(path, []byte("short"), 0o600))

	_, err := LoadOrCreateNodeSeed(context.Background(), path, bytes.NewReader(nil))

	require.ErrorContains(t, err, "must be 32 bytes")
}

func TestDerivedAgentRootKeyProviderGivenEpochWhenRequestedThenUsesNodeSeedDomainSeparation(t *testing.T) {
	t.Parallel()
	seed := bytes.Repeat([]byte{3}, 32)
	provider := DerivedAgentRootKeyProvider{NodeSeed: seed}

	got, err := provider.RootKey(context.Background(), "agent_1", 2)
	require.NoError(t, err)
	want, err := DeriveAgentRootKey(seed, "agent_1", 2)
	require.NoError(t, err)

	assert.Equal(t, want, got)
}

func TestStaticRootKeyProviderGivenLegacyKeyWhenRequestedThenReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()
	want := bytes.Repeat([]byte{7}, 32)
	provider := StaticRootKeyProvider{Key: want}

	got, err := provider.RootKey(context.Background(), "agent_1", 9)
	require.NoError(t, err)
	got[0] ^= 0xff

	assert.Equal(t, byte(7), want[0])
	_, err = (StaticRootKeyProvider{Key: []byte("short")}).RootKey(context.Background(), "agent_1", 1)
	assert.Error(t, err)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = provider.RootKey(canceled, "agent_1", 1)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestNodeSeedGivenInvalidRequestWhenLoadedThenFailsBeforeWriting(t *testing.T) {
	t.Parallel()
	_, err := LoadOrCreateNodeSeed(context.Background(), "", nil)
	assert.ErrorContains(t, err, "path is required")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = LoadOrCreateNodeSeed(canceled, filepath.Join(t.TempDir(), "seed"), nil)
	assert.ErrorIs(t, err, context.Canceled)
}
