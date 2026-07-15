# ACP Slot Pool 实施计划（中文版）

状态：提案

范围：paxd 的 ACP 进程池、粘性 session 路由、进程恢复，以及安全开放并发所需的最小 pax-manager 改动。

本文件是英文版 `acp_slot_pool_plan.md` 的中文版本，两份文档采用相同的架构决策和实施边界。实现时如果发现两份文档存在语义差异，应先同步修正文档，不能自行选择更方便实现的一种解释。

## 1. 最终目标

一个 pax-manager 到 paxd 的 agent tunnel，对应一组本地 ACP stdio 进程。每个本地 ACP 进程由一个 `ACPSlot` 表示。

只要 slot 对应的进程仍然存活，一个 native ACP session 就固定路由到这个 slot。如果 slot 进程已经消失，paxd 必须先选择新的 slot，通过 `session/resume` 恢复该 session，然后才能转发下一条 session 操作。

这不是引入一种与现有 slot 无关的新概念，而是扩展当前的单 ACP 模型。现在一个 supervisor `runtimeSlot` 实际上把一个 agent connection、一条 tunnel 的生命周期和一个持久 ACP worker 合在了一起。迁移后要把这个合体拆成：一个负责唯一 tunnel 的 connection runtime，以及它下面 N 个进程级 `ACPSlot` worker。

第一轮生产发布继续保留 pax-manager 现有的“一个 agent 同时只有一个 active run”限制。在这层限制下先验证多 slot 的进程生命周期、粘性路由、resume 和 tunnel recovery。上述行为稳定后，再解除 manager 的 agent 级并发锁。

## 2. 已确定的架构决策

以下内容属于计划本身，不留到写代码时临时决定。

1. 每个 `agent_connection` 仍然只有一条物理 ACP WebSocket tunnel。
2. 现有 `AgentConnectionSupervisor` 的 runtime slot 是单 ACP 模型中的合体 slot。由于当前 connection、tunnel 和 ACP process 是 1:1:1，它在效果上既是 connection runtime，也是唯一的 ACP worker。迁移后保留它作为 connection 级 runtime，继续负责唯一 tunnel 的连接、重连和 backoff；不能靠复制现有 runtime slot 来增加 ACP 容量，否则会同时创建多条 tunnel。
3. 从这个合体中拆出 ACP process 一侧，并扩展成 N 个进程级 `ACPSlot`。typed `ACPSlotSupervisor` 复用现有通用 `baseSupervisor/runtimeSlot` 机制管理这些进程，不另写一套无关的 supervisor framework。tunnel 短暂断开不能重启仍然健康的 ACP slot 进程。
4. v1 使用 SQLite 中可热更新的 `agent_connection.desired_acp_slots`，其含义是准确的热 ACP 进程数量。第一版不做基于负载的自动扩缩容、一致性哈希和自动 idle 回收。
5. session 路由以显式持久化的 route table 为准，不使用 consistent hash。
6. 粘性路由的权威 key 是 `(connection_id, native_session_id)`。不能假设 native session ID 在不同 agent 之间全局唯一。
7. `session/new` 是常规流程中唯一没有 native session ID 的请求。paxd 在转发前选择 slot；拿到成功响应后，必须先持久化 native session route，再把响应交给 pax-manager。
8. paxd 才是每个本地 ACP worker 的 protocol client，并拥有 initialize descriptor。descriptor 根据 paxd 已实现的能力、版本和本地配置生成；每个 slot process epoch 使用内部 request ID 恰好 initialize 一次。pax-manager 不提供、不确认 worker init，只接收 capability report。
9. 每个 slot 同时最多只有一个 active prompt turn。属于该 active turn 的 cancel，以及 ACP 主动发起的 permission/file/terminal request 的响应，可以在 active 期间通过。
10. v1 不在 paxd 内部用纯内存队列排队 busy slot 后面的 prompt，而是返回明确、可重试的 busy error。reliable frame 此时可能已经在 transport boundary 被标记为 received/applied；busy error 是明确的业务结果，不能再把它隐藏到另一条 paxd prompt queue 后面。
11. 本计划不实现 CPU、内存等 metrics，也不实现资源限制；但会建立后续 metrics 所需的稳定 slot identity 和只读 snapshot 接口。
12. `reliablemq` 继续保持每个 connection 一条 transport queue。identity 仍然是 `queue_id + stream + seq + direction`；v1 不把 `queue_id` 改绑到 slot 或 session，也不增加 per-session transport stream。
13. `reliablemq.Engine.Send` 改为异步 acceptance。它只校验并 enqueue outbound message，不等待 journal flush、WebSocket write 或持久化 `sent` patch；它不再返回 `Frame`，transport sequence 分配和发送由 queue owner 负责。
14. reliable producer 本身就是 outbound queue。paxd 不能在它前面再增加 ACP 专用 outbox。所有 slot 都 publish 到同一个 producer；journal worker 和 network worker 在同一条 ordered log 上独立前进。
15. backlog 和新接受的 frame 只有一条 network send path。reconnect 根据 peer cumulative ACK 重置这条 path，不能打开绕过 backlog 的 direct live-send path。
16. pax-manager 在 frame 到达后按 manager session 做 ACP 业务分拣。这是 application-level demultiplexing，不改变 reliablemq 的 sequence、ACK、journal 或 reconcile identity。
17. ACP 和 manager conversation API 都不新增幂等承诺。已经 journaled 或被 peer durable-recorded 的 transport work 保持有序、可 replay；尚未 flush 的 producer tail 存在本文明确说明的 send-first crash window。full process crash 下出现业务副作用歧义时，必须让调用方看到。

ACP v1 要求创建 session 前必须完成 initialization。稳定版 `session/resume` 要求提供 `sessionId`、`cwd` 和 `mcpServers`：

- https://agentclientprotocol.com/protocol/v1/session-setup
- https://agentclientprotocol.com/announcements/session-resume-stabilized

## 3. 名词与所有权

### 现有合体 runtime slot / connection runtime

现有通用 supervisor 的 `runtimeSlot` 以 `connection_id` 为 key。当前单 ACP 拓扑下，它每次启动一个 `AgentTunnelSession` attempt，而这个 attempt 会取得该 connection 唯一的持久 ACP process。因此，把现有 runtime slot 理解为旧的 ACP slot 是合理的：connection runtime 和 ACP worker 当前被折叠成了一个有效运行单元。

完成迁移后，同一个 supervisor 单元只承担 connection runtime 的职责，负责唯一 tunnel 的 reconnect 和 backoff；拆出来的进程级 `ACPSlot` 分别拥有具体的 ACP process。每个 connection 仍然只保留一个现有 runtime slot，绝不会创建 N 个它的副本。

### 如何复用现有 supervisor

现有通用 supervisor 继续作为生命周期框架，只增加第二个 typed specialization：

```text
baseSupervisor/runtimeSlot
  -> AgentConnectionSupervisor
       key: connection_id
       session: 一次 AgentTunnelSession attempt
  -> ACPSlotSupervisor
       key: slot_id
       session: 一个 ACPSlot process epoch
```

全局只有一个 daemon 级 `ACPSlotSupervisor`，与现有 daemon 级 `AgentConnectionSupervisor` 对称；它维护以 `slot_id` 为 key 的 runtime slot map。`connection_id` 通过 `ACPPoolRegistry` 对 slot 分组，但不会为每个 connection 单独启动一个 supervisor goroutine。

两个 typed supervisor 是由 SQLite desired state 和 pool registry 协调的平级 owner，不共享父子 cancellation scope。尤其是，一次 `AgentTunnelSession` attempt 结束时绝不能 cancel `ACPSlot` runtime。停止或删除 `agent_connection` 时，其 tunnel desired spec 和所有合成的 ACP slot spec 都消失；tunnel 独立关闭，slot supervisor 负责 drain 并停止进程。

现有通用实现可以直接复用 desired-list reconcile、wake 合并、周期 reconcile、start attempt、backoff 和进程内 stale-attempt 拒绝。为了正确管理 ACP process，需要先增加以下窄扩展：

1. desired-change 判断改为注入式 operation。现有 tunnel slot 继续比较 generation/restart nonce；ACP slot 比较稳定 `slot_id` 和 `CommandFingerprint`。仅 slot count 变化只会增加或删除 spec，不引入 pool generation。
2. 所有 reconcile 入口都必须串行，包括外部直接调用 `Reconcile` 的情况，不能只依赖 `Start` loop 天然串行。每一轮都先读取最新 `desired_acp_slots`，再合成 slot spec。
3. stop/replacement 必须支持 drain transition。移除 active ACP slot 时，先关闭 admission 并标记 `draining`；lease 释放后才 cancel/terminate process，除非显式 daemon shutdown/cancel policy 接管。现有立即 cancel 的 `runtimeSlot.Stop` 不满足要求。
4. 新 ACP process attempt 在 ready 前生成并持久化新 `process_epoch`。现有内存 attempt token 用于拒绝 daemon 内的 late callback；数据库 status 和 route write 还必须带 `(slot_id, process_epoch)` 条件，防止旧 process exit 覆盖 replacement。
5. `ACPSlotSupervisor` 提供 ACP 专用 session factory、status writer、exit handler 和 stop/drain hook，不能复用 tunnel 的 status writer 或 transport-queue rotation exit handler。

### ACP pool

每个 `agent_connection` 对应一个 pool。pool 负责：

- native session 路由；
- canonical paxd client-init descriptor 和 worker initialize result；
- JSON-RPC request ID 转换；
- slot admission 和 selection；
- 访问这个 connection 下的所有 `ACPSlot`。

pool 内存中的 `slots map[slot_id]*ACPSlot` 就是 live runtime registry，不再引入独立的 PID registry。每个 entry 保存当前 `process_epoch`、`LocalACPProcess` handle、phase，以及 admission/lease 状态。PID 和 process group ID 只用于 metrics 与终止进程，不能作为路由身份。

概念结构如下：

```go
type ACPSlot struct {
    SlotID       string
    ProcessEpoch string
    Process      LocalACPProcess // live handle；stopped 时为 nil
    Phase        SlotPhase
    // admission、prompt lease 和 lifecycle lease 状态
}
```

