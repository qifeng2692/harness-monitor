# harness-monitor

监控多个 AI coding harness 的本地信息流——从窗口输入开始，到会话执行完毕，每一步（用户输入 / 模型推理 / 工具调用参数 / 工具结果）**内容级可见**。按 harness 独立、按会话独立。

## 目标 harness

Hermes · Codex · Claude Code · pi · magpie（网关用量）

## 使用

### 快速开始

双击仓库根目录的 **`harness-monitor.exe`**（自动增量采集全部 harness → 直接进入 TUI）。

也可以创建桌面快捷方式指向它（工作目录设为 `G:\projects\harness-monitor`）。

### TUI 操作（三级导航：harness → 会话 → 时间线）

```
harness 层： ↑↓ 选 harness → enter 进入；f 搜索（限定当前 harness）
会话层：    ↑↓ 选会话 → enter 进入；F 前缀过滤；esc 返回
时间线层：  ↑↓ 滚动步骤 → enter 展开全文
            space 进入回放（n/→ 下一步 · p/← 上一步 · esc 退出）
            l 切换 live 跟随（每 2s 增量采集，自动滚到底部）
任意层：    / 搜索（搜所有内容原文，enter 跳转定位）· ? 帮助 · r 刷新 · q 退出
```

### CLI 快捷命令

```bash
hm collect --harness codex --incremental   # 增量采集（幂等；不带 --incremental 为全量回填）
hm ls --harness hermes                     # 会话列表
hm show codex <session-id>                 # 会话时间线
hm show codex <session-id> --full          # 全文模式
hm version
```

数据：`data/hm.db`（SQLite；环境变量 `HM_DATA_DIR` 覆盖数据目录）。

## 开发环境（Windows / git-bash）

Go 1.27.1 已装在 `C:\dev\go`。首次准备：

```bash
source scripts/env.sh   # Go PATH + 模块下载代理（必需）
```

构建与测试：

```bash
go build ./... && go vet ./...
go test ./... -count=1
go build -o bin/hm.exe ./cmd/hm
```

## 文档

- `docs/DESIGN.md` — 设计文档 + 统一事件 kind 规范
- `docs/DATA-FORMATS.md` — 各数据源实测格式（开发解析器的依据）
