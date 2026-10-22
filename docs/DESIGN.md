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

**会话置顶**：`session_pins(harness, session_id, pinned_at)` 独立于 events——用户在 GUI 会话列表点 📌 写入；`ListSessions` LEFT JOIN 该表，置顶排最前（按 pinned_at 倒序），其余按最近活动倒序。清库重采不影响置顶。

**存储**：SQLite `data/hm.db`（`HM_DATA_DIR` 可覆盖）。M1 内容直接存 SQLite；原始文件归档在 M2 补。

## 里程碑（M1，看板链）

- [x] 仓库 + 设计文档 + 数据格式调研
- [x] M1-1 骨架：Go module + CLI/TUI 壳 + SQLite 事件库
- [x] M1-2 magpie 采集器（usage.jsonl → 事件）
- [x] M1-3 Hermes 采集器（state.db → 事件，内容级）
- [x] M1-4 Codex 采集器（rollout JSONL 流式解析）
- [x] M1-5 Claude Code 采集器（projects JSONL）
- [x] M1-6 pi 采集器（sessions JSONL）
- [x] M1-7 TUI 三级导航 + 步骤内容展开
- [x] M1-8 live 跟随 + replay 回放 + 搜索（+ ingest_state 增量续读）

## 双击启动与安全软件（2026-10-04 实测）

**现象**：双击 exe（或桌面快捷方式）无任何反应；从终端/`Start-Process` 启动同一 exe 完全正常。

**根因**：本机运行火绒（Huorong，进程 `HipsDaemon`）。带 `cobra.Execute()` 的二进制在 **explorer 双击启动**环境下会被火绒静默终止（进程启动 2-3 秒后消失；无错误输出、无 WER 记录、无崩溃转储；`Start-Process` / 终端启动不受影响）。对照实验（2026-10-04）：

| 实验 | 结果 |
|---|---|
| 平凡 Go 程序（explorer 启动） | 正常存活 |
| cobra 程序，`Execute()` 被调用（explorer 启动） | **2-3 秒后被静默杀死**（改名/换目录/换 cobra 版本 v1.8.1 均复现） |
| cobra 导入但不调用 `Execute()` | 正常存活 |
| 手写解析器（无 cobra，含完整主逻辑） | 正常存活 |

**修复**：`cmd/hm/main.go` 改为手写子命令解析（不用 cobra，见文件头注释）。修复后 explorer/快捷方式双击稳定。

**防护措施**（保留在代码里，用于未来排查同类问题）：
- `hm-boot.log`（exe 同目录）：记录每次启动的阶段（boot → store.Open → collect → tui.Run → exit），双击出问题时看这个文件；
- panic 捕获：崩溃堆栈写入启动日志；
- `pauseOnError`：双击启动（无参数）出错时窗口暂停显示错误，不再一闪而过；
- SQLite `busy_timeout=10000` + WAL：多实例并发打开不冲突。

**教训**：本机（火绒环境）新增 CLI 工具不要引入 cobra；如需框架，先做 explorer 双击启动的存活测试再定。

## M1 验收结果（2026-10-04）

- 事件总量：151,649（hermes 45,495 / codex 51,068 / pi 53,381 / magpie 1,503 / claude 202）
- 独立会话：587 个；重复 source_key：0
- 内容级验证：推理最大 99.9KB、工具结果最大 292KB、工具参数最大 79.9KB 全文入库
- 数据完整性：codex 86/86 会话、pi 190/190 真实会话全覆盖（obs_ ledger 与共享 session_id 已甄别）
- 测试：`go test ./...` 全过；`go vet` 通过

## 会话用量 / 成本合计（2026-10-10）

库里有上万条 `llm.usage` 事件，但它们此前只是时间线上孤立的步骤——「这个会话烧了多少
token、花了多少钱」读不出来。同类工具（abtop 的 token/成本一览）的看家能力正是这个，
因此新增 `internal/usage` 做会话级合计，GUI 时间线顶部显示一条用量条。

**为什么需要一个包、而不是前端把数字加起来**：五家的记录形态不同，直接相加会算错。

| harness | 记录的含义 | 合计方式 |
|---|---|---|
| hermes | 「会话 × 模型」的汇总行 | 相加（每条即该模型在整个会话的合计） |
| claude | 每会话一条 `cost-state` 汇总（含分模型明细） | 相加（通常只有一条） |
| pi | 每次模型调用的用量 | 相加 |
| magpie | 每次模型调用的用量（另带 `status`） | 相加 |
| codex | 每次模型调用的用量 | 相加，但**必须取 `last_token_usage` 子块** |

codex 是唯一有坑的一家。它的 `event_msg/token_count` 事件同时带两个块：

