package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/pax-beehive/paxd/internal/control"
)

const (
	HeaderPaxKey                 = "X-Pax-Key"
	HeaderCloudflareAccessID     = "CF-Access-Client-Id"
	HeaderCloudflareAccessSecret = "CF-Access-Client-Secret"
)

var (
	ErrMissingStore      = errors.New("auth store is required")
	ErrMissingRemoteID   = errors.New("remote id is required")
	ErrMissingSecretRef  = errors.New("secret ref is required")
	ErrMissingCredential = errors.New("credential is required")
	ErrUnsupportedAuth   = errors.New("unsupported remote auth kind")
)

type HeaderProvider interface {
	Headers(ctx context.Context, remoteID string) (http.Header, error)
}

type RemoteAuthStore interface {
	GetRemoteAuthMaterial(ctx context.Context, remoteID string) (control.RemoteAuthMaterial, error)
}

type Provider struct {
	store    RemoteAuthStore
	resolver SecretResolver
}

func NewProvider(store RemoteAuthStore, resolver SecretResolver) *Provider {
	if resolver == nil {
		resolver = NewDefaultResolver()
	}
	return &Provider{store: store, resolver: resolver}
}

func (p *Provider) Headers(ctx context.Context, remoteID string) (http.Header, error) {
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		return nil, ErrMissingRemoteID
	}
	if p.store == nil {
		return nil, ErrMissingStore
	}
	resolver := p.resolver
	if resolver == nil {
		resolver = NewDefaultResolver()
	}

	material, err := p.store.GetRemoteAuthMaterial(ctx, remoteID)
	if err != nil {
		return nil, fmt.Errorf("get auth material for remote %q: %w", remoteID, err)
	}

	paxKey, err := resolveRequired(ctx, resolver, "pax node key", material.CloudAPIKeyRef)
	if err != nil {
		return nil, err
	}

	header := http.Header{}
	header.Set(HeaderPaxKey, paxKey)

	switch material.AuthKind {
	case "", control.RemoteAuthNone:
		return header, nil
	case control.RemoteAuthCloudflareAccess:
		cf := material.CloudflareAccess
		if cf == nil {
			return nil, fmt.Errorf("%w: cloudflare access config is required", ErrMissingCredential)
		}
		clientID := strings.TrimSpace(cf.ClientID)
		if clientID == "" {
			return nil, fmt.Errorf("%w: cloudflare access client id", ErrMissingCredential)
		}
		clientSecret, err := resolveRequired(ctx, resolver, "cloudflare access client secret", cf.ClientSecretRef)
		if err != nil {
			return nil, err
		}
		header.Set(HeaderCloudflareAccessID, clientID)
		header.Set(HeaderCloudflareAccessSecret, clientSecret)
		return header, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedAuth, material.AuthKind)
	}
}

func resolveRequired(ctx context.Context, resolver SecretResolver, name string, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("%w: %s", ErrMissingSecretRef, name)
	}
	value, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", name, err)
	}
	if value == "" {
		return "", fmt.Errorf("%w: %s resolved empty value", ErrMissingCredential, name)
	}
	return value, nil
}
