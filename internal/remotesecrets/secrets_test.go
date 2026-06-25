package remotesecrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreWritesNodeKeyAsOwnerOnlyFileRef(t *testing.T) {
	dir := t.TempDir()
	store := Store{HomeDir: dir}

	ref, err := store.StoreNodeKey(context.Background(), "prod", "node-secret")
	require.NoError(t, err)

	path := filepath.Join(dir, ".paxd", "secrets", "remotes", "prod", "node_key")
	assert.Equal(t, "file:"+path, ref)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "node-secret\n", string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())

	nodeKeyRef, err := store.NodeKeyRef("prod")
	require.NoError(t, err)
	assert.Equal(t, ref, nodeKeyRef)
}

func TestStoreRejectsUnsafeRemoteIDs(t *testing.T) {
	store := Store{HomeDir: t.TempDir()}

	tests := []string{"", "../prod", "prod/staging", "prod staging", ".hidden"}
	for _, remoteID := range tests {
		t.Run(remoteID, func(t *testing.T) {
			_, err := store.StoreNodeKey(context.Background(), remoteID, "node-secret")
			require.Error(t, err)
		})
	}
}

func TestStoreOverwritesExistingNodeKeyWithOwnerOnlyMode(t *testing.T) {
	dir := t.TempDir()
	store := Store{HomeDir: dir}

	_, err := store.StoreNodeKey(context.Background(), "prod", "old-secret")
	require.NoError(t, err)

	ref, err := store.StoreNodeKey(context.Background(), "prod", "new-secret")
	require.NoError(t, err)

	path := filepath.Join(dir, ".paxd", "secrets", "remotes", "prod", "node_key")
	assert.Equal(t, "file:"+path, ref)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new-secret\n", string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestStoreRejectsEmptyNodeKey(t *testing.T) {
	store := Store{HomeDir: t.TempDir()}

	_, err := store.StoreNodeKey(context.Background(), "prod", " ")

	require.Error(t, err)
}

func TestDeleteRemoteRemovesRemoteSecretDirectory(t *testing.T) {
	dir := t.TempDir()
	store := Store{HomeDir: dir}
	_, err := store.StoreNodeKey(context.Background(), "prod", "node-secret")
	require.NoError(t, err)

	err = store.DeleteRemote(context.Background(), "prod")

	require.NoError(t, err)
	assert.NoDirExists(t, filepath.Join(dir, ".paxd", "secrets", "remotes", "prod"))
}

func TestDeleteRemoteRejectsUnsafeRemoteID(t *testing.T) {
	err := (Store{HomeDir: t.TempDir()}).DeleteRemote(context.Background(), "../prod")

	require.Error(t, err)
}

func TestStoreHonorsCanceledContextBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	store := Store{HomeDir: dir}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := store.StoreNodeKey(ctx, "prod", "node-secret")

	require.ErrorIs(t, err, context.Canceled)
	_, statErr := os.Stat(filepath.Join(dir, ".paxd"))
	require.True(t, errors.Is(statErr, os.ErrNotExist))
}

func TestDeleteRemoteHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := (Store{HomeDir: t.TempDir()}).DeleteRemote(ctx, "prod")

	require.ErrorIs(t, err, context.Canceled)
}

func TestStoreUsesUserHomeWhenHomeDirIsOmitted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	store := Store{}

	ref, err := store.StoreNodeKey(context.Background(), "prod", "node-secret")
	require.NoError(t, err)

	path := filepath.Join(dir, ".paxd", "secrets", "remotes", "prod", "node_key")
	assert.Equal(t, "file:"+path, ref)
	assert.FileExists(t, path)
}