`ACPPoolRegistry` 负责 `connection_id -> *ACPPool`，`ACPPool.slots` 再负责 `slot_id -> *ACPSlot`。route resolution 通过这两层 map 查找，并比较 UUID process epoch；绝不能按 PID 查找 slot。

### ACP slot

一个逻辑 slot，具有稳定的 `slot_id` 和 ordinal。每次启动新的 stdio ACP 进程都会生成新的 `process_epoch`。

一个 slot 可以保存多个 idle ACP session，但同一时间只能执行一个 active prompt turn。

`process_epoch` 是每次启动 process 时新生成的 opaque UUID，表示这一次具体 ACP process 的 incarnation。它不是时间戳、递增计数器，也不是操作系统 PID。slot 重启后保留相同的 `slot_id`，但必须生成不同的 `process_epoch`。

session 必须绑定到 `(slot_id, process_epoch)`，绝不能只绑定 `slot_id`。同一个逻辑 slot 中启动的新 process 不继承旧 process 的 session residency；旧 session 使用前必须先 resume。

### Hot route

route 指向的 slot 当前 ready，并且 route 记录的 process epoch 与 slot 当前 epoch 一致。下一条 session 操作可以直接转发，不需要 resume。

### Cold route

native session route 仍然持久化存在，但已经没有匹配的 live process epoch。下一条有状态操作在转发之前必须先执行 `session/resume`。

## 4. 目标组件边界

结构变化可以概括为：

```text
当前：runtimeSlot(connection_id) = 一条 tunnel 生命周期 + 一个 ACP worker
目标：ConnectionRuntime(connection_id) = 一条 tunnel 生命周期
        -> ACPPool(connection_id) = N 个 ACPSlot worker
```

```text
AgentConnectionSupervisor
  -> AgentTunnelSession（一次 WebSocket attempt）
       -> ACPPoolRegistry.Get(connection_id)
       -> reconcile/replay barrier
       -> ACPRouter.HandleManagerFrame(...)

ACPSlotSupervisor
  -> 根据 SQLite desired_acp_slots 生成 ACPSlotSpec
  -> ACPSlot process lifecycle
       -> 每个 process epoch initialize 一次
       -> 管理 stdin/stdout
       -> 管理 process_epoch 和 process identity
       -> ACPRouter.HandleSlotFrame(slot_id, ...)

ACPRouter
  -> durable session route store
  -> slot admission/selection
  -> pending worker-response source validation
  -> 一个共享 reliablemq producer

reliablemq producer（每个 connection + stream 一个）
  -> ordered in-memory hot log
  -> asynchronous journal flush cursor
  -> asynchronous network cursor
```

边界约束：

- router 不建立 WebSocket；
- tunnel session 不启动 ACP 进程；
- slot supervisor 不解析 reliablemq envelope；
- ACP process 不直接持有 WebSocket sender；
- 不存在独立的 `ACPTransportOutbox`。sequence allocation、queueing、journaling、replay、ACK state 和唯一 network send path 全部由 reliablemq 持有。

## 5. 必须长期成立的 runtime invariants

实现和测试必须保证：

1. `slot_id` 跨进程重启保持稳定；`process_epoch` 每次启动都变化。
2. slot 只有在 initialize 成功后才能进入 ready。
3. 每个 slot 同时最多存在一个 prompt lease。
4. 一个 native session 在本地 pool 中同时最多存在一个 prompt lease。
5. hot route 必须且只能指向一个 `(slot_id, process_epoch)`；只有该 epoch 仍是 slot 当前存活的 process epoch 时才算有效 hot，仅仅 `slot_id` 相同不够。
6. slot process 退出时，必须同步关闭该 epoch 的内存 admission；随后 eager 执行带索引的 SQLite binding cleanup，但数据库清理不能成为唯一 correctness guard。
7. scale down 不能直接杀死 active slot。必须先进入 draining，active turn 完成后才能停止。
8. `session/new` 成功响应对 manager 可见之前，对应 route 必须已经持久化。
9. cold session 在 `session/resume` 成功之前不能收到 prompt。
10. resume 失败后绝不能创建替代 session。
11. 多个 slot 产生的 frame 进入同一个 reliablemq producer，并按 producer acceptance order 获得一条单调递增 transport sequence。因此 same-session order 一定保持；cross-session total order 虽然强于产品要求，但仍是 v1 transport 行为。
12. network cursor 只能发送当前 head sequence。无论 frame 属于哪个 session，只要 head 失败或不可用，就不能跳过它发送后面的 frame。
13. tunnel recovery 必须先完成 reconcile，才能 enable network cursor。manager-to-paxd inbound replay 完成前不能接收 manager 新工作；slot 新 output 可以继续 enqueue 在 outbound backlog 后面。
14. tunnel 断开不能停止健康的 ACP slot。network cursor 不可用期间，slot stdout 继续进入 reliable producer，并由 journal worker 异步持久化。
15. worker-originated JSON-RPC request ID 以 native session 为 scope；跨 slot 路由 response 时绝不能只看 ID。
16. command、env value、MCP secret、完整 lifecycle descriptor 不能写入日志和状态上报。

## 6. SQLite 数据模型

### 扩展 `agent_connection`

```text
desired_acp_slots INTEGER NOT NULL DEFAULT 1
```

规则：

- `desired_acp_slots` 直接属于现有 `agent_connection` 行，因为 pool 和物理 tunnel 共用同一个 `connection_id` 生命周期；
- 第一版限制为 `1..16`；
- 更新时不能增加 `agent_connection.generation`，不能旋转 transport queue，也不能仅因为 slot 数量变化就重连物理 tunnel；
- migration 为现有 agent connection 填入默认值 `1`；
- 不设置独立的 pool generation。全局唯一的 daemon 级 `ACPSlotSupervisor` 串行执行 reconcile pass；wake 可以合并，但每次执行 reconcile 前必须重新读取所有 connection 的最新 desired count。

### `acp_slot_status`

```text
slot_id TEXT PRIMARY KEY
connection_id TEXT NOT NULL REFERENCES agent_connection(id)
ordinal INTEGER NOT NULL
process_epoch TEXT
pid INTEGER
process_group_id INTEGER
process_start_token TEXT
phase TEXT NOT NULL
failure_class TEXT NOT NULL DEFAULT ''
last_error_code TEXT NOT NULL DEFAULT ''
last_error_message TEXT NOT NULL DEFAULT ''
started_at TEXT
ready_at TEXT
active_since TEXT
stopped_at TEXT
updated_at TEXT NOT NULL
UNIQUE(connection_id, ordinal)
```

`slot_id` 只允许通过统一 helper，由 `connection_id + ordinal` 确定性生成，例如：

```text
slot_ + base32(sha256(connection_id + NUL + ordinal))[:26]
```

同一个 ordinal 在 scale down 后重新 scale up 时，slot ID 必须保持不变。不能同时保留另一个随机创建 slot ID 的路径。

### `acp_session_route`

```text
connection_id TEXT NOT NULL REFERENCES agent_connection(id)
native_session_id TEXT NOT NULL
bound_slot_id TEXT
bound_process_epoch TEXT
last_slot_id TEXT
resume_params_json TEXT NOT NULL
created_at TEXT NOT NULL
last_used_at TEXT NOT NULL
updated_at TEXT NOT NULL
version INTEGER NOT NULL DEFAULT 1
PRIMARY KEY(connection_id, native_session_id)
CHECK ((bound_slot_id IS NULL) = (bound_process_epoch IS NULL))
```

增加 process exit 和 scale down 所需的 binding 索引：

```sql
CREATE INDEX idx_acp_session_route_process_binding
ON acp_session_route(connection_id, bound_slot_id, bound_process_epoch)
WHERE bound_process_epoch IS NOT NULL;
```

该表保存最后一次成功提交的 process binding，不保存实时健康状态：

- 两个 binding 字段都非空：存在一个持久化 binding candidate；
- 两个 binding 字段都为空：route 为 cold；
- 只有 `ACPPool.slots` 中存在完全匹配且 ready 的 `(slot_id, process_epoch)` 时，candidate 才是 effective hot；
- `resuming` 是内存中的 per-session recovery lease；
- resume 失败是 error/event，route 保持 cold，不是持久化 route state。

process exit 和 scale down 只 eager 清理匹配的 process incarnation：

```sql
UPDATE acp_session_route
SET last_slot_id = bound_slot_id,
    bound_slot_id = NULL,
    bound_process_epoch = NULL,
    updated_at = :now,
    version = version + 1
WHERE connection_id = :connection_id
  AND bound_slot_id = :slot_id
  AND bound_process_epoch = :process_epoch;
```

daemon-start transaction 在 supervisor 启动前清空所有 binding：

```sql
UPDATE acp_session_route
SET last_slot_id = COALESCE(bound_slot_id, last_slot_id),
    bound_slot_id = NULL,
    bound_process_epoch = NULL,
    updated_at = :now,
    version = version + 1
WHERE bound_process_epoch IS NOT NULL;
```

两种操作都不能删除 route row 或其中的 resume descriptor。

`resume_params_json` 保存恢复 session 所需的 lifecycle descriptor：

```json
{
  "cwd": "/absolute/path",
  "mcpServers": [],
  "additionalDirectories": []
}
```

该 descriptor 来自 `session/new`，或者来自一次成功的显式 `session/resume`。它属于敏感本地数据：

- 不能打印日志；
- 不能进入 node-control status 或 metrics；
- 必须保留当前协商协议版本恢复所需的字段和值；
- 使用与 ACP reliable transport journal 相同的本地文件权限和 retention policy；
- 如果未来允许任意包含 secret 的 MCP 配置，数据库加密属于独立安全加固任务。

paxd route table 不保存 manager 的 `sess_*`。manager session ID 与 native session ID 的翻译权仍然只属于 pax-manager。

### `acp_client_init_profile`

pool 必须保留 paxd 作为 ACP client 是如何初始化 worker 的。该状态使用独立的 connection-scoped record，不能藏在某个任意 slot 中：

```text
connection_id TEXT PRIMARY KEY REFERENCES agent_connection(id)
params_json TEXT NOT NULL
profile_hash TEXT NOT NULL
source TEXT NOT NULL
paxd_version TEXT NOT NULL
command_fingerprint TEXT NOT NULL
canonical_result_json TEXT
result_hash TEXT
created_at TEXT NOT NULL
updated_at TEXT NOT NULL
```

