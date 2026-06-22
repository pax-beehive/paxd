package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrUnsupportedSecretScheme = errors.New("unsupported secret ref scheme")
	ErrMissingSecret           = errors.New("secret not found")
	ErrInlineSecretDisabled    = errors.New("inline secret refs are disabled")
)

type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

type DefaultResolver struct {
	AllowInline bool
}

func NewDefaultResolver() DefaultResolver {
	return DefaultResolver{AllowInline: true}
}

func (r DefaultResolver) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	scheme, value, ok := strings.Cut(strings.TrimSpace(ref), ":")
	if !ok || scheme == "" {
		return "", fmt.Errorf("%w: %q", ErrUnsupportedSecretScheme, ref)
	}
	switch scheme {
	case "env":
		return resolveEnv(value)
	case "file":
		return resolveFile(value)
	case "inline":
		if !r.AllowInline {
			return "", ErrInlineSecretDisabled
		}
		if value == "" {
			return "", ErrMissingSecret
		}
		return value, nil
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedSecretScheme, scheme)
	}
}

func resolveEnv(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrMissingSecret
	}
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		return "", fmt.Errorf("%w: env:%s", ErrMissingSecret, name)
	}
	return value, nil
}

func resolveFile(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: file ref must use an absolute path", ErrUnsupportedSecretScheme)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file %q: %w", path, err)
	}
	value := strings.TrimRight(string(data), "\r\n")
	if value == "" {
		return "", ErrMissingSecret
	}
	return value, nil
}
