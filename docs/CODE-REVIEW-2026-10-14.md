# harness-monitor 全面代码审查报告

> 审查日期：2026-10-14 ｜ 审查对象：main @ 76f7520（36 commits，2026-10-04 立项）
> 审查方法：6 路分域深度审查（采集层 / 存储与汇总 / 子进程编排 / 解释与 CLI·TUI / GUI / 横切安全）+ 基线验证 + 关键发现独立复核（P0/P1 全部经第二人复核、可复现的均做了复现实验）
> 规模：Go ~14,400 行（43 文件）+ 原生 JS 976 行 + 文档 4 篇

---

## 一、执行摘要

这是一个**工程纪律明显高于同类个人项目平均水平**的代码库：SQL 全参数化、前端转义纪律扎实、无 shell 注入面、单写者落库架构严格正确、幂等键设计使重采不重不漏、注释里大量「实测 N 条」的数据佐证、历史坑（NO_PROXY、火绒双击）的根因分析堪称范本。

但审查发现 **2 个 P0、12 个 P1**，集中在三条线上：

1. **数据正确性**：hermes 用量「首采冻结」（注释声称的 UPSERT 语义从未存在，实测 9 组数据与源库永久背离且无自愈）；codex 增量采集每个新用户 turn 重复入库；claude/codex 的失败工具结果被说明书渲染为「执行完毕」（同一格式两份解析器已分叉）。
2. **生命周期两端**：GUI 关窗时对比批次终态写入被静默丢弃（数据库永久残留 running）；taskkill 失败回退只杀 .cmd 外壳留下 node 孤儿。
3. **GUI 前端**：回放功能自第一版起就是无限互递归（点击即栈溢出，高亮从未生效）；会话切换无竞态守卫导致数据错乱；对比实验台 metaCache 键冲突导致失败徽标按错误数据判定。

安全面结论：**无可利用的 P0 漏洞**。风险集中在一条链上——「不可信内容渲染（WebView2）+ StartCompare 绑定 + claude `bypassPermissions`」：当前无逃逸点，但三环中任何一环失守都是全机 RCE。另有一处开源前必办事项：git 历史残留真实用户名。

**总体评级：B+（骨架优良，正确性细节与生命周期管理需一轮集中修复）**。修复路线图见文末——P0 两条修复成本都在 20 行以内。

---

## 二、基线验证

| 项目 | 结果 |
|---|---|
| `go build ./...` | ✅ 通过 |
| `go vet ./...` | ✅ 通过 |
| `go test ./... -count=1` | ✅ 全部通过（89 个测试函数） |
| `gofmt -l` | 仅 pair_test.go 工作区 CRLF 行尾（索引为 LF，非真实格式问题） |
| race 检测 | 本机不可用（CGO 关闭且无 C 编译器）——**全项目并发代码从未经 -race 验证** |
| 测试覆盖率 | export 100% / usage 90.8% / explain 82.1% / compare 81.3% / tui 77.4% / **collect 52.4%** / **runner 58.6%** / **store 36.3%** |
| 集成测试 | `//go:build integration` 标签隔离，默认 `go test ./...` **不执行**（无 CI，意味着只在开发者手动跑时生效） |
| 仓库卫生 | ✅ 良好（二进制/日志/数据目录均已 gitignore）；⚠️ git 历史残留真实用户名（见安全节） |

---

## 三、发现统计

| 严重度 | 定义 | 数量 |
|---|---|---|
| P0 | 数据错误无自愈 / 功能从未工作 / 确定性崩溃 | **2** |
| P1 | 正常或常见路径下的错误行为 / 真实安全风险 | **12** |
| P2 | 特定场景错误 / 健壮性 / 深度防御 | ~32 |
| P3 | 卫生 / 死代码 / 可维护性 | ~22 |

标注约定：〔双确认〕= 两个审查路径独立发现；〔实证〕= 写过测试或用真实数据验证；〔复现〕= 已独立复现。

---

## 四、P0 详述

### P0-1〔复现〕GUI 回放功能：无限互递归，点击即栈溢出，高亮从未生效

