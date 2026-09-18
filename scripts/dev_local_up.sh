#!/usr/bin/env bash
#
# 本地可测试产品的一键启动脚本。
#
# 作用：在一台机器上起一套**完全独立**的 Lumen——
#   独立的端口、独立的数据目录、独立的假模型进程——
#   不碰生产库、不需要任何真实密钥、不产生任何费用。
#
# 它做四件事：
#   1. 起本地假模型（scripts/dev_mock_model.py），顶替 DeepSeek；
#   2. 起 lumen-server，指向独立数据路径；
#   3. 注册一台本地设备，灌入一批假记录与一条 Agent 任务摘要；
#   4. 打印 URL、PID、日志路径，供人工验收使用。
#
# 用法：
#   bash scripts/dev_local_up.sh          # 启动
#   bash scripts/dev_local_up.sh down     # 停止
#
# 产物目录（可整体删除，不影响任何其它东西）：
#   .dev-local/          —— 数据、日志、PID
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEV_DIR="$ROOT_DIR/.dev-local"

SERVER_PORT="${LUMEN_DEV_PORT:-18801}"
MODEL_PORT="${LUMEN_DEV_MODEL_PORT:-18802}"
BASE_URL="http://127.0.0.1:$SERVER_PORT"
ENROLL_TOKEN="dev-local-enroll"
ADMIN_TOKEN="dev-local-admin"
DEVICE_NAME="dev-local-mac-01"

SERVER_LOG="$DEV_DIR/server.log"
MODEL_LOG="$DEV_DIR/model.log"
REQUESTS_LOG="$DEV_DIR/model_requests.jsonl"

PIDS=("$DEV_DIR/server.pid" "$DEV_DIR/model.pid")

# ---- 停止 ----

stop_all() {
    for pidfile in "${PIDS[@]}"; do
        if [ -f "$pidfile" ]; then
            pid="$(cat "$pidfile")"
            if kill -0 "$pid" 2>/dev/null; then
                kill "$pid" 2>/dev/null || true
                echo "已停止 PID ${pid}（$(basename "$pidfile")）"
            fi
            rm -f "$pidfile"
        fi
    done
}

if [ "${1:-up}" = "down" ]; then
    stop_all
    echo "本地实例已停止。数据仍保留在 ${DEV_DIR}（要清空就删掉这个目录）。"
    exit 0
fi

# ---- 启动 ----

mkdir -p "$DEV_DIR/data" "$DEV_DIR/data/backups"
: > "$REQUESTS_LOG"

# 上次没停干净时先收尸，否则端口会冲突。
stop_all > /dev/null

# 每次 up 都从干净数据开始：验收需要的是**可复现的固定数据**，
# 而不是上次留下的状态。要保留现场就别重跑 up，用 down 停。
# 单设备上限（1 台）也要求这样：重复注册会被拒。
rm -f "$DEV_DIR/data/lumen.db" "$DEV_DIR/data/lumen.db-wal" "$DEV_DIR/data/lumen.db-shm"

echo "== 准备 =="
cd "$ROOT_DIR/server"
go build -o "$DEV_DIR/lumen-server" ./cmd/lumen-server
echo "服务端已编译: $DEV_DIR/lumen-server"

echo
echo "== 1. 启动本地假模型（顶替 DeepSeek）=="
# 用今天作为检索日期：界面上问"今天"就会查到下面灌入的假数据。
python3 "$ROOT_DIR/scripts/dev_mock_model.py" \
    --port "$MODEL_PORT" --log "$REQUESTS_LOG" \
    > "$MODEL_LOG" 2>&1 &
echo $! > "$DEV_DIR/model.pid"
sleep 1
if ! kill -0 "$(cat "$DEV_DIR/model.pid")" 2>/dev/null; then
    echo "假模型启动失败，见 $MODEL_LOG" >&2
    exit 1
fi
echo "假模型 PID: $(cat "$DEV_DIR/model.pid")  日志: ${MODEL_LOG}"

