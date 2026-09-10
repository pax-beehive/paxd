package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
)

func TestProviderHeadersIgnoresLegacyCloudflareAccess(t *testing.T) {
	store := fakeStore{
		material: control.RemoteAuthMaterial{
			RemoteID:       "remote_prod",
			CloudAPIKeyRef: "env:PAX_NODE_KEY",
			AuthKind:       control.RemoteAuthCloudflareAccess,
			CloudflareAccess: &control.CloudflareAccessAuth{
				ClientID:        "cf-client",
				ClientSecretRef: "env:PAX_CF_SECRET",
			},
		},
	}
	resolver := fakeResolver{values: map[string]string{
		"env:PAX_NODE_KEY": "node-secret",
	}}

	headers, err := NewProvider(store, resolver).Headers(context.Background(), "remote_prod")
	if err != nil {
		t.Fatalf("Headers() error = %v", err)
	}
	assertHeader(t, headers, HeaderPaxKey, "node-secret")
	assertHeader(t, headers, HeaderCloudflareAccessID, "")
	assertHeader(t, headers, HeaderCloudflareAccessSecret, "")
}

func TestProviderHeadersNoRemoteAuthReturnsOnlyPaxKey(t *testing.T) {
	store := fakeStore{
		material: control.RemoteAuthMaterial{
			RemoteID:       "remote_prod",
			CloudAPIKeyRef: "inline:node-secret",
			AuthKind:       control.RemoteAuthNone,
		},
	}

	headers, err := NewProvider(store, nil).Headers(context.Background(), "remote_prod")
	if err != nil {
		t.Fatalf("Headers() error = %v", err)
	}
	assertHeader(t, headers, HeaderPaxKey, "node-secret")
	if got := headers.Get(HeaderCloudflareAccessSecret); got != "" {
		t.Fatalf("%s = %q, want empty", HeaderCloudflareAccessSecret, got)
	}
}

func TestProviderHeadersReturnsNoPartialHeadersOnErrors(t *testing.T) {
	tests := []struct {
		name     string
		material control.RemoteAuthMaterial
		wantErr  error
	}{
		{
			name: "missing node key ref",
			material: control.RemoteAuthMaterial{
				RemoteID: "remote_prod",
				AuthKind: control.RemoteAuthNone,
			},
			wantErr: ErrMissingSecretRef,
		},
		{
			name: "unsupported auth kind",
			material: control.RemoteAuthMaterial{
				RemoteID:       "remote_prod",
				CloudAPIKeyRef: "inline:node-secret",
				AuthKind:       control.RemoteAuthKind("custom_gateway"),
			},
			wantErr: ErrUnsupportedAuth,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			headers, err := NewProvider(fakeStore{material: test.material}, nil).Headers(context.Background(), "remote_prod")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Headers() error = %v, want %v", err, test.wantErr)
			}
			if headers != nil {
				t.Fatalf("Headers() returned partial headers: %+v", headers)
			}
		})
	}
}

func TestProviderErrorDoesNotIncludeResolvedSecret(t *testing.T) {
	store := fakeStore{
		material: control.RemoteAuthMaterial{
			RemoteID:       "remote_prod",
			CloudAPIKeyRef: "env:PAX_NODE_KEY",
			AuthKind:       control.RemoteAuthKind("custom_gateway"),
		},
	}
	resolver := fakeResolver{values: map[string]string{"env:PAX_NODE_KEY": "node-secret"}}

	_, err := NewProvider(store, resolver).Headers(context.Background(), "remote_prod")
	if err == nil {
		t.Fatal("Headers() error = nil")
	}
	if strings.Contains(err.Error(), "node-secret") {
		t.Fatalf("error leaked resolved secret: %v", err)
	}
}

type fakeStore struct {
	material control.RemoteAuthMaterial
	err      error
}

func (s fakeStore) GetRemoteAuthMaterial(ctx context.Context, remoteID string) (control.RemoteAuthMaterial, error) {
	_ = ctx
	if s.err != nil {
		return control.RemoteAuthMaterial{}, s.err
	}
	if s.material.RemoteID != "" && s.material.RemoteID != remoteID {
		return control.RemoteAuthMaterial{}, control.ErrNotFound
	}
	return s.material, nil
}

type fakeResolver struct {
	values map[string]string
	err    error
}

func (r fakeResolver) Resolve(ctx context.Context, ref string) (string, error) {
	_ = ctx
	if r.err != nil {
		return "", r.err
	}
	return r.values[ref], nil
}

func assertHeader(t *testing.T, headers http.Header, name, want string) {
	t.Helper()
	if got := headers.Get(name); got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}
