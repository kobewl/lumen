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

# 端口被上一次没退干净的进程占用时，服务端会静默起不来（日志级别是 warn，
# 启动失败信息落在 stderr 而健康检查只会说"未就绪"）。这里先检查并明确报出来。
if lsof -nP -iTCP:"$SERVER_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    fail "端口 $SERVER_PORT 已被占用（可能是上次没退干净的 lumen-server）"
    lsof -nP -iTCP:"$SERVER_PORT" -sTCP:LISTEN | tail -n +2
    echo "  先执行: pkill -f lumen-server"
    exit 1
fi

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
"""模拟 DeepSeek API，同时记录请求内容供安全检查。

这个 mock 要同时扮演两个角色：
  1. 每日总结的生成器（旧链路）；
  2. Agent 的 Planner 与 Synthesizer（新链路）。

区分方式看系统提示词：Planner 的提示词里含 tool_calls 的 schema，
Synthesizer 与总结生成的提示词里没有。判错会导致计划解析失败，
因此这里必须按真实提示词格式判断，不能靠"第几次调用"猜。

第三个参数是场景日期，Planner 生成的计划要按它检索，否则会去查今天。
"""
import json, re, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

FORBIDDEN = ["/Users/", "/home/", "sk-real", "device_token", "clipboard",
             "screenshot", "source_code", "window_title", "absolute_path", "github.com"]
problems = []


def summary_reply(user):
    date = re.search(r'"date":\s*"([\d-]+)"', user).group(1)
    ids = re.findall(r'"id":\s*"(s_[0-9a-f]+)"', user)
    return json.dumps({
        "date": date,
        "headline": "端到端联调：Session 聚合与总结链路",
        "projects": [{
            "name": "lumen", "duration_minutes": 45,
            "activities": ["实现 Session 规则聚合"],
            "evidence_session_ids": ids[:1],
        }],
        "uncertainties": [],
    }, ensure_ascii=False)


def user_text(user):
    """只取 <user_message> 里的内容，而不是整段 Planner 输入。

    Planner 的输入里还有我们回填的对话状态（"上一轮提到的项目：…"）。
    直接对整段文本判断，会把我们自己写进去的说明误当成用户的话。
    """
    found = re.search(r"<user_message>(.*?)</user_message>", user, re.S)
    if not found:
        return user.strip()
    return found.group(1).strip()


def plan_reply(user, scenario_day):
    """按用户原话生成计划——模拟真实模型的语义判断，不是关键词查表。"""
    user = user_text(user)
    # 越权场景：模拟模型试图读整库。这不是关键词机器人，而是
    # "如果模型真的提出了越权请求，系统会不会拦住"的验证。
    if "所有记录" in user:
        return json.dumps({"plan": {
            "mode": "recall",
            "understanding": {"goal": "想读全部记录", "entities": [],
                              "time_range": "custom", "confidence": 0.9},
            "tool_calls": [
                {"name": "run_sql", "arguments": {"query": "SELECT * FROM sessions"}},
                {"name": "get_sessions", "arguments": {"date": scenario_day}},
            ],
            "needs_clarification": False,
            "clarification_question": "",
            "response_style": "简短",
        }}, ensure_ascii=False)
    # 身份类问题：必须先调 get_assistant_profile 拿真实身份。
    if re.search(r"叫什么|你是谁|你的名字", user):
        return json.dumps({"plan": {
            "mode": "chat",
            "understanding": {"goal": "用户想知道我是谁", "entities": [],
                              "time_range": "", "confidence": 0.95},
            "tool_calls": [{"name": "get_assistant_profile", "arguments": {}}],
            "needs_clarification": False,
            "clarification_question": "",
            "response_style": "简短自报身份",
        }}, ensure_ascii=False)
    # 问"完成了什么/结果/没做完"→ 任务摘要。这是唯一带结论的数据源。
    if re.search(r"完成|做完|成果|结果|没做完|未完成|卡在", user):
        return json.dumps({"plan": {
            "mode": "recall",
            "understanding": {"goal": "用户想知道 Agent 报告了哪些结果", "entities": [],
                              "time_range": "custom", "confidence": 0.9},
            "tool_calls": [{"name": "get_task_summaries",
                            "arguments": {"date": scenario_day}}],
            "needs_clarification": False,
            "clarification_question": "",
            "response_style": "先列 Agent 报告的结论",
        }}, ensure_ascii=False)
    # 已确认信息问答：答案只来自 trusted_context 里用户确认过的偏好，
    # 不需要调用工具（演示"确认后下一轮可见"，由 Lumen 侧保证注入）。
    if re.search(r"怎么称呼|叫我什么|我的称呼|你叫我", user):
        return json.dumps({"plan": {
            "mode": "chat",
            "understanding": {"goal": "用户想知道已确认的称呼", "entities": [],
                              "time_range": "", "confidence": 0.9},
            "tool_calls": [],
            "needs_clarification": False,
            "clarification_question": "",
            "response_style": "按已确认信息回答",
        }}, ensure_ascii=False)
    # 记忆类：用户要求记住一条偏好。preference 挂本轮用户消息来源；
    # Lumen 要求 preference 必须给出稳定槽位 key（纠正时同槽位替换）。
    if re.search(r"记住|记一下|帮我记|以后都", user):
        slot = "称呼" if re.search(r"叫我|怎么称呼|称呼", user) else "回答风格"
        return json.dumps({"plan": {
            "mode": "recall",
            "understanding": {"goal": "用户希望记住一条偏好", "entities": [],
                              "time_range": "today", "confidence": 0.85},
            "tool_calls": [
                {"name": "get_sessions", "arguments": {"date": scenario_day}},
                {"name": "save_memory_candidate", "arguments": {
                    "kind": "preference",
                    "key": slot,
                    "content": "用户希望我记住：" + re.sub(r"[，。！？\s]", "", user)[:60],
                    "confidence": 0.8,
                }},
            ],
            "needs_clarification": False,
            "clarification_question": "",
            "response_style": "确认记下（但要说明只是候选）",
        }}, ensure_ascii=False)
    # 其它情况当成"想看记录"：这正是不该用正则判语义的地方。
    return json.dumps({"plan": {
        "mode": "recall",
        "understanding": {"goal": "用户想看某个时间做了什么", "entities": ["lumen"],
                          "time_range": "custom", "confidence": 0.85},
        "tool_calls": [{"name": "get_sessions", "arguments": {"date": scenario_day}}],
        "needs_clarification": False,
        "clarification_question": "",
        "response_style": "列应用与时长",
    }}, ensure_ascii=False)


