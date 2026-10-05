# DESIGN: 对比实验台（Compare Lab）— harness-monitor 新功能

> 目标：程序内新增「对比实验台」视图。用户输入一个问题，勾选多个 harness，
> 点击开始 → 各 harness 并发运行 → 过程分别记录 → 在同一窗口并列显示，方便对比。
>
> 本文件是实施规格。所有命令与数据格式均经**本机真实探测**验证（2026-10-05）。

## 1. 功能定义

- 一个输入框（多行文本），一组 harness 多选框（hermes / codex / claude / pi）
- 点「开始对比」→ 为每个选中的 harness 启动一次非交互运行（同一 prompt）
- 运行过程**实时**流入 UI：每个 harness 一列，列内是该 harness 的步骤时间线
- 每列头部显示：状态（等待/运行中/完成/失败/超时）、耗时、token 用量、（claude 的）费用
- 运行结束后可回看历史批次（runs 列表）；每列底部显示最终回答
- 数据独立存储（runs / run_harness / run_events 三张表），不污染主监控库的导航

## 2. 实测命令（每家 harness 的非交互运行方式）

工作目录隔离：每个 harness 在 `data/runs/<run_id>/<harness>/` 下运行（自动创建，隔离副作用）。

| harness | 命令（prompt 传递方式见备注） | 输出格式 |
|---|---|---|
| hermes | `hermes chat -q <prompt> --oneshot --format stream-json --in <dir>` | JSONL 事件流 |
| codex | `codex exec --skip-git-repo-check --json -C <dir>` + **prompt 走 stdin** | JSONL |
| claude | `claude -p --output-format stream-json --verbose --permission-mode bypassPermissions <prompt>`（cwd=dir） | JSONL |
| pi | `pi -p --mode json` + **prompt 走 stdin**（cwd=dir） | JSONL |

**为什么 codex/pi 走 stdin**：它们是 npm 的 `.cmd` 包装器；Go 1.22+ 对 .cmd 的参数做安全转义检查，
多行/特殊字符 prompt 作为参数会直接报错。stdin 通道无此问题（已实测）。hermes/claude 是原生 exe，
prompt 作为参数安全（Go 直接传 argv，无 shell 解析）。

**已知噪声**：codex 在 stdin 有内容时可能输出 `Reading additional input from stdin...` 一行（非 JSON）——
解析器必须忽略非 JSON 行。pi 启动时可能输出 `Warning: ...` 行——同样忽略。

## 3. 数据模型（internal/store 扩展）

```sql
CREATE TABLE IF NOT EXISTS runs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  prompt     TEXT NOT NULL,
  harnesses  TEXT NOT NULL,            -- JSON 数组，如 ["hermes","codex"]
  created_at TEXT NOT NULL,
  status     TEXT NOT NULL DEFAULT 'running'   -- running | done
);
CREATE TABLE IF NOT EXISTS run_harness (
  run_id      INTEGER NOT NULL,
  harness     TEXT NOT NULL,
  status      TEXT NOT NULL DEFAULT 'pending', -- pending|running|done|failed|timeout
  started_at  TEXT NOT NULL DEFAULT '',
  ended_at    TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  tokens_in   INTEGER NOT NULL DEFAULT 0,
  tokens_out  INTEGER NOT NULL DEFAULT 0,
  cost_usd    REAL    NOT NULL DEFAULT 0,
  final_text  TEXT NOT NULL DEFAULT '',
  error       TEXT NOT NULL DEFAULT '',
  session_id  TEXT NOT NULL DEFAULT '',        -- 运行产生的会话 id（可跳主监控视图）
  PRIMARY KEY (run_id, harness)
);
CREATE TABLE IF NOT EXISTS run_events (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id   INTEGER NOT NULL,
  harness  TEXT NOT NULL,
  seq      INTEGER NOT NULL,
  time     TEXT NOT NULL,
  kind     TEXT NOT NULL,
  title    TEXT NOT NULL DEFAULT '',
  content  TEXT NOT NULL DEFAULT '',
  meta     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_run_events_rhs ON run_events(run_id, harness, seq);
```

加入现有 `schema` const（CREATE TABLE IF NOT EXISTS 对已有库自动生效，无需迁移脚本）。

## 4. 事件类型映射（每家解析器 → 统一 kind）

统一 kind（与主监控一致）：`session.start` / `turn.start` / `llm.reasoning` / `llm.response` /
`tool.call` / `tool.result` / `llm.usage` / `turn.end` / `session.end` / `error`。

真实 fixture 在 `internal/runner/testdata/`（每家两种：pong 简单问答 / tool 带工具调用）。

