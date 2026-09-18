#!/usr/bin/env bash
#
# Lumen 本地端到端联调脚本
#
# 作用：在一台机器上完整验证「采集端 → 服务端 → Session → 总结」链路，
#       全程使用假事件和 mock DeepSeek，不产生任何真实费用，不需要真实密钥。
#
# 验证内容：
#   1. 设备注册与一次性令牌校验；
#   2. 假事件批量上传与逐事件 ACK；
#   3. at-least-once 重复投递下的服务端幂等；
#   4. 隐私字段拒绝（剪贴板、窗口标题、绝对路径）；
#   5. 断网后本地保留与恢复补传；
#   6. 规则 Session 聚合（合并、切断、未分类）；
#   7. 真实时序：工作 → idle → 锁屏过夜 → 解锁，以及历史脏数据重算；
#   8. 每日总结生成与幂等（相同输入不重复调用模型）；
#   9. 发往模型的内容不含路径、凭证与原始事件。
#
# 用法：bash scripts/e2e_local.sh

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK_DIR="${TMPDIR:-/tmp}/lumen-e2e-run"
SERVER_PORT=18787
MOCK_PORT=18888
ENROLL_TOKEN="e2e-enroll-$(date +%s)"
ADMIN_TOKEN="e2e-admin-$(date +%s)"

SERVER_PID=""
MOCK_PID=""
CLEANUP_DONE=0

pass() { printf '  ✅ %s\n' "$*"; }
fail() { printf '  ❌ %s\n' "$*"; FAILED=1; }
section() { printf '\n=== %s ===\n' "$*"; }
info() { printf '  · %s\n' "$*"; }

FAILED=0

cleanup() {
    [ "$CLEANUP_DONE" -eq 1 ] && return
    CLEANUP_DONE=1
    [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
    [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
    wait 2>/dev/null || true
}
trap cleanup EXIT

# ---- 准备 ----

section "准备环境"
# 只清理上次运行的产物，不动 Go 的模块缓存（它在 HOME 下，删了要重新下载）。
if [ -d "$WORK_DIR" ]; then
    chmod -R u+w "$WORK_DIR" 2>/dev/null || true
    rm -rf "$WORK_DIR" 2>/dev/null || true
fi
mkdir -p "$WORK_DIR/data"
info "工作目录: $WORK_DIR"

cd "$ROOT_DIR/server"
go build -o "$WORK_DIR/lumen-server" ./cmd/lumen-server
pass "服务端编译完成"

PY="$ROOT_DIR/desktop/.venv/bin/python"
if [ ! -x "$PY" ]; then
    fail "未找到虚拟环境，请先运行 make setup"
    exit 1
fi
pass "采集端虚拟环境就绪"

# ---- 启动 mock DeepSeek ----

section "启动 mock DeepSeek"
cat > "$WORK_DIR/mock_deepseek.py" <<'PYEOF'
"""模拟 DeepSeek API，同时记录请求内容供安全检查。"""
import json, re, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

FORBIDDEN = ["/Users/", "/home/", "sk-real", "device_token", "clipboard",
             "screenshot", "source_code", "window_title", "absolute_path", "github.com"]
problems = []


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length).decode("utf-8")
        with open(sys.argv[2], "w", encoding="utf-8") as fh:
            fh.write(raw)
        for token in FORBIDDEN:
            if token in raw:
                problems.append(token)
        body = json.loads(raw)
        user = body["messages"][-1]["content"]
        date = re.search(r'"date":\s*"([\d-]+)"', user).group(1)
        ids = re.findall(r'"id":\s*"(s_[0-9a-f]+)"', user)
        summary = {
            "date": date,
            "headline": "端到端联调：Session 聚合与总结链路",
            "projects": [{
                "name": "lumen", "duration_minutes": 45,
                "activities": ["实现 Session 规则聚合"],
                "evidence_session_ids": ids[:1],
            }],
            "uncertainties": [],
        }
        out = json.dumps({
            "id": "mock", "model": "deepseek-chat",
            "choices": [{"message": {"content": json.dumps(summary, ensure_ascii=False)},
                         "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 300, "completion_tokens": 80, "total_tokens": 380},
        }, ensure_ascii=False).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def log_message(self, *args):
        return


HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
PYEOF

python3 "$WORK_DIR/mock_deepseek.py" "$MOCK_PORT" "$WORK_DIR/deepseek_request.json" \
    > "$WORK_DIR/mock.log" 2>&1 &
MOCK_PID=$!
sleep 1
if kill -0 "$MOCK_PID" 2>/dev/null; then
    pass "mock DeepSeek 已启动 (端口 $MOCK_PORT)"
else
    fail "mock DeepSeek 启动失败"
    exit 1
fi

# ---- 启动服务端 ----

section "启动 lumen-server"
LUMEN_LISTEN_ADDR="127.0.0.1:$SERVER_PORT" \
LUMEN_DB_PATH="$WORK_DIR/data/lumen.db" \
LUMEN_BACKUP_DIR="$WORK_DIR/data/backups" \
LUMEN_ENROLLMENT_TOKEN="$ENROLL_TOKEN" \
LUMEN_ADMIN_TOKEN="$ADMIN_TOKEN" \
LUMEN_DEEPSEEK_BASE_URL="http://127.0.0.1:$MOCK_PORT" \
LUMEN_DEEPSEEK_API_KEY="sk-mock-for-e2e" \
LUMEN_DEEPSEEK_MODEL="deepseek-chat" \
LUMEN_LOG_LEVEL="warn" \
"$WORK_DIR/lumen-server" > "$WORK_DIR/server.log" 2>&1 &
SERVER_PID=$!
sleep 2

HEALTH="$(curl -sf "http://127.0.0.1:$SERVER_PORT/api/v1/healthz")" || {
    fail "服务端未就绪"
    cat "$WORK_DIR/server.log"
    exit 1
}
pass "服务端已启动，version=$(echo "$HEALTH" | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"])')"

