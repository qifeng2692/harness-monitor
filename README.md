# harness-monitor

监控多个 AI coding harness 的本地信息流——从窗口输入开始，到会话执行完毕，每一步（用户输入 / 模型推理 / 工具调用参数 / 工具结果）**内容级可见**。按 harness 独立、按会话独立。

## 目标 harness

Hermes · Codex · Claude Code · pi · magpie（网关用量）

## 开发环境（Windows / git-bash）

首次准备（Go 1.27.1 已装在 `C:\dev\go`）：

```bash
source scripts/env.sh   # 提供 Go PATH + 模块下载代理（必需）
```

构建与运行：

```bash
go build ./...
go run ./cmd/hm --help
go run ./cmd/hm collect --harness magpie   # 采集（幂等，可重复执行）
go run ./cmd/hm ls --harness magpie        # 会话列表
go run ./cmd/hm show magpie hermes         # 会话时间线（--full 显示内容全文）
go run ./cmd/hm                            # TUI（三级导航）
```

数据：`data/hm.db`（SQLite；环境变量 `HM_DATA_DIR` 可覆盖）。

## 文档

- `docs/DESIGN.md` — 设计文档 + 统一事件 kind 规范
- `docs/DATA-FORMATS.md` — 各数据源实测格式（开发解析器的依据）
