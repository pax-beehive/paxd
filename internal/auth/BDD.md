# internal/auth BDD test cases

These scenarios define expected behavior for remote authentication header construction and secret resolution.

`auth` converts remote credentials and `remote_auth` records into safe outbound headers. It is used by runtime sessions and should be the only place where remote auth material becomes HTTP/WebSocket headers.

## Module boundaries

Upstream callers:

- `RemoteControlSession` requests headers for node-control WebSocket dials.
- `AgentTunnelSession` requests headers for ACP tunnel WebSocket dials.
- Cloud HTTP clients may request headers for remote API calls.

Downstream dependencies:

- `RemoteAuthStore` for node API key ref and remote auth config.
- `SecretResolver` for secret refs.
- OS environment or filesystem for first-version resolver schemes.

What to mock in `auth` tests:

- Mock `RemoteAuthStore` for header provider tests.
- Use fake environment/filesystem helpers for resolver tests.
- Do not use real runtime sessions, supervisors, or WebSocket dialers.

Who uses `auth` test helpers:

- Runtime tests may use a fake `HeaderProvider`.
- Auth-specific fake resolvers can be reused by control/store tests that need to assert secrets are not exposed.

Boundary rule:

- If a test asserts header construction or secret ref resolution, it belongs here.
- If a test asserts what a runtime does after auth fails, it belongs in `runtime`.
- If a test asserts where auth config is persisted, it belongs in `daemonstore`.

## Header provider

### Scenario: returns Pax node key header

Given remote auth store returns a node API key ref for `remote_prod`  
And the secret resolver resolves the ref  
When `HeaderProvider.Headers` is called for `remote_prod`  
Then the returned headers include `X-Pax-Key`  
And the header value equals the resolved node API key

### Scenario: returns Cloudflare Access headers

Given remote auth store returns Cloudflare Access client id and secret ref  
And the secret resolver resolves the ref  
When `HeaderProvider.Headers` is called  
Then headers include `CF-Access-Client-Id`  
And headers include `CF-Access-Client-Secret`  
And the secret is resolved only in memory

### Scenario: no remote auth returns only Pax key

Given remote auth kind is `none`  
When headers are requested  
Then only Pax node auth headers are returned  
And no Cloudflare Access headers are set

### Scenario: missing node key ref returns auth error

Given the remote has no node API key ref  
When headers are requested  
Then `HeaderProvider` returns an auth error  
And no partial headers are returned

### Scenario: missing secret ref returns auth error

Given Cloudflare Access auth is configured without a secret ref  
When headers are requested  
Then `HeaderProvider` returns an auth or config error according to policy  
And no partial CF Access secret header is returned

## Secret resolver

### Scenario: resolves env secret

Given environment variable `PAX_CF_SECRET_PROD` is set  
When `SecretResolver.Resolve` receives `env:PAX_CF_SECRET_PROD`  
Then it returns the environment value

### Scenario: resolves file secret

Given a file contains secret text  
When `SecretResolver.Resolve` receives `file:/absolute/path`  
Then it returns the file contents according to trimming policy

### Scenario: resolves inline secret for dev

Given ref `inline:secret-value`  
When `SecretResolver.Resolve` is called  
Then it returns `secret-value`  
And the resolver marks or treats inline as dev/debug-only according to policy

### Scenario: rejects unsupported secret scheme

Given ref `vault:secret`  
When `SecretResolver.Resolve` is called before that scheme is implemented  
Then it returns an unsupported scheme error

## Secret safety

### Scenario: errors do not include resolved secret

Given a secret resolver resolved a secret value  
And a later header construction step fails  
When the error is returned  
Then the error message does not include the resolved secret

### Scenario: headers provider does not log secrets

Given headers contain Pax key and CF Access secret  
When the provider logs debug information  
Then logs contain remote id and auth kind only  
And do not contain raw secret values