- **位置**：`gui/frontend/dist/app.js:261` + `app.js:552`
- **问题**：`renderTimeline()` 尾部在 `state.replay === true` 时调用 `highlightReplay()`；`highlightReplay()` 内部又无条件调用 `renderTimeline()`。互递归无终止条件（仅空事件列表两个提前 return）。自 f324c9a（CLI→GUI 重构首个提交）即存在。
- **复现**：Node 模拟同构调用链，4631 层互调后 `RangeError: Maximum call stack size exceeded`。
- **影响**：点击「回放」按钮即触发；500 步会话约 230 万次 renderStep 调用（WebView2 冻结几十秒后崩溃）。且 552 行之后的加高亮、滚动定位、进度提示全部不可达——**回放的 UI 反馈从未生效过**。该功能从未被真正验收，也没有任何自动化护栏。
- **修复**：删除 261 行的 `if (state.replay) highlightReplay();`，高亮逻辑改为渲染后由调用方（toggleReplay/replayStep）定位 DOM（`jumpToStep` app.js:394 已是正确模式，直接复用）。修后必测：点回放、按键步进、回放中切过滤、回放中开 LIVE 四条路径。

### P0-2〔双确认+实证〕hermes 用量「首采冻结」：注释声称的 UPSERT 语义从未存在，数值与源库永久背离

- **位置**：`internal/collect/hermes.go:627` + `internal/store/store.go:166`
- **问题**：hermes 的 llm.usage 事件 source_key 固定为 `hermes:usage:<session>:<model>`（不含区分新旧值的成分）；而 `session_model_usage` 表是**随会话持续增长的累积值**，采集器每轮重扫都发出含最新值的同一键事件，store 的 `INSERT OR IGNORE` 全部静默忽略。`hermes.go:76` 注释明确声称「幂等 **UPSERT**……用量变化都能跟上」——**该语义从第一天起就不存在**。
- **实证**：对本机双库只读比对——live state.db 有 627 组，hm.db 仅 348 条 hermes llm.usage；9 组已采集数值明显落后且永远追不上（例：session `20260922_155824_b4…` 的 glm-5.3-flash，hm.db 存 input=3,401,304，源库已是 4,814,828）；另有 279 组尚缺（下次首插后同样冻结）。
- **影响**：任何 hermes 会话在首次采集后继续使用（常态），GUI 用量条与导出的 token/费用从此停在首采快照，随时间越差越大，无自愈路径。
- **修复**（按侵入性递增）：① store 增加独立 upsert 通道 `UpsertUsageSnapshot(... ON CONFLICT DO UPDATE SET meta/time)`；② 或把 INSERT OR IGNORE 升级为 `ON CONFLICT(source_key) DO UPDATE` 且仅对 hermes usage 更新 meta/time；③ 同步修正 hermes.go:76 注释，并对「同键不同内容被忽略」计数打日志，让这类语义错配下次可被察觉。

---

## 五、P1 详述

### 采集层（3 条）

**P1-1〔实证〕codex 增量续读时每个新用户 turn 重复入库**
`internal/collect/codex.go:199-206, 382-397`。镜像行去重依赖 `hasUser` 预扫，但预扫只在 `!incremental || start.Offset == 0` 时执行；增量轮次（offset>0）`hasUser=false`，新追加的 `response_item/message-user` 镜像行不再被跳过，与权威 `UserMessage` 各出一条内容相同的 `turn.start`。实测构造文件复现：第二轮增量新增 2 条重复 turn.start。影响所有默认入口（双击采集、TUI live、`--incremental`）。修复：增量模式对 offset>0 文件也做预扫（bytes.Contains 粗筛可接受），或改逐行状态机。

**P1-2〔双确认+实证〕全量采集静默丢弃无换行结尾的最后一行**
`internal/collect/incremental.go:25-33`。`splitCompleteLines` 在 EOF 无换行时不产出 token，而全量与增量共用该 split，五家采集器均无「文件结束时补采末行」兜底。实测 magpie 两行文件（第二行无换行）全量 Collect 结果 `scanned=1`。harness 崩溃/被 kill/磁盘满时 JSONL 末行常无换行；增量可自愈，全量永久丢。修复：全量路径改用 `bufio.ScanLines` 或在 consumed < size 时补采。