- `total_token_usage`：该 rollout 文件的**累计计数器**。会话被恢复（`codex resume`）时
  新文件的计数器会继承父文件的值——实测同一会话横跨 **19 个 rollout 文件**，其中 3 个
  文件的计数器带着 121.6 万～277 万的上游基数；
- `last_token_usage`：**本次调用**的用量增量，与 pi/magpie 的逐次记录语义一致。

因此取 `last` 逐条相加：得到的才是「模型实际处理过的 token」（含恢复会话时重发的上下文，
也就是真正计费的部分），且与其余四家口径统一。实测两种错法的量级：

| 算法 | 某 codex 会话的合计（3061 次调用） |
|---|---|
| 逐条相加 `total_token_usage` | 1.03 万亿（放大 1044 倍） |
| 取全会话最大 `total` 快照 | 9.11 亿（漏计其余 18 个文件、且含继承基数） |
| **取 `last` 逐条相加（本实现）** | **9.91 亿** |

**费用的「未知」≠「免费」**：hermes 的 `actual_cost_usd`/`estimated_cost_usd` 常为 0
（供应商未回填），magpie 的记录里根本没有费用字段。此时 `costKnown=false`，界面显示
「费用未知」而不是 `$0.0000`——把「没报」显示成「不花钱」是误导。同理，magpie 的非 200
调用（限流 429 / 超时 499）单独计数为「失败调用」；meta 为空/坏的行（实测 codex 有 23 条）
如实报数为「未计入」。

**验证**：`internal/usage` 有 10 条单测覆盖五家口径与边界；另以本机真实库做逐会话交叉
验证——Go 实现与独立计算的 Python 脚本对 **615 个会话**（codex 86 / hermes 305 /
magpie 7 / claude 12 / pi 205）逐项一致（token 分项、合计、失败数、费用、跳过数）。
命令行核对工具：`go run ./cmd/hmusagecheck <harness> [session-id]`。

## 失败 / 异常可见（2026-10-11 实测）

失败此前在库和界面里都是不可见的：`error` 这个 kind 从规范到样式（TUI 红色、
GUI 的「错误」筛选与 `.kind-error`）全都存在，但**没有任何采集器产出过一条 error 事件**
——本机 20 万条事件里 error 为 0。更隐蔽的是「有失败但没标记」：一次退出码 1 的命令
在时间线上与成功的长得一模一样。

各类失败信号分散在各家的记录里，且都藏在**结构化字段**而非正文文本中：

| harness | 信号 | 字段位置 | 本机实测 |
|---|---|---|---|
| codex | `turn_aborted`（回合被中断） | `event_msg.payload.type` + `reason` | 17 条（此前落进 default 当「未映射类型」丢弃） |
| claude | 接口失败重试记录 | `type=system, subtype=api_error`（含 `error.status`/`retryAttempt`） | 28 条（此前未映射） |
| claude | 网关报错被写成「模型回复」 | `type=assistant, isApiErrorMessage=true`（`model="<synthetic>"`） | 2 条（此前当正常回复采进 `llm.response`） |
| pi | 模型调用失败 | `message.stopReason=error` + `errorMessage` | 196 条（此前 content 为空数组 → 静默跳过，这一轮凭空消失） |
| hermes | 工具调用失败 | 工具结果正文 JSON 的 `exit_code` 非 0 / 非空 `error` | 1358 条 |
| pi | 工具调用失败 | `message.isError=true` | 995 条 |

落地方式：

- **采集层**把上述信号归一为 `error` 事件（`meta.type` = `turn_aborted` / `api_error` /
  `stop_reason_error`）或写进工具结果的 `meta.is_error`（附带 `exit_code`）；
- **说明层**（`internal/explain`）把 `meta.type` 翻译成「失败在哪一层、意味着什么」，
  并对标记为失败的工具结果改说「执行失败」，带出退出码；
- **界面层**按 `kind=error` 与 `meta.is_error` 在步骤卡片上打红色徽标（「错误」/
  「失败·退出码 1」），筛选器选「错误」即可只看失败步骤。

**不做正文猜测**：只认结构化字段。用文本关键词扫正文会把正常输出里出现的 "error"
误判成失败（hermes 有 210 条 assistant 正文含 error 字样，绝大多数是正常讨论）。

**pi 报错标题要收敛**：`errorMessage` 实测常是整段 JSON / HTML 报错体（429 限流说明、
502 网关页），原样当标题会把列表撑爆——能识别出 HTTP 状态码时标题只写状态码
（「模型调用失败：429」），全文仍完整存在 content 里。

