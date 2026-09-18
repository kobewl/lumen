# server — Lumen 服务端

V0.1 使用 Go 单进程 + SQLite + Caddy。DeepSeek 通过远程 HTTPS API 调用，VPS 不部署任何模型。

DeepSeek Base URL 与模型名均使用环境变量配置，不在代码里固定模型版本。

## 模块

```text
auth/           一次性注册令牌、device token 哈希与校验
api/            HTTP 路由、鉴权中间件、请求体限制
events/         事件字段 allowlist 校验与禁用字段拒绝
sessions/       规则 Session 聚合（唯一允许切分会话的地方）
ai/             DeepSeek 客户端、上下文组装、输出校验、文本渲染
summary/        总结编排：幂等申请 → 调用 → 持久化 → 推送
scheduler/      每日总结、事件清理、WAL checkpoint、数据库快照
assistant/      Agent Runtime：Profile、AgentPlan、Policy Gate、能力注册表、合成与校验
feishu/         飞书渠道：长连接、用户白名单、幂等、回复发送与审计落库
notification/   通知渠道抽象（V0.1 只有飞书）
storage/        SQLite 打开、迁移与各表读写
ulid/           事件 ID 生成与解析
config/         环境变量解析（密钥只从环境读取）
```

## API

```text
POST /api/v1/devices/register                      一次性 enrollment token
POST /api/v1/events/batch                          Bearer device token，逐事件 ACK
GET  /api/v1/sessions?date=YYYY-MM-DD              Bearer device token
POST /api/v1/sessions/rebuild?date=...|from=&to=   Bearer admin token，按新规则重算历史
GET  /api/v1/task-summaries?date=...|project=...     Bearer device token，Agent 任务摘要
GET  /api/v1/summaries/daily?date=YYYY-MM-DD       Bearer device token
POST /api/v1/summaries/daily/generate?date=...     Bearer admin token
GET  /api/v1/healthz                               无鉴权，只返回非敏感状态
```

默认每天 22:30 把聚合后的 Session Context 发送给 DeepSeek API，一天一次，结果通过飞书发送。飞书只响应配置的允许用户。模型或飞书失败不影响事件与 Session。

## 问答：AI-first Agent Runtime

问答的默认入口是模型计划，不是正则：用户消息 + Profile + 有限对话状态 + 能力目录
→ 模型生成 AgentPlan（严格 JSON Schema）→ Policy Gate 审批（工具白名单、参数 schema、
只读、单轮调用数上限）→ 执行只读能力 → 事实回交模型合成回答 → 代码校验来源与支持等级。

因此同一意图的各种自然语言说法都能生效，加一种说法不需要改 Go 代码。
模型不可用时只对最明确的数据请求做确定性兜底（`assistant.MinimalFallback`），
它不是第二套关键词机器人。

`DeniedToolCalls`、`ConversationState`、`MemoryCandidate` 见
[`internal/storage/conversations.go`](internal/storage/conversations.go)；
身份配置见 `config.Config.Profile()`。

### 任务摘要（agent.task_summary）

专业 Agent（如 ZCode）通过采集端的 `lumen-desktop task-summary` 提交任务摘要，
事件走与传感器事件相同的校验、离线队列与幂等入库路径，额外投影到
`agent_task_summaries` 表（按 `device_id` + `task_id` 幂等 upsert）。

它是目前唯一带**结论**的数据源，因此能力层单独提供 `get_task_summaries`，
不与 `get_sessions` 合并：混在一起会让「Agent 报告的结论」和「应用时长的推断」
在回答里分不开。合成阶段的 `support_level` 由代码收紧——只拿到活动记录却
宣称「完成了某事」会被强制降为 inferred。

只接受元数据级摘要：完整对话、终端输出、代码、diff 与凭证在协议层就没有字段，
不是传了会被过滤。`privacy_mode` 固定为 `metadata_only`。

V0.1 不实现 Memory 确认入口、Episode、Reflection、Initiative Engine、command、向量检索或管理后台。
记忆候选会写入 `memory_candidates`（状态恒为 candidate），未经用户确认不进入上下文。

## Session 切断规则

`idle.state` 描述状态区间：`timestamp` 是区间**开始**，`data.duration_seconds`
是区间长度（缺省表示未结束）。`idle` 区间起点取最后一次输入的时刻，
`locked` 区间起点是锁屏时刻。

| 情形 | 处理 |
| --- | --- |
| `locked` | 一律切断，切断点 = 区间开始 |
| `idle` ≥ 8 分钟 | 切断，切断点 = 区间开始 |
| `idle` < 8 分钟 | 不切断（看视频这类短暂无输入不算离开） |
| 离开区间内的窗口活动 | 一律裁剪，不计入工作时长 |
| 系统伪应用（`loginwindow` / 屏保 / 认证窗口） | 直接忽略 |

历史数据兼容：没有 `schema_version` 的 `idle.state` 按旧语义解释
（`timestamp` 是区间结束，区间为 `[timestamp - duration, timestamp]`），
这样 2026-09-17 那批锁屏期间产生的假活动在重算时会被正确忽略，原始事件不动。

`RebuildDay` 会向前多看 24 小时以捕捉跨夜的离开区间，但只写回「开始时刻落在
当天」的 Session，不会把前一天的工作写成今天的记录。

## 运行

```bash
go run ./cmd/lumen-server          # 本地运行
go test ./...                      # 全部测试
go build -o ../bin/lumen-server ./cmd/lumen-server
```

环境变量清单见 [../deploy/lumen.env.example](../deploy/lumen.env.example)。

## 关键实现说明

### 幂等分层

| 层次 | 机制 |
| --- | --- |
| 事件写入 | `event_id` 为主键，冲突时返回 `duplicate`（成功状态，不是错误） |
| Session 重算 | 按 `date` 整日替换式重写，同输入结果一致 |
| Session ID | 由 `日期 + 项目 + 起止时间 + 算法版本` 派生，重算不改变 ID |
| 每日总结 | `date + prompt_version + input_hash` 唯一；输入未变则复用已有结果 |
| 通知投递 | `summary_id + provider` 唯一，同一总结不重复推送 |
| 下班确认 | 不即时生成总结，只做确定性确认；避免同一天推送两条（一条残缺） |

### 错误码

服务端失败使用明确的短错误码写入数据库，而不只是写日志：`rate_limited`、
`timeout`、`invalid_json`、`date_mismatch`、`truncated`、`auth_failed` 等。
失败绝不保存半截文本当成功结果。

### 数据最小化

发往模型的内容由 `ai.BuildInput` 构造，只包含项目名、时间段、应用名、
Git 提交信息首行与时长统计。原始事件、路径、设备标识、commit hash 全量值
都不会出现在请求里。

## 2C2G 约束

lumen-server 目标 < 250MB；整机目标 < 1GB。SQLite 开 WAL，单写者；定期 checkpoint，不每日完整 VACUUM；每日备份并保留 VPS 外加密副本。