def synth_reply(user):
    """合成阶段：只能依据事实回答，因此这里从事实里取值而不是写死文案。"""
    # 已确认信息问答：只依据 trusted_context 里代码注入的"已确认信息"段。
    # 段不存在就诚实说没有——候选记忆绝不会被注入，因此确认前只能答"没有"。
    um = re.search(r"<user_message>(.*?)</user_message>", user, re.S)
    question = um.group(1) if um else user
    if re.search(r"怎么称呼|叫我什么|我的称呼|你叫我", question):
        m = re.search(r"- 称呼：(.+?)（第 (\d+) 版）", user)
        if m:
            return json.dumps({"answer": f"按你确认过的偏好，{m.group(1)}（第 {m.group(2)} 版）。",
                               "support_level": "supported"}, ensure_ascii=False)
        return json.dumps({"answer": "我这边还没有你确认过的称呼记录。",
                           "support_level": "insufficient"}, ensure_ascii=False)
    if '"saved":true' in user.replace(" ", ""):
        return json.dumps({"answer": "记下了（候选，还没生效），以后我先说结论。",
                           "support_level": "supported"}, ensure_ascii=False)
    if '"name":"' in user:
        name = re.search(r'"name":"([^"]+)"', user).group(1)
        return json.dumps({"answer": f"我是{name}，你的联调助手。",
                           "support_level": "supported"}, ensure_ascii=False)
    if "Agent 报告了" in user or "已完成" in user:
        return json.dumps({"answer": "ZCode 那边报告完成了一件：接通任务摘要链路。",
                           "support_level": "supported"}, ensure_ascii=False)
    if "Visual Studio Code" in user:
        return json.dumps({"answer": "这次记录里用的是 Visual Studio Code，约 45 分钟。",
                           "support_level": "supported"}, ensure_ascii=False)
    return json.dumps({"answer": "这次没有查到可用的记录。",
                       "support_level": "insufficient"}, ensure_ascii=False)


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length).decode("utf-8")
        # 每次请求都留档：只记最后一次会让安全检查漏掉大部分 prompt。
        with open(sys.argv[2], "w", encoding="utf-8") as fh:
            fh.write(raw)
        with open(sys.argv[4], "a", encoding="utf-8") as fh:
            fh.write(raw + "\n")
        for token in FORBIDDEN:
            if token in raw:
                problems.append(token)

        body = json.loads(raw)
        system = body["messages"][0]["content"]
        user = body["messages"][-1]["content"]

        if "tool_calls" in system:
            content = plan_reply(user, sys.argv[3])
        elif '"skip"' in system and '"basis"' in system:
            # 主动关怀提案：依据 facts 里真实出现的 task_id。
            # basis 允许两种写法：task_id（模型可见）或事实来源的工具名。
            # 时段的内部 ID 刻意不进模型上下文，模型不引用它。
            ids = re.findall(r'"task_id"\s*:\s*"([^"]+)"', user)
            if ids:
                basis = ids[:2]
            elif '"sessions":[{"' in user.replace(" ", ""):
                basis = ["get_today_status"]
            else:
                basis = []
            if basis:
                content = json.dumps({
                    "skip": False, "reason": "用户在推进项目",
                    "question": "最近在项目上投入了不少，进展还顺利吗？",
                    "basis": basis,
                }, ensure_ascii=False)
            else:
                content = json.dumps({"skip": True, "reason": "没有可依据的记录",
                                      "question": "", "basis": []}, ensure_ascii=False)
        elif "support_level" in system:
            content = synth_reply(user)
        else:
            content = summary_reply(user)

        out = json.dumps({
            "id": "mock", "model": "deepseek-chat",
            "choices": [{"message": {"content": content}, "finish_reason": "stop"}],
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

# 场景日期在这里就要算出来：Planner 的计划需要它，服务端的日报场景也要用它。
# 当天 13:00 之后用今天，否则用昨天——事件落在未来会被 clock_skew 规则改写。
if [ "$(date +%H)" -ge 13 ]; then
    SCENARIO_DAY="$(date +%Y-%m-%d)"
else
    SCENARIO_DAY="$(date -v-1d +%Y-%m-%d 2>/dev/null || date -d 'yesterday' +%Y-%m-%d)"
fi

python3 "$WORK_DIR/mock_deepseek.py" "$MOCK_PORT" "$WORK_DIR/deepseek_request.json" \
    "$SCENARIO_DAY" "$WORK_DIR/deepseek_requests.jsonl" \
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
LUMEN_ASSISTANT_NAME="小灯" \
LUMEN_ASSISTANT_ROLE="联调助手" \
LUMEN_INITIATIVE_ENABLED="true" \
LUMEN_INITIATIVE_DRY_RUN="true" \
LUMEN_FEISHU_ALLOWED_USER_IDS="ou_e2e_care" \
LUMEN_LOG_LEVEL="warn" \
"$WORK_DIR/lumen-server" > "$WORK_DIR/server.log" 2>&1 &
SERVER_PID=$!

# 就绪等待用轮询而不是固定 sleep：全新编译的二进制首次执行要过 macOS
# 的签名扫描，偶发超过 2 秒，单次探测会把"慢"误判成"起不来"。
SERVER_READY=""
for _ in $(seq 1 40); do
    if HEALTH="$(curl -sf -m 2 "http://127.0.0.1:$SERVER_PORT/api/v1/healthz" 2>/dev/null)"; then
        SERVER_READY=1
        break
    fi
    sleep 0.5
done
if [ -z "$SERVER_READY" ]; then
    fail "服务端未就绪"
    cat "$WORK_DIR/server.log"
    exit 1
fi
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

# 场景日期已在启动 mock 之前算好（Planner 的计划要用它）。
info "场景日期: $SCENARIO_DAY"

python3 - "$WORK_DIR" "$SCENARIO_DAY" <<'PYEOF' || FAILED=1
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

python3 - "$WORK_DIR" "$SCENARIO_DAY" <<'PYEOF' || FAILED=1
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

python3 - "$SERVER_PORT" "$DEVICE_TOKEN" "$DIRTY_DAY" "$WORK_DAY" <<'PYEOF' || FAILED=1
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


# 重算不删除原始事件。
RAW_AFTER="$(sqlite3 "$WORK_DIR/data/lumen.db" 'SELECT COUNT(*) FROM events;')"
[ "$RAW_AFTER" = "14" ] && pass "重算后原始事件保留（$RAW_AFTER 条，含锁屏期间的脏数据）" \
    || fail "原始事件不应被删除，应为 14 条，实际 $RAW_AFTER 条"

# ---- 7. Agent 任务摘要 ----

section "7. Agent 任务摘要（agent.task_summary）"

# 用真实的采集端代码构造事件：不手写 JSON，这样测的是"ZCode 实际会提交什么"，
# 而不是"我们以为它会提交什么"。字段 allowlist 与隐私校验都在采集端执行。
TASK_SUMMARY="$WORK_DIR/task_summary.json"
python3 - "$TASK_SUMMARY" "$SCENARIO_DAY" "$ROOT_DIR" <<'PYEOF' || FAILED=1
"""用采集端代码构造一条合法的 agent.task_summary 事件。"""
import json, sys
from datetime import datetime, timezone
from pathlib import Path

out_path, day, root = sys.argv[1], sys.argv[2], sys.argv[3]
sys.path.insert(0, str(Path(root) / "desktop"))
from lumen_desktop.event import Event
from lumen_desktop.privacy import PrivacyFilter

occurred = datetime.strptime(day, "%Y-%m-%d").replace(
    hour=15, minute=42, tzinfo=timezone.utc)

event = Event.agent_task_summary(
    event_id="01J9Z4QK7M3F8N2P5R7T9V1X3T",
    device_id="desktop-mac-01",
    occurred_at=occurred,
    task_id="zcode-e2e-001",
    title="接通任务摘要链路",
    source_agent="zcode-cli",
    status="done",
    outcomes=["新增 agent.task_summary 事件类型", "补齐协议 schema"],
    open_loops=["尚未接入真实 ZCode 上报"],
    source_session_id="sess-e2e-hidden",
    project="lumen",
)
problems = PrivacyFilter().validate_payload(event.to_payload())
if problems:
    print(f"  ❌ 采集端隐私校验未通过: {problems}")
    sys.exit(1)
# 内部会话 ID 必须在事件里（用于追溯），但不应出现在用户可见文本中——
# 后者由 assistant 层保证，这里只确认它确实被提交了。
payload = event.to_payload()
assert payload["data"]["source_session_id"] == "sess-e2e-hidden"
assert payload["data"]["privacy_mode"] == "metadata_only"
with open(out_path, "w", encoding="utf-8") as fh:
    json.dump(payload, fh)
print("  ✅ 采集端构造出合法任务摘要（字段 allowlist 与隐私校验均通过）")
PYEOF

TASK_BATCH="$(python3 -c 'import uuid; a="0123456789ABCDEFGHJKMNPQRSTVWXYZ"; r=uuid.uuid4().int; print("".join(a[(r>>(5*i))&31] for i in range(25,-1,-1)))')"
python3 - "$WORK_DIR" "$TASK_BATCH" <<'PYEOF' > "$WORK_DIR/task_batch.json"
import json, sys
work, batch = sys.argv[1], sys.argv[2]
event = json.load(open(f"{work}/task_summary.json"))
print(json.dumps({
    "device_id": "desktop-mac-01", "batch_id": batch,
    "sent_at": "2026-09-17T10:05:00Z", "events": [event],
}, ensure_ascii=False))
PYEOF

TASK_RESP="$(curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/events/batch" \
    -H "Authorization: Bearer $DEVICE_TOKEN" \
    -H 'Content-Type: application/json' \
    --data-binary "@$WORK_DIR/task_batch.json")"
TASK_STATUS="$(echo "$TASK_RESP" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["results"][0]["status"])')"
[ "$TASK_STATUS" = "accepted" ] && pass "任务摘要被服务端接受" \
    || fail "任务摘要应被接受，实际 $TASK_STATUS: $TASK_RESP"

# 幂等：重复投递同一条不应产生第二条。
curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/events/batch" \
    -H "Authorization: Bearer $DEVICE_TOKEN" -H 'Content-Type: application/json' \
    --data-binary "@$WORK_DIR/task_batch.json" > /dev/null
TASK_COUNT="$(sqlite3 "$WORK_DIR/data/lumen.db" 'SELECT COUNT(*) FROM agent_task_summaries;')"
[ "$TASK_COUNT" = "1" ] && pass "重复投递后任务摘要仍只有 1 条（幂等）" \
    || fail "任务摘要应只有 1 条，实际 $TASK_COUNT 条"

RAW_EVENT="$(sqlite3 "$WORK_DIR/data/lumen.db" \
    "SELECT COUNT(*) FROM events WHERE event_type = 'agent.task_summary';")"
[ "$RAW_EVENT" = "1" ] && pass "原始事件保留在事件流中（投影表之外仍可审计）" \
    || fail "原始任务摘要事件应保留，实际 $RAW_EVENT 条"

TASK_API="$(curl -s "http://127.0.0.1:$SERVER_PORT/api/v1/task-summaries?date=$SCENARIO_DAY" \
    -H "Authorization: Bearer $DEVICE_TOKEN")"
