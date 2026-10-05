#!/usr/bin/env bash
# drive_chain.sh — 驱动 Hermes 看板任务链
# 用法: BUDGET=360 bash drive_chain.sh t_id1 t_id2 ...
# 轮询任务状态；只对 blocked 的任务做 promote+dispatch；预算用尽 exit 3；全部完成 exit 0
set -u
BUDGET=${BUDGET:-360}
DEADLINE=$(($(date +%s) + BUDGET))
IDS=("$@")

st_of() {
  hermes kanban show "$1" 2>/dev/null | grep -m1 "status:" | sed 's/.*status:[[:space:]]*//' | tr -d '\r'
}

echo "=== drive_chain: ${#IDS[@]} tasks, budget ${BUDGET}s ==="
while true; do
  now=$(date +%s)
  if [ "$now" -ge "$DEADLINE" ]; then
    echo "--- BUDGET EXHAUSTED ---"
    hermes kanban list 2>/dev/null | grep -E "t_(""$(IFS='|'; echo "${IDS[*]}")"")" | head -10
    exit 3
  fi

  alldone=true
  line=""
  for id in "${IDS[@]}"; do
    st=$(st_of "$id")
    line="$line $id=$st"
    case "$st" in
      done|archived) ;;
      *) alldone=false ;;
    esac
  done
  echo "[$(date +%H:%M:%S)]$line"

  if $alldone; then
    echo "--- ALL DONE ---"
    exit 0
  fi

  # 恢复 blocked（仅 blocked；绝不 promote 还在 todo 的孩子）
  for id in "${IDS[@]}"; do
    st=$(st_of "$id")
    if [ "$st" = "blocked" ]; then
      echo "[recover] $id blocked → promote + dispatch"
      hermes kanban promote "$id" 2>&1 | head -1
      hermes kanban dispatch 2>&1 | head -2
    fi
  done

  sleep 25
done