**验证**：新增 8 条单测（各 harness 的失败信号 + 正常路径不被误标 + 标题收敛）；
另以本机真实数据源采到临时库（**不碰仓库 `data/`**）交叉验证：error 事件 244 条
（claude 30 / codex 17 / pi 197）、失败标记 2353 条（hermes 1358 / pi 995）；
再用 Node 把 GUI 真正发布的 `app.js` 里的判定函数抽出来喂全库 **201667 条真实事件**：
判定 2597 条、漏判 0、假阳性 0。命令行核对工具：`go run ./cmd/hmerrcheck <临时目录>`。

## 工具调用配对（call ↔ result，2026-10-08 实测）

一次工具调用的「请求」与「返回」在时间线上经常不相邻——实测本机库（`data/hm.db`）：

| harness | 结果落在调用之后 >5 步 | 最长间隔 |
|---|---|---|
| codex | 1198 | 11685 步 |
| hermes | 709 | 3152 步 |
| pi | 189 | 12 步 |
| claude | 0（最长 4 步） | — |

只看相邻步骤读不出「这个结果是哪次调用的」，一次工具往返会被拆成两段无关联的记录。
`internal/explain/pair.go` 用采集器写入的稳定 id 做会话内配对（**调用看 `meta.call_id`，
结果看 `parent_id`**，实测四家 100% 可配对）：

| harness | 调用侧 | 结果侧 |
|---|---|---|
| hermes | `meta.call_id`（`parent_id` 同值） | `parent_id` |
| codex | `meta.call_id` | `parent_id` |
| claude | `meta.call_id` | `parent_id`（`meta.tool_use_id` 同值） |
| pi | `meta.call_id` | `parent_id`（`meta.call_id` 同值） |

配对带来的三件事：

1. **间隔如实告知**——调用步骤说明「结果在第 N 步之后才回来（中间隔了 M 步）」，结果步骤回指
   「这是第 N 步那次调用的返回」；
2. **工具结果能拿到真名**——codex 的 `tool.result` 只有 `call_id`、claude 只有 `tool_use_id`，
   单看该步只能说「未知工具」（实测占全部 `tool.result` 的 25.2%，11423/45342 条），
   配对后从调用侧补回工具名；
3. **诚实报告缺失**——调用无结果（实测 44 次，多为会话在工具执行中被中断）与结果无调用
   （原始记录缺调用侧）分别说明，不静默错配。

配对是纯函数（`PairEvents` / `ExplainOnePaired`），主监控、对比实验台、`cmd/explainpreview` 共用同一实现。

## 重复调用环（tool loop，2026-10-22 实测）

`hm stats` 能回答「失败了多少次」，答不了「同一个动作是不是卡在原地反复做」。后者才是
agent 空转最常见的样子：同一条命令、同一个参数，失败一次、再一次、又一次——每次都记了
日志，但没人把它单独拎出来说「这里在打转」。同类工具把这条当核心信号：MINDLAS 叫它
tool loop，rungraph 叫它 retry storm。

判定口径（`internal/loops` 纯函数 + `internal/store/loops.go` 取数层）：

| 维度 | 口径 | 理由 |
|---|---|---|
| 「重复」 | 同一会话 + 同一工具 + 同一组参数 | 结构事实，不依赖失败信号 → codex/claude 也能查出来 |
| 「次数」 | 按 `meta.call_id` 去重 | 实测库里 5139 个 call_id 被采了两遍，按行数算会把「重复采集」误报成「重复调用」 |
| 「失败」 | 只认 `meta.is_error` | hermes 成功也写 false（判得了）、pi 只在失败时写 true、codex/claude 不带该字段（判不了） |
| 阈值 | 默认 3 | 实测：阈值 3 命中 367 组 / 126 会话；阈值 5 命中 96 组 |

**为什么不在 SQL 里 JOIN**：`tool.call LEFT JOIN tool.result ON parent_id =
json_extract(meta,'$.call_id')` 在本机 500MB 库上跑 180 秒没跑完（无索引的 JSON 提取 +
字符串 JOIN）。改成「两次平表查询 + Go 里归组」后各只要 ~1 秒，整条命令 ~10 秒。

**验收**：`internal/loops` 12 条单测（含 call_id 去重、跨 harness 不混组、判不了不当作
成功）、`internal/store` 2 条单测（取数与失败判定）；真实库 E2E 与 Python 口径逐项核对
一致（367 组 / 126 会话 / 阈值 5 → 96 组）。


## 验证方式

- 每个采集器：以本机真实文件为输入，`hm collect` 跑通并打印统计；`hm show` 抽查内容与源文件一致（含全文）；
- TUI：能进入任意 harness 的任意会话、看到每一步内容、能回放；
- 验收判据（M1）：能实时看完一次完整会话，每一步内容可展开；能回放任意历史会话；按 harness/会话独立导航成立。
