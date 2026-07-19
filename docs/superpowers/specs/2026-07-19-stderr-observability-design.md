# paxd 滚动日志、harness stderr 捕获与诊断快照 — 设计

日期：2026-07-19
状态：已批准，待实现

## 背景

三个相关的可观测性缺口：

1. paxd 自身日志依赖 launchd 重定向到 `~/.paxd/logs/paxd.log`，launchd 不轮转，文件无限增长；systemd 侧走 journal 尚可，但两个平台行为不一致，也没有 paxd 自管的 debug 日志。
2. ACP 子进程（harness）的 stderr 被 `io.Copy(io.Discard, …)` 丢弃（`internal/runtime/acp_slot.go`、`internal/runtime/agent_tunnel_session.go`）。harness 起不来或崩溃时，状态里只有 `process_ended`，没有原因。
3. transport 队列积压、未 ack 帧数、重连次数、slot 阶段等运行时指标没有任何本地出口；transport journal 积压只在 flush 失败时打一条日志。

## A. 滚动日志（新包 `internal/logrotate`）

自写轮转器，不引入外部依赖：

- `RotatingWriter` 实现 `io.Writer`，内部持锁（logger 之外还有 stderr 捕获路径并发写入）。
- 写入前检查当前文件大小，超限则轮转：`paxd.log → paxd.log.1 → … → paxd.log.N`，丢弃最旧备份，重开新文件。
- 配置（`daemon` 段）：
  - `log_file`，默认 `~/.paxd/logs/paxd.log`
  - `log_max_size_mb`，默认 20
  - `log_max_backups`，默认 3（最坏磁盘占用 = 20MB × (1+3) = 80MB）
- `paxd run` 启动时 `log.SetOutput`：
  - stderr 是终端（`os.Stderr` 为 char device）→ `io.MultiWriter(os.Stderr, rotator)`，前台开发体验不变；
  - 非终端（launchd/systemd 托管）→ 只写 rotator，避免与服务重定向双写。
- launchd plist：保留 `StandardErrorPath`（兜 panic 与 init 前崩溃，量小），移除 `StandardOutPath`。systemd 单元不改，但由于非终端时 `log` 输出全部走 rotator，journal 此后只会收到 panic 与 init 前的崩溃输出——两个平台统一以 `~/.paxd/logs/paxd.log` 为主日志。
- 现有 `daemon.log_level` 配置保持现状，日志级别治理不在本次范围。

## B. harness stderr 捕获（`internal/runtime`）

新增 `stderrTail` 组件：

- 一个 goroutine 逐行读子进程 stderr：
  1. 每行经标准 logger 写入主日志，前缀 `[harness stderr] connection_id=… slot_id=… `；
  2. 尾部保留在 8KB 环形缓冲。
- 替换 `acp_slot.go` 与 `agent_tunnel_session.go`（直连遗留路径）中的 `io.Copy(io.Discard, proc.Stderr())`。
- slot 进程异常退出时：`ACPSlot` 暴露 stderr 尾部，`ACPSlotSession` 把截断到最后 2KB 的尾部放进 `Exit.Details["stderr_tail"]`；ACP slot supervisor 持久化到 `ACPSlotStatus.LastErrorMessage`。`paxl daemon agent list` 与云端 status 上报即可看到退出原因。
- 行长防御：单行超过环形缓冲容量时按字节截断，不因超长行放大内存。

## C. 诊断快照（control query + local API）

新增 control 查询 `GetDiagnostics`，local API 暴露 `GET /v1/diagnostics` 返回 JSON。数据源：

| 来源 | 内容 | 状态 |
|------|------|------|
| daemonstore status 表 | remote/connection/slot 的 phase、reconnect attempt、next retry、last error | 现成 |
| `Supervisor.Snapshot()` | 各 slot 的 backoff/phase | 现成 |
| write-behind store `Stats()` | dirty frames/patches/bytes、连续 flush 失败数 | 现成 |
| Producer stats（新埋点） | per-queue 的 tail、acked_through、未 ack 数（tail−acked_through）、bound、last_error | 新增 |
| daemon 自身 | 版本、启动时间、日志文件当前大小与路径 | 新增 |

新埋点实现：包装 `ReliableEngineFactory`，记录每个 queueID 对应的 `*reliablemq.Producer`，暴露 `Stats()` 聚合接口；`runtimeSupervisors` 实现 `control.DiagnosticsProvider` 注入 control service。

消费方式：`curl --unix-socket ~/.paxd/paxd.sock http://paxd/v1/diagnostics`。`paxl` 侧展示留待后续。

## D. 配套设计文档（本次只写文档，不实现）

- `docs/slot_pool_elasticity_plan.md`：slot 池弹性伸缩（`slot_unavailable` 触发扩容、空闲缩容/缩到零、冷启延迟预算）。
- `docs/transport_data_gc_plan.md`：`acp_session_route` 与 transport journal 的回收（TTL、compaction、启动清理策略）。

## 测试

- `logrotate`：边界轮转、备份位移、丢弃最旧、并发写。
- `stderrTail`：环形截断、行前缀、超长行。
- slot 失败路径：进程退出后 `ACPSlotStatus.LastErrorMessage` 含 stderr 尾部（集成测试）。
- `GetDiagnostics`：fake store/provider 驱动的单元测试；local API 路由测试。

## 非目标

- 日志级别过滤 / slog 迁移。
- Prometheus 格式指标、新监听端口。
- `paxl` / `paxd doctor` 的 CLI 展示层。
- stderr 的实时查询接口。
