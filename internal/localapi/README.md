# internal/localapi

`internal/localapi` is the local request/response adapter for paxd control.

It serves the same HTTP handler over two possible listeners:

- Unix domain socket, enabled by default for `paxctl`, TUI, and local programs
- optional `127.0.0.1` debug HTTP listener for curl, Postman, and browser debugging

This package owns:

- HTTP route mapping
- request parsing
- response encoding
- local source attribution (`source=local`)

This package must not:

- write desired-state tables directly
- import the GORM store directly
- wake supervisors directly
- implement business validation independently from `internal/control`

All behavior should be delegated to `control.Service`.

## Primary Interfaces

`localapi` should expose a single handler constructor:

```go
func NewHandler(control control.Service) http.Handler
```

Daemon/bootstrap code is responsible for serving that handler on listeners:

```go
http.Serve(unixListener, localapi.NewHandler(controlService))
http.Serve(localhostDebugListener, localapi.NewHandler(controlService))
```

Transport-specific request helpers can stay internal to this package:

```go
type CommandIDProvider interface {
    CommandID(r *http.Request) string
}

type ErrorEncoder interface {
    WriteError(w http.ResponseWriter, err control.ControlError)
}
```

Handlers should construct typed `control.Command` and `control.Query` values before calling `control.Service`.

## Testing

Transport tests should use a mock `control.Service`.

They should assert:

- an HTTP request maps to the expected typed `control.Query` or `control.Command`
- a mock service result maps to the expected HTTP status and response body

They should not assert database state.

## Harness authentication

`harness_auth.login` and `harness_auth.status` expose short-lived Claude and Codex native CLI
login sessions through the shared control layer. Login commands bypass the
durable command journal so authorization codes are never persisted there.
See [the login protocol and lifecycle](../harnessauth/README.md).
