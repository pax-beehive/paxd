# internal/supervisor BDD test cases

These scenarios define expected behavior for runtime reconciliation and slot lifecycle management.

`supervisor` reconciles desired state from the store into actual runtime slots. It owns start/stop/restart decisions, interruptible backoff, session reaping, and stale generation protection. It does not parse control API messages and does not implement WebSocket/process bridging itself.

## Module boundaries

Upstream callers:

- `control.Service` calls supervisor wake ports after desired-state commits.
- daemon bootstrap starts supervisors and owns their root contexts.
- runtime sessions report phase events and exits back to their owning slots.

Downstream dependencies:

- daemonstore repository ports for listing desired remotes/agent connections and writing status.
- runtime session factories for `RemoteControlSession` and `AgentTunnelSession`.
- clock/timer abstraction for deterministic backoff tests.

What to mock in `supervisor` tests:

- Mock daemonstore repository ports with in-memory desired rows and recorded status writes.
- Mock runtime session factories and sessions; do not open real WebSockets or start real processes.
- Mock clock/timers for backoff tests.
- Do not use `localapi`, `controlws`, real `auth.HeaderProvider`, or real `acpforwarder`.

Who uses `supervisor` test helpers:

- `control` tests may use fake supervisor wake ports, not full supervisors.
- runtime session tests may use fake slots only if they need to verify exit classification handoff.

Boundary rule:

- If a test asserts diff/reconcile/start/stop/restart/backoff decisions, it belongs here.
- If a test asserts WebSocket frame behavior, it belongs in `controlws` or runtime session tests.
- If a test asserts desired-state mutation, it belongs in `control`.

## Reconcile triggers

### Scenario: reconcile runs after explicit wake

Given desired state has one enabled agent connection  
And no runtime slot exists for it  
When `Wake()` is called  
Then the supervisor reconciles desired state  
And creates a runtime slot  
And starts a session for that connection

### Scenario: reconcile runs on ticker fallback

Given the supervisor missed an explicit wake  
And desired state differs from actual runtime state  
When the reconcile ticker fires  
Then the supervisor reconciles desired state  
And actual runtime state converges toward desired state

### Scenario: reconcile runs after session exit

Given a runtime session exits with a classified transient error  
When the slot receives the exit  
Then the supervisor updates status  
And schedules backoff according to policy

## Remote supervisor

### Scenario: starts node-control session for enabled remote

Given store lists an enabled remote with generation `1`  
And no remote slot exists  
When `RemoteSupervisor` reconciles  
Then it creates a remote slot  
And starts one `RemoteControlSession`  
And writes status `connecting` or `connected` according to session lifecycle

### Scenario: records successful node-control connection

Given the current remote session publishes phase `connected`
When the owning supervisor receives the session event
Then it writes remote status `connected`
And its snapshot reports phase `connected`

### Scenario: does not start session for disabled remote

Given store lists a disabled remote  
When `RemoteSupervisor` reconciles  
Then no remote control session is started  
And status is stopped or unchanged according to policy

### Scenario: restarts remote session when generation changes

Given a remote slot is running generation `1`  
And store now lists the same remote with generation `2`  
When `RemoteSupervisor` reconciles  
Then it cancels the generation `1` session  
And starts or schedules a generation `2` session  
And stale generation `1` exits cannot overwrite generation `2` status

### Scenario: interrupts backoff when restart nonce changes

Given a remote slot is waiting in backoff for restart nonce `3`  
And store now lists restart nonce `4`  
When `RemoteSupervisor` reconciles  
Then it stops the backoff timer  
And attempts the latest desired spec immediately

## Agent connection supervisor

### Scenario: starts ACP tunnel session for enabled agent connection

Given store lists an enabled agent connection with desired state `running`  
And no slot exists for it  
When `AgentConnectionSupervisor` reconciles  
Then it creates an agent connection slot  
And starts one `AgentTunnelSession`

### Scenario: records running agent tunnel

Given the current agent tunnel session publishes phase `running`
When the owning supervisor receives the session event
Then it writes agent connection status `running` with the observed PID
And its snapshot reports phase `running`

