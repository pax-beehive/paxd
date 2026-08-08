package e2ee

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type RootKeyProvider interface {
	RootKey(ctx context.Context, agentID string, keyEpoch int64) ([]byte, error)
}

type DerivedAgentRootKeyProvider struct {
	NodeSeed []byte
}

type StaticRootKeyProvider struct {
	Key []byte
}

func (p StaticRootKeyProvider) RootKey(
	ctx context.Context,
	_ string,
	_ int64,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRootKey(p.Key); err != nil {
		return nil, err
	}
	return append([]byte(nil), p.Key...), nil
}

func (p DerivedAgentRootKeyProvider) RootKey(
	ctx context.Context,
	agentID string,
	keyEpoch int64,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return DeriveAgentRootKey(p.NodeSeed, agentID, keyEpoch)
}

func LoadOrCreateNodeSeed(ctx context.Context, path string, random io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("E2EE node seed path is required")
	}
	if random == nil {
		random = rand.Reader
	}
	seed, err := readNodeSeed(path)
	if err == nil {
		return seed, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create E2EE secret directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("protect E2EE secret directory: %w", err)
	}
	seed = make([]byte, rootKeyBytes)
	if _, err := io.ReadFull(random, seed); err != nil {
		return nil, fmt.Errorf("generate E2EE node seed: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return readNodeSeed(path)
	}
	if err != nil {
		return nil, fmt.Errorf("create E2EE node seed: %w", err)
	}
	created := true
	defer func() {
		_ = file.Close()
		if created {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(seed); err != nil {
		return nil, fmt.Errorf("write E2EE node seed: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync E2EE node seed: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close E2EE node seed: %w", err)
	}
	created = false
	return append([]byte(nil), seed...), nil
}

func readNodeSeed(path string) ([]byte, error) {
	seed, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(seed) != rootKeyBytes {
		return nil, fmt.Errorf("E2EE node seed must be %d bytes", rootKeyBytes)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("protect E2EE node seed: %w", err)
	}
	return append([]byte(nil), seed...), nil
}
