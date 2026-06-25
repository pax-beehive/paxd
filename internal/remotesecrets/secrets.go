package remotesecrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	ErrInvalidRemoteID = errors.New("invalid remote id")
	ErrMissingNodeKey  = errors.New("node key is required")
)

var safeRemoteID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

type Store struct {
	HomeDir string
}

func (s Store) StoreNodeKey(ctx context.Context, remoteID string, nodeKey string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	remoteID = strings.TrimSpace(remoteID)
	if !safeRemoteID.MatchString(remoteID) {
		return "", fmt.Errorf("%w: %q", ErrInvalidRemoteID, remoteID)
	}
	nodeKey = strings.TrimSpace(nodeKey)
	if nodeKey == "" {
		return "", ErrMissingNodeKey
	}

	home, err := s.homeDir()
	if err != nil {
		return "", err
	}
	path := nodeKeyPath(home, remoteID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", fmt.Errorf("create remote secret dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(nodeKey+"\n"), 0600); err != nil {
		return "", fmt.Errorf("write node key secret: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		return "", fmt.Errorf("chmod node key secret: %w", err)
	}
	return "file:" + path, nil
}

func (s Store) NodeKeyRef(remoteID string) (string, error) {
	remoteID = strings.TrimSpace(remoteID)
	if !safeRemoteID.MatchString(remoteID) {
		return "", fmt.Errorf("%w: %q", ErrInvalidRemoteID, remoteID)
	}
	home, err := s.homeDir()
	if err != nil {
		return "", err
	}
	return "file:" + nodeKeyPath(home, remoteID), nil
}

func (s Store) DeleteRemote(ctx context.Context, remoteID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	remoteID = strings.TrimSpace(remoteID)
	if !safeRemoteID.MatchString(remoteID) {
		return fmt.Errorf("%w: %q", ErrInvalidRemoteID, remoteID)
	}
	home, err := s.homeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".paxd", "secrets", "remotes", remoteID)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("delete remote secret dir: %w", err)
	}
	return nil
}

func nodeKeyPath(home string, remoteID string) string {
	return filepath.Join(home, ".paxd", "secrets", "remotes", remoteID, "node_key")
}

func (s Store) homeDir() (string, error) {
	if strings.TrimSpace(s.HomeDir) != "" {
		return s.HomeDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return home, nil
}
