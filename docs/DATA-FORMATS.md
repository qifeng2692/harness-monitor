# 数据源格式调研（实测于 2026-10-04，本机 Windows）

> 本文档记录对各 harness 本地数据文件的**实测**结构。所有样例均来自真实文件。
> 解析器开发时以此为准；遇到与此不符的情况，先更新本文档再改代码。

## 1. Hermes Agent

### 1.1 SQLite 状态库
- 路径：`%LOCALAPPDATA%\hermes\state.db`（即 `~/AppData/Local/hermes/state.db`）
- 只读打开：`file:<path>?mode=ro`（WAL 模式下与运行中的 Hermes 并存读取没问题）
- 关键表：
  - `sessions`（286 行实测）：`id, source(desktop|oneshot|cron|kanban|cli|weixin), model, message_count, tool_call_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, estimated_cost_usd, actual_cost_usd, cwd, git_branch, git_repo_root, title, started_at, ended_at, last_activity_at, parent_session_id, ...`
  - `messages`（30k 行实测）：`id, session_id, role(system|user|assistant|tool), content, tool_calls(JSON 字符串), tool_name, tool_call_id, timestamp(浮点秒), reasoning_content(推理全文), finish_reason, ...`
    - assistant 行的 `tool_calls` 是 JSON 数组：`[{"id": "call_xx", "type": "function", "function": {"name": "...", "arguments": "{...}"}}]`
    - tool 行的 `content` 是工具结果全文（实测最大 95.6 KB，如 web_extract 结果）
    - `reasoning_content` 是模型推理全文（实测单条 4.3 KB）
  - `session_model_usage`：按 (session_id, model) 聚合的 token/成本明细
- 时间戳是 Unix 秒（浮点）。

### 1.2 实时日志
- 路径：`~/AppData/Local/hermes/logs/agent.log`
- 行格式（正则友好）：
  - API 调用：`2026-10-04 12:01:46,568 INFO [20261004_115146_2a3461] agent.conversation_loop: API call #13: model=... provider=custom in=135125 out=1367 total=136492 latency=7.1s cache=122752/135125 (91%) id=...`
  - 工具调用：`2026-10-04 12:02:15,892 INFO [session_id] agent.tool_executor: tool terminal completed (1.92s, 704 chars)`
  - 方括号内是 session_id——可作为流式实时通道（尾部 tail 增量读）。
- 注意：这是**文本日志**，用于实时流；完整结构化数据在 state.db。

### 1.3 官方扩展点（后续任务用）
- Observer Hooks（插件，只读遥测契约）：`pre/post_api_request`、`pre/post_tool_call`、`on_session_start/end`、子代理事件等。
- Shell hooks：`config.yaml` 的 `hooks:` 块，CLI+Gateway+Desktop 覆盖。
- 官方 Langfuse 插件参考：`~/AppData/Local/hermes/hermes-agent/plugins/observability/langfuse/`。

## 2. Codex

### 2.1 会话文件（rollout JSONL）
- 路径：`~/.codex/sessions/YYYY/MM/DD/rollout-<ISO时间>-<uuid>.jsonl`
- 实测：141 个文件 / 616.5 MB；单文件最大 172 MB。
- 每行一个 JSON 对象，顶层有 `type` + `payload`（部分记录 payload 内含 `type`）。
- 记录类型（实测统计，单会话）：
  - `session_meta`（1 条）：`session_id, cwd, originator, cli_version, model_provider, base_instructions`
  - `turn_context`：轮次上下文
  - `event_msg/task_started`：`turn_id, started_at, model_context_window`
  - `event_msg/task_complete`：`duration_ms, time_to_first_token_ms, last_agent_message`
  - `response_item/message`：`role(developer|user|assistant), content[{type: input_text|output_text, text}]`
  - `response_item/reasoning`：`content[{type: reasoning_text, text}]` —— **推理全文**
  - `response_item/function_call`：`name, arguments(JSON 字符串), call_id` —— 实测 `exec_command` 最多（含 `cmd, workdir, shell` 等参数）
  - `response_item/function_call_output`：`call_id, output` —— 完整输出
  - `event_msg/token_count`：`info.total_token_usage{input_tokens, cached_input_tokens, output_tokens, reasoning_output_tokens}` + `last_token_usage`
  - `event_msg/item_completed`：含 `item{type: UserMessage|...}`、`started_at_ms/completed_at_ms`
- 工具名实测分布（单会话）：`exec_command` 3311、`js` 227、`view_image` 53、`write_stdin` 37。
- **用户输入的两个载体**（实测均有，采集器两处都要处理）：
  - `event_msg/item_completed` 的 `item.type == "UserMessage"`：`content[0].text` = 用户真实输入（纯文本，如 `"我希望这个 项目适配于PC和移动端…"`）；
  - `response_item/message` `role=="user"`：可能混入 `<INSTRUCTIONS>` 等系统注入内容（如 AGENTS.md 全文）——**作为用户输入要标记或过滤**，不能直接当作对话内容展示。
  - 建议：以 `item_completed/UserMessage` 为 `turn.start` 的权威来源；`response_item/message user` 仅在无对应 item 时降级使用。

### 2.2 官方扩展点（后续任务用）
- Hooks（官方文档 developers.openai.com/codex/hooks）：`SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop, SessionEnd, SubagentStart/Stop, PreCompact/PostCompact, PermissionRequest` 等 12 类。
- 配置：`~/.codex/hooks.json` 或 `config.toml` 的 `[hooks]` 表。当前本机未配置 hooks。
- `codex exec --json` 输出 JSONL 事件流（thread.started/turn.started/item.*/turn.completed），适合无头采集。

## 3. Claude Code