**P1-3 文件截断/原地重写后 source_key 碰撞，新数据被静默吞掉**
`internal/collect/incremental.go:42-53`。`size < offset` 时从头重读，行号型 source_key 与旧事件完全相同 → 内容已变的新行全部被 OR IGNORE 吞掉，无告警；size 相等或更大的原地重写更会从旧 offset 错位续读。`IngestPos.MTime` 注释称用于判定截断/轮转，实际从未参与判定。修复：水位线引入内容指纹（首行 hash 或 offset 处快照）；检测到变化先删该文件旧事件再重读，或以代次号新键前缀重读。

### 子进程编排层（3 条）

**P1-4〔复核〕Close 不等在跑批次收尾，数据库永久残留 running**
`internal/runner/orchestrator.go:251-271, 347-355`。`Close()` 对每个批次只 `ar.stop()`（取消 context）后立即 `close(o.ch)`；各 harness 的终态写入要经历「取消→killTree→管道 EOF→Wait→enqueue」毫秒级链路，几乎必然晚于 close，`enqueue` 因 `o.closed=true` 静默丢弃。全仓库无启动时清理残留 running 的逻辑。GUI 关窗退出（批次在 10 分钟超时内很常见）即触发：重启后历史列表永远显示「运行中」，停止按钮不可用。修复：Close 后等各批次终态入队再关 channel；store 提供启动时清理函数（无活跃编排器的 running 行批量改 stopped）。

**P1-5 taskkill 失败回退只杀 .cmd 外壳，node 工作进程成孤儿**
`internal/runner/kill_windows.go:21-23`。回退 `cmd.Process.Kill()` 只杀 cmd.exe 外壳——本机实测 codex/pi 均解析为 `.cmd` shim，node 子进程存活，恰是同文件注释自述要避免的后果。taskkill 本身又经 PATH 解析（可被劫持）且无重试。修复：taskkill 用 `%SystemRoot%\System32\taskkill.exe` 绝对路径；失败重试 2-3 次；仍失败在 Result.Error 如实上报「进程树可能未完全终止」。

**P1-6 hermes/claude 的 prompt 走 argv 通道，环境脆弱**
`internal/runner/runner.go:179, 184-185`。设计文档明示 Go 1.22+ 对 .cmd 参数的转义限制正是 codex/pi 改走 stdin 的原因，hermes/claude 依赖「原生 exe」假设——在 claude 经 npm 安装的机器上（.cmd shim），GUI 多行 prompt 会让两家 `cmd.Start()` 直接失败；Windows 命令行 32767 字符上限同样只打击这两家。附带：prompt 以 `-` 开头会被子 CLI 当选项吞掉（无 `--` 分隔符）。修复：支持 stdin 回退（claude `-p` 可读 stdin）或至少文档化边界 + 明确报错。

### 数据链路（2 条）

**P1-7〔复核〕claude/codex 的失败工具结果被说明书渲染为「执行完毕」**
`internal/collect/claude.go:323-332` vs `internal/runner/parse_claude.go:59`。采集侧 `claudeBlock` 结构体没有 `IsError` 字段——源 JSONL 中明明存在的 `"is_error": true` 在解析阶段直接丢弃，tool_result meta 只写 `{"tool_use_id":...}`；codex 的 `function_call_output` 同样无失败标记。铁证是 runner 侧解析器抓到了该字段（parse_claude.go:202 写入 meta）——**同一格式两份解析器已分叉**。hermes/pi 不受影响。后果：主监控 GUI 与 `hm export` 对失败的 Bash 调用说「执行完毕，把结果送回来了」，导出头部失败统计也不含它们。修复：claudeBlock 加 `IsError *bool` 并落 meta（对齐 runner 侧做法）；codex 确认 `function_call_output` 的可判定信号；explain/export 无需改动即恢复正确。

**P1-8〔复核〕run_events 落库即丢 parent_id，对比实验台配对残缺且文案错误归因**
`internal/store/run.go:259-261, 296-298`。runner 解析器明明为 tool.call/result 都设了 ParentID，但 run_events 表无该列，写读双丢。配对靠 meta 兜底链侥幸存活（codex 靠 item_id、claude/pi 靠 call_id）；**hermes 流协议无调用 id，完全不配对**，每条 tool.result 都带错误归因文案「原始记录里调用侧缺失」（事实是缺 id 不是缺调用）。修复：run_events 加 parent_id 列；孤儿文案区分「无 id 可配」与「调用缺失」两种原因。

### GUI 前端（2 条）