echo "$TASK_API" > "$WORK_DIR/task_api.json"
python3 - "$WORK_DIR/task_api.json" <<'PYEOF' || FAILED=1
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
failed = 0
if d.get("count") == 1:
    print("  ✅ /api/v1/task-summaries 返回 1 条")
else:
    print(f"  ❌ 应返回 1 条，实际 {d.get('count')}")
    failed = 1
item = (d.get("task_summaries") or [{}])[0]
if item.get("title") == "接通任务摘要链路" and item.get("status") == "done":
    print("  ✅ 标题与状态正确")
else:
    print(f"  ❌ 标题或状态不符: {item}")
    failed = 1
if len(item.get("outcomes") or []) == 2:
    print("  ✅ 已产出结果完整返回")
else:
    print(f"  ❌ 已产出结果应 2 条，实际 {item.get('outcomes')}")
    failed = 1
if item.get("source_session_id") == "sess-e2e-hidden":
    print("  ✅ 来源会话 ID 保留在 API 层（供追溯，不进入用户可见文本）")
else:
    print(f"  ❌ 来源会话 ID 应保留，实际 {item.get('source_session_id')}")
    failed = 1
sys.exit(failed)
PYEOF

# 隐私：把正文类字段塞进任务摘要，服务端必须拒绝整条事件。
python3 - "$WORK_DIR" <<'PYEOF' > "$WORK_DIR/task_bad.json"
import json, sys
work = sys.argv[1]
event = json.load(open(f"{work}/task_summary.json"))
event["id"] = "01J9Z4QK7M3F8N2P5R7T9V1X4A"
event["data"]["conversation"] = [{"role": "user", "content": "帮我改代码"}]
print(json.dumps({
    "device_id": "desktop-mac-01", "batch_id": "01J9Z4QK7M3F8N2P5R7T9V1X4B",
    "sent_at": "2026-09-17T10:05:00Z", "events": [event],
}, ensure_ascii=False))
PYEOF
BAD_RESP="$(curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/events/batch" \
    -H "Authorization: Bearer $DEVICE_TOKEN" -H 'Content-Type: application/json' \
    --data-binary "@$WORK_DIR/task_bad.json")"