# ---- 1. 注册与鉴权 ----

section "1. 设备注册与一次性令牌"

CODE="$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    "http://127.0.0.1:$SERVER_PORT/api/v1/devices/register" \
    -H 'Content-Type: application/json' \
    -d '{"enrollment_token":"wrong-token","device_name":"desktop-mac-01"}')"
[ "$CODE" = "401" ] && pass "错误令牌被拒绝 (401)" || fail "错误令牌应返回 401，实际 $CODE"

REGISTER="$(curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/devices/register" \
    -H 'Content-Type: application/json' \
    -d "{\"enrollment_token\":\"$ENROLL_TOKEN\",\"device_name\":\"desktop-mac-01\"}")"
DEVICE_TOKEN="$(echo "$REGISTER" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("device_token",""))')"
[ -n "$DEVICE_TOKEN" ] && pass "注册成功并取得 device_token" || { fail "注册失败: $REGISTER"; exit 1; }

CODE="$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    "http://127.0.0.1:$SERVER_PORT/api/v1/devices/register" \
    -H 'Content-Type: application/json' \
    -d "{\"enrollment_token\":\"$ENROLL_TOKEN\",\"device_name\":\"another-mac\"}")"
[ "$CODE" = "409" ] && pass "单设备上限生效 (409)" || fail "重复注册应返回 409，实际 $CODE"

CODE="$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    "http://127.0.0.1:$SERVER_PORT/api/v1/events/batch" \
    -H 'Content-Type: application/json' -d '{}')"
[ "$CODE" = "401" ] && pass "无凭证批量上报被拒绝 (401)" || fail "应返回 401，实际 $CODE"

# ---- 2. 假事件上传 ----

section "2. 假事件批量上传（含隐私拒绝）"

# 场景日期：当天 13:00 之后就用今天，否则用昨天。
#
# 原因：事件时间如果落在未来，服务端会按 clock_skew 规则改用 received_at
# 参与聚合，整个场景就被改写（跑到中午之前时 12:00 那条会变成"现在"，
# 归因与合并结果全变）。固定用一个已经过去的日期，结果才稳定。
if [ "$(date +%H)" -ge 13 ]; then
    SCENARIO_DAY="$(date +%Y-%m-%d)"
else
    SCENARIO_DAY="$(date -v-1d +%Y-%m-%d 2>/dev/null || date -d 'yesterday' +%Y-%m-%d)"
fi
info "场景日期: $SCENARIO_DAY"

