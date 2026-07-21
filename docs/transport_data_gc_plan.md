# 会话路由与 Transport Journal 数据回收 — 设计草案

日期：2026-07-19
状态：transport outbound ACK 回收已实现；session route 回收仍为设计

## 问题

两处本地数据只增不减：

1. **`acp_session_route`（`~/.paxd/paxd.db`）**：每个 native session 一行，含 resumeParams。启动时只清 binding（`ClearAllACPSessionRouteBindings`），行本身永不删除。长期运行的机器会累积数千行死会话路由。
2. **transport journal（`~/.paxd/transport.db`，reliablemq `transport_journal` 表）**：出入站帧持久化。acked/applied 帧是否被 prune 取决于 paxkit reliablemq 的实现——**需要先验证**；若无 compaction，聊天流量会让该库以每 turn 数十 KB 的速度增长。

## 目标

- 两个库的磁盘占用有界。
- 回收不破坏正确性：未 ack 的帧、可能被 resume 的会话路由不能删。

## 设计要点

### Transport outbound retention BDD

- Given ACK 正在高频推进，when ACK 落库，then ACK 热路径不执行删除。
- Given ACKed 记录仍在最近 72 小时内，when GC 运行，then 记录保留。
- Given ACKed 记录已超过 72 小时但位于每个 queue 最新 100 个 seq 内，when GC 运行，then 记录保留。
- Given ACKed 记录同时超过时间窗口和 debug tail，when GC 运行，then 单次最多删除 1000 条。
- Given daemon context 被取消，when transport DB 关闭，then GC 先停止，不再访问已关闭的 DB。

### acp_session_route 回收

- 表已有 `last_used_at`（每次路由命中更新）。
- 策略：TTL 清理，`last_used_at` 超过 `route_ttl`（建议默认 30 天）且当前无 binding 的行删除。
- 时机：daemon 启动时 + 每日定时；单次删除加 LIMIT 分批，避免长事务。
- 注意：删除后云端若再发该 session 的帧，得到 `session_route_missing`（-32002，requiresResume）——这是协议已有的语义，云端本就要处理，TTL 只是让"多旧的会话还能 resume"有了明确边界。该边界需与云端会话保留策略对齐。

### transport journal 回收

分两步：

1. **验证现状**（先做，不改代码）：写集成测试观察 reliablemq 在帧 acked/applied 后是否删除或复用行；确认 `transport.db` 的实际增长曲线。
2. **按验证结果选择**：
   - 若 reliablemq 已有 prune：确认触发条件与保留窗口是否可配，暴露到 paxd 配置即可。
   - 若没有：在 paxkit 层加 compaction API（按 queue 删除 `seq ≤ acked_through − keep_window` 的出站帧、`applied` 的入站帧），paxd 定时调用。保留窗口（建议 1000 帧或 24h，取大）用于重连后的重放对账，与 `replayLimit=1000`（`agent_tunnel_session.go`）保持一致。
3. **queue 轮转清理**：`RotateAgentTransportQueueID` 换队列后，旧 queueID 的帧成为孤儿，应在轮转成功后整队列删除。
4. SQLite 空间回收：删除后定期 `PRAGMA incremental_vacuum`（建库时启用 `auto_vacuum=INCREMENTAL`）或低频 `VACUUM`。

### 配置草案

```yaml
daemon:
  route_ttl: 720h                         # acp_session_route TTL（尚未实现）
  transport_journal_gc_interval: 10m
  transport_journal_keep_acked_for: 72h
  transport_journal_keep_latest: 100
  transport_journal_gc_batch_size: 1000
```

## 依赖与顺序

1. reliablemq prune 行为验证（独立、零风险，也是毒帧行为验证的顺路活）。
2. route TTL 清理（纯 paxd 侧，可先行）。
3. journal compaction（可能涉及 paxkit 改动）。
4. queue 轮转孤儿清理 + vacuum。

## 风险

- 删了还需要的帧会导致重连对账失败 → 保留窗口必须 ≥ replay 需求，且以 `acked_through` 为硬边界。
- 与 reliablemq 的事务边界：compaction 必须与正常写入互不阻塞（SQLite WAL + busy_timeout 已配，分批删除即可）。
