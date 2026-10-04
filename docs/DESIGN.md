# harness-monitor — 设计文档

> 状态：M1 开发中 ｜ 2026-10-04 立项 ｜ 关联评估报告：`D:\Desktop\harness-monitor-评估报告.md`

## 目标（用户三条硬性要求）

1. **步骤级内容**：看到每一步的信息本身——用户输入原文、模型推理、工具参数、工具结果全文，不是只记录 token 消耗；
2. **按 harness 独立**：每个 harness 单独记录、单独显示（Hermes / Codex / Claude Code / pi / magpie）；
3. **按会话独立**：每个对话单独记录、单独显示——每个会话一条独立时间线。

## 技术栈（成熟框架，Go）

| 组件 | 选型 | 状态 |
|---|---|---|
| 语言 | Go ≥ 1.22 | 工具链安装中（winget GoLang.Go） |
| TUI | [bubbletea](https://github.com/charmbracelet/bubbletea) v1.x + lipgloss + bubbles | 事实标准 |
| 存储 | [modernc.org/sqlite](https://gitlab.com/cznic/sqlite)（纯 Go，免 CGO）+ FTS5 | Windows 友好 |
| 文件监听 | [fsnotify](https://github.com/fsnotify/fsnotify) | tail 增量读取 |
| CLI | [cobra](https://github.com/spf13/cobra) | 子命令组织 |
| 日志 | [log/slog](https://pkg.go.dev/log/slog)（标准库） | 零依赖 |

## 架构

```
数据源（本机实测，见 docs/DATA-FORMATS.md）
  Hermes   : state.db (SQLite) + logs/agent.log
  Codex    : ~/.codex/sessions/**/*.jsonl (rollout)
  Claude   : ~/.claude/projects/**/*.jsonl
  pi       : ~/.pi/agent/sessions/**/*.jsonl
  magpie   : ~/.config/magpie/usage.jsonl
        │
        ▼  采集（M1：文件/DB 增量读取；M2：hooks 实时通道）
  ┌─────────────────────────────┐
  │  hmd 守护进程（Go）           │
  │  · 各源 adapter → 统一事件    │
  │  · 增量 offset 断点续读       │
  │  · SQLite 事件库 + 原始归档   │
  └──────────┬──────────────────┘
             │
  ┌──────────▼──────────────────┐
  │  hm TUI（bubbletea）         │
  │  三级导航：                   │
  │  harness 列表 → 会话列表 →    │
  │  会话时间线（步骤内容展开）    │
  │  live 跟随 / replay 回放      │
  └─────────────────────────────┘
```

## 统一事件模型（内容级）

```go
type Event struct {
    ID        int64     // 自增
    Harness   string    // "hermes" | "codex" | "claude" | "pi" | "magpie" —— 一等维度
    SessionID string    // 该 harness 内的会话 ID
    TurnID    string    // 可选
    Seq       int64     // 会话内序号
    Time      time.Time
    Kind      string    // session.start|turn.start|llm.response|tool.call|tool.result|...
    // 内容载荷（原文，不截断）
    Title     string    // 一行摘要（列表展示用）
    Content   string    // 内容原文（推理/参数/结果/输入）
    Meta      string    // JSON: token/耗时/状态等属性
    ParentID  string    // 关联父事件（tool_call_id 等）
}
```

关键纪律：
- **Content 存原文**（实测单条最大 95.6 KB 也照存；>512 KB 外置归档）；
- **Harness + SessionID 是一等维度**——查询、列表、导航全部先按它们分组；
- token/耗时是 `Meta` 属性，不是事件的替代。

## 里程碑（M1 范围）

- [x] 仓库 + 设计文档 + 数据格式调研
- [ ] 项目骨架（go.mod + cobra + bubbletea 骨架可编译运行）
- [ ] magpie 采集器（usage.jsonl → 事件）
- [ ] Hermes 采集器（state.db 只读 → 事件）
- [ ] Codex 采集器（rollout JSONL 流式 → 事件）
- [ ] TUI 三级导航（harness → 会话 → 时间线）+ 步骤内容展开
- [ ] live 跟随 + replay 回放

## 验证方式

- 每个采集器：以本机真实文件为输入，跑通并打印事件统计（如"Codex: 141 会话 / N 事件"）；
- TUI：能进入任意会话、看到每一步内容、能回放；
- 验收判据（M1）：能实时看完一次完整会话，每一步内容可展开；能回放任意历史会话；按 harness/会话独立导航成立。
