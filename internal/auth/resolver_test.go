package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultResolverResolvesEnvFileAndInlineRefs(t *testing.T) {
	ctx := context.Background()
	t.Setenv("PAX_SECRET_TEST", "env-secret")
	path := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	resolver := NewDefaultResolver()
	tests := []struct {
		ref  string
		want string
	}{
		{ref: "env:PAX_SECRET_TEST", want: "env-secret"},
		{ref: "file:" + path, want: "file-secret"},
		{ref: "inline:inline-secret", want: "inline-secret"},
	}
	for _, test := range tests {
		got, err := resolver.Resolve(ctx, test.ref)
		if err != nil {
			t.Fatalf("Resolve(%q) error = %v", test.ref, err)
		}
		if got != test.want {
			t.Fatalf("Resolve(%q) = %q, want %q", test.ref, got, test.want)
		}
	}
}

func TestDefaultResolverRejectsUnsupportedAndMissingRefs(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		ref     string
		wantErr error
	}{
		{name: "bare", ref: "node_key_ref", wantErr: ErrUnsupportedSecretScheme},
		{name: "vault not implemented yet", ref: "vault:pax/node", wantErr: ErrUnsupportedSecretScheme},
		{name: "missing env", ref: "env:PAX_MISSING_SECRET", wantErr: ErrMissingSecret},
		{name: "relative file", ref: "file:relative/path", wantErr: ErrUnsupportedSecretScheme},
		{name: "empty inline", ref: "inline:", wantErr: ErrMissingSecret},
	}
	resolver := NewDefaultResolver()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := resolver.Resolve(ctx, test.ref)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Resolve(%q) error = %v, want %v", test.ref, err, test.wantErr)
			}
		})
	}
}

func TestDefaultResolverCanDisableInlineRefs(t *testing.T) {
	_, err := (DefaultResolver{AllowInline: false}).Resolve(context.Background(), "inline:secret")
	if !errors.Is(err, ErrInlineSecretDisabled) {
		t.Fatalf("Resolve(inline) error = %v, want %v", err, ErrInlineSecretDisabled)
	}
}