`params_json` 是 paxd 生成的完整 worker `initialize.params` canonical JSON，extension field 也必须保留。`protocolVersion`、`clientCapabilities`、`clientInfo` 等已知字段用于 validation，但持久化 descriptor 不能是 lossy projection。`profile_hash` 从 canonical JSON 计算。`source` 表示本地 descriptor builder/config revision，不能来自 manager request。canonical worker result 与 `command_fingerprint` 绑定；ACP command 变化时 result 失效。

这条记录服务于 recovery、diagnostics 和未来 capability-aware routing。它不能写日志或进入 status/metrics，数据库权限和 retention policy 与 lifecycle descriptor 相同。

同一次 paxd 生命周期中，pool 还持有 local profile 的 immutable snapshot。startup 时，paxd 根据当前代码/config 重建 descriptor，并和持久化 row 比较 hash。profile 变化时 cached result 失效，并触发正常 per-slot process replacement/initialization，不需要 manager approval。

### 配置变更入口

在现有 typed `agent_connection.update` control command 中增加 optional `desired_slots`。

如果一次更新只修改 `desired_slots`：

1. 校验范围 `1..16`；
2. 更新 `agent_connection.desired_acp_slots`；
3. 不增加 `agent_connection.generation`；
4. 不旋转 `transport_queue_id`；
5. 只唤醒 `ACPSlotSupervisor`。

wake 可以合并。daemon 级 slot reconcile loop 必须串行执行，并在计算 scale up/down delta 前重新读取 `desired_acp_slots`，因此排队较早的 reconcile 不能用旧配置覆盖新 desired state。

如果同一次 command 还修改了 ACP command、working directory 或 env，则仍然执行原有 agent-connection generation 规则，同时唤醒 tunnel supervisor 和 slot supervisor。

周期性 reconcile ticker 继续作为漏掉 wake 时的兜底。

### Status 所有权

迁移后，`agent_connection_status` 只表示 tunnel 级状态。因为一个 connection 可能有多个 ACP PID，其中的单个 `pid` 字段应清空或废弃。

每个 ACP process 的 PID、phase 和 failure 必须进入 `acp_slot_status`。

runtime snapshot 可以汇总：

```text
desired_slots
ready_slots
active_slots
```

但不能挑一个任意 slot PID 重新写回 connection status。

## 7. Slot 状态机

```text
stopped
  -> starting
  -> initializing
  -> ready
  -> active
  -> ready
  -> draining
  -> stopping
  -> stopped

starting / initializing / ready / active
  -> backoff       transient process failure
  -> failed        不可重试的配置错误
```

状态语义：

- `ready`：初始化完成、当前没有 prompt lease；
- `active`：正在执行一个 `session/prompt`；
- `draining`：允许当前 turn 所需的 cancel 和 response 通过，但拒绝新 session 和新 prompt；
- `backoff`：重试 timer 由 `ACPSlotSupervisor` 持有；

`session/new` 和内部 `session/resume` 也需要 exclusive lifecycle lease。它们可以用带 operation kind 的 active 状态表示，但不能和 prompt 重叠。

## 8. 进程所有权

扩展 `LocalACPProcess`，暴露无敏感信息的 process identity：

```go
type ProcessIdentity struct {
    PID          int
    ProcessGroup int
    StartToken   string
    StartedAt    time.Time
}

type LocalACPProcess interface {
    Identity() ProcessIdentity
    Stdin() io.WriteCloser
    Stdout() io.Reader
    Stderr() io.Reader
    Wait() error
    Terminate(ctx context.Context) error
}
```

Unix 下 ACP root process 必须进入独立 process group。正常停止时先向整个 group 发送 graceful signal，超时后再强制停止整个 group，避免 ACP 创建的 tool subprocess 泄漏。`StartToken` 是由操作系统提供的 process birth identity，用于 orphan sweep 时防止 PID reuse 误杀；仅凭持久化 PID 整数绝不能获得向进程发 signal 的权限。

paxd 是其本地 data directory 的单一 owner。执行 orphan cleanup 前必须先取得覆盖整个进程生命周期的 exclusive daemon lock；如果另一个 paxd 已持有该锁，当前实例启动失败。取得锁之后，所有持久化 slot process identity 必然属于上一个 owner，因此不需要保存 daemon-owner epoch。ACP process 生命周期绝不跨越一次 paxd 生命周期，也不重新 attach 旧 stdio process。

### Daemon lock 跨平台具体约定

新增一个小型 `internal/daemonlock` package，通过平台文件实现同一个 process-lifetime 接口：

```go
var ErrAlreadyRunning = errors.New("another paxd owns this database")

type Lock interface {
    Release() error
}

func Acquire(ctx context.Context, dbPath string) (Lock, error)
```

调用 `Acquire` 前，`cmdRun` 只允许做 path preflight：展开并 canonicalize daemon SQLite path；如果 database parent directory 不存在，则先创建它；并拒绝已知 network/remote filesystem。这个 preflight 绝不能打开、migrate、查询或写入 SQLite database。lock path 随后使用 canonical absolute daemon SQLite path 加 `.lock`，例如 `~/.paxd/paxd.db.lock`。生成 lock path 前先解析 parent directory，避免同一个 DB 的 relative path 和 symlink path 获得两把独立的锁。v1 只保证 local filesystem，因为 NFS/SMB 的 locking semantics 会变化。

共同规则：

1. `cmdRun` 可以先创建并 canonicalize database parent directory，但必须在打开或 migrate SQLite、orphan cleanup、启动 local API 和 supervisor 之前取得锁。
2. lock file 以 owner-only permission 打开或创建，不能 truncate。取得锁之后可以重写 PID/start metadata 供诊断，但文件内容绝不是锁的权威来源。
3. `Lock` object 在整个 daemon 生命周期中强引用 descriptor/handle，不能依赖 finalizer。
4. 持锁期间和启动时绝不能 unlink lock file。Unix 下删除后重建会得到另一个 inode，两个进程可能各自持有一把锁。
5. lock descriptor/handle 绝不能继承给 ACP 或 tool child process，否则 paxd crash 后 child 会继续持有锁。
6. graceful shutdown 先停止 traffic 和全部 ACP process，最后才显式 unlock 并关闭 daemon lock。
7. crash recovery 依赖内核关闭 owner descriptor/handle。残留 lock file 是正常且无害的。

macOS、Linux 和受支持的 BSD 使用 build-tagged Unix 实现：

```go
f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) // 不使用 O_TRUNC
unix.CloseOnExec(int(f.Fd()))
err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
```

`EWOULDBLOCK`/`EAGAIN` 映射为 `ErrAlreadyRunning`，其他 error 都导致启动失败。`Release` 先执行 `LOCK_UN`，再关闭文件。`flock` 归属于 open file description，所有 descriptor copy 都关闭后才释放，因此必须设置 close-on-exec，并禁止通过 `ExtraFiles` 传给 child。

Windows 使用单独的 build-tagged 文件和 `golang.org/x/sys/windows`：

1. 用 `CreateFileW(..., OPEN_ALWAYS, ...)` 以 read/write 打开 lock file；允许 read/write sharing，但不允许 delete sharing；security attributes 传 nil，使 handle 默认不可继承。
2. 再调用 `SetHandleInformation`，明确清掉 `HANDLE_FLAG_INHERIT`。
3. 对 byte range `[0,1)` 调用 `LockFileEx`，flags 为 `LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY`。
4. `ERROR_LOCK_VIOLATION` 在有界 startup retry window 结束后映射为 `ErrAlreadyRunning`。Microsoft 文档说明 process termination 后 OS 会自动 unlock，但资源紧张时可能有短暂延迟。
5. `Release` 使用相同 range 调用 `UnlockFileEx`，然后关闭 handle。

`Acquire` 在两个平台都使用受 context 控制的有界重试：初始 50 ms、最大 500 ms、总计最多 5 秒。超时返回 `ErrAlreadyRunning`；绝不能 steal、delete 或 replace 已持有的锁。仍然存活但卡死的 paxd 应继续持锁，必须先终止它才能启动另一个实例。发生 SIGKILL、`TerminateProcess` 或机器重启后，kernel lock 会自动消失，即使 lock file 仍然存在。

参考：

- https://pkg.go.dev/golang.org/x/sys/unix#Flock
- https://man7.org/linux/man-pages/man2/flock.2.html
- https://learn.microsoft.com/windows/win32/api/fileapi/nf-fileapi-lockfileex
- https://learn.microsoft.com/windows/win32/api/fileapi/nf-fileapi-createfilew

在启动 slot supervisor 或 tunnel supervisor 之前，daemon-start barrier 必须：

1. 取得 exclusive daemon lock；
2. 终止并等待所有能够安全确认的持久化 process/process group；
3. 在一个事务中清空所有 session 的 `bound_slot_id` 和 `bound_process_epoch`，但保留 native session ID 和 resume descriptor；
4. 把旧 slot status 标为 stopped，并清空其中的 process identity；
5. 使用空的内存 pool 启动，再创建 desired slots；
6. barrier 完成后才能进行 tunnel reconcile、replay 和 live traffic。

process runner 必须提供平台对应的 containment 或经过身份校验的 orphan reaper，保证第 2 步安全。无法保证时 startup barrier 必须 fail closed，不能冒险向可能被复用的无关 PID 发 signal。

每次启动 process 前，`ACPSlot` 都必须生成一个新的 UUID 作为 `process_epoch`。不能从 PID 推断，因为操作系统会复用 PID。如果 process 已退出、但负责清理 binding 的事务尚未完成，router 仍然必须把 epoch 不匹配的 route 当作 cold。paxd 启动时不保留任何旧 binding：daemon-start barrier 必须在新 slot 和 tunnel traffic 启动前清空全部 binding。

在同一次 paxd 生命周期内，process `Start` 返回 handle 后，`ACPPool.slots` 立即注册该 `ACPSlot`；只有 initialize 完成并进入 `ready` 后才能参与 routing。`Wait` 无论因为什么原因返回，`ACPSlot` 都必须先在锁内关闭 admission，并从 live entry 中移除该 process epoch，然后再 best-effort 批量清理 SQLite binding。

## 9. Initialization policy

paxd 是连接每个本地 ACP worker 的 protocol client。因此 worker 的 `initialize.params` contract 由 paxd 持有，不属于 pax-manager，也不属于 manager 侧 conversation caller。

