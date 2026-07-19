# ACP Slot 池弹性伸缩 — 设计草案

日期：2026-07-19
状态：设计文档，暂不实现

## 问题

slot 数量目前是静态 desired state（`agent_connection.desired_acp_slots`，默认 2）：

- 每个 slot 是一个常驻 ACP 子进程（codex-acp / claude-agent-acp 多为 node 进程，单个常驻内存以百 MB 计）。空闲 harness 白白占内存。
- 需求超过 slot 数时，router 返回 `slot_unavailable` / `slot_busy`（`internal/runtime/acp_router.go`），云端只能重试，没有自动扩容。
- prompt lease 是每 slot 单活跃 prompt（`acquirePromptLease`），并发会话多时吞吐直接被 slot 数卡死。

## 目标

- 需求驱动扩容：出现容量压力时自动增加 slot，上限可配。
- 空闲缩容：长时间无活跃会话的 slot 自动排空退出，支持缩到零。
- 冷启延迟可控：缩到零后首个请求的延迟有预算和兜底。

## 设计要点

### 伸缩信号

- **扩容信号**：滑动窗口内 `slot_unavailable` / `slot_busy` 错误次数超过阈值；或 ready slot 的 bound routes 均值超过阈值（`CountBoundACPSessionRoutesBySlot` 已有数据）。
- **缩容信号**：slot 连续空闲（无活跃 prompt、无 bound route 活动）超过 `idle_ttl`（建议默认 10 分钟）。

### 执行机制

复用现有 desired state 对账架构，**不引入新的执行路径**：

- 新增一个 autoscaler 组件（daemon 层），只负责把 `desired_acp_slots` 的"有效值"调整写回 daemonstore，由现有 `ACPSlotSupervisor` 对账生效。
- 区分 `desired_acp_slots`（用户配置的上限）与 `effective_slots`（autoscaler 决定的当前值），`0 ≤ effective ≤ desired`。
- 缩容走现有 drain 路径（`BeginSlotDrain` → 排空 → `RemoveSlot`），不打断进行中的 turn。

### 缩到零与冷启

- 缩到零后，router 收到请求时返回 `slot_unavailable`（现状），同时 autoscaler 观察到该错误立即拉起 slot。
- 冷启延迟 = 进程启动 + ACP initialize，实测预算应在秒级。前置依赖：**冷恢复异步化**（见代码评审：`resumeColdRoute` 同步阻塞入站泵的问题），否则冷启会放大队头阻塞。
- 可选优化：per-connection 配 `min_slots`（默认 0 或 1），对延迟敏感的 harness 保温一个 slot。

### 配置草案

```yaml
agents:
  - name: work
    acp_slots:
      max: 4          # 现 desired_acp_slots 语义收窄为上限
      min: 0
      idle_ttl: 10m
      scale_up_threshold: 3   # 窗口内容量错误次数
```

## 依赖与顺序

1. 冷恢复异步化（先修，独立收益）。
2. 诊断快照落地（伸缩决策需要的信号大多来自这里的埋点）。
3. autoscaler + effective_slots 对账。
4. 缩到零 + min_slots 保温。

## 风险

- 伸缩抖动：需要冷却时间（scale-up/scale-down cooldown）防止振荡。
- 与云端 desired state 的权责：云端下发的是 max/min，本机 autoscaler 掌握 effective，需要在 node-control 协议里明确谁写谁读。
