# Lumen v0.01.1

一个单用户、以对话为入口的个人 Agent。只做四件事：聊天、设置定时任务、记录个人信息、管理 Todo。

## v0.01.1：对话与记忆优化

- 更正记忆可以使用原 ID，即使调整信息名称也不会变成两条记录；最新记忆优先于旧聊天。
- 工具写入后，本轮模型立即读取最新状态。同一轮重复的相同创建操作只执行一次，减少重复 Todo 或提醒；更正操作仍按顺序执行。
- 删除记忆会重置模型的短期聊天上下文，并移除本轮旧工具读取结果；原始聊天记录仍留在本机，但不再发送给后续模型调用。其他记忆、Todo 和任务保留。
- 飞书发送 `/new` 或准确的「新对话」，或点击网页顶部「新对话」，无需调用模型即可开启新对话。旧聊天保留在数据库，面板显示新对话。
- 最近聊天增加 24,000 字符上限，保留完整消息，减少长对话的上下文费用。
- 兼容原配置 `LUMEN_ASSISTANT_NAME`、`LUMEN_ASSISTANT_TONE`。

已部署 v0.01 时，更新代码并重启现有进程即可，继续使用原数据库和凭据。启动时只新增 `settings` 表，不移动或清空个人数据；升级前建议备份数据库。

## 从零开始的范围

- **对话**：DeepSeek / OpenAI 兼容 API，原生 tool calling，多轮历史与个人记忆进入上下文。
- **记忆**：明确要求「记住」时保存；同名信息覆盖，可查看、修改、删除。
- **Todo**：自然语言或面板添加、改标题、标记完成、删除，支持截止时间。
- **定时任务**：一次、每天、每周；提醒原文或到时执行 Agent 指令；可暂停、重新启用、删除，保留结果和错误。

飞书个人聊天是 v0.01 的主要对话入口，沿用出站长连接；网页用于管理和调试。所有数据存在本机 SQLite，服务重启后仍保留。没有模型密钥时，面板、Todo、记忆和普通定时提醒仍能使用；聊天和定时 Agent 任务需要模型。

旧版桌面采集、工作总结和证据链实现已移至 `legacy/`，不参与新版本运行。旧业务数据库不自动迁移；新数据库默认为 `data/lumen-v001.db`，请勿指向旧库。部署前请停止旧版采集端，旧服务可以保留用于查历史。

## 运行

Python 3.11+，飞书入口使用官方 SDK；网页核心只需标准库，无需安装 Node、Go 或本地模型。

```bash
# 在仓库根目录运行；密钥从环境变量读取
python3 -m pip install -r requirements.txt
export LUMEN_MODEL_API_KEY='你的模型密钥'
export LUMEN_MODEL_BASE_URL='https://api.deepseek.com'
export LUMEN_MODEL='deepseek-chat'
export LUMEN_FEISHU_APP_ID='你的飞书 App ID'
export LUMEN_FEISHU_APP_SECRET='你的飞书 App Secret'
export LUMEN_FEISHU_ALLOWED_USER_IDS='你的 open_id'
python3 -m lumen
```

在飞书中给机器人发个人消息即可对话；打开 http://127.0.0.1:8787 管理记忆和任务。飞书配置全部留空时仅启动网页。`deploy/lumen.env.example` 列出可选配置；程序不自动加载 .env，需要由 shell / 服务管理器注入。

使用 OpenAI 时设置 `LUMEN_MODEL_BASE_URL=https://api.openai.com/v1`，并配置支持 tool calling 的模型名称。其他兼容提供商同理，Base URL 应为 `/chat/completions` 前的部分。

远程访问时设置 `LUMEN_HOST=0.0.0.0` 和至少 24 字符的 `LUMEN_ACCESS_TOKEN`；网页顶部输入访问令牌。公网部署需要通过反向代理启用 HTTPS。令牌仅保存在当前浏览器会话，模型密钥不会返回给浏览器。

## 飞书配置

模型配置也兼容旧版 `LUMEN_DEEPSEEK_API_KEY`、`LUMEN_DEEPSEEK_BASE_URL` 和 `LUMEN_DEEPSEEK_MODEL`，新 MODEL 变量优先。

