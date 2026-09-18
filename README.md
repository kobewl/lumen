# Lumen

> Privacy-first Personal AI Context & Memory System  
> 当前状态：V0.1 已部署到生产并跑通真实链路 ｜ macOS First ｜ 单用户 ｜ 2C2G VPS

Lumen 在 Mac 上低打扰地采集工作元数据，在本地完成最小化和隐私过滤，再批量同步到自己的服务器。服务端把事件聚合成 Work Session，并每天调用一次 **DeepSeek API** 生成工作总结。

一句话目标：**不用回忆，也能知道昨天做到哪里。**

## 当前实现状态

第一版代码已经完成，`make e2e` 可以一键验证整条链路。

| 能力 | 状态 |
| --- | --- |
| Window / Idle / Git 三个传感器 | ✅ 已实现（真实读取本机状态） |
| 本地隐私过滤与字段 allowlist | ✅ 已实现（隐私边界有专门测试覆盖） |
| 本地 SQLite 与离线同步队列 | ✅ 已实现（断网补传、退避重试） |
| 批量同步与逐事件 ACK | ✅ 已实现（at-least-once + 幂等） |
| 设备注册与鉴权 | ✅ 已实现（一次性令牌、token 只存哈希） |
| 规则 Session Engine | ✅ 已实现（合并、切断、未分类、迟到重算） |
| DeepSeek 每日总结 | ✅ 已实现（结构化输出校验、幂等、预算控制） |
| 总结查询接口 | ✅ 已实现 |
| 飞书总结推送 | ✅ 已实现（长连接、白名单、证据引用） |
| AI-first 问答（Agent Runtime） | ✅ 已实现（模型出计划 → Policy Gate → 只读能力 → 合成回答） |
| Agent 任务摘要数据源 | ✅ 已实现（ZCode CLI 入口 → 离线队列 → 幂等入库 → get_task_summaries） |
| 可配置助手身份 | ✅ 已实现（`LUMEN_ASSISTANT_NAME` 等环境变量驱动，代码不写死名字） |
| 定时任务与保留清理 | ✅ 已实现（22:30 总结、事件清理、每日快照） |
| 部署文件与备份脚本 | ✅ 已实现 |
| 真实部署与联调 | ✅ 已部署到 VPS，真实采集→同步→Session→DeepSeek 总结全链路验证通过 |
| 飞书真实联调 | ✅ 已实现（长连接、白名单、总结推送与问答全部真机验证） |
| 连续 7 天稳定性观察 | ⏳ 从 2026-09-17 开始 |

## 快速开始

```bash
make setup        # 准备环境（Python venv + pyobjc + Go 依赖）
make e2e          # 一键端到端联调（假事件 → Session → mock DeepSeek）
make check        # 全部测试 + 静态检查 + 密钥扫描
```

`make e2e` 不需要任何真实密钥，也不会产生 API 费用。

部署到 VPS 见 [deploy/README.md](deploy/README.md)；本地开发与调试见 [docs/本地开发指南.md](docs/本地开发指南.md)。

## V0.1 MVP 范围

必须完成：

- Desktop：Python 后台进程、Window / Idle / Git 传感器、本地 SQLite、隐私过滤、批量同步；
- Server：Go 单进程、SQLite、设备鉴权、事件写入、规则 Session、每日总结；
- AI：服务端通过 HTTPS 调用 DeepSeek API，VPS 不部署任何模型；
- Feishu：每日总结推送，以及自然语言问答；使用出站长连接，不新增公网回调端口。

## 两类数据源：活动记录 vs Agent 报告

Lumen 的数据分两类，可信度不同，回答里必须分开：

- **活动记录**（`window.activity` / `idle.state` / `git.activity`）：用了哪些应用、
  各多久、有哪些提交。从它推出的「完成了什么」是**推断**；
- **Agent 任务摘要**（`agent.task_summary`）：专业 Agent（如 ZCode）自己汇报的
  任务标题、状态、已产出结果与未完成事项。这是**结论**本身。

因此问「完成了什么」会走 `get_task_summaries`（标 `supported`），
问「用了什么、多久」走 `get_sessions`（标 `inferred`）。
如果模型只拿到活动记录却宣称「完成了某事」，代码会强制把支持等级降为 `inferred`
并在证据行标注来源，用户能自己判断哪部分可信。

接入说明见 [docs/Agent任务摘要接入.md](docs/Agent任务摘要接入.md)。

## 助手是怎么回答问题的（Agent Runtime）

问答不是「正则判意图 → switch 分支 → 固定文案」。默认路径是：

