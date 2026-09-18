# deploy — 2C2G VPS 部署

V0.1 只部署 Caddy 和 lumen-server 两个业务容器。SQLite 使用持久卷；DeepSeek 通过远程 API 调用。

## 文件

```text
Dockerfile              lumen-server 镜像（多阶段构建，非 root 运行）
docker-compose.yml      两个容器的编排
Caddyfile               TLS 终止与反向代理
lumen.env.example       环境变量样例（全部是占位符）
backup.sh               一致性快照 + 轮转 + 可选加密异地备份
```

## 首次部署

### 1. 服务器准备

```bash
sudo useradd -r -s /usr/sbin/nologin -d /var/lib/lumen lumen
sudo mkdir -p /var/lib/lumen /etc/lumen
sudo chown lumen:lumen /var/lib/lumen
sudo chown root:lumen /etc/lumen
sudo chmod 0750 /etc/lumen
```

推荐安装 Tailscale 并保持私网访问，不开放公网 SSH 与业务端口。

### 2. 配置密钥

```bash
sudo cp lumen.env.example /etc/lumen/lumen.env
sudo chown root:lumen /etc/lumen/lumen.env
sudo chmod 0640 /etc/lumen/lumen.env
sudo vi /etc/lumen/lumen.env
```

关键变量：

| 变量 | 说明 |
| --- | --- |
| `LUMEN_ENROLLMENT_TOKEN` | 一次性注册令牌，用 `openssl rand -base64 32` 生成 |
| `LUMEN_ADMIN_TOKEN` | 管理接口令牌，同样随机生成 |
| `LUMEN_DEEPSEEK_API_KEY` | DeepSeek 密钥 |
| `LUMEN_DEEPSEEK_MODEL` | 模型名通过环境变量配置，便于跟随供应方升级 |
| `LUMEN_FEISHU_APP_ID` / `LUMEN_FEISHU_APP_SECRET` | 飞书应用凭证 |
| `LUMEN_FEISHU_ALLOWED_USER_IDS` | 唯一允许的用户 ID，留空则飞书整体禁用 |

完整清单见 `lumen.env.example`。

### 3. 启动

```bash
sudo docker compose up -d
curl -k https://localhost/api/v1/healthz
```

### 4. 注册 Mac

```bash
lumen-desktop register --token <LUMEN_ENROLLMENT_TOKEN 的值>
```

注册成功后建议立即在服务器上清空 `LUMEN_ENROLLMENT_TOKEN` 并重启容器，使注册接口彻底关闭。

### 5. 配置自动备份

```bash
sudo crontab -e -u root
# 每天凌晨 3:30 备份（服务端内部已有快照逻辑，这里作为独立兜底）
30 3 * * * LUMEN_BACKUP_PASSPHRASE='...' LUMEN_BACKUP_REMOTE='user@backup-host:/backups/' /usr/local/bin/backup.sh >> /var/log/lumen-backup.log 2>&1
```

仅在同一台 VPS 上保留副本**不算**完整备份，异地副本必须加密。

## 密钥分层（重要）

```text
GitHub 构建的镜像        只有程序，不含任何密钥
服务器 /etc/lumen/       所有业务密钥只在这里
docker compose 运行时    通过 env_file 把密钥挂给容器
```

因此：

- 同一个镜像可以部署到不同环境，各自使用自己的密钥；
- 即使镜像仓库泄露，也不包含 DeepSeek 或飞书凭证；
- `docker compose pull` 与容器升级都不会覆盖 `/etc/lumen` 和 `/var/lib/lumen`。

## 运维

### 日常检查

```bash
curl -k https://localhost/api/v1/healthz | jq
# 关注：db 状态、last_event_at、last_summary、counts
df -h /var/lib/lumen
```

### 日志

```bash
sudo docker compose logs -f lumen-server
sudo docker compose logs --tail=200 lumen-caddy
```

日志只记录事件 id、类型、状态与 token 用量，不含完整请求正文、窗口标题或任何凭证。

### 资源

- `lumen-server` 目标 < 250MB，compose 里设了 400m 硬上限；
- 整机常驻目标 < 1GB，峰值门槛 < 1.2GB；
- 磁盘超过 80% 时先调小 `LUMEN_EVENT_RETENTION_DAYS` 并清理旧备份。

### 回滚

```bash
sudo docker compose down
LUMEN_IMAGE=ghcr.io/OWNER/lumen-server:<previous-sha> sudo docker compose up -d
```

回滚不恢复旧的 secret 文件，也不覆盖数据库备份。

## 数据恢复演练（每季度做一次）

```bash
cp /var/lib/lumen/backups/lumen-YYYYMMDD-HHMMSS.db /tmp/restore-test.db
sqlite3 /tmp/restore-test.db "PRAGMA integrity_check;"
sqlite3 /tmp/restore-test.db "SELECT date, project, start_at FROM sessions ORDER BY start_at DESC LIMIT 5;"
sqlite3 /tmp/restore-test.db "SELECT date, status, model FROM daily_summaries ORDER BY date DESC LIMIT 5;"
rm /tmp/restore-test.db
```

## 故障排查顺序

| 现象 | 排查顺序 |
| --- | --- |
| 总结失败 | `daily_summaries.status` / `error_code` → DeepSeek HTTP 状态 → 调用预算 → 输入 schema |
| 飞书失败 | 长连接状态 → 用户白名单 → App 凭证 → `notification_deliveries` 去重状态 |
| 事件缺失 | 采集端 `sync_queue` → batch 响应 → 服务端 `events` 表 |
| 服务变慢 | 磁盘与 WAL 大小 → 内存 → 外部 API 超时 |