daemon startup 以及相关本地配置变化时，paxd 使用以下信息构建并 canonicalize `ACPClientInitDescriptor`：

- paxd 实现的 ACP protocol version；
- paxd 在本地真正实现的 client capabilities；
- paxd client identity/version；
- worker adapter 支持的显式本地 extension/config fields。

descriptor 保留完整 generated JSON，包括 extension fields，并生成 deterministic profile hash。它属于 connection，不属于 session 或 slot。manager request 不能修改它。

每个新 process epoch 在 ready 前执行以下内部 handshake：

1. snapshot 当前 local descriptor 和 command fingerprint；
2. 使用 paxd 保留 request ID 合成 `initialize`；
3. 只发给一个 worker process，并在 outbound reliable producer 之前由 internal waiter 消费 response；
4. 第一个 ready worker 的成功 result 持久化为 pool canonical worker capability result；
5. 后续 worker result 在 negotiated protocol/capability fields 上必须语义兼容。失败或不兼容的 worker 保持 non-ready，并产生 slot failure。

这是 per-worker initialization，不是 broadcast。每个 process epoch 有自己的 request ID，最多收到一次 `initialize`。任何 worker initialize request/response 都不能进入 manager session、SSE subscriber 或 conversation response waiter。

canonical result 产生后，paxd 通过现有 authenticated paxd node-status reporting path 向 pax-manager 发送 sanitized typed `ACPPoolCapabilityReport`，不进入 ACP reliablemq stream。report 包含 connection ID、profile/result hash、protocol version、无敏感信息的 client/agent capability fields、command fingerprint 和 report generation。完整 raw descriptor/result JSON 只保留在本地。report 是可 upsert 的 observation，不是 approval request；周期/reconnect report 不能重新 initialize worker。

概念结构：

```go
type ACPPoolCapabilityReport struct {
    ConnectionID       string
    ReportGeneration   int64
    ClientProfileHash  string
    WorkerResultHash   string
    ProtocolVersion    json.RawMessage
    ClientCapabilities json.RawMessage // sanitized
    AgentCapabilities  json.RawMessage // sanitized
    CommandFingerprint string
}
```

structured manager conversation path 完全不发送 ACP `initialize`。它等待 pool readiness/capability status，然后直接执行 `session/new` 或 `session/resume`。legacy raw ACP proxy compatibility 未来可以在显式 compatibility check 后，从 reported canonical result 回答 client-facing initialize；但这个 request 永远不能控制或到达 worker，而且不属于第一条 concurrent path。

## 10. 路由策略

### Manager 到 paxd 的 frame 分类

| Frame | 处理方式 |
|---|---|
| `initialize` request | structured manager path 中属于 unexpected，因为 worker init 已由 paxd 完成。legacy proxy compatibility 可以从 canonical capability report 回答，但绝不能 forward/broadcast 给 worker。 |
| `session/new` request | 选择 ready slot，并保存 request-to-slot pending state。 |
| 显式 `session/resume` | 选择或校验 slot，转发，并在成功后更新 route。 |
| 带 `sessionId` 的 request/notification | 查询 `(connection_id, native_session_id)`。 |
| 没有 method 的 JSON-RPC response | 必须带 durable `native_session_id` dispatch metadata，再用 `(native_session_id, raw request ID)` 匹配 pending source slot/process epoch；payload 原样转发。 |
| 未知的 sessionless method | v1 fail closed，返回 proxy JSON-RPC error。 |

### Slot selection

候选 slot 必须 ready 且没有 draining。

选择顺序：

1. 已存在且匹配 process epoch 的 hot route；
2. hot route 数量最少；
3. `last_assigned_at` 最早；
4. ordinal 最小，作为确定性 tie-breaker。

不使用 consistent hash；持久化 route 始终是权威来源。

### `session/new`

1. 校验并提取 resumable lifecycle descriptor。
2. 在一个 ready slot 上取得 lifecycle lease。
3. 在 pool 内存记录：

   ```text
   request_id -> slot_id + process_epoch + descriptor
   ```

   pool 生命周期跨 tunnel reconnect，因此该 pending state 也跨 tunnel reconnect。
4. 把 request 转发给 slot。
5. error response：清理 pending state，释放 lease。
6. success response：提取 native session ID。
7. 在一个 SQLite transaction 中插入 route，把 `bound_slot_id` 和 `bound_process_epoch` 设为当前 slot process。
8. transaction commit 后才能把 success response 发给 pax-manager，然后释放 lease。

route 持久化失败时不能转发 success。paxd 返回 proxy error。新创建的 ACP session 此时属于 orphan candidate；如果 agent 支持 `sessionCapabilities.close`，后续可以清理。

### 已有 effective-hot session

对于带 native session ID 的操作：

1. 读取 route binding；
2. 在 `ACPPool.slots` 中解析 `bound_slot_id`，确认 ready slot 的当前 process epoch 与 `bound_process_epoch` 匹配；
3. 获取对应 slot lease；
4. 不执行 resume，直接转发；
5. 成功 delivery 后更新 `last_used_at`。

任一 binding 字段为空，或者内存 slot entry 不匹配时，route 都是 effective cold。恢复前使用 version compare 清理残留的非空 stale binding。

### 已有 effective-cold session

1. 选择 ready slot 并获取 lifecycle lease。
2. 获取内存中的 per-session recovery lease；这就是派生的 `resuming` 状态，不写入 SQLite。
3. 使用 version compare 清理可能残留的 stale binding，然后使用 native session ID 和保存的 lifecycle descriptor 发送内部 `session/resume`。
4. 内部 response 由 paxd 自己消费，不能冒充用户原始操作的 response。
5. resume 成功后，使用 version compare 把 `bound_slot_id` 和 `bound_process_epoch` 更新为选中的 process。
6. route commit 成功后才能转发原始操作。
7. resume 失败时保持两个 binding 字段为空，释放 lease，记录脱敏 recovery-failure event，并把 resume error 返回给原始请求。绝不能调用 `session/new`。

同一个 native ID 的并发 cold resume 使用 in-process per-session recovery lease 加 route version compare 串行化。失败竞争的一方重新读取 route，不能再次发 resume。

### 迁移期间 route 不存在

已有 session 可能早于 slot pool 表。此时 pax-manager 已经知道 native ID，但 paxd 没有 route，也没有 resume descriptor。paxd 不能猜测 slot 并直接转发 prompt。

paxd 返回：

```json
{
  "code": -32002,
  "message": "ACP session route requires resume",
  "data": {
    "kind": "session_route_missing",
    "requiresResume": true
  }
}
```

manager conversation path 只处理一次该错误：

1. 从 manager session config 重建 cwd、MCP servers 和 additional directories；
2. 显式发送 `session/resume`；
3. resume 成功后，用新的 request ID 重试原始 prompt；
4. 不能循环重试；
5. 不能 fallback 到 `session/new`。

显式 resume 成功时，paxd 创建 durable route。legacy raw tunnel 调用方必须自己发送 `session/resume`。

### Active prompt

prompt lease 从 `session/prompt` 被接受并准备 delivery 时开始，直到该 slot 返回匹配的 JSON-RPC response，或者 slot process 退出时结束。

持续出现的 `session/update` notification 不能释放 prompt lease。

active 期间允许以下 frame 进入 owning slot：

- `session/cancel`；
- permission response；
- file request response；
- terminal request response。

如果 sticky route 对应的 slot 正在执行另一个 session，v1 返回：

```json
{
  "code": -32001,
  "message": "ACP slot is busy",
  "data": {
    "kind": "slot_busy",
    "retryable": true
  }
}
```

paxd 不为 busy slot 建立隐藏的纯内存 prompt queue。reliablemq dispatch 到 router 后，要么把 operation 交给 ACP stdin，要么返回明确的 `slot_busy` 业务结果；transport `applied` 不代表 prompt 已经执行完成。

## 11. Worker-originated request 的 response 路由

JSON-RPC request ID 是 correlation value，不是全局 routing key。普通 manager-to-worker request 不需要 ID translation：manager 拥有这些 ID，paxd 转发 request 时也已经知道 selected slot。

真正需要显式路由的是 ACP worker 主动发起、等待 ACP client 回答的 request。主要例子是 `session/request_permission`。后续 JSON-RPC response 通常只有 `id`，不会重复 method、session ID 或 slot identity。

所有能够归属到 native session 的 ACP data frame，都必须在 reliablemq envelope metadata 中携带 `native_session_id`。Envelope 已经有通用 metadata 字段，但需要 ACP adapter 主动填入；reliablemq 不会自动从 ACP payload 提取。因此 worker-originated request 的 correlation identity 就是 `(native_session_id, raw request ID)`，原始 ID 无需 translation。

现有 approval model 已经保存了 worker 原始 JSON-RPC ID，并把 approval 关联到 manager session。durable session record 可以把 manager session 解析成 native session ID；原始 permission payload 中也有 `params.sessionId`。因此 manager 不需要第二套 request ID，也不需要新的全局 correlation namespace。

```text
worker request payload:
  id = 1
  params.sessionId = native-A

manager approval/client-request state:
  manager_session_id = manager-A
  raw_request_id = 1

durable session record:
  manager-A -> native-A

manager response:
  payload.id = 1
  reliablemq metadata.native_session_id = native-A

paxd pending source:
  (native-A, raw JSON id 1) -> slot_id + process_epoch
```

流程：

1. paxd 收到带 ID 的 worker request 时，要求它具有 native session ID，并在 publish 前记录 `(native_session_id, raw request ID)` 对应的 source `(slot_id, process_epoch)`；payload 不改写，outbound reliablemq envelope 携带 `native_session_id` metadata。
2. manager 把原始 ID 保存在已经关联 manager session 的 approval/client-request state 中。当前 approval model 的 `NativeID` 或原始 `RawPayload` 加 `RequestSessionID` 已经表达了这个关系；不增加 tunnel ID，也不增加额外 approval correlation 字段。
3. manager 发送 response 时保留 payload 中的原始 JSON-RPC ID，把 manager session 解析成 native session，再将 `native_session_id` 加入 reliablemq outbound metadata。metadata 和 payload 一起 journal、一起 replay。
4. paxd 使用这对值查询 pending source，确认完全相同的 slot process epoch 仍然存活，把 response 原样写给该 worker，然后删除 pending entry。

