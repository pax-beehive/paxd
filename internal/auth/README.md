# internal/auth

`internal/auth` resolves remote authentication material into request headers.

Remote authentication belongs to `remotes` and `remote_auth`, not to individual agent connections.

Both node-control WebSocket sessions and agent ACP tunnel sessions should use the same header provider.

## Primary Interfaces

```go
type HeaderProvider interface {
    Headers(ctx context.Context, remoteID string) (http.Header, error)
}

type SecretResolver interface {
    Resolve(ctx context.Context, ref string) (string, error)
}

type RemoteAuthStore interface {
    GetRemoteAuthMaterial(ctx context.Context, remoteID string) (RemoteAuthMaterial, error)
}

type RemoteAuthMaterial struct {
    RemoteID          string
    CloudAPIKeyRef    string
    Kind              string // none | cloudflare_access
    CloudflareAccess  *CloudflareAccessAuth
}

type CloudflareAccessAuth struct {
    ClientID        string
    ClientSecretRef string
}
```

This package owns:

- `X-Pax-Key` header construction from the remote node key ref
- compatibility with legacy `cloudflare_access` records, without resolving or
  transmitting their service-token credentials
- secret ref resolution through `SecretResolver`

This package must not:

- store resolved secrets in status rows
- log resolved secrets
- return resolved secrets through debug APIs or TUI views
- attach auth material to `agent_connections`

First-version secret ref schemes:

- `env:NAME`
- `file:/absolute/path`
- `inline:value` for dev/debug only

Future vault, keychain, or KMS integrations should be added by composing or replacing `SecretResolver`; callers should still pass only secret refs.

## Boundary

`HeaderProvider` is the only place that should turn remote credential refs and `remote_auth` records into outbound HTTP/WebSocket headers.

Runtime sessions should ask for headers by `remoteID`. They should not read `remote_auth` directly.
