# 部署与恢复

## 本机运行与升级

Python 3.11+，在仓库根目录安装 `requirements.txt`，将本地配置注入进程，执行 `python3 -m lumen`。飞书 SDK 是唯一需要安装的一组渠道依赖，模型推理由远端 API 完成。

已有 v0.01.1 部署继续使用原 SQLite 和模型、飞书凭据。先停服务并备份，再 `git pull origin main`、安装依赖、重启。增量升级不清空个人数据；版本更早的旧 Go 事件库不是本版本的数据源，不能当个人事务库升级。

v1.1.0 会增加捕获诊断、模型调用用途、删除权限与确认请求的数据表/字段。已有 Soul/日常写入权限保留；五类模型删除默认需要确认，在网页「今日」调整。旧敏感记忆会被停止向模型提供，本地内容保留，需自行检查、修改或删除。自动捕获每天默认最多 24 次尝试、为普通回答预留 20 次总额度，配置见环境样例。

```bash
# 停止当前服务后运行（在仓库目录）
python3 -m lumen --db /你的路径/lumen-v001.db --backup /你的路径/backups
python3 -m lumen --db /你的路径/lumen-v001.db --check
```

## Linux 常驻服务

`lumen.service` 是 systemd 模板。预先创建 `lumen` 系统用户，将代码放到 `/opt/lumen`，在其中创建 `.venv` 并安装依赖；让服务用户可读代码，创建且授权其读写 `/var/lib/lumen`。配置文件 `/etc/lumen/lumen.env` 仅放主机上，权限 0600 root:root，systemd 会在降权前读取。

将环境样例中的 `LUMEN_DB_PATH` 改为 `/var/lib/lumen/lumen-v001.db`，`LUMEN_BACKUP_DIR` 改为 `/var/lib/lumen/backups`。复制 unit 到 `/etc/systemd/system/lumen.service`，`systemctl daemon-reload` 后 `systemctl enable --now lumen`。

```bash
systemctl status lumen
journalctl -u lumen -n 50
curl http://127.0.0.1:8787/api/health
```

默认 localhost，可使用 SSH 隧道看网页。对外监听需要至少 24 字符的访问令牌和 HTTPS。反向代理到 localhost 并保留自定义 Host 时，将域名加入 `LUMEN_ALLOWED_HOSTS`。飞书只需要出站 HTTPS/WebSocket，不需要公网回调。

## 备份、导出与恢复

- 网页「今日」下载 JSON 导出或 SQLite 一致性备份，需要同一访问令牌。
- 默认每天自动 SQLite 备份，保留最近七份 `lumen-auto-*`；手动备份不轮转。默认目录为数据库旁边的 `backups/`，可配置。
- `--backup`、`--export`、`--restore` 是离线管理操作。先停止服务；同一数据库的运行锁会拒绝第二个进程。

```bash
python3 -m lumen --db data/lumen-v001.db --export /安全路径/lumen-export.json
python3 -m lumen --db data/lumen-v001.db --restore /安全路径/lumen-backup.db
python3 -m lumen --db data/lumen-v001.db --check
```

恢复前检查备份完整性和必要数据表，使用临时文件一致性恢复；已有目标库会先另存为 `lumen-pre-restore-*`。恢复后再启动服务。自动备份失败会写错误日志；不要仅因为进程活着就认为备份成功。

数据文件权限 0600，备份目录 0700，systemd 使用 0077 umask。SQLite、导出和备份含个人信息，应用不做加密，异地备份需要自行加密并保管。

## 回滚

代码在 GitHub 每个阶段都有独立提交。回滚代码前停止进程，并恢复对应版本的 SQLite 备份；新字段通常向后兼容，但不要把代码回滚当成完整的数据回滚。保留旧配置和凭据，不删除个人数据。

真实平台验收见 [docs/ACCEPTANCE.md](../docs/ACCEPTANCE.md)。systemd 模板仍需要按实际主机路径、用户和权限配置，本仓库的自动化验证不替代服务器上线检查。