manager 必须提供 typed worker-response send path：调用方必须传 session context，由该入口解析 native session，并在调用 reliablemq 前设置 metadata。manual approval、auto-approval 和 reusable-grant response 全部走这个入口，不能直接调用 generic raw tunnel writer。普通 manager-originated request 的发送链路不变。

两个 session 可以同时存在 numeric ID `1`，native session ID 会区分它们。numeric `1` 和 string `"1"` 通过 raw JSON 表示保持不同。同一个 native session 在旧 ID 仍 pending 时重复使用该 ID，视为 protocol error 并拒绝。

paxd pending entry 只是 source/epoch guard，不是 ID translator。它只存在 pool memory，在收到 response、worker exit 或达到有界 expiry 时删除。transient tunnel reconnect 时 pool 和 worker 仍然存活，所以 entry 也保留。full paxd restart 会杀掉 worker，因此无需持久化。旧 epoch 的 late response 必须 fail closed，绝不能发给 replacement process。

未来任何需要 response 的 worker-originated request 都必须提供 native session identity。sessionless request 在 v1 fail closed，不为了它引入通用 JSON-RPC ID translation layer。

## 12. Reliable outbound queue

### 边界与 API

当前 `persistentACPProcess.sendOutbound` 会自行 append frame、读取 optional sender、写 WebSocket，然后调用 `MarkSent`。当前 `reliablemq.Engine.Send` 也会先 append，再同步调用 `Sender.Send`，最后把分配好的 `Frame` 返回给调用方。这两种形式都不是目标状态。

目标 API 只负责 acceptance：

```go
type Producer interface {
    Send(ctx context.Context, msg reliablemq.OutboundMessage) error
}
```

这只改变本地 paxkit producer API，不改变 reliablemq wire protocol。paxd 和 pax-manager 的 producer call site 在升级 paxkit 时都必须通过新的 return contract 编译，但 peer receiver 不需要理解 asynchronous acceptance。新旧版本可以继续互通，因为 envelope format 和 receive semantics 都不变。

这里的兼容只表示 transport wire-compatible，不表示旧 peer 能支持 multi-session routing。concurrent mode 仍然必须由 session-metadata/pool capability gate 控制。

### Receiver 端改动

`Engine.Receive(ctx, env)` 继续保持 durable receive、deduplication、ordered dispatch 和 replay contract。它不调用、也不解释 `Engine.Send`。receiver 端只需要以下 integration change：

1. 保留 inbound `Frame.Metadata`，并传给 ACP router 或 manager session mux。paxd 用 `native_session_id` 加 raw JSON-RPC ID 路由 worker-response frame；manager 用相同 metadata 按 session 分类 worker frame。当前丢弃 metadata、直接把 payload 写入唯一 stdin 的 single-process dispatcher 要替换成 `ACPRouter`。
2. inbound data frame durable-record 后，`Receive` 把 cumulative ACK 提交给 connection-owned network writer，不能再直接调用 `Sender.Send` 并等待 WebSocket write。ACK submission 必须 non-blocking，并 coalesce 到最大的连续 `through`。
3. 收到 ACK 时，推进 producer owner 内存中的 `ackedThrough`，唤醒 journal/eviction work，并 batch persist cumulative checkpoint；不能同步 patch 每一条 frame。
4. reconcile message 只 bind/reset 同一个 producer 的 network cursor，不能创建 receiver-owned replay sender。

只有第 1 项属于 multi-session ACP routing。第 2-4 项来自 asynchronous producer 和 single-WebSocket-writer 设计；它们都不改变 received envelope 的 wire-visible meaning。

`Send` 校验 message，并把 owned copy enqueue 到 producer。成功返回只表示“当前 live producer 已接受”，不表示已经 journaled、写入 socket、被 peer received、被业务 applied 或已 ACK。它不返回 `Frame`，因为调用方不能使用 transport sequence/status 做 ACP 业务路由。

caller cancellation 只在 acceptance 之前生效。message enqueue 后由 producer lifecycle context 接管；slot/request context cancel 不能把它从 queue 中删除。`Send` 内执行的 outbound middleware 必须是有界、纯本地、无 I/O 的逻辑。需要访问网络或数据库的 hook 必须在 `Send` 前完成，或者放到 producer owner path 中异步执行，不能重新把阻塞引入 acceptance API。

`Send` 不能等待 SQLite/Postgres、journal batch、WebSocket I/O、reconnect 或 consumer ACK。同步 error 只包括 invalid input、producer 尚未启动或已经关闭。producer 对外可用之前必须先完成 queue bootstrap，包括加载初始 sequence/checkpoint；第一次 `Send` 不能 lazy 访问数据库。

producer 属于 process，不属于某次 tunnel attempt。reconcile 完成后，tunnel attempt 把自己的 connection generation 和 socket sender 绑定到 producer；disconnect 只能解除同一 generation。旧 attempt 的 late cleanup 不能 detach 新 connection。bind 的含义是 enable 已存在的 network cursor，不是创建新的 replay loop 或 sender。

多个 slot 可以并发输出，因此 ingress 必须 multi-producer safe。public enqueue path 不使用显式 mutex，也不能向可能塞满的 bounded channel 发送，从而阻塞 slot stdout。目标结构是 lock-free MPSC append，加 best-effort non-blocking wake signal；一个 owner goroutine drain queue，并独占 producer mutable state。

内存不能成为无限 durability policy。接受的 entry 必须持续、批量移动到 transport journal。rollout 前必须为 flush degraded 状态定义明确的 age/bytes 上限和 fatal/degraded policy。普通 network outage 不应导致内存无限增长：entry 一旦持久化即可从 hot log evict，需要发送时再从 journal 读取。

所有 slot output 先经过 `ACPRouter`，完成 response state transition 和 pending worker-response source registration，然后调用这个共享 producer。paxd 不增加 `ACPTransportOutbox`、per-slot sender 或 direct WebSocket write。

`slot_id` 和 `process_epoch` 可以作为 diagnostic metadata。native session ID 是 business dispatch metadata，包括 manager 回复 worker-originated request 的场景。它们都不属于 `FrameKey`，不形成 transport partition；pax-manager 的业务路由不能依赖 slot metadata。

保持 connection-scoped queue 是明确决策。slot-scoped queue 会为每个 process worker 增加独立 sequence/ACK/reconcile state，把 slot identity 暴露给 pax-manager，而且 cold session 在另一个 slot/process epoch 上 resume 时会很别扭。session-scoped queue 则要求 manager 在解析某些 lifecycle response 前先发现 partition，并大量增加 durable queue state，却不能帮助 paxd 的本地 sticky routing。产品要求只是 same-session order，不需要这两种改造。这个简单 v1 方案的代价是 cross-session transport head-of-line blocking；该代价被接受并明确后置。

### 一条 log 与四个位置

acceptance 后，一个 sequencer 分配下一条 `seq`，并把 frame append 到 ordered hot log。atomic ingress append 是 acceptance order 的 linearization point；已经 accepted 的 entry 可能短暂等待 owner 分配 sequence。之后 log 有四个逻辑位置：

```text
tail             已接受并完成 sequence 分配的最大 seq
persistedThrough journal 中已连续持久化的最大 seq
nextToSend       当前 network connection 下一条可以写的 seq
ackedThrough     peer 已 cumulative durable-receive 的最大 seq
```

journal flusher 和 network sender 是同一条 log 上相互独立的 consumer：

- 正常情况下 network cursor 紧跟 `tail`，socket 可用时立即发送每个新 head。它不等待 `persistedThrough`，从而保留 reliablemq 的 send-first fast path。
- journal cursor 批量前进，持久化 payload、metadata 和最新 cumulative ACK/checkpoint state，不延迟 network output。
- network cursor 卡住时，journal cursor 可以超过它。frame 持久化后可以从 hot memory evict；network 恢复时，如果 head 已被 evict，就从 journal 读回来。
- journal cursor 暂时落后 network cursor 时，frame 留在 hot memory，直到完成 journal 或 cumulative ACK。daemon 在此期间 crash 可能丢失尚未 flush 的 tail；reconnect reconcile 通过 advance producer sequence/checkpoint 处理 peer-ahead。这个窗口是 send-first contract 的明确结果，不是 exactly-once guarantee。

只有 `ackedThrough` 是 durable delivery high-water mark。per-frame `sent` 只允许作为 optional in-memory diagnostics，不能每次 network write 都产生一条 database patch。batch persistence 必须把新 insert frame 和最新状态合并；cumulative ACK persistence 覆盖 stale pending/sent row。数据库 row 已经发送但仍显示 pending 是安全的：它可能 replay，peer 会按 transport key 去重。

`MarkSent` 从 required durable-store correctness path 中删除。`StatusSent` 可以暂时为 schema compatibility 保留，但 replay 必须把 pending 和 legacy sent row 同等处理。per-frame send failure 只作为 connection telemetry/in-memory diagnostics，不能同步 patch 数据库。producer-owned network cursor 取代外部并发调用 `ReplayOutbound`；tunnel code 在 reconcile 后要求 cursor resume，而不是运行第二个 replay sender。

每个 journal batch 必须在一个原子操作中收敛 newly persisted frames、next outbound sequence 和 cumulative ACK/checkpoint state。ACK 可能在对应 frame batch flush 前到达；最终 batch 必须把该 frame 直接存成已被 high-water mark 覆盖的状态，或者按 journal retention policy 省略/回收，不能把它重新变成 unacked work。

实现必须用 bytes 和 age 定义 hot-entry eviction 与 flush-failure limit。为了控制内存，绝不能静默丢弃 accepted frame。如果 journal 长时间不可用并跨过 safety limit，owning connection/daemon 必须进入明确的 degraded 或 fatal state；这是 operational backpressure，不能变成 `Send` 内部隐藏的阻塞。

### 唯一 network writer

每条 connection 只有一个 network writer 串行执行 WebSocket write。data sender 每次只推进一个 `nextToSend`；write failure 后该 seq 仍然是 head，同时 cursor disable，直到 reconnect。后面的 data 不能绕过它。

ACK envelope 不属于 outbound ACP sequence，但必须使用同一个 connection-owned socket writer，保证 data 和 ACK 不会并发写 WebSocket。ACK scheduling 可以高于 data，但不能改变 data ordering。

