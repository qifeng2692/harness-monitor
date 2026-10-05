# 大模型为什么"会"调用工具？——机制说明（真实数据还原）

> 本文用本机真实数据（Hermes request_dump、state.db、Codex rollout）还原「工具调用」的完整机制。
> 一句话：**模型不是天生知道怎么调工具。每次请求 harness 都会递一份「工具菜单」；模型被训练过，
> 需要动手时按固定格式写一张「订单」；harness 读到订单去执行，再把结果放回对话。循环直到不再点单。**

## 全流程四步

### 第 1 步：每次请求，「菜单」先递上（harness 做）

Hermes 发给模型的真实 API 请求（`~/AppData/Local/hermes/sessions/request_dump_*.json`）里，
`body.tools` 字段列着 **26 个工具的完整说明书**。每个工具三条信息：

| 信息 | 例子（terminal 工具） |
|---|---|
| 名字 | `terminal` |
| 用途说明（什么时候该用） | "Execute shell commands. The host OS, shell... write commands for THAT platform..." |
| 参数格式（点单怎么填） | JSON Schema：`command` 必填、`timeout` 可选、`background` 布尔… |

这就是「菜单」。**每一次请求，菜单都在里面**（模型没有记忆，菜单是随请求附带的）。

### 第 2 步：模型写「订单」（模型做）

模型自己不执行任何东西——它只会输出文字。当它判断任务需要跑命令/读文件，就按训练时
学会的格式输出一段结构化文字（不是给人看的，是给 harness 读的）。本机 Hermes 数据库
`messages.tool_calls` 字段里的真实记录：

```json
[{
  "id": "call_227e0037686b4702b715ad62",
  "type": "function",
  "function": { "name": "kanban_show", "arguments": "{}" }
}]
```

另一条真实记录（一次点两单，并行）：

```json
[
  { "function": { "name": "kanban_heartbeat", "arguments": "{\"note\":\"开始开发...\"}" } },
  { "function": { "name": "terminal", "arguments": "{\"command\":\"python --version && pwd\"}" } }
]
```

### 第 3 步：harness 执行（harness 做）

harness 读到订单：查菜单 → 找到对应工具 → 在本机真正执行 → 抓住输出。
订单里带 `call_id`（流水号），用来配对"哪张订单对应哪个结果"——并行点多单也不会串。

### 第 4 步：结果回传，循环（harness → 模型）

执行结果以「tool」身份塞回对话（Codex rollout 真实记录）：

```
function_call:        name=exec_command  arguments={"cmd": "echo ..."}  call_id=call_00_ehlM...
function_call_output: call_id=call_00_ehlM...  output="Chunk ID: 8aa440 ... Process exited with code 0 ..."
```

模型看到结果继续判断：够了吗？
- 够 → 写最终回答（不再点单，本轮结束）
- 不够 → 再点一单（回到第 2 步）

## 回答「为什么知道调用哪个工具」

1. **知道有哪些工具**：菜单每次都在请求里（harness 塞的）
2. **知道每个工具干嘛**：菜单里每个工具的「用途说明」就是选择依据——
   模型把当前需要和说明书做匹配："我要跑命令" ↔ "terminal: Execute shell commands" ✓
3. **知道怎么点单**：训练出来的。模型在训练阶段见过海量「任务→订单」样本，
   学会了这种输出格式，以及「什么时候该点单、什么时候直接回答」
4. **模型不执行任何东西**：它只写字。真正的执行、权限控制、错误处理全在 harness 手里。
   `call_id` 就是 harness 用来配对「订单 ↔ 结果」的机制

## 两个佐证细节

- **工具幻觉可被抓出**：模型若编一个菜单里没有的工具名，harness 查不到 → 拒绝/报错
- **可并行点单**：一次响应可含多个工具调用（本机有真实记录），harness 并行执行，
  各自带 `call_id` 回来，互不混淆

## 各家 harness 的对应实现

| Harness | 菜单（请求侧） | 订单（响应侧） |
|---|---|---|
| Hermes / OpenAI 兼容 | `tools` 参数 | `tool_calls` 字段 |
| Anthropic Claude | `tools` 参数 | `tool_use` 内容块 |
| Codex | tools（内置） | `function_call` / `function_call_output` |
| pi | 扩展注册的工具 | TypeScript 扩展 API 的回调 |

**结论**：所谓"大模型会调用工具"，是「模型（会按格式写订单）+ harness（递菜单、执行订单、回传结果）」
双方按协议配合的结果。任何一半单独存在都不成立。