echo
echo "== 2. 启动 lumen-server（独立数据路径）=="
LUMEN_LISTEN_ADDR="127.0.0.1:$SERVER_PORT" \
LUMEN_DB_PATH="$DEV_DIR/data/lumen.db" \
LUMEN_BACKUP_DIR="$DEV_DIR/data/backups" \
LUMEN_ENROLLMENT_TOKEN="$ENROLL_TOKEN" \
LUMEN_ADMIN_TOKEN="$ADMIN_TOKEN" \
LUMEN_DEEPSEEK_BASE_URL="http://127.0.0.1:$MODEL_PORT" \
LUMEN_DEEPSEEK_API_KEY="dev-local-key" \
LUMEN_DEEPSEEK_MODEL="dev-mock" \
LUMEN_ASSISTANT_NAME="${LUMEN_DEV_ASSISTANT_NAME:-小灯}" \
LUMEN_ASSISTANT_ROLE="个人助手/伙伴" \
LUMEN_OWNER_DISPLAY_NAME="liang" \
LUMEN_LOG_LEVEL="info" \
"$DEV_DIR/lumen-server" > "$SERVER_LOG" 2>&1 &
echo $! > "$DEV_DIR/server.pid"
sleep 1
if ! kill -0 "$(cat "$DEV_DIR/server.pid")" 2>/dev/null; then
    echo "服务端启动失败，见 $SERVER_LOG" >&2
    exit 1
fi
echo "服务端 PID: $(cat "$DEV_DIR/server.pid")  日志: $SERVER_LOG"

# 等健康检查通过；服务端在第一次请求时才会建库，因此这里重试。
for _ in $(seq 1 30); do
    if curl -sf "$BASE_URL/api/v1/healthz" > /dev/null 2>&1; then
        break
    fi
    sleep 0.3
done
if ! curl -sf "$BASE_URL/api/v1/healthz" > /dev/null 2>&1; then
    echo "服务端未就绪，见 $SERVER_LOG" >&2
    exit 1
fi
echo "健康检查通过: $BASE_URL/api/v1/healthz"

echo
echo "== 3. 灌入假记录 =="
REGISTER="$(curl -s -X POST "$BASE_URL/api/v1/devices/register" \
    -H 'Content-Type: application/json' \
    -d "{\"enrollment_token\":\"$ENROLL_TOKEN\",\"device_name\":\"$DEVICE_NAME\"}")"
DEVICE_TOKEN="$(printf '%s' "$REGISTER" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("device_token",""))')"
# 用服务端返回的 device_id，而不是我们自己猜的名字：事件里的 device_id
# 必须与凭证一致，否则整批会被拒（device_mismatch）。
ASSIGNED_DEVICE_ID="$(printf '%s' "$REGISTER" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("device_id",""))')"
if [ -z "$DEVICE_TOKEN" ] || [ -z "$ASSIGNED_DEVICE_ID" ]; then
    echo "设备注册失败: $REGISTER" >&2
    exit 1
fi
echo "设备已注册: $ASSIGNED_DEVICE_ID"

python3 - "$DEV_DIR" "$ASSIGNED_DEVICE_ID" <<'PYEOF'
"""构造一批贴近真实的假记录：两次工作时段 + 一条 Agent 任务摘要。

时间落在"今天"，这样问"今天"就能查到；同时第二段紧跟在第一段之后，
用于验证多段记录合并区间（最小开始 ~ 最大结束）。
"""
import datetime as dt
import json
import os
import sys
import uuid

work, device_id = sys.argv[1], sys.argv[2]
ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"


def ulid() -> str:
    raw = uuid.uuid4().int
    return "".join(ALPHABET[(raw >> (5 * i)) & 0x1F] for i in range(25, -1, -1))


now = dt.datetime.now().astimezone().replace(microsecond=0)
morning = now.replace(hour=9, minute=10, second=0)
afternoon = now.replace(hour=14, minute=5, second=0)


def ts(moment: dt.datetime) -> str:
    return moment.isoformat()


