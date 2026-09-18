#!/usr/bin/env python3
"""本地开发用的假模型服务，替真实 DeepSeek 扮演"模型"这一角色。

为什么需要它
------------
手工验收要反复问同一批问题。如果每次都打真实 API，会有两个问题：
每次回答都不一样（验收结论不可复现），以及会真实花钱。
这个脚本用同样的 HTTP 协议（/chat/completions）顶在 DeepSeek 的位置，
因此被验收的是 **Lumen 的运行时**（Planner → Policy Gate → Capability →
Synthesizer → 校验），而不是这个脚本。

它是什么、不是什么
------------------
它是**模型的替身**：阅读系统提示、输出计划 JSON、再依据给定事实组织回答。
它**不是**产品逻辑：Lumen 的 Go 代码里没有任何关键词路由；
所有"这句话什么意思"的判断都发生在这里，正如真实部署里发生在模型里。
因此这里的规则简陋是预期的——真实的语义理解由 DeepSeek 负责。

用法：
    python3 scripts/dev_mock_model.py --port 18899 --log /tmp/mock.jsonl
然后把 LUMEN_DEEPSEEK_BASE_URL 指向 http://127.0.0.1:18899
"""
from __future__ import annotations

import argparse
import json
import re
from http.server import BaseHTTPRequestHandler, HTTPServer

# 计划里允许出现的能力名，与 Lumen 的能力目录保持一致。
# 出现目录外的名字（比如 run_sql）是**故意**的：用来验收 Policy Gate 会拦住它。
CAPABILITIES_OUTSIDE_CATALOG = {"run_sql": {"query": "SELECT * FROM events"}}


def pick_plan(user: str, scenario_day: str) -> dict:
    """扮演模型生成计划：先理解这句话想要什么，再决定读哪些数据。"""
    text = user.strip()

    # 越权尝试：用户要求"所有记录/原始数据"时，模拟一个想读整库的模型。
    # 这是验收 Policy Gate 的场景，不是产品里的关键词分支。
    if re.search(r"所有记录|全部记录|原始数据|整库|数据库里", text):
        return plan("recall", "用户想读全部原始记录", "custom", [
            {"name": "run_sql",
             "arguments": dict(CAPABILITIES_OUTSIDE_CATALOG["run_sql"])},
            {"name": "get_sessions", "arguments": {"date": scenario_day}},
        ])

    # 身份类：必须先拿真实身份，不能凭提示词里的描述编造。
    if re.search(r"你是谁|你叫什么|你的名字|你能做什么|你能干啥", text):
        return plan("chat", "用户想知道我是谁", "", [
            {"name": "get_assistant_profile", "arguments": {}},
        ])

    # 指代不清：没有上下文时应该问一句而不是猜。
    if re.search(r"那个项目|它怎么样了|那边的进展", text) and "lumen" not in text:
        return clarify("用户说的项目指代不清")

    # 结果类：只有任务摘要是带结论的数据源。
    if re.search(r"完成|做完|成果|结果|没做完|未完成|卡在|还有什么没", text):
        return plan("recall", "用户想知道 Agent 报告了哪些结果", "custom", [
            {"name": "get_task_summaries", "arguments": {"date": scenario_day}},
        ])

    # 项目类：先对齐项目名，再查该项目的记录。
    match = re.search(r"([A-Za-z][A-Za-z0-9_-]{2,})\s*(?:这个)?项目", text)
    if match:
        return plan("recall", "用户想了解某个项目的进展", "last_7_days", [
            {"name": "get_known_projects", "arguments": {}},
            {"name": "get_sessions", "arguments": {"project": match.group(1)}},
        ])

    # 默认：想知道用了什么、多久。
    return plan("recall", "用户想看某个时间的记录", "custom", [
        {"name": "get_today_status", "arguments": {}},
    ])


def plan(mode: str, goal: str, time_range: str, calls: list[dict]) -> dict:
    return {"plan": {
        "mode": mode,
        "understanding": {"goal": goal, "entities": [],
                          "time_range": time_range, "confidence": 0.9},
        "tool_calls": calls,
        "needs_clarification": False,
        "clarification_question": "",
        "memory_candidates": [],
        "response_style": "先结论后依据",
    }}


def clarify(goal: str) -> dict:
    return {"plan": {
        "mode": "clarify",
        "understanding": {"goal": goal, "entities": [],
                          "time_range": "", "confidence": 0.3},
        "tool_calls": [],
        "needs_clarification": True,
        "clarification_question": "你指的是哪个项目？我这边最近看到 lumen，是它吗？",
        "memory_candidates": [],
        "response_style": "一句话问清楚",
    }}


def extract_blocks(user: str) -> dict:
    """从合成提示里取出各分隔块的内容。

    这里刻意依赖真实的分隔标签：如果 Lumen 不再用标签分隔不可信内容，
    这个脚本会取不到数据从而暴露出来——分隔本身也是被验收的对象。

    返回每类标签的**原文块列表**（含标签本身），供后续按需再解析。
    """
    blocks: dict[str, str] = {}
    for tag in ("user_message", "capability_data", "denied_steps"):
        found = re.findall(rf"<{tag}(?: [^>]*)?>.*?</{tag}>", user, re.S)
        if found:
            blocks[tag] = "\n".join(found)
    return blocks


def unescape(s: str) -> str:
    return (s.replace("&lt;", "<").replace("&gt;", ">")
             .replace("\\u003c", "<").replace("\\u003e", ">").replace("&amp;", "&"))