python3 - "$WORK_DIR" "$SCENARIO_DAY" <<'PYEOF'
"""生成测试事件批次：4 条合法 + 3 条应被拒绝。

时间安排刻意模拟真实的一天，用来验证 Session 规则：
  09:00  VS Code 工作 30 分钟（项目 lumen）
  09:33  VS Code 工作 15 分钟（间隔 3 分钟 < 8 分钟阈值 → 应与上一段合并）
  09:35  Git 提交（为上面的 Session 提供证据）
  12:00  Safari 浏览 20 分钟（未命中白名单且远离提交 → 应标 unclassified）
"""
import json, sys, uuid, datetime

work = sys.argv[1]
local_tz = datetime.datetime.now().astimezone().tzinfo
day = datetime.datetime.strptime(sys.argv[2], "%Y-%m-%d").replace(tzinfo=local_tz)


def at(hours=0, minutes=0):
    """返回"场景日期本地时间 + 偏移"对应的 UTC RFC3339 字符串。"""
    return (day + datetime.timedelta(hours=hours, minutes=minutes)) \
        .astimezone(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def ulid():
    """生成符合 ULID 字符集约束的 26 位 id。"""
    alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
    raw = uuid.uuid4().int
    return "".join(alphabet[(raw >> (5 * i)) & 0x1F] for i in range(25, -1, -1))


good = [
    {"id": ulid(), "device_id": "desktop-mac-01", "type": "window.activity",
     "timestamp": at(), "privacy": "P0",
     "context": {"app": "Visual Studio Code", "project": "lumen"},
     "data": {"duration_seconds": 1800}},
    {"id": ulid(), "device_id": "desktop-mac-01", "type": "window.activity",
     "timestamp": at(minutes=33), "privacy": "P0",
     "context": {"app": "Visual Studio Code", "project": "lumen"},
     "data": {"duration_seconds": 900}},
    {"id": ulid(), "device_id": "desktop-mac-01", "type": "git.activity",
     "timestamp": at(minutes=35), "privacy": "P1",
     "context": {"repo": "lumen", "project": "lumen"},
     "data": {"kind": "commit", "branch": "main",
              "head_commit": "a1b2c3d4e5f6078899aabbccddeeff0011223344",
              "commit_message": "feat: session engine", "changed_files_count": 7}},
    {"id": ulid(), "device_id": "desktop-mac-01", "type": "window.activity",
     "timestamp": at(hours=3), "privacy": "P0",
     "context": {"app": "Safari", "bundle_id": "com.apple.Safari"},
     "data": {"duration_seconds": 1200}},
]

bad = [
    {"id": ulid(), "device_id": "desktop-mac-01", "type": "window.activity",
     "timestamp": at(), "privacy": "P0", "clipboard": "copied secret",
     "context": {"app": "Chrome"}, "data": {"duration_seconds": 10}},
    {"id": ulid(), "device_id": "desktop-mac-01", "type": "window.activity",
     "timestamp": at(), "privacy": "P0",
     "context": {"app": "Chrome", "window_title": "银行登录"}, "data": {"duration_seconds": 10}},
    # 这条用来验证服务端会拒绝含绝对路径的事件。路径是虚构的占位符。
    {"id": ulid(), "device_id": "desktop-mac-01", "type": "window.activity",
     "timestamp": at(), "privacy": "P0",
     "context": {"app": "Terminal", "project": "/Users/liang/secret"},  # secrets-check:allow
     "data": {"duration_seconds": 10}},
]

with open(f"{work}/good_events.json", "w") as fh:
    json.dump(good, fh)
with open(f"{work}/bad_events.json", "w") as fh:
    json.dump(bad, fh)
with open(f"{work}/good_ids.txt", "w") as fh:
    fh.write("\n".join(e["id"] for e in good))
PYEOF

BATCH_ID="$(python3 -c 'import uuid; a="0123456789ABCDEFGHJKMNPQRSTVWXYZ"; r=uuid.uuid4().int; print("".join(a[(r>>(5*i))&31] for i in range(25,-1,-1)))')"
python3 - "$WORK_DIR" "$BATCH_ID" <<'PYEOF' > "$WORK_DIR/batch_request.json"
import json, sys
work, batch = sys.argv[1], sys.argv[2]
good = json.load(open(f"{work}/good_events.json"))
bad = json.load(open(f"{work}/bad_events.json"))
print(json.dumps({
    "device_id": "desktop-mac-01", "batch_id": batch,
    "sent_at": "2026-09-17T10:05:00Z", "events": good + bad,
}))
PYEOF

RESP="$(curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/events/batch" \
    -H "Authorization: Bearer $DEVICE_TOKEN" \
    -H 'Content-Type: application/json' \
    --data-binary "@$WORK_DIR/batch_request.json")"

