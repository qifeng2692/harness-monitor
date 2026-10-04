@echo off
rem harness-monitor 启动器：采集最新事件 + 打开 TUI
cd /d %~dp0..
echo [1/2] 采集最新事件（增量，幂等）...
bin\hm.exe collect --harness hermes --incremental >nul 2>&1
bin\hm.exe collect --harness codex --incremental >nul 2>&1
bin\hm.exe collect --harness claude --incremental >nul 2>&1
bin\hm.exe collect --harness pi --incremental >nul 2>&1
bin\hm.exe collect --harness magpie >nul 2>&1
echo [2/2] 启动 TUI（q 退出）...
bin\hm.exe tui