### 3.1 会话文件（对话 JSONL）
- 路径：`~/.claude/projects/<path-encoded-dir>/<session-uuid>.jsonl`；子代理在 `<session-uuid>/subagents/agent-*.jsonl`
- 实测：12 个文件 / 0.8 MB。
- 每行一个 JSON：`{parentUuid, isSidechain, type(user|assistant), message{...}, ...}`
  - `message.role`、`message.model`（如 `deepseek-v4-flash`）
  - assistant `message.content` 是数组：`[{type: "thinking", thinking: "..."}, {type: "text", text: "..."}, {type: "tool_use", id, name, input: {...}}, ...]`
  - user `message.content` 可含 `[{type: "tool_result", tool_use_id, content}]`
- 工具调用参数实测：`{"type": "tool_use", "id": "call_00_...", "name": "Glob", "input": {"pattern": "**/*hermes*"}}`
- 工具结果实测：`{"tool_use_id": "call_00_...", "type": "tool_result", "content": "No files found"}`

### 3.2 官方扩展点（后续任务用）
- Hooks：30+ 事件（`PreToolUse, PostToolUse, PostToolUseFailure, SessionStart, SessionEnd, UserPromptSubmit, Stop, StopFailure, PreCompact, PostCompact, SubagentStart/Stop` 等）。
- 配置：`~/.claude/settings.json` 的 `"hooks"` 块（当前本机只有 env 块，无 hooks）。
- OTel：`CLAUDE_CODE_ENABLE_TELEMETRY=1` + `OTEL_*` 环境变量导出 metrics/logs/traces（traces 需 `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1`）。
- 注意：本机 Claude Code 直连 `https://api.deepseek.com/anthropic`（不走 magpie）。

## 4. pi

### 4.1 会话文件（v3 格式 JSONL）
- 路径：`~/.pi/agent/sessions/<cwd-encoded>/<ISO时间>_<uuid>.jsonl`
- 实测：205 个文件 / 304.5 MB；单文件最大 95 MB。
- 首行：`{"type": "session", "version": 3, "id": "...", "timestamp": "...", "cwd": "..."}`
- 记录类型：`session, model_change, thinking_level_change, message, compaction, custom_message, custom, session_info`
- `message` 记录：`{type: "message", id, parentId, timestamp, message: {...}}`
  - `message.role`: `system | user | assistant | toolResult`
  - user 消息：`content: [{type: "text", text: "..."}]`，带 `timestamp`（毫秒）
  - assistant 消息：`content: [{type: "thinking", thinking: "..."}, {type: "text", text: "..."}, {type: "toolCall", id: "call_00_...", name: "bash", arguments: {...}}, ...]`
    - **工具调用块实测结构**：`{"type": "toolCall", "id": "call_00_TmCdkNJCJfeZrAhNgfQ29594", "name": "bash", "arguments": {"command": "cd /g/powershell && ls -la ..."}}` —— `arguments` 是**已解析的对象**（不是字符串）
  - toolResult 消息：`{role: "toolResult", toolCallId: "call_00_...", toolName: "bash", content: [{type: "text", text: "完整输出"}]}`
  - 消息比例实测（单会话前 3000 行）：user 13 / assistant 1443 / toolResult 1453 —— 工具循环是主体
- `compaction` 记录：上下文压缩事件（92 条实测于大文件）——可观测上下文管理。

### 4.2 官方扩展点（后续任务用）
- TypeScript 扩展：`~/.pi/agent/extensions/*.ts`，`pi.on()` 订阅生命周期（`before_agent_start → message/tool events → agent_end/agent_settled`）。
- 本机已有扩展示例：`bounded-compaction.ts`, `rsi.ts`。

## 5. magpie 网关

### 5.1 用量日志
- 路径：`~/.config/magpie/usage.jsonl`
- 每行一条调用记录（实测 750+ 条，持续增长）：
  ```json
  {"route_id": 1791085907388, "t": "2026-10-04T11:59:58+08:00", "agent": "hermes", "provider": "workbuddy-ai-plugin", "host": "workbuddy-ai as q...@gmail.com", "model": "deepseek-v4.1-flash", "req": "...", "served": "...", "in": 3029, "out": 1446, "cache_read": 68992, "reasoning": 497, "effort": "max", "ms": 7271, "ttft_ms": 2252, "first_text_ms": 4544, "status": 200, "rid": "...", "ep": "/v1/chat/completions"}
  ```
- `agent` 字段标识来源（hermes/OpenAI/pi/curl...）；`status` 含 429/499 等错误码（实测 429×87）。
- 其他可复用文件：`quota-history.json`、`credits-daily.json`、`providers.json`。

## 6. 采集注意事项（通用）

1. **编码**：所有文件 UTF-8；解析用容错模式（遇到坏行跳过并计数，不中断）。
2. **大文件**：单文件可达 172 MB——必须**流式按行读取**（bufio.Scanner + 大 buffer，或 json.Decoder），禁止一次性 ReadAll。
3. **追加写**：JSONL 是追加模式；增量读取用"记录 byte offset + 按行消费"实现 tail。
4. **关联键映射**（统一事件模型）：
   - Hermes: `session_id`(sessions.id) → `turn`(无显式 ID，按 user 消息切分) → `tool_call_id`
   - Codex: `session_id`(session_meta) → `turn_id`(task_started) → `call_id`(function_call)
   - Claude: 文件名 uuid = `session_id` → `tool_use.id`
   - pi: `session.id` → `message.id`/`parentId` 链 → `toolCallId`
5. **时间**：Hermes=Unix 秒浮点；Codex=ISO8601(UTC)；Claude=ISO8601；pi=ISO8601(毫秒)；magpie=ISO8601(+08:00)。统一转为 UTC RFC3339 + 保留原值。