echo "$BAD_RESP" > "$WORK_DIR/task_bad_resp.json"
python3 - "$WORK_DIR/task_bad_resp.json" <<'PYEOF' || FAILED=1
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
r = d["results"][0]
if r["status"] == "rejected" and "conversation" in json.dumps(r, ensure_ascii=False):
    print("  ✅ 携带完整对话的任务摘要被服务端拒绝")
else:
    print(f"  ❌ 携带正文的任务摘要应被拒绝，实际 {r}")
    sys.exit(1)
PYEOF

# 同样一条夹带路径的摘要也应被拒绝（敏感内容检测覆盖 data 字段）。
python3 - "$WORK_DIR" <<'PYEOF' > "$WORK_DIR/task_path.json"
import json, sys
work = sys.argv[1]
event = json.load(open(f"{work}/task_summary.json"))
event["id"] = "01J9Z4QK7M3F8N2P5R7T9V1X4C"
event["data"]["outcomes"] = ["改了 /Users/liang/Documents/Project/lumen/x.go"]  # secrets-check:allow
print(json.dumps({
    "device_id": "desktop-mac-01", "batch_id": "01J9Z4QK7M3F8N2P5R7T9V1X4D",
    "sent_at": "2026-09-17T10:05:00Z", "events": [event],
}, ensure_ascii=False))
PYEOF
PATH_RESP="$(curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/events/batch" \
    -H "Authorization: Bearer $DEVICE_TOKEN" -H 'Content-Type: application/json' \
    --data-binary "@$WORK_DIR/task_path.json")"
echo "$PATH_RESP" > "$WORK_DIR/task_path_resp.json"
python3 - "$WORK_DIR/task_path_resp.json" <<'PYEOF' || FAILED=1
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
r = d["results"][0]
if r["status"] == "rejected" and r.get("code") == "sensitive_value":
    print("  ✅ 结果条目里夹带绝对路径被拒绝（data 字段同样受检）")
else:
    print(f"  ❌ 夹带路径的摘要应被拒绝，实际 {r}")
    sys.exit(1)
PYEOF

# ---- 8. AI-first 问答链路 ----

section "8. AI-first 问答链路（/api/v1/ask）"

# 这句话刻意不用任何关键词：「忙了些什么」不含「做了什么」，
# 旧的 ParseQuery 会把它判成 unsupported 并回一份功能菜单。
# 现在它必须走到模型计划并真的检索到数据——语义判断不在 Go 代码里。
ask() {
    curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/ask" \
        -H "Authorization: Bearer $ADMIN_TOKEN" \
        -H 'Content-Type: application/json' \
        -d "$(python3 -c 'import json,sys; print(json.dumps({"text": sys.argv[1]}))' "$1")"
}

ask "我那天都忙了些什么呀" > "$WORK_DIR/ask_recall.json"

python3 - "$WORK_DIR/ask_recall.json" <<'PYEOF' || FAILED=1
import json, sys

d = json.load(open(sys.argv[1], encoding="utf-8"))
failed = 0
if d.get("mode") == "recall":
    print("  ✅ 计划模式为 recall（模型决定语义，不是正则）")
else:
    print(f"  ❌ 计划模式应为 recall，实际 {d.get('mode')}")
    failed = 1
if "get_sessions" in (d.get("tool_calls") or []):
    print("  ✅ 模型选择了 get_sessions 并被 Policy Gate 放行")
else:
    print(f"  ❌ 应调用 get_sessions，实际 {d.get('tool_calls')}")
    failed = 1
if d.get("source_session_ids"):
    print(f"  ✅ 回答带真实来源（{len(d['source_session_ids'])} 个时段）")
else:
    print("  ❌ 检索类回答必须带可审计的真实来源")
    failed = 1
if "Visual Studio Code" in d.get("answer", ""):
    print("  ✅ 回答基于能力返回的事实（不是模型凭提示词编造）")
else:
    print(f"  ❌ 回答应基于检索到的事实，实际: {d.get('answer')}")
    failed = 1
if d.get("support_level") == "supported":
    print("  ✅ 支持等级标记为 supported（结论直接来自事实）")
else:
    print(f"  ❌ 支持等级应为 supported，实际 {d.get('support_level')}")
    failed = 1
# 用户可见文本里不能出现内部 ID 或工程术语。
text = d.get("answer", "")
bad = [b for b in ("s_", "unclassified", "session") if b in text]
if bad:
    print(f"  ❌ 回答含内部术语: {bad}")
    failed = 1
else:
    print("  ✅ 回答里没有内部术语与内部 ID")
sys.exit(failed)
PYEOF


# 身份：换 LUMEN_ASSISTANT_NAME 就应该换称呼，且不依赖代码里的常量。
ask "你叫什么名字" > "$WORK_DIR/ask_identity.json"

python3 - "$WORK_DIR/ask_identity.json" <<'PYEOF' || FAILED=1
import json, sys

d = json.load(open(sys.argv[1], encoding="utf-8"))
failed = 0
if "get_assistant_profile" in (d.get("tool_calls") or []):
    print("  ✅ 身份问题先调用 get_assistant_profile 取真实身份")
else:
    print(f"  ❌ 应先调用 get_assistant_profile，实际 {d.get('tool_calls')}")
    failed = 1
if "小灯" in d.get("answer", ""):
    print("  ✅ 回答使用了配置里的名字（LUMEN_ASSISTANT_NAME=小灯）")
else:
    print(f"  ❌ 回答应使用配置的名字，实际: {d.get('answer')}")
    failed = 1
if "Lumen" in d.get("answer", ""):
    print("  ❌ 改配置后不应再出现写死的 Lumen")
    failed = 1
sys.exit(failed)
PYEOF


# 真实内存的模型请求里，Planner 提示词必须注入配置身份，而不是写死名字。
python3 - "$WORK_DIR/deepseek_requests.jsonl" <<'PYEOF' || FAILED=1
import json, sys

failed = 0
planner_prompts = []
for line in open(sys.argv[1], encoding="utf-8"):
    line = line.strip()
    if not line:
        continue
    body = json.loads(line)
    system = body["messages"][0]["content"]
    if "tool_calls" in system:
        planner_prompts.append(body)

if not planner_prompts:
    print("  ❌ 未捕获到 Planner 提示词")
    sys.exit(1)
# 身份现在由上下文装配器注入 trusted_context（用户侧消息），系统提示只留规则。
body0 = planner_prompts[0]
system = body0["messages"][0]["content"]
user_prompt = body0["messages"][-1]["content"]
if "小灯" in user_prompt and "联调助手" in user_prompt and "trusted_context" in user_prompt:
    print("  ✅ Planner 提示词在 trusted_context 中注入了配置的身份")