## 13. Tunnel reconnect 与 replay barrier

reconnect 不再区分“replay sender”和“live sender”。backlog 和新 output 都由同一个 network cursor 读取：

```text
WebSocket connected
  -> reliablemq producer reconcile
  -> 把 manager-to-paxd received/unapplied frame 通过 ACPRouter replay
  -> 设置 outbound nextToSend = peer ackedThrough + 1
  -> enable 唯一 network writer/cursor
       -> 从 hot log 读取精确 head；不存在时从 journal 读取
       -> 发送 head；只有 socket write 成功才能 advance
       -> 继续追 backlog，追上后跟随 tail
  -> tunnel 才进入 ready，可以接收 manager 新工作
```

要求：

- tunnel 断开期间，slot output 继续 enqueue 和 journal；
- slot、replay helper 和 reconnect path 都不能直接把 ACP data 写入新 WebSocket；
- journal read 必须循环直到 cursor 追上 `tail`；1000 之类的 limit 只代表 batch size；
- 不存在 replay/live handoff，也不需要用 write lock 保护 handoff。新 accepted frame append 在旧 head 后面，自然由同一个 cursor 稍后发送；
- reconnect 不能优先发送 in-memory “live” data。producer 对每一个 `nextToSend` 都必须先从 hot memory 或 journal resolve 该精确 sequence；
- manager reconcile 完成前不能把新 tunnel 注册成 claimable；
- recovery barrier 前到达的新 prompt，由 manager turn queue 保留或返回 transport recovering，不能提前送进 paxd。

必须区分三种事件：

- tunnel disconnect：slot process 和已经提交的 binding 都保留；
- slot process exit：立即关闭 admission，并 eager 清理该 process epoch 的 binding；
- paxd daemon restart：daemon-start barrier 保证旧 ACP process 全部消失，并在 replay 前清空所有持久化 binding，但保留 native session ID 和 resume descriptor。

本计划不声称 full daemon crash 下 operation exactly-once。frame 一旦 journaled 就可以 replay；peer 已 durable receive 的 frame 即使 producer local journal 落后，也可以通过 reconcile 覆盖。只进入 producer unflushed hot tail 的 frame，如果 daemon 在任一端 durable record 之前整体 crash，可能丢失。这是 non-blocking send-first API 必须明确承担的 availability/durability tradeoff；调用方不能把成功 `Send` 描述为 durable application commit。存在业务副作用歧义时必须显式暴露，不能偷偷创建替代 session。

## 14. Desired slot reconcile

`ACPSlotSupervisor` 是现有通用 supervisor 的 ACP typed wrapper。它的 store 读取 enabled/running `agent_connection` 行，并根据每行的 `desired_acp_slots` 合成准确数量的 `ACPSlotSpec`：

```go
type ACPSlotSpec struct {
    SlotID             string
    ConnectionID       string
    Ordinal            int
    CommandFingerprint string
    Command            []string
    WorkingDir         string
    Env                map[string]string
}
```

### Scale up

desired count 从 N 变为 M，且 M > N：

1. 创建或复用 ordinal `[N, M)`；
2. 各 slot 独立启动 process；
3. 完成 initialize；
4. 只有 ready slot 才加入 routing candidates。

整个过程不重连物理 tunnel。

### Scale down

M < N：

1. 优先选择最大 ordinal；
2. 标记为 draining；
3. idle draining slot 立即停止；
4. active slot 等待当前 turn 完成或显式 cancel，不能因为 desired count 变化就直接 kill；
5. process terminate 前，清理该 process epoch 的带索引 binding；
6. 保留 slot status 和 ordinal identity，便于以后 scale up。

### Command/config 变化

`CommandFingerprint` 变化时重启受影响 slot。通用 supervisor 通过注入的 desired-change operation 触发 replacement，不使用 pool generation。条件允许时采用 rolling replacement，尽量保留至少一个 ready slot。

每个 replacement process 使用新 process epoch，并清理旧 epoch binding。

## 15. pax-manager 按 session 分拣

### 现有能力与准确缺口

pax-manager 已经有一些 session-aware 基础：

- ACP middleware 能转换 manager session ID 和 native session ID；
- pending `session/new` state 能把 native result 绑定到 manager session；
- SSE subscriber 已经保存 manager session ID；
- reliablemq dispatch 已经按 connection queue 有序并去重。

但这些还不能让一条共享 paxd tunnel 安全承载多个并发 session。当前 live state 仍然只有一个 mutable `sessionID`、一个 tunnel-wide `paired` bit、一个 raw `userWS`，response waiter 也不保存 session context。`withSessionContext` 会临时覆盖 tunnel session。SSE delivery 还会把没有 `sessionId` 的 frame 视为匹配所有 subscriber。这些行为只有在 tunnel-wide single-active gate 阻止重叠时才安全。

本计划中的 manager 改动，是在 reliablemq dispatch 之后增加 ACP-layer session mux。它不是 transport partition，不会重新排序，也不会按 session 独立 ACK。

### Routing key resolution

每个 manager-originated request 在发送前必须先登记 manager session：

```text
request_id -> manager_session_id + response_waiter + request_kind
```

manager-originated request ID 在一条物理 agent tunnel 内继续保持全局唯一。mux 按以下优先级解析 inbound frame 的 manager session：

1. frame 带有明确 native `sessionId` 时，由现有 middleware 转成 manager session ID；
2. JSON-RPC response 没有 `sessionId` 时，通过 pending request ID 查 session；
3. `session/new` 通过已有 pending-session-new entry，以及 result 中返回的 native ID 查 session；

`ACPPoolCapabilityReport` 不进入这条 frame-resolution chain。它通过现有 node-status API 到达 manager，并按 connection ID 和 report generation upsert。manager 可以用它做 readiness、compatibility 和未来 routing decision，但不回复、不合成 worker initialization。report 没有 response waiter 或 SSE delivery path。

无法分类的 sessionless frame 必须 fail closed，并且日志不能包含 payload。mux 绝不能把 mutable tunnel `currentSessionID` 当 fallback，也不能把缺少 session 的 response 广播到所有 SSE subscriber。

`session/new` 的 manager session ID 来自本次 call 的显式 request context，并在 send 前写入 pending state。并发路径不能通过 tunnel-global `ensureManagerSessionID` state 分配或发现它。

对于 worker-originated client request，manager 保存未改写的 raw request ID 和 manager session identity，并在发送 response 时解析出对应的 native session。response 在 durable reliablemq dispatch metadata 中携带这个 native session ID。manager 不为这条链路分配 replacement ID。

### Per-session state

用显式 session-owned state 替换 tunnel-wide business state：

```text
sessions[manager_session_id]
  -> active turn/admission state
  -> SSE subscribers
  -> optional conversation/raw consumer

pending[request_id]
  -> manager_session_id
  -> response waiter / request kind

worker_client_requests[native_session_id, raw_request_id]
  -> manager_session_id + native_session_id
  -> approval / client-request state
```

物理 tunnel 和 reliablemq engine 继续由 connection 持有。connection 仍然只有一个 network writer。session consumer 在 middleware translation 后收到 immutable frame copy；某个 slow session consumer 不能占住 network writer 或其他 session 的 registry operation。

并发路径中，request goroutine 不能持有 `agentWriteMu`/`userWriteMu` 直接写 WebSocket。它只能提交给 connection-owned producer 或 session consumer，由对应 single owner 完成 write/delivery。

每个 per-session consumer 必须保持该 session 的 reliablemq dispatch order。bounded SSE/client mailbox 如果遇到 slow subscriber，只能断开该 subscriber 并明确返回 retry/resync signal；不能静默 drop frame、阻塞 transport dispatcher 或影响其他 session。

conversation/SSE path 使用 per-session turn admission 替换现有 `paired bool`：

- 不同 manager session 可以同时拥有 active turn；
- 同一 manager session 同时最多一个 prompt turn；
- release、timeout 和 disconnect 只清理本 session 及其 pending waiter，不能影响其他 session；
- paxd `slot_busy` 要么变成 typed retryable conversation result，要么进入显式 manager turn queue；tunnel writer 不能 spin/sleep retry。

history projection、logging context、waiter delivery 和 SSE delivery 全部使用 frame dispatch context 中 resolved manager session ID，不能读取 `ACPTunnelAgent.sessionID`。

legacy raw user WebSocket tunnel 在 request-ID 和 session-context isolation 单独验证完成前继续 single claim。第一条开放并发的路径是 structured conversation/SSE API。

### Recovery 与 compatibility

先在 pax-manager 现有 one-active-agent gate 下验证 paxd pool。只有 session mux tests 通过后才解除 gate。新 reconnect tunnel 在 manager-side reliablemq reconcile、inbound replay 完成，并取得当前 pool readiness/capability report 前不能 claim 新 turn。

manager/native session middleware 继续作为 cloud-side 唯一 ID translation owner。`session_route_missing` 只处理一次：重建 lifecycle descriptor，显式发送 `session/resume`，然后用新 request ID 重试原 operation；绝不能 fallback 到 `session/new`。

这个 mux 保证 manager business boundary 上的 same-session routing 和 admission order。由于 reliablemq 仍是一条 connection-level sequence，recovery 期间缺失或卡住的 transport head 仍可能延迟所有 session。要消除这种 cross-session transport head-of-line，需要未来给 envelope、journal、ACK、queue state、reconcile 和两端服务一起增加 partition key；这明确不属于本计划。

## 16. 实施切片

每个 slice 都必须足够小、可独立 review，并保持测试通过。每个 merge 点都要产出一个命名的、可运行的 pax-manager+paxd 稳定组合。组合之间允许 breaking change；除非本节明确写出 bridge point，否则不要为了 old/new peer 互通引入额外复杂度。顺序仍然是：先写 BDD scenario，再写 failing test，最后写 implementation。

### Slice 0：Characterization 与发布 contract

- 把本计划的关键场景加入各模块 `BDD.md`；
- 固化当前 persistent process 在 tunnel reconnect 时的行为；
- 固化当前 reconcile/replay 顺序和 1000-frame batch 行为；
- 增加 clock、ID generator、route store、process runner、ACP JSON-RPC inspection 等窄接口；
- 增加 structured manager path 的兼容性/版本 gate：manager 可以要求拿到某个 paxd capability generation 后才开始 lifecycle work；
- 不改变生产行为。

