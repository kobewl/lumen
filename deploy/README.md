# 运行、升级与数据

Python 3.11+，在仓库根目录执行 `python3 -m pip install -r requirements.txt`，由 shell 或服务管理器注入 `lumen.env.example` 中的配置，启动 `python3 -m lumen`。进程保持运行，飞书长连接与定时执行才能工作。

从 v0.01.1 升级时继续使用原 SQLite 数据库和模型、飞书凭据。启动进行增量表/字段升级。重启前备份数据库，关闭旧进程后启动新进程，不要同时运行两套机器人。

systemd 的 WorkingDirectory 指向仓库根目录，ExecStart 指向 Python 解释器并附加 `-m lumen`，EnvironmentFile 指向本机配置文件，Restart=on-failure。模型密钥和飞书 Secret 留在本机配置，不提交 Git。

默认监听 localhost，可用 SSH 隧道访问网页。对外监听需要至少 24 字符访问令牌，并通过 HTTPS 反向代理访问。

备份使用 SQLite backup API；在线不要只复制 .db 文件，因为可能存在未合并的 WAL。恢复时停止服务，恢复数据库，验证 `PRAGMA integrity_check`，再启动。数据库、导出和备份包含个人信息，需要自行妥善保管。