else:
    print("  ❌ Planner 提示词应在 trusted_context 中包含配置的名字与定位")
    failed = 1
if "- 你的名字是" in system:
    # 工具目录的 result_schema 里会出现配置名（get_assistant_profile），
    # 那是数据源声明而不是身份注入；身份块（"- 你的名字是：…"）只应
    # 出现在 trusted_context，不再拼进系统提示。
    print("  ❌ 系统提示不应再拼身份块（身份统一走 trusted_context）")
    failed = 1
else:
    print("  ✅ 系统提示不再拼身份块（身份统一走 trusted_context）")
if "你是 Lumen" in system:
    print("  ❌ Planner 提示词不应写死 Lumen")
    failed = 1
if "get_assistant_profile" not in system:
    print("  ❌ Planner 提示词应包含能力目录")
    failed = 1
sys.exit(failed)
PYEOF


# 任务摘要链路：问"完成了什么"必须走 get_task_summaries，且回答要说明
# 结论来自 Agent 报告，而不是说成 Lumen 自己观察到的。
ask "今天完成了什么" > "$WORK_DIR/ask_task.json"

python3 - "$WORK_DIR/ask_task.json" <<'PYEOF' || FAILED=1
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
failed = 0
if "get_task_summaries" in (d.get("tool_calls") or []):
    print("  ✅ 问「完成了什么」选择了 get_task_summaries（唯一带结论的数据源）")
else:
    print(f"  ❌ 应选择 get_task_summaries，实际 {d.get('tool_calls')}")
    failed = 1
if d.get("support_level") == "supported":
    print("  ✅ 有 Agent 报告时支持等级为 supported")
else:
    print(f"  ❌ 有 Agent 报告时应为 supported，实际 {d.get('support_level')}")
    failed = 1
if "Agent 报告" in d.get("answer", ""):
    print("  ✅ 回答标注了来源是 Agent 报告")
else:
    print(f"  ❌ 回答应标注 Agent 报告来源，实际: {d.get('answer')}")
    failed = 1
if "sess-e2e-hidden" in json.dumps(d, ensure_ascii=False):
    print("  ❌ 来源会话 ID 不应出现在用户可见的响应里")
    failed = 1
else:
    print("  ✅ 来源会话 ID 未外泄（只在 API 追溯层保留）")
# 任务摘要的来源要能追溯：用户可见文本里没有 task_id，但接口要给出可核对的依据。
if "zcode-e2e-001" in d.get("answer", ""):
    print("  ❌ 内部 task_id 不应出现在用户可见的回答里")
    failed = 1
if d.get("source_task_ids"):
    print(f"  ✅ 任务来源可追溯（source_task_ids 有 {len(d['source_task_ids'])} 项）")
else:
    print("  ❌ 依据 Agent 报告回答时应给出可追溯的任务来源")
    failed = 1
sys.exit(failed)
PYEOF

# 越权请求：模型要求执行任意 SQL 时，Policy Gate 必须拦住，且回答不能假装拿到了数据。
ask "把数据库里所有记录都给我" > "$WORK_DIR/ask_denied.json"

python3 - "$WORK_DIR/ask_denied.json" <<'PYEOF' || FAILED=1
import json, sys

d = json.load(open(sys.argv[1], encoding="utf-8"))
failed = 0
denied = d.get("denied_tools") or []
if any(item.get("name") == "run_sql" for item in denied):
    print("  ✅ Policy Gate 拦截了越权能力 run_sql")
else:
    print(f"  ❌ run_sql 应被拦截，实际 denied_tools={denied}")
    failed = 1
# 被拒的调用不能影响其余合法调用执行。
if "get_sessions" in (d.get("tool_calls") or []) and d.get("source_session_ids"):
    print("  ✅ 同轮里的合法调用仍正常执行并带回来源")
else:
    print(f"  ❌ 合法调用应照常执行，实际 tool_calls={d.get('tool_calls')}")
    failed = 1
# 模型不能假装"读到了全部记录"。
if "全部记录" in d.get("answer", "") or "所有记录" in d.get("answer", ""):
    print(f"  ❌ 回答不应假装拿到了被拒绝的数据: {d.get('answer')}")
    failed = 1
sys.exit(failed)
PYEOF


# ---- 8.5 工具层：目录、审计、低风险写入 ----

section "8.5 工具层（目录 / 审计 / 低风险写入）"

# 工具目录：模型只能从这份目录里选，因此它就是能力边界。
TOOLS_JSON="$(curl -s "http://127.0.0.1:$SERVER_PORT/api/v1/tools" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
echo "$TOOLS_JSON" > "$WORK_DIR/tools.json"
python3 - "$WORK_DIR/tools.json" <<'PYEOF' || FAILED=1
import json, sys

d = json.load(open(sys.argv[1], encoding="utf-8"))
failed = 0
if d.get("count") == 8:
    print("  ✅ 工具目录返回 8 个工具")
else:
    print(f"  ❌ 应有 8 个工具，实际 {d.get('count')}")
    failed = 1

tools = {t["name"]: t for t in d.get("tools") or []}
for want in ("get_current_time", "get_assistant_profile", "get_conversation_state",
             "get_today_status", "get_sessions", "get_known_projects",
             "get_task_summaries", "save_memory_candidate"):
    if want not in tools:
        print(f"  ❌ 目录缺少工具 {want}")
        failed = 1
if not failed:
    print("  ✅ 八个工具齐全")

# 只有一个是写入工具，且必须是低风险 + 必须带来源证据。
writes = [t for t in tools.values() if t["risk"] != "read"]
if len(writes) == 1 and writes[0]["risk"] == "write_low" and writes[0]["requires_evidence"]:
    print("  ✅ 唯一的写工具是低风险写入且要求来源证据")
else:
    print(f"  ❌ 写工具的治理属性不符: {writes}")
    failed = 1

# 写工具不能接受 source_ids：来源只能由代码注入。
mem = tools.get("save_memory_candidate", {})
if any(p.get("name") == "source_ids" for p in mem.get("parameters") or []):
    print("  ❌ 写工具不应接受 source_ids 参数（来源必须由代码注入）")
    failed = 1
else:
    print("  ✅ 写工具不接受 source_ids（来源由代码注入）")

# 每个工具都要有中文说明与结果 schema。
for name, tool in tools.items():
    if not tool.get("summary") or not tool.get("result_schema"):
        print(f"  ❌ 工具 {name} 缺少说明或结果 schema")
        failed = 1
sys.exit(failed)
PYEOF

# 记忆写入：只写候选、必须带来源、不得自动晋升。
ask "以后都先给我说结论，记住这点" > "$WORK_DIR/ask_memory.json"

