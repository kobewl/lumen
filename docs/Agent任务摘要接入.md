# 接上 Agent 任务摘要（ZCode）

这份文档说明专业 Agent（先支持 ZCode）如何把自己的工作结果提交给 Lumen。

## 为什么需要它

Lumen 原来的三类事件只能说明「用了哪些应用、各多久」。从这些痕迹推不出
「完成了什么」——那是推断，不是事实。任务摘要补上了这个缺口：
它是 Lumen 目前**唯一带结论**的数据源。

因此在回答里两类数据的措辞必须不同，这一点由代码强制保证：

| 数据源 | 能回答 | 支持等级 |
| --- | --- | --- |
| `get_task_summaries` | 完成了什么、结果是什么、还有什么没做完 | `supported` |
| `get_sessions` / `get_today_status` | 用了什么、多久、在哪些应用上花时间 | `inferred` |

如果模型只拿到活动记录却在回答里宣称「完成了某功能」，代码会把支持等级
从 `supported` 强制降为 `inferred`，并在证据行里标注来源，用户因此能自己判断。

## 提交方式

命令行，一条命令提交一个任务：

```bash
lumen-desktop task-summary \
  --task-id "zcode-2026-09-18-001" \
  --title "给事件协议加任务摘要类型" \
  --status done \
  --outcome "新增 agent.task_summary 事件类型" \
  --outcome "补齐协议 schema 与 golden 样例" \
  --open-loop "尚未接入真实 ZCode 上报" \
  --project lumen \
  --source-agent zcode-cli \
  --source-session-id sess-9f3c1d8b
```

提交后事件进入本地离线队列，由 Lumen 的同步循环投递到服务器——
因此 ZCode 在没有网络、或 Lumen 采集进程没在运行时完成任务也不会丢数据。

常用参数：

| 参数 | 说明 |
| --- | --- |
| `--task-id` | 必填。任务在 ZCode 内的稳定 ID，是服务端的幂等键 |
| `--title` | 必填。单行标题，最多 120 字 |
| `--status` | `done` / `partial` / `blocked` / `abandoned` / `unknown`，默认 `unknown` |
| `--outcome` | 已产出结果，可多次指定或用多行文本；最多 8 条，每条 120 字 |
| `--open-loop` | 未完成事项 / 下一步，规则同 `--outcome` |
| `--project` | 项目名；不传则不写项目 |
| `--occurred-at` | 发生时间（RFC3339），默认当前时间 |
| `--sync-now` | 提交后立即尝试同步一次（可选） |
| `--json` | 以 JSON 输出结果，便于脚本解析 |

同一任务重复提交是安全的：服务端按 `(device_id, task_id)` 幂等 upsert。
ZCode 先报 `partial`、结束时再报 `done` 是推荐做法，用户只会看到最新状态。

## 能提交什么、不能提交什么

这是**元数据级摘要**，只允许：

- 任务标题、状态；
- 已产出的结果（短句，如「新增 X 能力」）；
- 未完成事项 / 下一步；
- 来源 Agent 标识与会话 ID（仅用于追溯）。

明确**不接受**（这些不是"传了会被过滤"，而是字段根本不存在）：

- 完整对话记录、prompt 与 response 原文；
- 终端输出、命令与日志正文；
- 代码、diff、patch；
- 文件内容、绝对路径；
- 凭证、token、密钥。

边界在两处强制执行，本地就是第一道：

1. **采集端**：字段 allowlist 按事件类型分派，`conversation`、`diff`、`code`
   这类字段名直接抛 `PrivacyError`；标题与条目有长度上限，超长即拒绝而不是截断；
   上传前还会扫描路径、URL 与凭证特征。
2. **服务端**：同样的字段校验 + 敏感值扫描（含 `data` 里的自由文本与其数组元素），
   不通过就拒绝整条事件并记安全事件。

`privacy_mode` 字段固定为 `metadata_only`，服务端只认这一个值，
因此协议上不存在「先传正文以后再说」的路径。

## 事件长什么样

```json
{
  "id": "01J9Z4QK7M3F8N2P5R7T9V1X3E",
  "device_id": "desktop-mac-01",
  "type": "agent.task_summary",
  "timestamp": "2026-09-17T09:42:00Z",
  "privacy": "P1",
  "context": { "app": "ZCode", "project": "lumen" },
  "data": {
    "schema_version": 1,
    "task_id": "zcode-2026-09-18-001",
    "title": "给事件协议加任务摘要类型",
    "status": "done",
    "outcomes": ["新增 agent.task_summary 事件类型", "补齐协议 schema"],
    "open_loops": ["尚未接入真实 Agent 上报"],
    "source_agent": "zcode-cli",
    "source_session_id": "sess-9f3c1d8b",
    "privacy_mode": "metadata_only"
  }
}
```

契约定义见 [`protocol/schemas/event.schema.json`](../protocol/schemas/event.schema.json)，
样例见 [`protocol/golden/events.json`](../protocol/golden/events.json)。

`timestamp` 就是任务的 `occurred_at`：一条事件只有一个权威时间点，
不在 `data` 里重复放一份，避免两者不一致。

## 查询接口

```text
GET /api/v1/task-summaries?date=YYYY-MM-DD   Bearer device token
GET /api/v1/task-summaries?project=lumen     Bearer device token
```

`source_session_id` 会出现在响应里（供追溯），但不会进入模型上下文，
也不会出现在用户可见的回复文本中。

## 问答如何用它

模型在 Planner 阶段看到能力目录后自己决定调用哪个能力。
问「完成了什么 / 做完了哪些 / 还有什么没做完」时会选 `get_task_summaries`；
问「用了什么、多久」时会选 `get_sessions`。两者可以同时调用：
任务摘要说明结果，时段说明时间投入。

新增能力不需要改 Go 代码里的任何分支——这是把语义判断交给模型的直接收益。

## 仍未实现

- **真实 ZCode 自动上报**：目前只有 CLI 入口，ZCode 需要自己调用它。
  没有接入 ZCode 的私有插件协议，也不读取它的完整会话历史。
- **本地 HTTP 入口**：只有 CLI。如果 ZCode 更适合走 localhost HTTP，
  可以按同一个 `Event.agent_task_summary` 与 `PrivacyFilter` 实现，不需要改协议。
- 任务摘要的**纠正与删除入口**：目前只能通过重报覆盖。
- 长期记忆确认入口、Episode、Reflection、主动提醒、向量检索。