**P1-9 会话切换无竞态守卫：旧响应覆盖新会话数据，说明文本张冠李戴**
`gui/frontend/dist/app.js:90-115, 508-529`。`selectSession`/`liveTick` 发起请求后不校验「会话是否仍是当前选中」即写入 state；liveTick 由 3 秒 setInterval 驱动且无重入守卫（对比实验台有 pendingReload，主监控没有）。大会话响应慢于切换速度时：时间线标题显示 B、内容是 A，explains 按索引取值错位。修复：请求令牌模式（`const token = ++state.sessionToken`，每个 await 后校验）+ liveTick 加 pending 守卫。

**P1-10 对比实验台列渲染丢失 metaCache，失败徽标按主监控会话的 meta 错判**
`gui/frontend/dist/app.js:810 + 266-275 + 725`。`loadRun` 为每列正确构建了 metaCache，但 `renderCompareColumn` 组装 ctx 时漏传，`parseMeta` fallback 到主监控当前会话的全局缓存，键是裸 seq（批次 seq 从 1 重计，必然冲突）。后果：失败徽标按无关会话的 meta 判定——该显示的不显示、不该显示的显示；批次反向污染主监控缓存。修复：810 行补 `metaCache: c.metaCache`；更稳妥的做法是 parseMeta 无 ctx 缓存时不 fallback 全局，缓存键改 `harness+seq`。

### 安全（1 条）

**P1-11〔复核〕claude 以 `--permission-mode bypassPermissions` 运行 + prompt 全文透传 = 无闸门执行通道**
`internal/runner/runner.go:185`。DESIGN-runner.md §8 记录这是 v1 决策（隔离目录内运行），但隔离仅限 cwd——bypass 模式下 claude 可全盘操作。用户把不可信文本（网页内容、采集到的可疑会话）当作对比问题粘贴时，提示注入指令被直接执行。开源后其他用户的安全预期与此不符。修复：改 `acceptEdits`（或 `default` + `--allowedTools` 白名单），或至少 UI 明示「bypass 模式下粘贴不可信文本 = 交给 agent 直接执行」。

### 性能（1 条）

**P1-12 全量拉取 + O(n²) 渲染 + 全量 DOM 重建，千级事件即明显卡顿**
`gui/app.go:174-196` + `app.js:304, 248`。GetEvents 无分页返回全部事件全文；renderStep 对每个卡片 `findIndex`（O(n²)）；每次全量 innerHTML 重建；LIVE 每 3 秒重复一遍。真实库存在万级步骤会话（README 实测 codex 11685 步）。修复：Go 层加 `GetEventsAfter(harness, session, afterSeq)` 增量接口；渲染前建 `Map(seq→idx)`；长期虚拟滚动。

---

## 六、P2 分类综述（~32 条）

### 健壮性 / 生命周期
- kill 后无 stdout 管道强关兜底：孙进程持有写端时读循环永久阻塞，Run 永不返回（runner.go:390-411）
- `ReadBytes` 对超长无换行行无内存上限，恶意/异常输出可耗尽内存（runner.go:390-398）
- 采集 flush 失败后错误粘滞但扫描不中断，batch 无上限增长，大文件中途一次 DB 失败可致数百 MB 内存峰值（claude/pi/codex 采集器）
- 预扫函数把 scanner 错误向上抛：单个坏文件（如单行超 32MB 的 ErrTooLong）中止整个 harness 采集且 offset 不推进——不可信数据文件植入一条超长行即让该家采集永久卡死（codex.go:612-639、claude.go:751-782）〔安全/可用性 DoS〕
- Scanner 单行上限各家不一（claude 8MB/magpie 1MB/codex·pi 32MB），超限行为同上
- SQLite DSN 未做 URI 转义，路径含 `?#%` 时解析错（hermes.go:157、store.go:132）
- magpie 采集无分批，全量内存累积后一次性插入（magpie.go:93-123）
- 增量续读 session_id 从文件名推导，与首行权威值靠 harness 命名约定耦合，非标准文件名时会话裂成两条时间线（codex.go:750-759）〔实证当前一致〕
- 无 UTF-8 BOM 处理，首行带 BOM 时丢首条事件（五家通用；实测本机无 BOM）
- **schema 无版本管理**：`PRAGMA user_version`=0，全部 IF NOT EXISTS，无升级路径——下一次 schema 变更（如 P0-2/P1-8 修复就需要）没有安全通道
- Stop 为纯异步语义，返回时不保证子进程已终止，API 契约未写明（orchestrator.go:208-219）
- GUI 无 SingleInstanceLock，多开实例互相偷增量、穿插写库（gui/main.go）
- GUI startup 打开库失败仅 fmt.Println，release 构建用户完全不可见（gui/app.go:42-49）
- rundbcheck 向真实库写探针批次，异常中断残留脏数据；explainpreview/hmerrcheck 参数校验缺失直接 panic