python3 - "$WORK_DIR/ask_memory.json" "$WORK_DIR/data/lumen.db" <<'PYEOF' || FAILED=1
import json, sqlite3, sys

reply = json.load(open(sys.argv[1], encoding="utf-8"))
db = sqlite3.connect(sys.argv[2])
failed = 0

if "save_memory_candidate" in (reply.get("tool_calls") or []):
    print("  ✅ 模型选择了 save_memory_candidate")
else:
    print(f"  ❌ 应调用写工具，实际 {reply.get('tool_calls')}")
    failed = 1

if not reply.get("denied_tools"):
    print("  ✅ 写调用没有被拒绝")
else:
    print(f"  ❌ 写调用不应被拒，实际 {reply['denied_tools']}")
    failed = 1

rows = db.execute("SELECT kind, status, source_ids FROM memory_candidates").fetchall()
if len(rows) != 1:
    print(f"  ❌ 应写入 1 条候选记忆，实际 {len(rows)} 条")
    failed = 1
else:
    kind, status, source_ids = rows[0]
    if status == "candidate":
        print("  ✅ 只写候选状态（candidate），未自动生效")
    else:
        print(f"  ❌ 状态应为 candidate，实际 {status}")
        failed = 1
    if not json.loads(source_ids):
        print("  ❌ 候选记忆必须带可核实的来源")
        failed = 1
    else:
        print(f"  ✅ 来源由代码注入（{len(json.loads(source_ids))} 个）")

# 已确认记忆必须仍为空：没有确认入口就绝不晋升。
confirmed = db.execute(
    "SELECT COUNT(*) FROM memory_candidates WHERE status = 'confirmed'").fetchone()[0]
if confirmed == 0:
    print("  ✅ 候选未自动晋升为已确认记忆")
else:
    print(f"  ❌ 候选不应自动晋升，实际 {confirmed} 条已确认")
    failed = 1
db.close()
sys.exit(failed)
PYEOF

# 候选记忆不得进入后续轮次的模型上下文。
ask "你好" > /dev/null
if grep -q "先看结论" "$WORK_DIR/deepseek_requests.jsonl"; then
    # 只有当前这轮请求里出现是允许的（用户原话），此后不应再出现候选内容。
    AFTER_MEMORY="$(awk '/save_memory_candidate/{found=1} found' "$WORK_DIR/deepseek_requests.jsonl" | grep -c "先看结论" || true)"
    if [ "$AFTER_MEMORY" -le 1 ]; then
        pass "候选记忆未进入后续轮次的模型上下文"
    else
        fail "候选记忆内容出现在后续模型请求里（$AFTER_MEMORY 次）"
    fi
else
    pass "候选记忆未进入模型请求"
fi

# 审计：每次工具调用都必须留痕，包括被拒的调用。
AUDITS="$(curl -s "http://127.0.0.1:$SERVER_PORT/api/v1/tool-audits?limit=200" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
echo "$AUDITS" > "$WORK_DIR/audits.json"
python3 - "$WORK_DIR/audits.json" <<'PYEOF' || FAILED=1
import json, sys

d = json.load(open(sys.argv[1], encoding="utf-8"))
audits = d.get("audits") or []
failed = 0

if not audits:
    print("  ❌ 审计表为空：工具调用必须留下记录")
    sys.exit(1)
print(f"  ✅ 审计记录了 {len(audits)} 次工具调用")

decisions = {a["decision"] for a in audits}
if "allowed" in decisions:
    print("  ✅ 放行的调用已记录")
else:
    print("  ❌ 缺少放行记录")
    failed = 1
if "denied" in decisions:
    print("  ✅ 被拒的调用也记录了（越权尝试可追溯）")
else:
    print("  ❌ 缺少拒绝记录：越权尝试必须可追溯")
    failed = 1

# 拒绝记录必须带原因。
for a in audits:
    if a["decision"] in ("denied", "error") and not a.get("reason"):
        print(f"  ❌ 拒绝/失败记录缺少原因: {a}")
        failed = 1

# 审计只记元信息：不应包含用户可见文本或内部 ID。
blob = json.dumps(audits, ensure_ascii=False)
for bad in ("sess-e2e-hidden", "unclassified"):
    if bad in blob:
        print(f"  ❌ 审计不应包含 {bad}")
        failed = 1

# 证据条数必须记录（否则"审计"无法回答"这次调用了什么依据"）。
with_evidence = [a for a in audits if a.get("evidence_n", 0) > 0]
if with_evidence:
    print(f"  ✅ 审计记录了证据条数（{len(with_evidence)} 条有来源）")
else:
    print("  ❌ 检索类调用应记录证据条数")
    failed = 1

# 参数值必须被裁短：审计要长期保存，不能堆积用户全文。
for a in audits:
    for value in (a.get("args") or {}).values():
        if isinstance(value, str) and len(value) > 70:
            print(f"  ❌ 审计里的参数未被裁短: {len(value)} 字")
            failed = 1
sys.exit(failed)
PYEOF

# 审计接口需要管理令牌。
CODE="$(curl -s -o /dev/null -w '%{http_code}' \
    "http://127.0.0.1:$SERVER_PORT/api/v1/tool-audits")"
[ "$CODE" = "401" ] && pass "审计接口拒绝未授权访问 (401)" || fail "审计接口应返回 401，实际 $CODE"

CODE="$(curl -s -o /dev/null -w '%{http_code}' \
    "http://127.0.0.1:$SERVER_PORT/api/v1/tools")"
[ "$CODE" = "401" ] && pass "工具目录拒绝未授权访问 (401)" || fail "工具目录应返回 401，实际 $CODE"

# ---- 8.6 主动关怀（dry-run 本地闭环） ----

section "8.6 主动关怀（dry-run）"

# 未授权访问必须拒绝。
CODE="$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    "http://127.0.0.1:$SERVER_PORT/api/v1/initiative/run")"
[ "$CODE" = "401" ] && pass "关怀触发拒绝未授权访问 (401)" || fail "应返回 401，实际 $CODE"

# 关怀要有依据：先补一条"今天"的活动记录（e2e 可能在 13 点前运行，
# 主体种子数据落在场景日期=昨天）。批量上报会自动重算受影响日期。
python3 - "$WORK_DIR" <<'PYEOF'
import datetime as dt, json, os, sys, uuid

work = sys.argv[1]
ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
raw = uuid.uuid4().int
ulid = "".join(ALPHABET[(raw >> (5 * i)) & 0x1F] for i in range(25, -1, -1))
now = dt.datetime.now().astimezone().replace(microsecond=0)
morning = now.replace(hour=9, minute=0, second=0)
if morning > now:
    morning = now - dt.timedelta(hours=1)