events = [
    {"id": ulid(), "device_id": device_id, "type": "window.activity",
     "timestamp": ts(morning), "privacy": "P0",
     "context": {"app": "ZCode", "project": "lumen"},
     "data": {"duration_seconds": 5400}},
    {"id": ulid(), "device_id": device_id, "type": "window.activity",
     "timestamp": ts(afternoon), "privacy": "P0",
     "context": {"app": "ZCode", "project": "lumen"},
     "data": {"duration_seconds": 3300}},
    {"id": ulid(), "device_id": device_id, "type": "git.activity",
     "timestamp": ts(afternoon + dt.timedelta(minutes=20)), "privacy": "P1",
     "context": {"repo": "lumen", "project": "lumen"},
     "data": {"kind": "commit", "branch": "main",
              "head_commit": "b" * 40,
              "commit_message": "feat: 接通任务摘要数据源", "changed_files_count": 6}},
    {"id": ulid(), "device_id": device_id, "type": "window.activity",
     "timestamp": ts(now.replace(hour=16, minute=40, second=0)), "privacy": "P0",
     "context": {"app": "Safari", "bundle_id": "com.apple.Safari"},
     "data": {"duration_seconds": 1500}},
]

task = {
    "id": ulid(), "device_id": device_id, "type": "agent.task_summary",
    "timestamp": ts(afternoon + dt.timedelta(hours=1)), "privacy": "P1",
    "context": {"app": "ZCode", "project": "lumen"},
    "data": {
        "schema_version": 1,
        "task_id": "dev-local-task-001",
        "title": "接通 Agent 任务摘要数据源",
        "status": "done",
        "outcomes": ["新增 agent.task_summary 事件类型", "补齐字段 allowlist 与幂等入库"],
        "open_loops": ["尚未接入 ZCode 自动上报"],
        "source_agent": "zcode-cli",
        "source_session_id": "dev-local-sess-001",
        "privacy_mode": "metadata_only",
    },
}

batch_id = ulid()
with open(os.path.join(work, "batch.json"), "w", encoding="utf-8") as fh:
    json.dump({"device_id": device_id, "batch_id": batch_id,
               "sent_at": dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
               "events": events + [task]}, fh)
print(f"已生成 {len(events) + 1} 条事件（含 1 条 Agent 任务摘要）")
PYEOF

RESP="$(curl -s -X POST "$BASE_URL/api/v1/events/batch" \
    -H "Authorization: Bearer $DEVICE_TOKEN" \
    -H 'Content-Type: application/json' \
    --data-binary "@$DEV_DIR/batch.json")"
printf '%s' "$RESP" | python3 -c '
import json, sys
d = json.load(sys.stdin)
accepted = sum(1 for r in d["results"] if r["status"] == "accepted")
rejected = [r for r in d["results"] if r["status"] != "accepted"]
print(f"入库 {accepted} 条，被拒 {len(rejected)} 条")
for r in rejected:
    print("  被拒:", r)
'
# 令牌只用于刚才那次调用，不落盘。
unset DEVICE_TOKEN

echo
echo "== 4. 重算 Session（让记录变成可查询的工作时段）=="
TODAY="$(date +%Y-%m-%d)"
curl -s -X POST "$BASE_URL/api/v1/sessions/rebuild?date=$TODAY" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H 'Content-Type: application/json' \
    -d '{}' | python3 -c '
import json, sys
d = json.load(sys.stdin)
days = d.get("days") or []
total = sum(int(day.get("session_count", 0)) for day in days)
algorithm = d.get("algorithm", "?")
print("重算完成：规则 " + str(algorithm) + "，" + str(len(days)) + " 天共 " + str(total) + " 个时段")
'

echo
echo "== 已就绪 =="
echo "地址:   $BASE_URL"
echo "服务端: PID $(cat "$DEV_DIR/server.pid")   日志 $SERVER_LOG"
echo "假模型: PID $(cat "$DEV_DIR/model.pid")   日志 $MODEL_LOG"
echo "数据:   $DEV_DIR/data/lumen.db"
echo "模型请求留档: $REQUESTS_LOG"
echo
echo "提交流程示例（管理员令牌仅本地）："
echo "  curl -s -X POST $BASE_URL/api/v1/ask -H 'Authorization: Bearer $ADMIN_TOKEN' \\"
echo "       -H 'Content-Type: application/json' -d '{\"text\":\"我今天都忙了些什么？\"}'"
echo
echo "停止: bash scripts/dev_local_up.sh down"
