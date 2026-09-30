# v0.01 运行与部署

使用 Python 3.11+，先 `python3 -m pip install -r requirements.txt`，在仓库根目录执行 `python3 -m lumen`。将 `lumen.env.example` 中配置注入进程环境；密钥只放主机本地配置，不能进入 Git。

常驻运行可使用 systemd，`WorkingDirectory` 指向仓库根目录，`ExecStart` 为 Python 的绝对路径加 `-m lumen`，通过 `EnvironmentFile` 读取本机配置，并设置 `Restart=on-failure`。定时任务依赖这个常驻进程。

从旧版切换时先备份旧库、保留旧服务配置，停止旧采集端。新版本使用独立 `lumen-v001.db`，不迁移旧事件或工作总结；飞书 App ID、Secret 和单用户 open_id 可沿用。切换时停止旧机器人进程，避免同一消息被新旧服务重复处理。旧部署文件存放在 `legacy/deploy/`，不能直接用于新版本。

默认仅监听 localhost，可通过 SSH 隧道访问。对外监听需设置访问令牌并配置 HTTPS 反向代理，推荐仅允许自己的设备访问。

备份：停服务后复制整个数据目录；在线备份使用 SQLite 的 backup API（不要只复制 WAL 模式下的 .db 文件）。恢复时停止进程、替换备份、重新启动。业务库包含个人信息和完整聊天，备份需妥善保管。
