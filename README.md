# Lumen

一个以飞书为入口的个人事务助手：理解你说的话，记住你确认的信息，把目标变成可以跟进的事情。

当前版本 **v0.4.0**。完整发展路线与验收标准见 [路线图](docs/ROADMAP.md)。旧版活动采集实现已全部移除，Git 历史仍可追溯。

## 现在可以做什么

- **对话与记忆**：多轮聊天，记忆更正/忘记，候选确认，临时记忆有效期；新对话保留个人数据。
- **计划草稿**：提出目标后拟定可编辑步骤；网页确认或飞书 `/approve ID` 后一次性创建项目和任务，重复确认不重复创建。
- **任务与目标**：Todo 完成/改期/优先级/项目归属；可创建独立关联提醒，完成任务后提醒停止。
- **知识笔记**：保存想法、资料和摘录，按关键词搜索并读取实际记录。
- **定时任务**：一次、每天、每周提醒或 Agent 任务，后台执行，飞书持久化推送。
- **网页工作台**：管理同一套记忆、任务、项目、笔记和执行记录。

Lumen 把已确认的事实与候选记忆分开，不把猜测保存成人格。当前上下文有大小限制，需要时工具查询更多记录；模型无法访问你的电脑或其他账号。

## 快速启动

Python 3.11+。飞书使用官方 SDK，网页和业务核心只用标准库。

```bash
python3 -m pip install -r requirements.txt
export LUMEN_MODEL_API_KEY='你的模型密钥'
export LUMEN_MODEL_BASE_URL='https://api.deepseek.com'
export LUMEN_MODEL='deepseek-chat'
export LUMEN_FEISHU_APP_ID='你的飞书 App ID'
export LUMEN_FEISHU_APP_SECRET='你的飞书 App Secret'
export LUMEN_FEISHU_ALLOWED_USER_IDS='你的个人 open_id'
python3 -m lumen
```

给飞书机器人发个人文本消息，或打开 http://127.0.0.1:8787 。飞书配置全部留空时可只用网页。没有模型密钥也可用管理面板和普通提醒。

程序不自动加载 .env。配置样例见 [deploy/lumen.env.example](deploy/lumen.env.example)；旧 `LUMEN_DEEPSEEK_*` 模型变量兼容，`LUMEN_MODEL_*` 优先。默认时区 `Asia/Shanghai`，数据库 `data/lumen-v001.db`。

飞书后台启用机器人、长连接事件，订阅 `im.message.receive_v1`，授权 `im:message.p2p_msg:readonly`、`im:message:send_as_bot` 并发布应用。只允许一个 open_id，仅响应他的个人聊天，忽略群聊与其他用户。

## 试着这样说

- 「记住：我喜欢直接简洁的回答。」
- 「帮我记一条临时信息：下周之前我在上海。」
- 「创建学习项目，目标是完成数据库课程。」
- 「帮我记一个高优先级 Todo：复习事务，明天截止，今晚八点提醒我。」
- 「保存笔记：SQLite 事务要点……」
- 「找一下之前关于事务的笔记。」
- 「忘记我的临时地点。」

飞书发送 `/help` 查看不依赖模型的事务快捷命令，用 `/plans` 查看草稿、`/approve ID` 确认、`/reject ID` 取消、`/remember ID` 确认记忆。

飞书发送 `/new` 或准确的「新对话」可重置短期上下文。候选记忆在网页点「确认记住」后生效。删除和过期记忆会重置旧聊天上下文，但原始记录仍留在本机数据库。

## 升级、部署与数据

从 v0.01.1 更新代码后重启原进程即可，保留原凭据和数据库。启动会增加表和字段，不清空现有数据。部署前使用 SQLite backup API 备份，不要在线只复制 WAL 模式的 .db 文件。

远程监听需至少 24 字符的 `LUMEN_ACCESS_TOKEN`，网页顶部输入令牌，公网需 HTTPS。推荐本地访问或 SSH 隧道。详情见 [部署说明](deploy/README.md)。

个人记忆、最近聊天、相关事务记录会发送给你配置的模型提供商。笔记是本机保存，需要查询时才读取正文。凭据从环境读取，不返回浏览器。数据和 .env 被 Git 忽略。

## 开发

```bash
make test
make check
```

核心代码在 `lumen/`，测试在 `tests/`，研究与阶段规划在 `docs/`。`make check` 检查测试、Python 编译和 Git 暂存区的敏感信息；提交前先暂存要提交的文件。