稳定组合：

- `slot-pool-baseline`：当前 manager + 当前 paxd 行为。

验收：

- 现有测试全部通过；
- 新 characterization tests 能明确展示当前生命周期和顺序；
- compatibility gate 已存在，但在后续 slice 前保持 permissive。

### Slice 1：现有单 worker 路径上的 paxd-owned initialize

- 先不引入 slot pool，把 worker `initialize` ownership 移到 paxd，仍然使用当前单个 persistent ACP process path；
- paxd 本地生成 connection-scoped client-init descriptor，canonicalize 后计算 `client_profile_hash`，向唯一 worker 发送内部 initialize request，由 paxd 自己消费 response，并计算 `worker_result_hash`；
- 保存本地 init profile/result record，并通过 node-status/control reporting 发出脱敏 `ACPPoolCapabilityReport`。report 必须包含足够的非敏感信息，让 operator 能确认 paxd 已经如何完成 init：connection ID、report generation、paxd version、command fingerprint、profile hash、result hash、protocol version、脱敏 client capability keys、脱敏 worker capability keys、initialization phase、initialized-at time、last error code/message；
- 完整 raw initialize params/result 只保存在本机；command env、MCP secret、raw descriptor JSON 绝不能进入 manager status；
- pax-manager structured conversation path 停止发送 ACP `initialize`。manager 等待兼容的 `ACPPoolCapabilityReport` 后，直接从 `session/new` 或 `session/resume` 开始；
- legacy raw ACP proxy 可以删除，也可以用 cached result 回答 `initialize`，但绝不能再把 manager initialize 转发给 worker。

稳定组合：

- `paxd-owned-init-v1`：更新后的 paxd + 更新后的 manager。一个 tunnel、一个 worker，manager 不再发送 worker initialize，session 仍走旧 process owner。

验收：

- 每次 worker process start 只收到一次由 paxd 生成的 initialize；
- structured manager session creation 不发送 ACP `initialize`；
- manager 可以从 capability report 看出 paxd 完成了 init，但看不到 raw secret；
- reconnect/report refresh 不会重新 initialize 健康 worker；
- initialize 失败或不兼容时，agent 进入 typed、脱敏的 unavailable status。

### Slice 2：Process identity、daemon lock 与安全启动 barrier

- 增加 `ProcessIdentity`、OS start token、exclusive daemon lock 和 `Identity()`；
- Unix ACP process 使用独立 process group；
- group termination 先 graceful、后 forced；
- 增加安全的 daemon-start orphan termination barrier；没有校验 process birth identity 时绝不能仅凭持久化 PID 发 signal。

稳定组合：

- `single-worker-owned-init-locked`：保持 Slice 1 的单 worker 行为，同时具备 daemon owner 与 orphan cleanup 安全性。

验收：

- 一个 subprocess 持有 daemon lock 时，第二次 acquire 必须在打开/migrate SQLite 前返回 `ErrAlreadyRunning`；
- 显式 release 后可以立即重新 acquire；
- 强制 kill owner 后，即使一个模拟 ACP 的 child 仍然存活，也必须能在有界时间内重新 acquire，以证明 lock descriptor/handle 没有被 child 继承；
- 只有 stale lock file、没有 kernel lock 时不能阻止启动；
- Unix 和 Windows CI 都运行 subprocess crash-release tests；不支持的平台必须明确失败，不能悄悄退化成 PID file；
- Unix 下 ACP 创建的普通 child 与 root 具有相同 PGID；
- group termination 同时停止 root 和 child；
- daemon restart 不会保留或重新 attach 旧 ACP process；
- PID reuse 不会导致无关进程被误杀；
- 测试使用 testify `assert`/`require`。

### Slice 3：SQLite slot 与 route state

- 增加 model、migration、repository port 和 transaction route operation；
- 现有 connection 默认一个 slot；
- 增加 desired-slot validation 和 supervisor wake，但真实 slot path 可用前，生产环境中大于一的值要 reject 或 feature-gate；
- 增加 route version compare、带索引的按 process epoch 批量 binding clear，以及 daemon-start clear-all transaction；
- 复用 Slice 1 的 init profile store，不能再加第二份 init state。

稳定组合：

- `single-worker-with-slot-state`：生产仍然跑一个 worker，但 slot routing 需要的 durable state 已经存在并可检查。

验收：

- store tests 覆盖 create、bind、按 process epoch clear、daemon start clear-all、conflict、slot count validation 和 descriptor persistence；
- status projection 不包含敏感 lifecycle data；
- desired-slot-only update 不 rotate transport queue，也不重启物理 tunnel。

### Slice 4：独立 `ACPSlot` 与 fake router core

- 从 `persistentACPProcess` 中拆出 process/stdin/stdout ownership，形成独立 `ACPSlot`；
- 实现 slot state machine、process epoch、paxd-owned initialize、prompt/lifecycle lease 和 event sink；
- 先用 fake slot 实现 ACPRouter classification、pending `session/new`、sticky route、cold resume、active prompt tracking、cancel bypass，以及 pending `(native_session_id, raw request ID) -> slot/process epoch` validation；
- 增加内部 RPC waiter，用于 initialize/resume response；
- 暂时不连接真实 tunnel。

稳定组合：

- `router-core-dark`：生产 traffic 暂不使用 router；unit 和 fake integration tests 证明 routing 语义。

验收：

- process exit 关闭 waiter，并且每个 epoch 只产生一次 terminal event；
- race test 下仍然只能取得一个 active prompt lease；
- 不同 fake slot 上的两个 session 都可以发送 request ID `1`，每个未改写 response 只回到自己的 source slot；
- `session/new` response 在 route commit 前不能发出；
- cold prompt 必须先用原 lifecycle descriptor resume；
- resume failure 不能产生 `session/new`；
- pre-migration unknown route 返回 `session_route_missing`，且不能写入任何 ACP stdin。

### Slice 5：`ACPSlotSupervisor` 与 count-one 真实 slot path

- 重构现有通用 supervisor：注入 desired-change 判断、串行化全部 reconcile 入口，并支持 drain-aware stop/replacement；
- 将 `ACPSlotSupervisor` 实现为以 `slot_id` 为 key 的 typed specialization，不能另写一套 reconcile loop framework；
- 根据 SQLite exact desired count reconcile，但真实路径先只在 count 1 下启用；
- 通过 registry 用 `connection_id` 获取唯一 `ACPPool`；
- `AgentTunnelSession` 在 desired count 1 下接入 pool/router，并在该版本组合中删除或禁用旧 persistent-process owner；
- manager 继续保持 single-active。

稳定组合：

- `slot-path-count-one`：一个 tunnel、一个 `ACPSlot`、paxd-owned initialize、sticky route table 已启用，manager 仍然 single-active。

验收：

- 通用框架重构后，现有 remote 和 tunnel supervisor 行为继续由原有测试覆盖；
- count one 通过新 slot/router path 端到端工作；
- 重启唯一 slot 会生成新 process epoch、清理旧 binding，并在首次使用时 resume；
- 即使生产 count 仍为一，也要测试 active scale-down/drain 行为；
- 旧 process epoch 的 late exit 不能覆盖新 epoch status。

### Slice 6：Reliable producer 与 single network writer

- 修改 `reliablemq.Engine.Send` 及 host-facing interface，只返回 acceptance error，不返回 `Frame`；
- 同一个 slice 内更新 `paxkit/docs/reliablemq-design.md`、reliablemq BDD scenario 和两端 host adapter，改成 send-first acceptance/crash-window contract，package docs 中不能残留旧 persist-before-send 承诺；
- producer 对外可用前完成 sequence/checkpoint bootstrap；
- 在 reliablemq 中实现 non-blocking multi-producer ingress、single sequencer、asynchronous journal flusher 和 single network cursor，不能增加 ACP outbox package；
- 从 caller path 删除同步 `Sender.Send` 和 per-frame durable `MarkSent`，保留 cumulative ACK persistence 和 batch-coalesced frame state；
- 所有 data 和 ACK WebSocket write 都通过一个 connection-owned writer；
- reliable inbound frame 进入 `ACPRouter`；所有完成 translation 的 slot stdout 进入共享 reliable producer；
- 实现 cursor-based reconnect、完整 journal batching 和 recovery-ready barrier。

稳定组合：

- `slot-path-count-one-async-producer`：生产仍然一个 slot，但最终 transport architecture 已经启用并可观察。

验收：

- journal sink 和 socket writer 被故意 block 时，`Send` 仍然返回，accepted message 保持 producer order；
- 并发 fake slot producer 不会因为 ingress channel 塞满而阻塞，并产生唯一、连续 sequence；
- 健康 send-first path 中，network cursor 不等待 journal flush；
- head 发送失败后，后续 frame 不能绕过；
- `sent` 不会为每个 frame 产生 durable patch；cumulative ACK 与 batch flush 最终安全收敛 stale pending row；
- slot process 在 transient tunnel reconnect 后仍然存活；
- 超过 1000 个 frame 能完整、按序 replay；
- replay 期间产生的新 output 不能越过 backlog；
- recovery barrier 前 prompt 不能进入 slot。

### Slice 7：manager single-active gate 下的多 slot

- 允许 desired slots 大于一；
- 创建多个 session，验证确定性分配和粘性；
- 重启一个 slot，验证只有它的 session 变 cold 并 resume；
- scale down/up 不重连物理 tunnel；
- 保持 pax-manager 的 single-active-agent gate，先验证 pool lifecycle 和 recovery，再开放业务并发。

稳定组合：

- `multi-slot-single-active`：一个 tunnel、N 个 slot，但同一 agent 同时只有一个 manager active turn。

验收：

- 默认 slot count 1 时，现有 manager API 行为不回归；
- sticky route 和 resume 端到端工作；
- tunnel status 与 slot status 可以独立观察；
- SQLite desired slot count 热更新不重启物理 tunnel。

### Slice 8：pax-manager session mux，继续 gate