### 正确性边界（多数当前 0 触发、属防御缺口）
- usage：codex `last_token_usage` 缺失/为空时回退到含继承基数的累计计数器——一旦触发即天文数字级重复计数（usage.go:115-119，实测当前 0 例）〔待验证〕
- usage：claude 多条 cost-state 会被求和（快照按增量累加）（usage.go:12，实测当前 34 文件均恰 1 条）〔待验证〕
- explain 相邻性判断假设 seq 严格连续，seq 空洞时「隔了 N 步」说明失真（explain.go:88-99）
- usage TotalTokens 补齐在会话层与模型桶层规则不一致，混合记录时会话合计 ≠ 桶合计（usage.go:218-226）
- magpie meta 缺 model 时桶名被污染为「模型名+状态码」（usage.go:237-244）
- explain 的 toolFailureNote 只认 is_error 不认 exit_code≠0（runner 侧 codex 只写 exit_code）——compare-lab 的 codex 失败命令仍说「执行完毕」（explain.go:291-306）
- hermes 无 id 调用的 `idx<N>` 兜底键跨消息碰撞，当前不串台属侥幸（hermes.go:451-452）
- 时间零值经 formatTime 变空串，污染 ListSessions 的 MIN(time) 字符串排序（store.go:456-461）
- InsertEvents 中途失败回滚后仍返回虚增的 inserted/skipped（store.go:192-207）
- 事件全量加载无分页（ListEvents），导出整篇驻留内存；codex 单条 content 实测可达 ~8MB

### GUI / UX
- kindClass/status 拼 class 属性未转义——当前 kind 是内部白名单不可利用，属防御缺失〔双确认：GUI+安全两路〕
- 回放中切会话按钮 active 样式残留；keydown 未排除 TEXTAREA（对比框打字驱动隐藏回放）；切回对比视图不刷新当前批次；LIVE 强制滚底打断阅读且 explains/usage 不随 LIVE 更新；explorer 启动不校验目录存在；ListRuns N+1 查询（50 批次 101 条 SQL）；搜索高亮对变长 ToLower 字符错位；aboutDataDir 永远为空；搜索跳转双倍加载；stopRun 无 finally；DataDir 末级兜底相对路径（Program Files 下全功能不可用）

### 安全深度防御
- Wails 绑定面构成 XSS→RCE 摆放大器：WebView2 内 JS 可直调 StartCompare（经 bypassPermissions）——当前无逃逸点，但未来任何一处 innerHTML 遗漏 esc 后果是 RCE 而非 XSS〔结构性〕
- harness 二进制 PATH 查找 + 全量环境（含 API key）继承，`HM_HARNESS_BIN_*` 无校验——供应链失守时的放大器
- 导出 Markdown：正文有正确的动态围栏保护，但 title/kind/explain 字段未转义——可伪造文档结构/注入链接/raw HTML（export.go:55-90）
- 采集内容零脱敏全量入库、可导出、可全文搜索（用户误粘贴的密钥同此路径）；建议可选正则脱敏开关
- TUI 渲染保留 ANSI 转义序列（终端注入，实际影响显示错乱为主）
- git 历史（6ef5ea6 等 ba2da80 之前）残留真实用户名（本机用户目录路径 `C:\Users\<用户名>`）——**开源前 filter-repo 或 squash 首发**
- taskkill 经 PATH 解析可被劫持（与 P1-5 同修）

---

## 七、P3 清单（~22 条，摘要）