### hermes（stream-json）
| 源事件 | 映射 |
|---|---|
| `{"type":"system","subtype":"init","model","session_id"}` | session.start（meta 存 model/session_id） |
| `{"type":"tool_use","name","input"}` | tool.call（content=input JSON 美化，meta.tool_name） |
| `{"type":"tool_result","name","output","duration_ms","is_error"}` | tool.result（content=output，meta 存全部） |
| `{"type":"text","text"}` | 文本增量（"P"、"ONG" 分片）→ **累积**，遇到非 text 事件或进程结束时冲刷为一条 llm.response |
| `{"type":"result","session_id","text","tokens","duration_ms","exit_code"}` | llm.usage（tokens）+ final_text=text；同时标记完成 |

注意：hermes 的 stream-json 协议**不包含 reasoning**（源码注释明确）——这是协议限制，v1 如实记录；
推理可在主监控视图通过 session_id 回看（会话落盘含推理）。

### codex（exec --json）
| 源事件 | 映射 |
|---|---|
| `thread.started {thread_id}` | session.start |
| `item.started` item.type=command_execution {command} | tool.call（content=command） |
| `item.completed` item.type=command_execution {aggregated_output,exit_code} | tool.result |
| `item.completed` item.type=agent_message {text} | llm.response |
| `item.*` item.type=reasoning（若有） | llm.reasoning |
| `item.*` item.type=error {message} | error |
| `turn.completed {usage:{input_tokens,output_tokens,...}}` | llm.usage |
| `turn.failed {error}` | error（并标记 failed） |

### claude（stream-json）
| 源事件 | 映射 |
|---|---|
| `system/init {session_id,model,tools}` | session.start |
| `assistant` message.content[] 块：`thinking` | llm.reasoning（thinking 全文） |
| 同上 `text` | llm.response |
| 同上 `tool_use {name,input}` | tool.call |
| `user` message.content[] 块：`tool_result {content,is_error}` | tool.result |
| `result {result,duration_ms,total_cost_usd,usage}` | llm.usage（cost+tokens）+ final_text=result |

### pi（--mode json）
| 源事件 | 映射 |
|---|---|
| `session {id,cwd}` | session.start |
| `tool_execution_start {toolCallId,toolName,args}` | tool.call |
| `tool_execution_end {toolCallId,toolName,result:{structuredContent:{output},content},isError}` | tool.result |
| `turn_end {message:{content:[thinking|text|toolCall],usage,model}}` | thinking→llm.reasoning；text→llm.response；usage→llm.usage；toolCall 跳过（已由 tool_execution 覆盖，避免重复） |
| `agent_settled` | session.end |
| `message_start/message_update/message_end` | **忽略**（流式增量太细；turn_end 有全量） |

final_text = 最后一个 turn_end 的 text 块。

## 5. 进程管理（internal/runner）

- 每 harness 一个 goroutine：`exec.Command` + `StdoutPipe`
- **读取必须用 `bufio.Reader.ReadBytes('\n')`（或 Scanner 带 ≥1MB buffer）**：
  pi 的单行最大可达 ~118KB（message_start 携带完整 system prompt），默认 64KB Scanner 会报错
- prompt 通过 stdin 的：`cmd.Stdin = strings.NewReader(prompt)`；其余 `Stdin = nil`
- 超时：默认 10 分钟/harness，超时 `cmd.Process.Kill()`（记录 status=timeout）
- 退出码非 0 且未收到完成事件 → status=failed，error=最后一条 error 事件或 stderr 摘要
- 每家结束 → 更新 run_harness 行；全部结束 → runs.status='done'
- **网关瞬时故障**（实测 12:33–12:40 出现 502 窗口）：hermes 会输出 result 里带 error 字段；
  codex 输出 turn.failed；这些如实记录为 failed + error 文本，UI 原样显示（不是本工具的 bug）
- 并发写库：所有 run_events 插入经一个 channel 串行化，避免 SQLite 单写者冲突

## 6. API 层（gui/app.go 新增）

```go
// 启动一次对比运行：创建 run 行 → 并发 spawn → 立即返回 runID（过程经事件推送）
StartCompare(prompt string, harnesses []string) (int64, error)
// 停止一个运行中批次：kill 所有子进程，status=timeout? 不——用 stopped
StopRun(runID int64) error
// 批次列表（倒序）+ 单批次详情（含每 harness 行）
ListRuns() ([]RunInfo, error)
GetRun(runID int64) (RunDetail, error)
// 某 harness 在批次中的事件流（与主监控 GetEvents 同结构，前端复用渲染）
GetRunEvents(runID int64, harness string) ([]EventInfo, error)
// 自然语言说明（复用 internal/explain）
ExplainRunEvents(runID int64, harness string) ([]explain.StepExplain, error)
```

实时推送：每次有新事件/状态变化，`runtime.EventsEmit(a.ctx, "run:update", runID)`。
前端 `window.runtime.EventsOn("run:update", ...)` 收到后拉取增量。

**实现落点（HM-C4）**：真实逻辑在 `internal/compare`（包 `compare`），`gui/app.go` 只做绑定透传——
Wails 绑定层是 package main，cmd/ 无法 import，独立成包才能让验收程序（`cmd/hmc4check`）跑同一段代码。
前端绑定名不变：`StartCompare / StopRun / ListRuns / GetRun / GetRunEvents / ExplainRunEvents`
（Wails 生成 `window.go.main.App.*`）。