```text
用户消息 + Profile + 有限对话状态 + 能力目录
  → 模型生成 AgentPlan（严格 JSON Schema）
  → Policy Gate 审批（工具白名单、参数 schema、只读、调用数上限）
  → 执行受限 Capability（只能读到聚合后的时段，拿不到 SQL/Shell/原始事件）
  → 结果回交模型合成 Answer（标注 supported / inferred / insufficient / conflicted）
  → 代码校验来源、支持等级与敏感字段
```

代码不决定「用户这句话是什么意思」，只决定「模型想做的事允不允许做」。
正则在模型不可用时仅作为最小安全兜底，不是默认入口。

助手身份（名字、定位、称呼、语言、语气、主动性）全部由环境变量驱动，
换人格不需要改代码：`LUMEN_ASSISTANT_NAME`、`LUMEN_ASSISTANT_ROLE`、
`LUMEN_OWNER_DISPLAY_NAME`、`LUMEN_ASSISTANT_LANGUAGE`、`LUMEN_ASSISTANT_TONE`、
`LUMEN_ASSISTANT_PROACTIVITY`。

明确延期：剪贴板正文、截图/OCR、Episode、Reflection、长期 Memory 的确认入口、向量检索、主动提醒、多 Agent 拆分、原生 tool_calls 迁移、服务端下行命令、Tray、Timeline、安装包，以及其他平台。

## 技术基线

```text
Mac: Python + pyobjc + SQLite
  └─ Window / Idle / Git → Privacy Filter → Sync Queue
                      HTTPS Batch ↓
VPS: Caddy + Go + SQLite
  └─ Event Store → Session Engine → Daily Summary → DeepSeek API
```

2C2G 足够运行本项目。VPS 只运行 Caddy、Go 服务和 SQLite；LLM 推理由 DeepSeek API 完成。

## 仓库结构

```text
desktop/    macOS 采集、过滤、本地存储与同步（Python）
server/     API、事件存储、Session、DeepSeek 总结（Go）
protocol/   两端共享的 JSON Schema 与 golden 样例
deploy/     Dockerfile、Caddy、Compose、备份脚本
scripts/    端到端联调脚本
docs/       开发指南
```

## 隐私设计（V0.1 核心约束）

这些不是「以后再说」的目标，而是第一版就生效的机制：

- **字段 allowlist**：上传字段逐项构造，而不是先序列化再删敏感字段。将来对象里多出字段也不会意外泄露；
- **窗口标题默认不上传**：只有命中配置的项目白名单时，才把项目名作为 `context.project` 上传，标题原文始终丢弃；
- **上传前二次校验**：`validate_payload` 对每个字符串值检查路径、URL、凭证特征，任何一条不过就不上传；
- **服务端拒绝禁用字段**：`clipboard`、`screen`、`source_code`、`diff`、`window_title` 等字段出现即拒绝整条事件并记安全事件；
- **发给模型的内容最小化**：只发聚合后的项目、时长、应用名与提交信息，不含原始事件、路径或设备标识；
- **日志脱敏**：只记录事件 id、类型、状态与 token 用量，不记录完整请求正文与任何凭证；
- **密钥分层**：DeepSeek Key、飞书 App Secret 只在服务器 `/etc/lumen`，不进入 Git、镜像或构建产物；device_token 只在 macOS 钥匙串。

## 最短开发路线

1. ~~用假事件打通注册、批量同步和幂等入库~~ ✅
2. ~~接入 Window / Idle / Git，真实运行一天~~ ✅ 代码就绪，待实际运行观察
3. ~~用规则生成 Session~~ ✅
4. ~~每晚调用一次 DeepSeek API 生成总结~~ ✅
5. ~~接入飞书总结与最小问答~~ ✅ 真机验证通过，问答已改造为 AI-first Agent Runtime
6. 连续运行 7 天后再决定是否加入剪贴板和长期记忆

## 长期方向

Lumen 不以通用电脑控制为主线，而是沿着「感知 → 理解 → 记忆 → 沟通 → 谨慎行动」演进：

- V0.2：把记忆候选的确认入口做出来（现在只存候选、不自动晋升），并补多轮追问；
- V0.3：可控的剪贴板元数据、本地 Timeline 与连接器；
- V0.4：用户确认、可纠正、可遗忘的长期记忆；
- V0.5：低打扰且可解释的主动提醒；
- V1.0：多设备个人上下文伙伴，并为外部 Agent 提供最小权限 Context API。

产品与架构文档单独维护，不随代码仓库公开；仓库内的模块 README 记录的是
实现约束与踩过的坑，改代码前先读对应模块的 README。