### Scenario: does not start deleted agent connection

Given store lists an agent connection with desired state `deleted`  
When `AgentConnectionSupervisor` reconciles  
Then no tunnel session is started  
And any existing slot is stopped

### Scenario: restarts ACP tunnel when runtime-affecting generation changes

Given an agent tunnel slot is running generation `5`  
And store now lists generation `6`  
When `AgentConnectionSupervisor` reconciles  
Then it cancels the generation `5` session  
And starts or schedules generation `6`  
And only generation `6` can write current status

### Scenario: manual restart increments nonce without generation change

Given an agent tunnel slot is running generation `5` and restart nonce `1`  
And store now lists generation `5` and restart nonce `2`  
When `AgentConnectionSupervisor` reconciles  
Then it cancels the current session  
And starts or schedules a new session with restart nonce `2`

## Stop and delete behavior

### Scenario: disable cancels running session

Given a slot has a running session  
And desired state changes to disabled or stopped  
When the supervisor reconciles  
Then it cancels the session context  
And writes phase `stopping` or `stopped` according to lifecycle  
And does not retry after the session exits

### Scenario: delete cancels backoff

Given a slot is waiting in backoff  
And desired state changes to deleted  
When the supervisor reconciles  
Then it stops the backoff timer  
And writes phase `stopped`  
And removes or marks the slot inactive according to slot manager policy

### Scenario: stop does not block reconcile loop indefinitely

Given a running session does not exit immediately after cancellation  
When a stop action is applied  
Then the supervisor does not block the entire reconcile loop indefinitely  
And later desired updates can still be recorded as pending desired spec

## Interruptible backoff

### Scenario: transient exit enters backoff

Given a running session exits with failure class `transient`  
When the slot handles the exit  
Then it writes phase `backoff`  
And sets reconnect attempt and next retry timestamp  
And starts an interruptible timer

### Scenario: backoff timer fires and restarts session

Given a slot is in backoff  
When the backoff timer fires  
Then the slot starts a new session using the latest desired spec  
And increments or records reconnect attempt according to policy

### Scenario: desired update interrupts backoff

Given a slot is in backoff for generation `2`  
And desired state changes to generation `3`  
When the supervisor receives wake  
Then it cancels the backoff timer  
And attempts generation `3` immediately

### Scenario: restart interrupts failed auth/config state

Given a slot is in failed state due to auth or config failure  
And desired restart nonce increases  
When the supervisor reconciles  
Then it attempts the latest desired spec again  
And writes starting/backoff/failed according to the new session result

## Failure classification

### Scenario: auth failure enters failed state

Given a session exits with failure class `auth`  
When the slot handles the exit  
Then it writes phase `failed`  
And failure class `auth`  
And it does not schedule normal transient backoff

### Scenario: config failure enters failed state

Given a session exits with failure class `config`  
When the slot handles the exit  
Then it writes phase `failed`  
And failure class `config`  
And it waits for generation or restart nonce change before retrying

### Scenario: terminal cancellation enters stopped state

Given a session exits with failure class `terminal` after disable/delete  
When the slot handles the exit  
Then it writes phase `stopped`  
And it does not retry

## Pending desired coalescing

### Scenario: keeps only latest pending spec while old session stops

Given a slot is stopping generation `1`  
And desired updates arrive for generation `2`, then generation `3`  
When the old session finally exits  
Then the slot starts generation `3`  
And generation `2` is not started

### Scenario: stale exit does not overwrite newer status

Given generation `2` session is running  
And a canceled generation `1` session exits late  
When the slot processes the generation `1` exit  
Then the status update is conditional and affects zero rows  
And generation `2` status remains current

## Snapshot and observability

### Scenario: snapshot reports current slots

Given remote and agent slots exist  
When the supervisor snapshot is requested  
Then it returns slot ids, generations, restart nonces, phases, and pending backoff state  
And it does not include secret values

### Scenario: status writes do not include resolved secrets

Given a session fails due to auth header construction  
When the supervisor writes failure status  
Then last error fields contain safe error code/message  
And do not include resolved secrets or raw auth headers