- 死代码：gui GetEventMeta/trimContent、collect fileStartPos/HasSession（「已入库文件从末尾起跟」注释宣称的行为未实现）
- 抄写漂移：fmtTime ×4（行为已微差）、oneLine ×2、truncate ×3、model.Event→explain.Event 转换 ×4——建议 internal/viewutil 或 explain.FromModel 统一
- 五家口径杂项：codex tool.call meta 键 `name` vs 他家 `tool_name`；codex item_completed 多块 content 丢后续；claude tool_result 非 text 块（image）静默丢（pi/codex 有占位/计数）；codex token_count Title 固定；magpie 失败调用不出 error 事件；claude RetrySource 解析后未用
- store：parseTime 失败静默零值；RFC3339Nano 变长小数秒同秒内排序不准；导出默认名 8 字符前缀碰撞时静默覆盖（hermes 同秒会话天然同前缀）；SearchEvents 前导通配必然全表扫（18 万行毫秒级，暂可接受）；decodeHarnesses 损坏 JSON 静默退化
- runner：终态判定 ctx 优先，完成/取消竞态窗口内可误标 timeout/stopped；codex 配置告警噪声可掩盖真实 stderr 失败原因；Result.Events 全量驻留内存；run 目录永不清理；并发批次共享全局 ~/.codex 等配置（DESIGN §8 未列此边界）
- CLI：未知命令双重报错；export 默认路径 CWD 相对
- TUI：highlight 与 SQLite LIKE 折叠口径不一致（注释自称一致）
- cmd 工具：hmd 是 M2 占位（发布即可用性为零）
- TUI 定位评估：**保留**（有 20 个测试、GUI 没有的 replay/live、双击零依赖入口），但建议接入 explain 摘要并抽公共 helpers

---

## 八、安全评估摘要

**已逐点验证安全的项（负向发现同样有价值）**：
1. SQL 注入：全部参数化，SearchEvents 的 LIKE 正确转义 `\ % _` 且配 ESCAPE；hermes 外部库以 `mode=ro` 只读打开
2. Shell/命令注入：全程 exec.Command 直传 argv，无 cmd /c / sh -c；codex/pi prompt 走 stdin
3. 路径穿越：导出文件名经 sanitize；run 目录 = 数字 ID + 白名单 harness 名；无接受前端任意路径的绑定
4. GUI 绑定面克制：无通用写文件/执行绑定；前端资产全本地 embed，无远程内容
5. 前端 XSS：全部 25 处 innerHTML 插值过 esc() 或数值化（仅 class 属性 2 处例外，见 P2）
6. 解析健壮性：json.Unmarshal 进 struct 无类型混淆 panic 路径；流式限流；recover 捕获
7. 依赖：直接依赖仅 6 个，间接依赖未见已知未修复高风险版本（离线评估）

**风险链**（当前未闭合，三环任何一环失守即全机 RCE）：
不可信内容渲染（WebView2）→ 前端任一未来 esc 遗漏 → JS 调 StartCompare → claude bypassPermissions。

**加固优先级**：① 降 claude 权限模式（一行改动，收益最大）② app.js 转义规则固化为评审规则 + 恶意会话样本 fuzz ③ 导出元数据转义 + 密钥脱敏选项 ④ 采集超长行改跳过 ⑤ 开源前清 git 历史。

---

## 九、测试与质量保障评估

**结构性缺口（按影响排序）**：
1. **internal/store 零直接单测**（无 store_test.go；run_test.go 仅 4 个）——全项目数据地基，escapeLike/InsertEvents 幂等重放/ListSessions 排序全部零覆盖，覆盖率 36.3% 全场最低
2. **hermes 采集器零集成测试**——五家中唯一无 SQLite fixture 测试的，P0-2 正是从这里漏网
3. **三家 JSONL 增量续读零测试**——incremental_test.go 只用 magpie 验证了追加与半行；codex 重复（P1-1）、截断碰撞（P1-3）正是增量正确性这一最难部分的空白
4. **killTree 零覆盖**（任何 build tag 下）——安全敏感度最高的 24 行代码；「真实子进程 + 假 harness」的测试金字塔中间层缺失
5. **GUI/前端零测试**——P0-1 与两条 GUI P1 全落在这两层；建议把 app.go 绑定层（注入临时库即可测）与前端纯函数（esc/kindClass/stepIssue 提取 + node:test）纳入测试
6. **integration 测试默认不执行**（build tag 隔离且无 CI）——四家真实链路只在开发者手动跑时生效
7. **-race 从未运行**——orchestrator 锁、kill goroutine 等并发代码未经 race 验证