- 引入 connection-owned session mux，每个 frame/call context 都携带 resolved manager session identity；
- manager 回复 worker-originated request 时，把 native session ID 加入 reliablemq metadata，并在 journal/replay 中保留；
- manual approval、auto-approval 和 reusable-grant response 统一走 typed session-aware worker-response sender；这些 response 禁止 direct raw tunnel write；
- response waiter registration 保存 request ID、manager session ID 和 request kind；
- 带明确 session 的 notification 只能送给该 session；无 session response 通过 request-ID correlation 路由；枚举 sessionless control frame，未知类型 fail closed；
- structured conversation path 删除 `withSessionContext`，并删除所有依赖 mutable tunnel `sessionID` 的 routing/history fallback；
- SSE/conversation consumer 按 manager session 建立索引，删除 `sessionID == ""` 时 broadcast-all 的 fallback；
- legacy raw tunnel 暂时保持 single claim；
- per-agent concurrency gate 继续开启。

稳定组合：

- `session-mux-gated`：manager 内部已经有正确 per-session routing，但外部仍然只允许一个 active turn。

验收：

- 两个 interleaved、没有 `sessionId` 的 response 只能到达各自 request ID 所属 waiter/session；
- 带明确 session ID 的 interleaved notification 只能到达匹配的 SSE subscriber；
- 未知 sessionless frame 绝不能 broadcast 到所有 session；
- 启动两个 manager session 不能产生任何 worker initialize request；两者使用同一 report generation；
- 某个 session close/timeout 不能 release 或 cancel 另一个 session。

### Slice 9：开放 concurrent turns

- structured conversation/SSE API 将 agent-wide `paired`/single-active gate 替换为 per-session turn admission；
- 增加 busy handling 和 reconnect-ready gating；
- broad rollout 前增加一次性的 `session_route_missing` resume-and-retry；
- 先只对 internal/dev node 开启。

稳定组合：

- `multi-slot-concurrent`：一个 tunnel、N 个 slot，多个 structured manager session 可以并发 prompt。

验收：

- 分配到不同 slot 的两个 manager session 可以并发 prompt；
- 同一个 session 的两个 prompt 仍然串行；
- busy sticky slot 不能污染或重路由其他 active session；
- 某个 session close/timeout 不能 release 或 cancel 另一个 session。

### Slice 10：Metrics 接入点

只暴露只读 `ACPSlotSnapshot`：

```text
slot ID
process epoch
process identity
phase
active native session ID
active tool-call ID
last ACP activity time
```

本 slice 不采集、不上传 CPU/内存。

验收：metrics 模块不需要 import router internals，也不能通过 snapshot 修改 slot state。

## 17. 测试策略

所有新 backend package 和发生实质修改的 package，新代码 UT coverage 至少 80%。优先使用 BDD scenario 命名和 testify `assert`/`require`。

### Unit tests

- daemon lock contention、显式 release、stale-file behavior 和 error classification；
- process group identity 与 termination；
- slot 合法和非法状态转换；
- 每个 process epoch 只 initialize 一次；
- canonical paxd client-init 持久化，并且包含 extension field 时 hash 仍然稳定；
- 不同 process epoch 使用不同 internal initialize request ID；
- reconnect 重复 capability report 不重新 initialize worker；
- manager initialize input 不能修改 local client profile；
- worker initialize result 不兼容时，只有该 slot 保持 non-ready；
- request ID string/number 类型保持；
- slot selection 和 deterministic tie-break；
- route version conflict；
- worker-request `(native session, raw ID)` source registration、response cleanup、expiry、raw ID type preservation 和 stale process-epoch rejection；
- process epoch binding clear；
- daemon-start clear-all binding transaction；
- prompt lease 和 cancel bypass；
- busy error；
- resume descriptor 重建；
- route commit 前不能发送 success；
- 使用 fake clock 的 scale up/down；
- reliable producer 在第一次 acceptance 前完成 bootstrap；
- non-blocking concurrent producer acceptance 和连续 sequence assignment；
- journal/network cursor 独立前进；
- network head failure 后不能被 later frame 绕过；
- cumulative ACK 覆盖 stale pending/sent journal row；
- hot-memory eviction 后按精确 sequence 从 journal 读取；
- reconnect cursor 重置为 peer `ackedThrough + 1`；
- manager response 通过 request ID 关联 session；
- SSE exact-session delivery 和 sessionless fail-closed dispatch；
- per-session turn admission 和 cleanup isolation。

### Integration tests

使用可控 fake ACP stdio server，不依赖 sleep：

- 强制 kill 持锁 helper process 后，replacement daemon 必须在 bounded retry window 内取得同一把锁；
- 持锁 parent 被 kill 后，即使 child 继续存活，也不能继续持有 daemon lock；
- 两个 slot 在不同 connection 下返回相同 UUID 形状的 native ID；
- 两个 slot 都用原始 ID `1` 发 permission request，交错到达的 manager response 能回到正确 worker；
- 一份 locally built profile 使用不同 internal ID 初始化 N 个 worker，并且没有 initialize frame/response 进入 session/SSE routing；
- scale-up 后新增 worker 必须在 ready 前取得当前 paxd descriptor；
- manager 只收到 sanitized capability report/upsert，不发送 ACP initialize request；
- prompt 持续输出、进入长 tool call、最终完成；
- tool call 期间 tunnel 断开，但 process 继续存活；
- prompt 期间 slot process 退出；
- cold route 使用 cwd 和 MCP config resume；
- pre-migration route 缺失，manager 显式 resume，原 prompt 只重试一次；
- transport backlog 超过 1000；
- reconnect replay 读取 journal backlog 时继续产生 slot output，新 output 不能越过 backlog；
- journal flush 被 block 时，健康 network send 继续前进；
- network 被 block 时，journal batching 继续前进并 evict hot entry；
- 两个 manager session 共享一条 blank-session paxd tunnel，交错发送 prompt，并且只收到各自 response/update；
- desired slot count 在某个 slot active 时变化；
- paxd restart 杀掉旧 ACP process、清空全部 binding、保留 resume descriptor，并在首次使用时 resume。

### Concurrency tests

相关 package 必须运行 `go test -race`。尽可能使用 channel 和 fake clock，避免依赖不稳定的 timing sleep。

## 18. 发布顺序

允许 breaking version combination，但每一步发布都必须给出一个已知可运行的 manager+paxd 组合，并保留 rollback target。

1. 发布 `paxd-owned-init-v1`：paxd initialize 现有单 worker，manager structured path 不再发送 ACP `initialize`，manager 能展示脱敏 capability report。这是第一个 operator 可验证的 breaking 组合。
2. 发布 `single-worker-owned-init-locked`：增加 daemon lock、process identity 和安全 startup cleanup，不改变 manager 行为。
3. 发布 `single-worker-with-slot-state`：增加 desired-slot、route、slot status 和 startup binding-clear 存储，生产仍然只跑一个 worker。
4. 发布 `router-core-dark`：合入 router/slot fake 测试行为，生产 traffic 暂不使用。
5. 发布 `slot-path-count-one`：真实 traffic 通过一个 `ACPSlot` 和 route table，manager 保持 single-active，并在该组合中删除或禁用旧 persistent-process owner。
6. 发布 `slot-path-count-one-async-producer`：切到最终 reliable producer/single-writer transport，但生产仍只跑一个 slot。
7. 发布 `multi-slot-single-active`：允许 desired slot count 大于一，在 manager 仍然每 agent 只 admit 一个 active turn 的条件下验证 stickiness、resume、scale up/down 和 tunnel reconnect。
8. 发布 `session-mux-gated`：manager 内部使用 per-session mux，但外部 concurrency gate 继续开启。
9. 对 internal/dev node 发布 `multi-slot-concurrent`。
10. reconnect、replay、demux 和 race tests 稳定后再广泛发布。

允许记录的无敏感诊断字段：

```text
connection_id
slot_id
slot_ordinal
process_epoch
phase
native_session_id_hash
request_direction
request_method
transport_seq
recovery_phase
```

## 19. 明确后置的内容

- CPU、内存、IO metrics 和 manager metrics tables；
- 根据资源使用自动 kill；
- Linux cgroup 和 Windows Job Object accounting；
- 基于负载的 min/max 自动扩缩容；
- slot idle reap 和 warm-pool policy；
- session archive/release policy；
- consistent hashing；
- 一个 agent connection 多条物理 ACP tunnel；
- 跨 slot 聚合 `session/list`；
- 任意 legacy client capability negotiation；
- full paxd daemon crash 下的 exactly-once continuation；
- ACP request 或 conversation API 幂等保证；
- 把 `queue_id` ownership 从 connection 改成 slot 或 session；
- per-session reliablemq sequence、ACK、journal、reconcile 或 replay partition；
- 允许一个 session 绕过另一个 session 缺失的 transport sequence；
- concurrent legacy raw user WebSocket claim。

exact-count pool 稳定后再增加 idle reap。后续 idle policy 只能停止没有 active lease 的 ready slot；停止前把 route 标为 cold，session 再次使用时只能通过 `session/resume` 恢复。

## 20. Definition of Done

满足以下条件时，ACP slot foundation 才算完成：

1. 一条 agent tunnel 能把多个 native session 路由到多个 ACP slot；
2. 每个 slot process epoch 都使用 paxd-owned connection client-init descriptor initialize 一次；manager 只收到 sanitized capability report，不发生 initialize request/response session broadcast；
3. 已提交 binding 能跨 tunnel reconnect 保留；paxd restart 会清空全部 binding，但保留 native ID 和 resume descriptor；
4. slot process restart 后清理其 binding，下一条操作先 resume 再 prompt；
5. 一个 slot 永远不会并发执行两个 prompt；
6. worker-originated request ID 即使碰撞，也不能把 permission/client response 错发到其他 slot；
7. replay 完成前不能接收新 prompt；
8. SQLite slot count 热更新不重启物理 tunnel；
9. 旧 `persistentACPProcess` owner 已经删除；
10. reliablemq 只有一个 asynchronous producer queue，以及一条同时处理 replay/live output 的 cursor-based network path；不存在 ACP-specific outbox；
11. `Engine.Send` 不等待 journal/network I/O，network 不绕过 head，cumulative ACK persistence 能让 stale sent state 保持 replay-safe；
12. 两个 structured manager session 可以共享一条 paxd tunnel，不依赖 mutable session fallback，不发生 response/SSE cross-delivery，也不使用 tunnel-wide turn admission；
13. 新 backend code 达到 80% UT coverage，相关测试通过 race detector。