实现后的确切签名与数据结构：

```go
// internal/compare
type RunHarnessInfo struct { Harness, Status, StartedAt, EndedAt string; DurationMs, TokensIn, TokensOut int64
                             CostUSD float64; FinalText, Error, SessionID string }   // json camelCase
type RunInfo struct { ID int64; Prompt string; Harnesses []string; CreatedAt, Status string
                      Active bool; Runners []RunHarnessInfo }   // json camelCase
type RunDetail = RunInfo   // 详情与列表项同构，前端复用同一渲染函数

func (s *Service) StartCompare(prompt string, harnesses []string) (int64, error) // 立即返回 runID
func (s *Service) StopRun(runID int64) error
func (s *Service) ListRuns() ([]RunInfo, error)          // 倒序，最近 50 个
func (s *Service) GetRun(runID int64) (RunDetail, error)
func (s *Service) GetRunEvents(runID int64, harness string) ([]model.Event, error)  // 与主监控同结构
func (s *Service) ExplainRunEvents(runID int64, harness string) ([]explain.StepExplain, error)
func New(st *store.Store) *Service   // 内部 runner.New(st)；Service.OnUpdate 接 runtime.EventsEmit
func (s *Service) Close()            // App.shutdown 调：停在跑批次 + 关 writer
```

`RunInfo.Active` 报告编排器此刻是否仍在跑该批次（前端据此启用「停止」按钮；批次 done 后必为 false）。
`GetRunEvents` 的 JSON 形状与主监控 `GetEvents` 一致（`EventInfo`），前端复用渲染逻辑。

**验收工具**：`cmd/hmc4check` 无 GUI 驱动同一套 `internal/compare`（PONG 四家 → 轮询 GetRun →
打印各家 status/duration/tokens/final_text/events → ListRuns 历史）；`-stop-after 6s` 可验收
StopRun 路径（各家应记 stopped）。它默认用 `data/` 下与 GUI 同一个库，因此产生的批次在
GUI 的历史列表里同样可见（跨进程持久化）。

## 7. 前端（gui/frontend/dist）

- 顶栏加视图切换：`[监控] [对比实验台]`（简单 show/hide，不引入路由）
- 对比实验台布局：
  - 上：多行输入框 + harness 勾选（hermes/codex/claude/pi，默认全勾）+ [开始对比] + [停止]
  - 中：历史批次下拉/列表（最近 N 个）
  - 下：并列列区。每 harness 一列（flex，等宽），列头=状态徽标+耗时+tokens；列体=步骤卡片
    （复用主视图 renderStep 的样式：kind 徽标 + 标题 + 内容全文可展开）；列尾=最终回答块
- 运行中：每 1.5s 拉增量（或事件驱动）；状态徽标动画
- 样式复用 style.css 现有变量/类，新增 `.compare-*` 类

## 8. 已知边界与决策

- **工作目录隔离**：每 harness 独立空目录（`data/runs/<id>/<harness>/`）——对比的是"harness 对同一问题的处理过程"，
  隔离避免互相踩文件；空目录限制文件类工具发挥，属预期（v1 决策，可后续加"共享工作目录"选项）
- **权限**：claude 用 bypassPermissions（本地隔离目录内运行）；codex 默认沙箱；hermes/pi 默认。
  若某家因权限卡住 → 表现为超时，如实记录
- **magpie 不参与对比**：它是网关不是 harness（agent 运行器），只作为四家的模型路由层
- **模型**：各 harness 使用各自当前配置的模型（这正是"对比 harness"的含义）
- **reasoning**：hermes stream 协议不含推理（协议限制）；claude/pi/codex 有则记录
- **codex 的 `error` 事件可能是噪声**：实测 codex 会为「忽略未知配置项」（如
  `~/.codex/config.toml` 里的 `disable_response_storage`）发 `item.completed item.type=error`。
  按第 4 节映射它会落成 `kind=error` 事件（如实记录，不丢），但它**不是运行失败**——
  该家 status 仍为 done。前端渲染时不要把 `error` 事件与 status=failed 混为一谈（HM-C5）

## 9. 验收标准（E2E）

1. `go build ./... && go vet ./...` 通过；新增单测全过（fixtures 解析断言）
2. GUI 启动 → 对比实验台 tab → 输入 "Reply with exactly: PONG" → 勾选全部 4 家 → 开始
3. 四列全部出现且实时更新；最终 4 列都到终态（done/failed 如实）
4. 至少 2 家 done 且 final_text 正确（预期 4 家，网关故障时如实 failed）
5. 截图确认：并列显示、步骤可见、状态/耗时正确
6. 历史批次：重启后仍能看到刚才的批次并回看
7. 从 explorer 双击启动的 GUI 里跑通（火绒环境下子进程存活——重点验证项）