**建议的最小补救**（性价比排序）：store 补 escapeLike 表驱动 + InsertEvents 重放幂等测试；hermes 采集器 SQLite fixture 测试；「全量 vs 增量采出相同事件集」一致性测试；假 harness（cmd.exe 脚本）覆盖超时/取消/立即退出三路径；建 CI（build+vet+test+coverage，Windows runner）。

---

## 十、工程亮点（审查中确认的优秀实践）

1. **source_key 幂等体系**：确定性键 + UNIQUE + OR IGNORE，重复采集/断点重跑/崩溃恢复不重不漏，并有全量重扫 inserted=0 的测试锁定
2. **半行处理教科书级**：`splitCompleteLines` 只消费完整行、offset 恰停换行后，配专测——正确解决 live 跟随读到写入中半行的经典问题
3. **单写者落库架构**：channel 串行化 + WAL + busy_timeout + SetMaxOpenConns(1) + 批量 drain，「result 先于终态写入」的顺序保证让 GUI 看到终态时结果必已就绪
4. **explain 纯函数单一实现**：O(n) 单遍配对（map+FIFO），GUI/导出/对比台/预览四处共用零漂移；调用无结果/结果无调用不猜不报错，失败可见性设计贯穿
5. **usage 归一的专业性**：识破 codex `total_token_usage` 计数器陷阱取 last 增量（对照表：错法放大 1044 倍）；「费用未知 ≠ $0」贯穿到底
6. **NO_PROXY 净化闭环**：根因分析 + 对照实验数据（批次 #8 vs #9）+ 实现 + 三情形测试——历史坑修复的范本
7. **大文件纪律**：流式 Scanner（按实测最长行留余量）+ 事件数/字节双上限分批，172MB 文件/8.2MB 长行全程有实测注释，严格禁 ReadAll
8. **原文保真**：arguments 一律 json.RawMessage 透传，规避 Go JSON 的 HTML 转义与键排序陷阱，五家口径一致
9. **实测驱动文化**：注释大量「实测 N 条/最大 N KB」佐证，验收工具链（hmusagecheck/hmerrcheck/hmc4check）+ Python/Node 独立交叉验证（615 会话逐项一致、201667 事件判定零漏零假阳）
10. **前端转义纪律**：976 行手写 JS 的 25 处插值全过 esc()——最易失守的地方实际只漏 class 属性 2 处

---

## 十一、修复路线图

**第一批（P0 + 高收益 P1，约 1-2 天）**
1. P0-1 回放互递归（删 1 行 + 复用 jumpToStep）
2. P0-2 hermes 用量 upsert 通道 + 修正注释
3. P1-1 codex 增量预扫
4. P1-4 Close 等待收尾 + 启动清理残留 running
5. P1-7 claudeBlock 补 IsError 字段（一字段之差）
6. P1-8 run_events 补 parent_id 列（一列之差）
7. P1-9/P1-10 GUI 令牌守卫 + metaCache 传参（各约 10 行）
8. P1-11 降 claude 权限模式（一行）

**第二批（其余 P1 + 高危 P2，约 2-3 天）**
9. P1-2 全量末行补采；P1-5 taskkill 绝对路径+重试；P1-6 stdin 回退/文档化
10. 水位线内容指纹（P1-3）；schema user_version（为 P0-2/P1-8 的库变更铺路）
11. kill 后管道强关兜底；采集超长行改跳过；flush 粘滞错误修复

**第三批（结构与测试，持续）**
12. store 单测补齐 + hermes fixture 测试 + 全量/增量一致性测试 + 假 harness 中间层测试
13. CI（Windows runner：build/vet/test/coverage）+ 一次性 race 跑
14. GetEventsAfter 增量接口 + 前端 Map 索引（P1-12）
15. 开源前：git filter-repo 清历史用户名；导出元数据转义；viewutil 去重

---

*审查方法附注：6 路分域审查各自完整通读所辖文件（含全部测试与 fixtures）；标注〔实证〕的发现均以临时测试或本机真实数据（hm.db 180,762 事件、hermes state.db、claude/codex/pi 真实会话文件）验证；P0 与全部 P1 由主审独立复核代码路径，可复现的（回放递归、TUI 越界）均做了独立复现实验。工作区已恢复干净，基线全绿。*