def synth_facts(blocks: dict) -> list[dict]:
    """把 <capability_data> 里的 JSON 逐段解析出来。"""
    out = []
    for chunk in re.findall(r"<capability_data(?: [^>]*)?>(.*?)</capability_data>",
                            blocks.get("capability_data", ""), re.S):
        body = chunk.strip()
        if body.endswith("…（内容过长，已截断）"):
            body = body[: -len("…（内容过长，已截断）")]
        try:
            out.append(json.loads(body))
        except json.JSONDecodeError:
            continue
    return out


def synthesize(user: str) -> dict:
    """扮演模型：只依据给定事实组织回答，并标注支持等级。"""
    blocks = extract_blocks(user)
    facts = synth_facts(blocks)
    denied = unescape(blocks.get("denied_steps", ""))

    # 有被拒的步骤必须说出来，不能让用户以为"查过了但没有"。
    if denied:
        return {"answer": "我没法把原始记录给出来，这一步被系统拦下了；"
                          "能查到的是聚合后的记录，需要的话我可以按天或按项目讲。",
                "support_level": "insufficient"}

    for fact in facts:
        # 身份：直接用能力返回的真实身份。
        if "name" in fact and "role" in fact:
            return {"answer": f"我是{fact['name']}，{fact['role']}。",
                    "support_level": "supported"}

        # Agent 报告：带结论，但要说明结论是 Agent 报的。
        if fact.get("task_summaries") is not None:
            items = fact["task_summaries"]
            if not items:
                return {"answer": "今天没有收到 Agent 报告的任务结果。",
                        "support_level": "insufficient"}
            done = [i for i in items if i.get("status") == "done"]
            loops = [loop for i in items for loop in (i.get("open_loops") or [])]
            parts = []
            for item in done:
                outcomes = "、".join(item.get("outcomes") or []) or "无细节"
                parts.append(f"{item.get('source_agent', 'Agent')} 报告完成了「{item['title']}」（{outcomes}）")
            answer = "；".join(parts) if parts else "这次没有标记为完成的任务。"
            if loops:
                answer += f"。还没收尾的有：{'、'.join(loops)}"
            return {"answer": answer, "support_level": "supported"}

        # 活动记录：只能说"用了什么、多久"，不能包装成"完成了什么"。
        if fact.get("sessions") is not None:
            sessions = fact["sessions"]
            total = fact.get("total_minutes", 0)
            # 显式使用总时长这个事实字段，而不是自己把各段加起来：
            # 校验"总时长等于各段之和"是 Lumen 那边的事。
            if not sessions:
                answer = f"这个时间段没有查到记录（合计 {total:g} 分钟）。"
                return {"answer": answer, "support_level": "insufficient"}
            lines = []
            for s in sessions[:5]:
                apps = "、".join(s.get("apps") or []) or "未记录应用"
                lines.append(f"{s.get('project')} 约 {s.get('duration_minutes', 0):g} 分钟（{apps}）")
            return {"answer": f"这段时间共 {len(sessions)} 段记录、合计约 {total:g} 分钟："
                              + "；".join(lines) + "。看起来主要在这些应用上。",
                    "support_level": "supported"}

    if blocks.get("user_message"):
        return {"answer": "我在，有什么想了解的？", "support_level": "insufficient"}
    return {"answer": "这次没有查到可用的记录。", "support_level": "insufficient"}


class Handler(BaseHTTPRequestHandler):
    def do_POST(self) -> None:  # noqa: N802
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length).decode("utf-8")
        if self.server.log_path:  # type: ignore[attr-defined]
            with open(self.server.log_path, "a", encoding="utf-8") as fh:  # type: ignore[attr-defined]
                fh.write(raw + "\n")

        body = json.loads(raw)
        system = body["messages"][0]["content"]
        user = body["messages"][-1]["content"]

        if "tool_calls" in system and "plan" in system:
            content = json.dumps(pick_plan(user, self.server.scenario_day),  # type: ignore[attr-defined]
                                 ensure_ascii=False)
        elif "support_level" in system:
            content = json.dumps(synthesize(user), ensure_ascii=False)
        else:
            # 每日总结链路不走本脚本的验收范围。
            content = json.dumps({"date": "", "headline": "本地验收未覆盖每日总结",
                                  "projects": [], "uncertainties": []},
                                 ensure_ascii=False)

        payload = json.dumps({
            "id": "dev-mock", "model": "dev-mock",
            "choices": [{"message": {"content": content}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 200, "completion_tokens": 60, "total_tokens": 260},
        }, ensure_ascii=False).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args) -> None:
        return


def main() -> None:
    parser = argparse.ArgumentParser(description="Lumen 本地开发假模型服务")
    parser.add_argument("--port", type=int, default=18899)
    parser.add_argument("--log", default="", help="把每个请求追加写入这个文件")
    parser.add_argument("--scenario-day", default="",
                        help="计划里检索的日期（YYYY-MM-DD），默认今天")
    args = parser.parse_args()

    import datetime
    server = HTTPServer(("127.0.0.1", args.port), Handler)
    server.log_path = args.log  # type: ignore[attr-defined]
    server.scenario_day = args.scenario_day or datetime.date.today().isoformat()  # type: ignore[attr-defined]
    print(f"dev-mock-model 监听 http://127.0.0.1:{args.port}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
