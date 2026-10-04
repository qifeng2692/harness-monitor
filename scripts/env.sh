#!/usr/bin/env bash
# 开发环境（git-bash）：Go 工具链 + 模块下载代理
# 用法：source scripts/env.sh
export PATH="/c/dev/go/bin:$PATH"
export HTTPS_PROXY="http://127.0.0.1:7890"
export HTTP_PROXY="http://127.0.0.1:7890"
