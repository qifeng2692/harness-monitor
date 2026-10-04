# harness-monitor — 设计文档

> 状态：M1 开发中 ｜ 2026-10-04 立项 ｜ 关联评估报告：`D:\Desktop\harness-monitor-评估报告.md`

## 目标（用户三条硬性要求）

1. **步骤级内容**：看到每一步的信息本身——用户输入原文、模型推理、工具参数、工具结果全文，不是只记录 token 消耗；
2. **按 harness 独立**：每个 harness 单独记录、单独显示（Hermes / Codex / Claude Code / pi / magpie）；
3. **按会话独立**：每个对话单独记录、单独显示——每个会话一条独立时间线。

## 技术栈（成熟框架，Go）

| 组件 | 选型 |
|---|---|
| 语言 | Go 1.27（装在 `C:\dev\go`；开发 shell 先 `source scripts/env.sh`） |
| TUI | [bubbletea](https://github.com/charmbracelet/bubbletea) v1 + lipgloss + bubbles |
| CLI | [cobra](https://github.com/spf13/cobra) |
| 存储 | [modernc.org/sqlite](https://gitlab.com/cznic/sqlite)（纯 Go，免 CGO） |
| 日志 | 标准库 log/slog |

## 架构

```
数据源（本机实测，见 docs/DATA-FORMATS.md）
  Hermes   : state.db (SQLite) + logs/agent.log
  Codex    : ~/.codex/sessions/**/*.jsonl (rollout)
  Claude   : ~/.claude/projects/**/*.jsonl
  pi       : ~/.pi/agent/sessions/**/*.jsonl
  magpie   : ~/.config/magpie/usage.jsonl
        ▼  采集：hm collect --harness <name>（幂等、增量、可重复执行）
  ┌─────────────────────────────┐
  │  采集器 internal/collect/    │
  │  各源 adapter → 统一事件      │
  └──────────┬──────────────────┘
             ▼
  ┌─────────────────────────────┐
  │  SQLite 事件库 data/hm.db    │
  │  events / sessions           │
  └──────────┬──────────────────┘
             ▼
  ┌─────────────────────────────┐
  │  hm TUI（bubbletea）         │
  │  三级导航：harness → 会话 →   │
  │  时间线（步骤内容展开）        │
  │  live 跟随 / replay 回放      │
  └─────────────────────────────┘
```

- **M1**：`hm collect` 手动/重复执行（幂等增量）；TUI live 模式内周期轮询采集。
- **M2**：常驻 `hmd` 守护进程 + hooks 实时通道（Hermes observer hooks / Codex hooks / Claude hooks / pi 扩展）。

## 统一事件模型（内容级）

```go
// internal/model
type Event struct {
    ID        int64     // 自增主键
    Harness   string    // "hermes" | "codex" | "claude" | "pi" | "magpie" —— 一等维度
    SessionID string    // 该 harness 内的会话 ID
    Seq       int64     // 会话内序号（升序）
    Time      time.Time // 统一为 UTC；原始值保留在 meta
    Kind      string    // 见下方 kind 规范
    Title     string    // 一行摘要（列表展示用）
    Content   string    // 内容原文（推理/参数/结果/输入；不截断）
    Meta      string    // JSON：token/耗时/状态等属性
    ParentID  string    // 关联父键（tool_call_id 等）
    SourceKey string    // 幂等去重键（UNIQUE）
}
```

关键纪律：
- **Content 存原文**（实测单条最大 95.6 KB 也照存；超 512 KB 的分块外置是 M2 的事）；
- **Harness + SessionID 是一等维度**——查询、列表、导航全部先按它们分组；
- token/耗时是 `Meta` 属性，**不是**事件的替代。

## 事件 kind 规范（所有采集器统一）

| kind | 含义 | content | meta 关键字段 |
|---|---|---|---|
| `session.start` | 会话开始/元数据 | 摘要或空 | cwd, model, source, version |
| `session.meta` | 会话中途元信息变化（模型切换等） | 摘要 | model, provider |
| `turn.start` | 用户输入 | 用户输入原文 | turn_id |
| `llm.response` | 模型正文输出 | 正文原文 | model |
| `llm.reasoning` | 模型推理 | 推理全文 | — |
| `tool.call` | 工具调用 | 参数原文（JSON/命令） | tool_name, call_id |
| `tool.result` | 工具结果 | 结果全文 | tool_name, call_id |
| `llm.usage` | 用量/成本记录 | 原文（JSON 行） | tokens, ms, status |
| `turn.end` | 轮次结束 | 空 | duration_ms, ttft_ms |
| `session.end` | 会话结束 | 空 | — |
| `compaction` | 上下文压缩 | 摘要 | — |
| `error` | 错误 | 错误原文 | type |

**幂等**：`events.source_key` 建 UNIQUE 索引，由各采集器生成稳定键（如 `hermes:<session>:<msgid>`、`codex:<file>:<line>`）；重复 `collect` 不产生重复事件。

**存储**：SQLite `data/hm.db`（`HM_DATA_DIR` 可覆盖）。M1 内容直接存 SQLite；原始文件归档在 M2 补。

## 里程碑（M1，看板链）

- [x] 仓库 + 设计文档 + 数据格式调研
- [ ] M1-1 骨架：Go module + CLI/TUI 壳 + SQLite 事件库
- [ ] M1-2 magpie 采集器（usage.jsonl → 事件）
- [ ] M1-3 Hermes 采集器（state.db → 事件，内容级）
- [ ] M1-4 Codex 采集器（rollout JSONL 流式解析）
- [ ] M1-5 Claude Code 采集器（projects JSONL）
- [ ] M1-6 pi 采集器（sessions JSONL）
- [ ] M1-7 TUI 三级导航 + 步骤内容展开
- [ ] M1-8 live 跟随 + replay 回放 + 搜索

## 验证方式

- 每个采集器：以本机真实文件为输入，`hm collect` 跑通并打印统计；`hm show` 抽查内容与源文件一致（含全文）；
- TUI：能进入任意 harness 的任意会话、看到每一步内容、能回放；
- 验收判据（M1）：能实时看完一次完整会话，每一步内容可展开；能回放任意历史会话；按 harness/会话独立导航成立。