event = {
    "id": ulid, "device_id": "desktop-mac-01", "type": "window.activity",
    "timestamp": morning.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "privacy": "P0",
    "context": {"app": "ZCode", "project": "lumen"},
    "data": {"duration_seconds": 3600},
}
with open(os.path.join(work, "care_event.json"), "w", encoding="utf-8") as fh:
    json.dump({"device_id": "desktop-mac-01", "batch_id": ulid + "B",
               "sent_at": dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
               "events": [event]}, fh)
PYEOF
curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/events/batch" \
    -H "Authorization: Bearer $DEVICE_TOKEN" -H 'Content-Type: application/json' \
    --data-binary "@$WORK_DIR/care_event.json" | python3 -c '
import json, sys
r = json.load(sys.stdin)["results"][0]
assert r["status"] == "accepted", r
print("  · 已补一条今天的活动记录（供关怀引用）")
'

# 首次触发用 force：e2e 可能在任意时刻运行（含静默时段），
# force 只跳过频率限制，仍走完整链路并写 outbox。
RUN="$(curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/initiative/run?force=true" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
echo "$RUN" > "$WORK_DIR/initiative_run.json"

python3 - "$WORK_DIR/initiative_run.json" "$WORK_DIR/data/lumen.db" <<'PYEOF' || FAILED=1
import json, sqlite3, sys

d = json.load(open(sys.argv[1], encoding="utf-8"))
failed = 0

if d.get("action") == "dry_run":
    print("  ✅ 默认 dry-run：走了完整链路但没有真实发送")
else:
    print(f"  ❌ 应为 dry_run，实际 {d.get('action')} ({d.get('reason')})")
    failed = 1

if d.get("text"):
    print(f"  ✅ 生成了有依据的问题：{d['text'][:40]}…")
else:
    print("  ❌ 应生成问题全文")
    failed = 1

if not d.get("forced"):
    print("  ❌ force 标记应回显")
    failed = 1

db = sqlite3.connect(sys.argv[2])
rows = db.execute(
    "SELECT status, text, basis_json, evidence_json FROM initiative_outbox").fetchall()
if len(rows) != 1:
    print(f"  ❌ outbox 应有 1 条，实际 {len(rows)} 条")
    failed = 1
else:
    status, text, basis, evidence = rows[0]
    if status != "dry_run":
        print(f"  ❌ outbox 状态应为 dry_run，实际 {status}")
        failed = 1
    else:
        print("  ✅ outbox 落了 dry_run 记录（草稿 + 审计二合一）")
    if json.loads(basis):
        print("  ✅ 依据（basis）落库且来自本轮核实的事实")
    else:
        print("  ❌ 问题必须带依据")
        failed = 1
    if json.loads(evidence):
        print("  ✅ 本轮核实过的证据 ID 一并留档")
    else:
        print("  ❌ 证据 ID 应留档")
        failed = 1
db.close()
sys.exit(failed)
PYEOF

# 第二次不带 force：应被频率策略拦住（间隔或静默，取决于运行时刻）。
RUN2="$(curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/initiative/run" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
python3 - "$RUN2" <<'PYEOF' || FAILED=1
import json, sys

d = json.loads(sys.argv[1])
if d.get("action") == "skipped" and d.get("reason"):
    print(f"  ✅ 第二次触发被策略拦住（{d['reason']}），不重复打扰")
else:
    print(f"  ❌ 第二次应被跳过，实际 {d.get('action')} ({d.get('reason')})")
    sys.exit(1)
PYEOF

# outbox 查询端点（管理令牌）。
OUTBOX="$(curl -s "http://127.0.0.1:$SERVER_PORT/api/v1/initiative/outbox" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
python3 - "$OUTBOX" <<'PYEOF2' || { fail "outbox 查询端点应返回记录"; FAILED=1; }
import json, sys
d = json.loads(sys.argv[1])
if d.get("count", 0) >= 1:
    print("  ✅ outbox 查询端点返回 %d 条记录" % d["count"])
    sys.exit(0)
print("  ❌ outbox 查询端点应返回记录")
sys.exit(1)
PYEOF2

# ---- 8.7 记忆生命周期（候选 → 确认/拒绝 → 已确认信息注入） ----

section "8.7 记忆生命周期（确认 / 拒绝 / 纠正 / 下一轮可见）"

# 8.5 已保存一条"回答风格"偏好候选。确认前：Profile 视图必须为空（候选不泄漏），
# 但候选列表端点可见（等管理确认）。
PROFILE="$(curl -s "http://127.0.0.1:$SERVER_PORT/api/v1/profile?user_id=debug" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
python3 - "$PROFILE" <<'PYEOF' || FAILED=1
import json, sys
d = json.loads(sys.argv[1])
if d.get("count") == 0 and not d.get("entries"):
    print("  ✅ 确认前已确认信息为空（候选记忆不泄漏进上下文数据源）")
else:
    print(f"  ❌ 确认前 Profile 应为空，实际 {d}")
    sys.exit(1)
PYEOF

# 无令牌访问必须 401。
CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$SERVER_PORT/api/v1/memory/candidates")
if [ "$CODE" = "401" ]; then
    echo "  ✅ 候选列表端点要求管理令牌（无令牌 401）"
else
    echo "  ❌ 候选列表端点无令牌应 401，实际 $CODE"
    FAILED=1
fi

CAND_LIST="$(curl -s "http://127.0.0.1:$SERVER_PORT/api/v1/memory/candidates?status=candidate" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
CAND_ID=$(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); rows=[c for c in d.get("candidates",[]) if c["status"]=="candidate" and c.get("key")=="回答风格"]; print(rows[0]["id"] if rows else "")' "$CAND_LIST")
if [ -n "$CAND_ID" ]; then
    echo "  ✅ 候选列表端点可见（含稳定槽位 key：回答风格）"
else
    echo "  ❌ 应能列出 8.5 保存的候选"
    FAILED=1
fi

# 确认 → v1 生效；重复确认幂等。
CONFIRM="$(curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/memory/confirm" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
    -d "{\"candidate_id\": \"$CAND_ID\"}")"
python3 - "$CONFIRM" <<'PYEOF' || FAILED=1
import json, sys
d = json.loads(sys.argv[1])
if d.get("status") == "confirmed" and d.get("key") == "回答风格" and d.get("version") == 1:
    print("  ✅ 确认生效：写入已确认信息（回答风格 v1）")
else:
    print(f"  ❌ 确认结果不符: {d}")
    sys.exit(1)
PYEOF

AGAIN="$(curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/memory/confirm" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
    -d "{\"candidate_id\": \"$CAND_ID\"}")"
