#!/usr/bin/env python3
"""本地开发用的假模型服务，替真实 DeepSeek 扮演"模型"这一角色。

为什么需要它
------------
手工验收要反复问同一批问题。如果每次都打真实 API，会有两个问题：
每次回答都不一样（验收结论不可复现），以及会真实花钱。
这个脚本用同样的 HTTP 协议（/chat/completions）顶在 DeepSeek 的位置，
因此被验收的是 **Lumen 的运行时**（Planner → 工具执行器（策略 + 审计）→
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

# 目录之外的工具名，与 Lumen 的工具目录保持一致。
# 出现目录外的名字（比如 run_sql）是**故意**的：用来验收 Policy Gate 会拦住它。
TOOLS_OUTSIDE_CATALOG = {"run_sql": {"query": "SELECT * FROM events"}}


def user_text(user: str) -> str:
    """从 Planner 的用户输入里取出"用户说的话"。

    这一步不能省：Planner 的输入里除了用户消息，还有系统回填的对话状态
    （"上一轮提到的项目：…""上一轮的时间范围：…"）。直接对整段文本做判断，
    会把**我们自己写进去的说明文字**误当成用户的话——
    这正是验收时抓到过一次的真实错误。
    """
    found = re.search(r"<user_message>(.*?)</user_message>", user, re.S)
    if not found:
        return user.strip()
    return unescape(found.group(1).strip())


def pick_plan(user: str, scenario_day: str) -> dict:
    """扮演模型生成计划：先理解这句话想要什么，再决定调哪些工具。"""
    text = user_text(user)

    # 越权尝试：用户要求"所有记录/原始数据"时，模拟一个想读整库的模型。
    # 这是验收 Policy Gate 的场景，不是产品里的关键词分支。
    if re.search(r"所有记录|全部记录|原始数据|整库|数据库里", text):
        return plan("recall", "用户想读全部原始记录", "custom", [
            {"name": "run_sql",
             "arguments": dict(TOOLS_OUTSIDE_CATALOG["run_sql"])},
            {"name": "get_sessions", "arguments": {"date": scenario_day}},
        ])

    # 身份类：必须先拿真实身份，不能凭提示词里的描述编造。
    if re.search(r"你是谁|你叫什么|你的名字|你能做什么|你能干啥", text):
        return plan("chat", "用户想知道我是谁", "", [
            {"name": "get_assistant_profile", "arguments": {}},
        ])

    # 问候/闲聊开场：不需要读记录。
    # 验收里它用于演示 TemporalGuard（见 synthesize 的问候逻辑）。
    if re.search(r"^(你好|您好|在吗|嗨|哈喽|hello|hi)\s*[~！？!?。.]*$", text):
        return plan("chat", "用户打招呼", "", [])

    # 时间类：模型不自己算日期，先问系统。
    if re.search(r"现在几点|今天几号|今天是星期几|哪天", text):
        return plan("chat", "用户想知道当前时间", "", [
            {"name": "get_current_time", "arguments": {}},
        ])

    # 对话状态：用户问"我们刚才聊到哪""上一轮说了什么"。
    if re.search(r"刚才|上一轮|之前聊|说到哪", text):
        return plan("chat", "用户想知道对话上下文", "", [
            {"name": "get_conversation_state", "arguments": {}},
        ])

    # 已确认信息问答：答案只来自 trusted_context 里用户确认过的偏好，
    # 不需要调用工具（演示"确认后下一轮可见"，由 Lumen 侧保证注入）。
    if re.search(r"怎么称呼|叫我什么|我的称呼|你叫我", text):
        return plan("chat", "用户想知道已确认的称呼", "", [])

    # 记忆类：用户明确要求记住某事。preference 挂本轮用户消息来源；
    # Lumen 要求 preference 必须给出稳定槽位 key（纠正时同槽位替换）。
    if re.search(r"记住|记一下|帮我记|以后都", text):
        slot = "称呼" if re.search(r"叫我|怎么称呼|称呼", text) else "回答风格"
        return plan("recall", "用户希望记住一条偏好", "today", [
            {"name": "get_today_status", "arguments": {}},
            {"name": "save_memory_candidate", "arguments": {
                "kind": "preference",
                "key": slot,
                "content": "用户希望我记住：" + re.sub(r"[，。！？\s]", "", text)[:60],
                "confidence": 0.8,
            }},
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
        "response_style": "一句话问清楚",
    }}


def extract_blocks(user: str) -> dict:
    """从合成提示里取出各分隔块的内容。

    这里刻意依赖真实的分隔标签：如果 Lumen 不再用标签分隔不可信内容，
    这个脚本会取不到数据从而暴露出来——分隔本身也是被验收的对象。

    返回每类标签的**原文块列表**（含标签本身），供后续按需再解析。
    """
    blocks: dict[str, str] = {}
    for tag in ("user_message", "tool_data", "denied_steps"):
        found = re.findall(rf"<{tag}(?: [^>]*)?>.*?</{tag}>", user, re.S)
        if found:
            blocks[tag] = "\n".join(found)
    return blocks


def unescape(s: str) -> str:
    return (s.replace("&lt;", "<").replace("&gt;", ">")
             .replace("\\u003c", "<").replace("\\u003e", ">").replace("&amp;", "&"))


def synth_facts(blocks: dict) -> list[dict]:
    """把 <tool_data> 里的 JSON 逐段解析出来。"""
    out = []
    for chunk in re.findall(r"<tool_data(?: [^>]*)?>(.*?)</tool_data>",
                            blocks.get("tool_data", ""), re.S):
        body = chunk.strip()
        if body.endswith("…（内容过长，已截断）"):
            body = body[: -len("…（内容过长，已截断）")]
        try:
            out.append(json.loads(body))
        except json.JSONDecodeError:
            continue
    return out


def day_part_greeting(day_part: str) -> str:
    """按可信时段给出一致的问候。"""
    return {
        "早上": "早上好！", "上午": "上午好！", "中午": "中午好！",
        "下午": "下午好！", "晚上": "晚上好！", "深夜": "深夜了还没休息呀。",
    }.get(day_part, "")


def synthesize(user: str) -> dict:
    """扮演模型：只依据给定事实组织回答，并标注支持等级。

    事实按"信息量"排序后再回答，而不是按工具返回顺序取第一条：
    同一轮里可能既查了项目清单（帮助对齐名字）又查了记录，
    只回清单等于没回答问题。

    被拒的步骤必须**先**说，再讲取到的事实：用户有权知道
    "这一步没做成"，否则会以为我们查过而没有。

    问候演示：用户打招呼时先按"模型的坏习惯"回一句早上好——
    TemporalGuard 应当带纠正事实重写一次；重写提示里带"纠正要求"，
    这里读取可信时段给出一致的问候。
    """
    blocks = extract_blocks(user)
    facts = synth_facts(blocks)
    denied = unescape(blocks.get("denied_steps", ""))

    correction = re.search(r"纠正要求：(.*)", user)
    if correction:
        m = re.search(r"（周[一二三四五六日]，(早上|上午|中午|下午|晚上|深夜)", user)
        if m:
            greeting = day_part_greeting(m.group(1))
            return {"answer": f"{greeting}有什么我能帮忙的吗？", "support_level": "supported"}

    user_text = unescape(blocks.get("user_message", ""))
    if re.search(r"你好|您好|在吗|嗨|哈喽|hello|hi", user_text, re.I):
        # 坏习惯：不管时段先来一句早上好（由 TemporalGuard 负责纠正）。
        return {"answer": "早上好呀！有什么我能帮忙的吗？", "support_level": "supported"}

    # 已确认信息问答：只依据 trusted_context 里代码注入的"已确认信息"段。
    # 段不存在就诚实说没有——候选记忆绝不会被注入，因此确认前只能答"没有"。
    if re.search(r"怎么称呼|叫我什么|我的称呼|你叫我", user_text):
        m = re.search(r"- 称呼：(.+?)（第 (\d+) 版）", user)
        if m:
            return {"answer": f"按你确认过的偏好，{m.group(1)}（第 {m.group(2)} 版）。",
                    "support_level": "supported"}
        return {"answer": "我这边还没有你确认过的称呼记录。",
                "support_level": "insufficient"}

    reply = answer_from_facts(blocks, facts)
    if denied:
        # 只说"这一步没做成"，不改变支持等级：回答里的结论仍然来自取到的事实，
        # 而被拒的那一步本来就没有产生任何事实。
        reply["answer"] = "有一处请求被系统拦下了（我没有直接读原始记录或库表的权限）；" + reply["answer"]
    return reply


def answer_from_facts(blocks: dict, facts: list[dict]) -> dict:
    """按事实的信息量组织回答。"""

    def find(pred):
        for item in facts:
            if pred(item):
                return item
        return None

    # 身份：直接用工具返回的真实身份。
    profile = find(lambda f: "name" in f and "role" in f)
    if profile:
        return {"answer": f"我是{profile['name']}，{profile['role']}。",
                "support_level": "supported"}

    # 当前时间：直接陈述。
    current = find(lambda f: "day_part" in f)
    if current:
        return {"answer": f"现在是 {current['date']} {current['time']}"
                          f"（{current['weekday']}{current['day_part']}）。",
                "support_level": "supported"}

    # 记忆写入结果：只转述工具给的结论，不承诺"已经记住"。
    memory = find(lambda f: f.get("saved") is not None)
    if memory:
        if memory.get("saved"):
            return {"answer": f"记下了（候选，还没生效）：{memory['content']}",
                    "support_level": "supported"}
        return {"answer": "这条我没能记下。", "support_level": "insufficient"}

    # Agent 报告：带结论，但要说明结论是 Agent 报的。
    tasks = find(lambda f: f.get("task_summaries") is not None)
    if tasks:
        items = tasks["task_summaries"]
        if not items:
            return {"answer": "这个时间段没有收到 Agent 报告的任务结果。",
                    "support_level": "insufficient"}
        done = [i for i in items if i.get("status") == "done"]
        loops = [loop for i in items for loop in (i.get("open_loops") or [])]
        parts = []
        for item in done:
            outcomes = "、".join(item.get("outcomes") or []) or "无细节"
            parts.append(f"{item.get('source_agent', 'Agent')} 报告完成了"
                         f"「{item['title']}」（{outcomes}）")
        answer = "；".join(parts) if parts else "这次没有标记为完成的任务。"
        if loops:
            answer += f"。还没收尾的有：{'、'.join(loops)}"
        return {"answer": answer, "support_level": "supported"}

    # 活动记录：只能说"用了什么、多久"，不能包装成"完成了什么"。
    sessions = find(lambda f: f.get("sessions") is not None)
    if sessions:
        rows = sessions["sessions"]
        total = sessions.get("total_minutes", 0)
        # 显式使用总时长这个事实字段，而不是自己把各段加起来：
        # 校验"总时长等于各段之和"是 Lumen 那边的事。
        if not rows:
            return {"answer": f"这个时间段没有查到记录（合计 {total:g} 分钟）。",
                    "support_level": "insufficient"}
        lines = []
        for row in rows[:5]:
            apps = "、".join(row.get("apps") or []) or "未记录应用"
            lines.append(f"{row.get('project')} 约 {row.get('duration_minutes', 0):g} 分钟（{apps}）")
        # 如果同轮还对齐过项目清单，顺带说一句，说明查的是哪些项目。
        known = find(lambda f: f.get("projects") is not None)
        prefix = ""
        if known and known.get("projects"):
            prefix = f"（对齐到的项目：{'、'.join(known['projects'])}）"
        return {"answer": f"这段时间共 {len(rows)} 段记录、合计约 {total:g} 分钟{prefix}："
                          + "；".join(lines) + "。看起来主要在这些应用上。",
                "support_level": "supported"}

    # 项目清单：只有它时说明"有哪些项目"，不编造项目内容。
    projects = find(lambda f: f.get("projects") is not None)
    if projects:
        names = projects.get("projects") or []
        if not names:
            return {"answer": "最近没有可归类的项目记录。", "support_level": "insufficient"}
        return {"answer": f"最近出现过的项目有：{'、'.join(names)}。想看哪个项目的记录，"
                          f"告诉我就行。",
                "support_level": "supported"}

    # 对话状态。
    state = find(lambda f: "current_project" in f or "pending_question" in f or "last_time_range" in f)
    if state:
        if state.get("note"):
            return {"answer": "我们这是刚开始聊，还没有上文。",
                    "support_level": "insufficient"}
        return {"answer": f"上一轮我们聊到项目「{state.get('current_project') or '（没说项目）'}」，"
                          f"时间范围是 {state.get('last_time_range') or '未指定'}。",
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
        elif '"skip"' in system and '"basis"' in system:
            # 主动关怀提案：依据 facts 里真实出现的 task_id，
            # 没有任何依据时输出 skip（Lumen 侧会拒绝没有依据的提案）。
            ids = re.findall(r'"task_id"\s*:\s*"([^"]+)"', user)
            if ids:
                content = json.dumps({
                    "skip": False, "reason": "用户今天在推进项目",
                    "question": "今天在项目上投入了不少，进展还顺利吗？",
                    "basis": ids[:2],
                }, ensure_ascii=False)
            else:
                content = json.dumps({"skip": True, "reason": "没有可依据的记录",
                                      "question": "", "basis": []}, ensure_ascii=False)
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