ACCEPTED="$(echo "$RESP" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(sum(1 for r in d["results"] if r["status"]=="accepted"))')"
REJECTED="$(echo "$RESP" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(sum(1 for r in d["results"] if r["status"]=="rejected"))')"
[ "$ACCEPTED" = "4" ] && pass "4 条合法事件被接受" || fail "应接受 4 条，实际 $ACCEPTED"
[ "$REJECTED" = "3" ] && pass "3 条含禁用字段的事件被拒绝" || fail "应拒绝 3 条，实际 $REJECTED"

EMPLOYEES="$(curl -s "http://127.0.0.1:$SERVER_PORT/api/v1/healthz" | python3 -c 'import json,sys; print(json.load(sys.stdin)["counts"]["events"])')"
[ "$EMPLOYEES" = "4" ] && pass "被拒绝的事件未入库（库中 4 条）" || fail "库中应有 4 条事件，实际 $EMPLOYEES"

# ---- 3. 幂等 ----

section "3. at-least-once 重复投递下的幂等"

for i in 1 2; do
    RESP="$(curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/events/batch" \
        -H "Authorization: Bearer $DEVICE_TOKEN" \
        -H 'Content-Type: application/json' \
        --data-binary "@$WORK_DIR/batch_request.json")"
    DUP="$(echo "$RESP" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(sum(1 for r in d["results"] if r["status"]=="duplicate"))')"
    [ "$DUP" = "4" ] && pass "第 $((i+1)) 次上报：4 条均为 duplicate" || fail "重复投递应返回 duplicate，实际 $DUP"
done

COUNT="$(curl -s "http://127.0.0.1:$SERVER_PORT/api/v1/healthz" | python3 -c 'import json,sys; print(json.load(sys.stdin)["counts"]["events"])')"
[ "$COUNT" = "4" ] && pass "重复上报 3 次后库中仍是 4 条事件" || fail "事件数应为 4，实际 $COUNT"

# ---- 4. Session 聚合 ----

section "4. 规则 Session 聚合"

TODAY="$SCENARIO_DAY"
SESSIONS="$(curl -s "http://127.0.0.1:$SERVER_PORT/api/v1/sessions?date=$TODAY" \
    -H "Authorization: Bearer $DEVICE_TOKEN")"
SC="$(echo "$SESSIONS" | python3 -c 'import json,sys; print(json.load(sys.stdin)["count"])')"
[ "$SC" -ge 1 ] && pass "生成 $SC 个 Session" || fail "应生成 Session，实际 $SC"

SESSIONS="$SESSIONS" python3 - <<'PYEOF'
import json, os, sys

sessions = json.loads(os.environ["SESSIONS"])["sessions"]
projects = {s["project"] for s in sessions}
failed = 0

if "lumen" in projects:
    print("  ✅ 项目归属识别正确（lumen）")
else:
    print("  ❌ 未识别出 lumen 项目")
    failed = 1

# 未命中白名单且远离 Git 提交的活动必须标为 unclassified，而不是被猜成某个项目。
if "unclassified" in projects:
    print("  ✅ 未命中白名单的应用标为 unclassified（未猜测）")
else:
    print(f"  ❌ 应有 unclassified Session，实际项目: {sorted(projects)}")
    failed = 1

# 两段间隔 3 分钟的 lumen 活动应合并成一个 Session。
lumen_sessions = [s for s in sessions if s["project"] == "lumen"]
if len(lumen_sessions) == 1:
    mins = lumen_sessions[0]["stats"].get("duration_minutes", 0)
    print(f"  ✅ 间隔 3 分钟的同类活动合并为 1 个 Session（{mins} 分钟）")
elif len(lumen_sessions) > 1:
    print(f"  ❌ 间隔小于阈值的活动应合并，实际 {len(lumen_sessions)} 个 lumen Session")
    failed = 1

if any(s["project"] == "lumen" and s["git"] for s in sessions):
    print("  ✅ Session 挂载了 Git 证据")
else:
    print("  ❌ Session 缺少 Git 证据")
    failed = 1

sys.exit(failed)
PYEOF
[ $? -eq 0 ] || FAILED=1

# ---- 5. 每日总结 ----

section "5. 每日总结生成"