沿用旧版 `LUMEN_FEISHU_APP_ID`、`LUMEN_FEISHU_APP_SECRET`、`LUMEN_FEISHU_ALLOWED_USER_IDS`。v0.01 只允许一个用户 open_id，只响应他的个人文本消息，忽略群聊和其他用户。飞书开发者后台需要启用机器人、开启长连接事件接收、订阅 `im.message.receive_v1`，并授权机器人收发个人消息（`im:message.p2p_msg:readonly`、`im:message:send_as_bot`），发布应用版本并让自己在可用范围内。

入站消息用 message_id 持久化去重，重连重投不会重复调用模型。机器人回复和定时结果写入持久化发送队列，发送失败会退避重试（最长每小时一次）。飞书发送使用稳定 UUID 降低重试重复，仍依赖平台去重窗口，不保证永久 exactly-once。执行中崩溃的入站消息不会自动重做，请在网页检查已有写入后重新发消息。网络需要允许飞书 HTTPS 和官方长连接 WebSocket 域名。

## 可以直接说

- 「记住：我喜欢简洁的回答。」
- 「帮我记一个 Todo：整理本周计划。」
- 「把整理本周计划标记为完成。」
- 「明天早上 9 点提醒我交房租。」
- 「每天晚上 8 点帮我整理还没完成的 Todo，给出明天的建议。」
- 「忘记我的饮食偏好。」

## 定时执行约定

- 时间默认使用 `Asia/Shanghai`，可通过 `LUMEN_TIMEZONE` 修改。面板时间按服务配置的时区输入，不按浏览器时区。
- 调度每秒检查一次。服务停止时无法执行，恢复后会补执行到期任务；重复任务积压只执行一次，再跳到下一次未来时间。
- 结果写入对话和执行记录。浏览器每 4 秒刷新；关闭浏览器不影响后台执行。配置飞书后结果主动推送给主人；没有操作系统通知。
- 每天 / 每周按配置时区的当地时间重复。未支持 cron、月度、工作日规则。
- 失败任务暂停，需要手动重新启用并提供未来时间。若执行中服务崩溃，任务标记为中断，不自动重试，避免重复写入。
- 定时 Agent 可以读取个人数据、管理 Todo 和记忆，不能再创建其他定时任务。任务运行期间不可修改或删除。
- Todo 的截止时间不会自动变成提醒，提醒需要单独创建。
- 工具写入成功后立即持久化；后续模型失败不会撤销写入，错误信息会列出已成功操作。没有分布式 exactly-once 保证。

## 开发与验证

```bash
make test        # 标准库 unittest：存储、工具、Agent、调度和 HTTP 集成
make check       # 测试、暂存区敏感信息扫描和 Python 编译检查
make run
```

```text
lumen/core.py       SQLite、工具、定时调度
lumen/agent.py      模型客户端与工具调用循环
lumen/feishu.py     飞书长连接、消息去重、持久化推送
lumen/__main__.py   HTTP API 与服务入口
lumen/static/      对话、Todo、记忆、任务面板
tests/             自动化验证
scripts/           提交前敏感信息扫描
legacy/            归档的旧实现，不属于 v0.01
```

API：`GET /api/health`、`GET /api/state`、`POST /api/conversation/new`（`{}`，开启新对话）、`POST /api/chat`（`{"message":"…"}`）、`POST /api/action`（`{"name":"add_todo","args":{"title":"…"}}`）。配置了访问令牌时，业务 API 需要 `Authorization: Bearer <token>`。

个人记忆、Todo、定时任务和最近 30 条聊天消息会发送给所配置的模型提供商。删除记忆后会同时重置模型短期上下文，避免从旧聊天再次读取。原始聊天和历史发送记录仍在本机；用户重新提及该信息时会作为新输入进入模型。删除不是本机原始记录或外部模型提供商数据删除。数据量超过上下文限制时会提示清理，v0.01 不做向量检索或自动摘要。

v0.01 不提供桌面监控、电脑控制、Shell 工具、多 Agent、插件市场、多用户系统。先把对话与这三个个人工具做好。
