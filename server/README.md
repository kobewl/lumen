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
feishu/         长连接机器人、用户白名单、三类最小问答
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
GET  /api/v1/summaries/daily?date=YYYY-MM-DD       Bearer device token
POST /api/v1/summaries/daily/generate?date=...     Bearer admin token
GET  /api/v1/healthz                               无鉴权，只返回非敏感状态
```

默认每天 22:30 把聚合后的 Session Context 发送给 DeepSeek API，一天一次，结果通过飞书发送。飞书只响应配置的允许用户，并支持今天、昨天、指定项目三类查询，以及问候/身份/能力/「我下班了」的确定性回复。模型或飞书失败不影响事件与 Session。

V0.1 不实现 Memory、Episode、开放域聊天、Initiative Engine、command、向量检索或管理后台。

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
