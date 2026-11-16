# Secretary 多摘要只读分页工具

本 PR 增加只读分页工具，尚未部署；普通 ACP 行为保持兼容。

- `cmd/paxd/mcp_workspace.go`：capability 模式新增第五个只读工具 `workspace_summaries`。
- 参数 `session_id` 为当前整理来源，另有 cursor、limit；默认 20、最大 50，0 沿用默认。
- `internal/cloud/secretary_tools.go`：现有 node-auth bridge 允许 `tool: summaries`，外层执行 session 仍取环境，目标 session 保留在 arguments_json。
- 响应 `{items,next_cursor?}` 原样返回；正常 item 为完整 SummaryVersion。Manager 的工具页限制 64 KiB，超大单项只返回 `{summary_id,revision,title,requires_read:true}` 目录头，不截断正文。
- 目录头不代表已读，修改前用 `workspace_read({ref:{kind:"summary",id:"...",revision:N}})` 取得整版；摘要整版不分页，计入每执行累计 4 MiB 读取预算。超限在返回前明确拒绝，不记入读取账本。
- prompt 的 summaries_has_more 为 true 且 cursor 为空时，从第一页调用 summaries。Manager 继续负责权限、确切版本及任务范围；普通会话只暴露原四个工具。
- 不增加任何写工具，现有 read/history/expand 的可见正文过滤不变。

先部署支持 `summaries` 分支的 Manager，再升级 Secretary 所在 host 的 paxd。
旧 Manager 返回显式安全失败，不把失败当空页；未知后端错误仍沿用既有 unavailable 映射。

验证：两个修改包完整串行测试、定向边界测试、go vet、gofmt、build、真实 binary stdio discovery 通过；
两个修改文件 statement 覆盖 91.4% / 96.6%，均高于 80%。新测试采用 BDD 名称和 testify。

2026-11-04 契约收口：工具 description 同步目录头与完整读取规则；仅改说明，不改变 dispatch、schema 或 ACP 行为。定向 schema/discovery 测试与 binary 重新构建供跨服务验收。