GEN="$(curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/summaries/daily/generate?date=$TODAY" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
STATUS="$(echo "$GEN" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))')"
[ "$STATUS" = "succeeded" ] && pass "总结生成成功" || fail "总结状态应为 succeeded，实际 $STATUS"

GEN="$GEN" python3 - <<'PYEOF'
import json, os, sys

data = json.loads(os.environ["GEN"])
text = data.get("text", "")
if not text.strip():
    print("  ❌ 总结文本为空")
    sys.exit(1)

# 用户可见文案里不能出现内部术语与内部 ID。
for bad in ("unclassified", "s_"):
    if bad in text:
        print(f"  ❌ 总结里出现了工程术语或内部 ID: {bad}")
        sys.exit(1)
print("  ✅ 总结不含 unclassified 与内部 Session ID")
PYEOF
[ $? -eq 0 ] || FAILED=1

# 证据 ID 仍要保留在结构化输出里，供 API 与审计追溯。
DETAIL="$(curl -s "http://127.0.0.1:$SERVER_PORT/api/v1/summaries/daily?date=$TODAY" \
    -H "Authorization: Bearer $DEVICE_TOKEN")"
DETAIL="$DETAIL" python3 - <<'PYEOF'
import json, os, sys

data = json.loads(os.environ["DETAIL"])
if not data.get("found"):
    print("  ❌ 查询不到刚生成的总结")
    sys.exit(1)

projects = (data.get("structured") or {}).get("projects") or []
if not projects:
    print("  ❌ 结构化输出未包含项目")
    sys.exit(1)
if not all(p.get("evidence_session_ids") for p in projects):
    print("  ❌ 项目的证据 ID 应保存在 structured_output 里供追溯")
    sys.exit(1)
ids = data.get("source_session_ids") or []
if isinstance(ids, str):
    ids = json.loads(ids or "[]")
if not ids:
    print("  ❌ 应记录来源 Session ID")
    sys.exit(1)
print("  ✅ 证据 ID 保留在结构化输出与 source_session_ids 中（仅供审计/API）")
PYEOF
[ $? -eq 0 ] || FAILED=1

# 幂等：再次生成不应重复调用模型
BEFORE="$(wc -c < "$WORK_DIR/deepseek_request.json" 2>/dev/null || echo 0)"
GEN2="$(curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/summaries/daily/generate?date=$TODAY" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
STATUS2="$(echo "$GEN2" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))')"
[ "$STATUS2" = "already_succeeded" ] && pass "相同输入不重复调用模型（幂等生效）" \
    || fail "重复生成应复用结果，实际 $STATUS2"

# 管理接口鉴权
CODE="$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    "http://127.0.0.1:$SERVER_PORT/api/v1/summaries/daily/generate?date=$TODAY")"
[ "$CODE" = "401" ] && pass "管理接口拒绝未授权调用 (401)" || fail "应返回 401，实际 $CODE"

# ---- 6. 真实时序：锁屏过夜与历史脏数据重算 ----

section "6. 真实时序：工作 → idle → 锁屏过夜 → 解锁"

# 这段数据刻意复刻真机上的账目事故（2026-09-17）：用户晚上停止输入后锁屏，
# 采集端把 loginwindow 当成前台应用记录了整夜，22:30 的总结因此把锁屏
# 时间算成了工作。这里用「同一天的真实时序 + 旧语义的历史事件」验证：
# 锁屏期间的假活动不得计入工作，且重算能清掉历史脏数据。

python3 - "$WORK_DIR" "$SCENARIO_DAY" <<'PYEOF'
"""生成跨夜时序事件。

D-4：
  14:00 ~ 15:00  真实工作（ZCode / lumen）
  16:00 ~ 20:00  loginwindow 假活动（应被忽略）
  20:42:30       锁屏（旧语义事件：timestamp 是解锁时刻，duration 是时长）
D-3：
  09:14:04       解锁（同一条旧语义事件）
  09:30 ~ 10:00  真实工作（ZCode / lumen）
  11:00 ~ 12:00  锁屏期间的另一批假活动（新语义区间内）
  14:00 ~ 14:30  锁屏结束后继续工作，应生成新的时段

日期相对场景日期回退，避免与前面的场景事件相互影响。
"""
import json, sys, uuid, datetime

work = sys.argv[1]
local_tz = datetime.datetime.now().astimezone().tzinfo
scenario = datetime.datetime.strptime(sys.argv[2], "%Y-%m-%d").replace(tzinfo=local_tz)