python3 - "$AGAIN" <<'PYEOF' || FAILED=1
import json, sys
d = json.loads(sys.argv[1])
if d.get("status") == "already_confirmed":
    print("  ✅ 重复确认幂等（already_confirmed，不产生新版本）")
else:
    print(f"  ❌ 重复确认应幂等，实际 {d}")
    sys.exit(1)
PYEOF

# 称呼槽位还没确认：提问时模型只能答"没有"（候选正文不会被注入）。
ask "你怎么称呼我" > "$WORK_DIR/ask_slot_before.json"
python3 - "$WORK_DIR/ask_slot_before.json" <<'PYEOF' || FAILED=1
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
answer = d.get("answer") or ""
if d.get("support_level") == "insufficient" and "确认" in answer and "没有" in answer:
    print("  ✅ 未确认的槽位诚实回答没有（候选正文不进上下文）")
else:
    print(f"  ❌ 未确认槽位应回答没有确认记录，实际: {d.get('support_level')} {answer}")
    sys.exit(1)
PYEOF

# 保存称呼偏好 → 确认 → 再问：回答包含确认值（确认后下一轮可见，端到端）。
ask "记住：以后叫我梁哥" > "$WORK_DIR/ask_slot_save.json"
CAND2_LIST="$(curl -s "http://127.0.0.1:$SERVER_PORT/api/v1/memory/candidates?status=candidate" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
CAND2_ID=$(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); rows=[c for c in d.get("candidates",[]) if c.get("key")=="称呼"]; print(rows[0]["id"] if rows else "")' "$CAND2_LIST")
if [ -z "$CAND2_ID" ]; then
    echo "  ❌ 称呼候选应已保存"
    FAILED=1
else
    curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/memory/confirm" \
        -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
        -d "{\"candidate_id\": \"$CAND2_ID\"}" > /dev/null
    ask "你怎么称呼我" > "$WORK_DIR/ask_slot_after.json"
    python3 - "$WORK_DIR/ask_slot_after.json" <<'PYEOF' || FAILED=1
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
answer = d.get("answer") or ""
if "梁哥" in answer and (d.get("support_level")) == "supported":
    print("  ✅ 确认后下一轮可见：回答依据已确认信息（含来源标注）")
else:
    print(f"  ❌ 确认后应能在回答中看到已确认称呼，实际: {d.get('support_level')} {answer}")
    sys.exit(1)
PYEOF
fi

# 纠正走"拒绝"也要守住状态机：新候选被拒绝后，确认尝试必须 409，Profile 不变。
ask "记住：以后叫我老王" > /dev/null
CAND3_LIST="$(curl -s "http://127.0.0.1:$SERVER_PORT/api/v1/memory/candidates?status=candidate&limit=10" \
    -H "Authorization: Bearer $ADMIN_TOKEN")"
CAND3_ID=$(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); rows=[c for c in d.get("candidates",[]) if c.get("key")=="称呼"]; print(rows[0]["id"] if rows else "")' "$CAND3_LIST")
if [ -n "$CAND3_ID" ]; then
    curl -s -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/memory/reject" \
        -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
        -d "{\"candidate_id\": \"$CAND3_ID\", \"reason\": \"叫错了\"}" > /dev/null
    CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$SERVER_PORT/api/v1/memory/confirm" \
        -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
        -d "{\"candidate_id\": \"$CAND3_ID\"}")
    if [ "$CODE" = "409" ]; then
        echo "  ✅ 已拒绝的候选不能再确认（409，拒绝是终态）"
    else
        echo "  ❌ 拒绝后确认应 409，实际 $CODE"
        FAILED=1
    fi
    PROFILE_AFTER="$(curl -s "http://127.0.0.1:$SERVER_PORT/api/v1/profile?user_id=debug" \
        -H "Authorization: Bearer $ADMIN_TOKEN")"
    python3 - "$PROFILE_AFTER" "$CAND3_ID" <<'PYEOF' || FAILED=1
import json, sys
d = json.loads(sys.argv[1])
bad = sys.argv[2]
values = [e["value"] for e in d.get("entries", []) if e.get("key") == "称呼"]
if len(values) == 1 and "老王" not in values[0]:
    print("  ✅ 拒绝的候选不产生投影：称呼槽位只有确认值一个")
else:
    print(f"  ❌ 称呼槽位应只有确认值，实际 {values}")
    sys.exit(1)
PYEOF
else
    echo "  ❌ 老王候选应已保存"
    FAILED=1
fi

# /api/v1/ask 返回截断元信息字段（可解释性）。
ask "你好" > "$WORK_DIR/ask_truncations.json"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1],encoding="utf-8")); assert "context_truncations" in d, d' \
    "$WORK_DIR/ask_truncations.json" || { echo "  ❌ /api/v1/ask 应返回 context_truncations 字段"; FAILED=1; }

# ---- 9. 数据最小化 ----

section "9. 发往模型的数据最小化"

if [ -f "$WORK_DIR/deepseek_requests.jsonl" ]; then
    python3 - "$WORK_DIR/deepseek_requests.jsonl" <<'PYEOF' || FAILED=1
import json, sys

failed = 0
lines = [l for l in open(sys.argv[1], encoding="utf-8").read().splitlines() if l.strip()]
forbidden = ["/Users/", "/home/", "device_token", "clipboard", "screenshot",
             "source_code", "window_title", "absolute_path", "github.com", "Bearer"]
hits = set()
for line in lines:
    for token in forbidden:
        if token in line:
            hits.add(token)
if hits:
    print(f"  ❌ 发往模型的内容包含敏感信息: {sorted(hits)}")
    failed = 1
else:
    print(f"  ✅ 全部 {len(lines)} 次模型请求都不含路径、凭证、URL 或禁用字段")

# 总结请求必须只带聚合后的字段。
summary_users = []
for line in lines:
    body = json.loads(line)
    system = body["messages"][0]["content"]
    if "tool_calls" not in system and "support_level" not in system:
        summary_users.append(body["messages"][-1]["content"])
if not summary_users:
    print("  ❌ 未捕获到总结请求")
    failed = 1
else:
    required = ["projects", "duration_minutes", "apps"]
    missing = [k for k in required if k not in summary_users[0]]
    if missing:
        print(f"  ❌ 总结请求缺少必要的聚合字段: {missing}")
        failed = 1
    else:
        print("  ✅ 只包含聚合后的 Session 上下文（项目、时长、应用）")
sys.exit(failed)
PYEOF

else
    fail "未捕获到模型请求"
fi

# ---- 10. 备份 ----

section "10. 数据库备份"

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

# ---- 11. 敏感信息不落库、不落日志 ----

section "11. 敏感信息不落库、不落日志"

python3 - "$WORK_DIR" <<'PYEOF' || FAILED=1
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