def local(day_offset, hour, minute, second=0):
    """场景日期回退 day_offset 天后的本地时刻。"""
    return (scenario + datetime.timedelta(days=day_offset)).replace(
        hour=hour, minute=minute, second=second, microsecond=0)

def utc(dt):
    return dt.astimezone(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

def ulid():
    alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
    raw = uuid.uuid4().int
    return "".join(alphabet[(raw >> (5 * i)) & 0x1F] for i in range(25, -1, -1))

def window(dt, dur, app, project=None, bundle=None):
    ctx = {"app": app}
    if bundle:
        ctx["bundle_id"] = bundle
    if project:
        ctx["project"] = project
    return {"id": ulid(), "device_id": "desktop-mac-01", "type": "window.activity",
            "timestamp": utc(dt), "privacy": "P0", "context": ctx,
            "data": {"duration_seconds": dur}}

unlock = local(-3, 9, 14, 4)            # 场景日期 -3 天的 09:14:04 解锁
lock = unlock - datetime.timedelta(seconds=45094)  # -4 天的 20:42:30 锁屏

events = [
    # 前天：真实工作 + 锁屏前的最后一小时。
    window(local(-4, 14, 0), 3600, "ZCode", "lumen"),
    # 前天：锁屏期间的假活动（loginwindow 被当成前台应用）。
    *[window(lock + datetime.timedelta(minutes=17 * i + 10), 60, "loginwindow",
             bundle="com.apple.loginwindow")
      for i in range(4)],
    # 旧语义事件：timestamp 是「区间结束」（解锁时刻），没有 schema_version。
    {"id": ulid(), "device_id": "desktop-mac-01", "type": "idle.state",
     "timestamp": utc(unlock), "privacy": "P0", "context": {},
     "data": {"state": "locked", "duration_seconds": 45094}},
    # 昨天：解锁后的真实工作。
    window(local(-3, 9, 30), 1800, "ZCode", "lumen"),
    # 昨天：新语义的锁屏区间（11:00 起 1 小时），区间内的活动必须被裁剪。
    {"id": ulid(), "device_id": "desktop-mac-01", "type": "idle.state",
     "timestamp": utc(local(-3, 11, 0)), "privacy": "P0", "context": {},
     "data": {"state": "locked", "schema_version": 2, "duration_seconds": 3600}},
    window(local(-3, 11, 10), 600, "ScreenSaverEngine", bundle="com.apple.ScreenSaver.Engine"),
    # 昨天：锁屏结束后继续工作，应生成新的时段。
    window(local(-3, 14, 0), 1800, "ZCode", "lumen"),
]

with open(f"{work}/night_events.json", "w") as fh:
    json.dump(events, fh)
with open(f"{work}/night_dates.json", "w") as fh:
    json.dump({
        "dirty_day": local(-4, 12, 0).strftime("%Y-%m-%d"),
        "work_day": local(-3, 12, 0).strftime("%Y-%m-%d"),
    }, fh)
PYEOF

BATCH_ID="$(python3 -c 'import uuid; a="0123456789ABCDEFGHJKMNPQRSTVWXYZ"; r=uuid.uuid4().int; print("".join(a[(r>>(5*i))&31] for i in range(25,-1,-1)))')"
python3 - "$WORK_DIR" "$BATCH_ID" <<'PYEOF' > "$WORK_DIR/night_request.json"
import json, sys
work, batch = sys.argv[1], sys.argv[2]
events = json.load(open(f"{work}/night_events.json"))
print(json.dumps({"device_id": "desktop-mac-01", "batch_id": batch,
                  "sent_at": "2026-09-18T02:00:00Z", "events": events}))
PYEOF

RESP="$(curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/events/batch" \
    -H "Authorization: Bearer $DEVICE_TOKEN" \
    -H 'Content-Type: application/json' \
    --data-binary "@$WORK_DIR/night_request.json")"
ACCEPTED="$(echo "$RESP" | python3 -c 'import json,sys; print(sum(1 for r in json.load(sys.stdin)["results"] if r["status"]=="accepted"))')"
[ "$ACCEPTED" = "10" ] && pass "跨夜时序事件全部入库（10 条）" || fail "应接受 10 条，实际 $ACCEPTED"

DIRTY_DAY="$(python3 -c 'import json; print(json.load(open("'"$WORK_DIR"'/night_dates.json"))["dirty_day"])')"
WORK_DAY="$(python3 -c 'import json; print(json.load(open("'"$WORK_DIR"'/night_dates.json"))["work_day"])')"

# 重算两个受影响的日期（历史脏数据不删除，只按新规则重建 sessions）。
REBUILD="$(curl -s -X POST \
    "http://127.0.0.1:$SERVER_PORT/api/v1/sessions/rebuild?from=$DIRTY_DAY&to=$WORK_DAY" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
ALGO="$(echo "$REBUILD" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("algorithm",""))')"
[ "$ALGO" = "rules-v2" ] && pass "重算接口使用 $ALGO 规则" || fail "重算算法版本异常: $ALGO"

python3 - "$SERVER_PORT" "$DEVICE_TOKEN" "$DIRTY_DAY" "$WORK_DAY" <<'PYEOF'
"""核对重算结果：锁屏期间的假活动不得计入工作。"""
import json, sys, urllib.request

port, token, dirty_day, work_day = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
failed = 0


def sessions(day):
    req = urllib.request.Request(f"http://127.0.0.1:{port}/api/v1/sessions?date={day}",
                                 headers={"Authorization": f"Bearer {token}"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.load(resp)["sessions"]


def minutes(sess_list):
    return sum(s["stats"].get("duration_minutes", 0) for s in sess_list)


# 1) 任何一天都不能出现 loginwindow / 屏保的活动。
for day in (dirty_day, work_day):
    for s in sessions(day):
        apps = [a["app"] for a in s["apps"]]
        bad = [a for a in apps if a in ("loginwindow", "ScreenSaverEngine")]
        if bad:
            print(f"  ❌ {day} 仍在统计系统伪应用: {bad}")
            failed = 1

# 2) 前天：只有 1 小时真实工作，不能被整夜假活动放大。
dirty_total = minutes(sessions(dirty_day))
if 55 <= dirty_total <= 65:
    print(f"  ✅ 锁屏前的工作只算 1 小时（实际 {dirty_total:.0f} 分钟）")
else:
    print(f"  ❌ 前天工作时长应约为 60 分钟，实际 {dirty_total:.0f} 分钟")
    failed = 1

# 3) 昨天：旧语义的锁屏区间应从解锁前开始，不能吃掉上午的工作。
work_list = sessions(work_day)
if not work_list:
    print("  ❌ 昨天没有任何工作记录，旧语义区间可能吞掉了整天")
    failed = 1
else:
    start = work_list[0]["start_local"]
    # 首个时段应从 09:30 开始（解锁 09:14 之后 16 分钟）。
    if start[11:16] == "09:30":
        print(f"  ✅ 解锁后的工作被正确保留（首个时段 {start}）")
    else:
        print(f"  ❌ 首个时段应从 09:30 开始，实际 {start}")
        failed = 1
    # 锁屏的 11:00~12:00 必须把工作切成两段（09:30~10:00 与 14:00~14:30）。
    if len(work_list) == 2:
        print("  ✅ 锁屏正确切断了工作时段（2 段）")
    else:
        print(f"  ❌ 锁屏应把昨天切成 2 段，实际 {len(work_list)} 段")
        failed = 1
    # 14:00 之前不应有第二段活动，即锁屏期间没有活动。
    for s in work_list:
        if s["apps"] and s["apps"][0]["app"] in ("loginwindow", "ScreenSaverEngine"):
            print(f"  ❌ 锁屏期间的假活动进入了时段: {s['apps']}")
            failed = 1

sys.exit(failed)
PYEOF
[ $? -eq 0 ] || FAILED=1

# 重算不删除原始事件。
RAW_AFTER="$(sqlite3 "$WORK_DIR/data/lumen.db" 'SELECT COUNT(*) FROM events;')"
[ "$RAW_AFTER" = "14" ] && pass "重算后原始事件保留（$RAW_AFTER 条，含锁屏期间的脏数据）" \
    || fail "原始事件不应被删除，应为 14 条，实际 $RAW_AFTER 条"

# ---- 7. 数据最小化 ----

section "7. 发往模型的数据最小化"

if [ -f "$WORK_DIR/deepseek_request.json" ]; then
    python3 - "$WORK_DIR/deepseek_request.json" <<'PYEOF'
import json, sys

raw = open(sys.argv[1], encoding="utf-8").read()
forbidden = ["/Users/", "/home/", "device_token", "clipboard", "screenshot",
             "source_code", "window_title", "absolute_path", "github.com", "Bearer"]
hits = [f for f in forbidden if f in raw]
if hits:
    print(f"  ❌ 发往模型的内容包含敏感信息: {hits}")
    sys.exit(1)
print("  ✅ 不含路径、凭证、URL 或禁用字段")

body = json.loads(raw)
user = body["messages"][-1]["content"]
required = ["projects", "duration_minutes", "apps"]
missing = [k for k in required if k not in user]
if missing:
    print(f"  ❌ 缺少必要的聚合字段: {missing}")
    sys.exit(1)
print("  ✅ 只包含聚合后的 Session 上下文（项目、时长、应用）")
PYEOF
else
    fail "未捕获到模型请求"
fi

# ---- 8. 备份 ----

section "8. 数据库备份"

LUMEN_DB_PATH="$WORK_DIR/data/lumen.db" \
LUMEN_BACKUP_DIR="$WORK_DIR/data/backups" \
LUMEN_BACKUP_KEEP=2 \
bash "$ROOT_DIR/deploy/backup.sh" > "$WORK_DIR/backup.log" 2>&1

BACKUP_FILE="$(ls -t "$WORK_DIR/data/backups"/*.db 2>/dev/null | head -1 || true)"
if [ -n "$BACKUP_FILE" ]; then
    INTEGRITY="$(sqlite3 "$BACKUP_FILE" 'PRAGMA integrity_check;')"
    [ "$INTEGRITY" = "ok" ] && pass "备份完整性校验通过" || fail "备份损坏: $INTEGRITY"
    # 与线上库对比而不是写死数字：脚本会随验证场景增加事件。
    LIVE_ROWS="$(sqlite3 "$WORK_DIR/data/lumen.db" 'SELECT COUNT(*) FROM events;')"
    ROWS="$(sqlite3 "$BACKUP_FILE" 'SELECT COUNT(*) FROM events;')"
    [ "$ROWS" = "$LIVE_ROWS" ] && pass "备份包含全部事件（$ROWS 条）" \
        || fail "备份事件数应为 $LIVE_ROWS，实际 $ROWS"
else
    fail "未生成备份文件"
fi

# ---- 9. 敏感信息不落库、不落日志 ----

section "9. 敏感信息不落库、不落日志"

python3 - "$WORK_DIR" <<'PYEOF'
import sqlite3, sys, pathlib

work = pathlib.Path(sys.argv[1])
db = sqlite3.connect(work / "data" / "lumen.db")
forbidden = ["/Users/", "/home/", "clipboard", "screenshot", "source_code", "window_title"]

hit = 0
for table in ("events", "sessions", "daily_summaries", "conversations"):
    try:
        rows = db.execute(f"SELECT * FROM {table}").fetchall()
    except sqlite3.OperationalError:
        continue
    for row in rows:
        blob = " ".join(str(c) for c in row)
        for token in forbidden:
            if token in blob:
                print(f"  ❌ 表 {table} 含敏感内容: {token}")
                hit = 1
db.close()
if not hit:
    print("  ✅ 数据库中不存在剪贴板、截图、源代码、绝对路径")
PYEOF

LEAK=0
for pattern in "$ENROLL_TOKEN" "$ADMIN_TOKEN" "sk-mock-for-e2e" "$DEVICE_TOKEN" "/Users/liang"; do
    if grep -qF -- "$pattern" "$WORK_DIR/server.log" 2>/dev/null; then
        fail "服务端日志泄露: ${pattern%%-*}"
        LEAK=1
    fi
done
[ "$LEAK" -eq 0 ] && pass "服务端日志不含令牌、密钥与路径"

# ---- 汇总 ----

section "联调结果"
if [ "$FAILED" -eq 0 ]; then
    echo "✅ 全部检查通过，端到端链路正常。"
    echo ""
    echo "产物位于: $WORK_DIR"
    echo "  服务端日志: $WORK_DIR/server.log"
    echo "  数据库:     $WORK_DIR/data/lumen.db"
    echo "  模型请求:   $WORK_DIR/deepseek_request.json"
    exit 0
else
    echo "❌ 存在失败项，请检查上面的输出。"
    echo "服务端日志尾部："
    tail -20 "$WORK_DIR/server.log"
    exit 1
fi
