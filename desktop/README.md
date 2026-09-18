# desktop — macOS 采集端

V0.1 使用 Python 3.13 + pyobjc + SQLite，以源码和 launchd 方式运行。

## 范围

- Window：应用、起止时间、duration；完整标题默认不上送；
- Idle：active / idle / locked 状态区间；
- Git：显式 repo_roots 内的仓库、branch、commit 元数据；
- Privacy：字段 allowlist、路径裁剪、应用黑名单；
- Storage：events、sync_queue、settings；
- Sync：每 5 分钟或达到批量阈值时上传，逐事件 ACK。

不实现 Clipboard、Screen、Tray、Timeline、安装包和服务端下行命令。

## 什么时间不算工作

真实数据出过一次账目错误：用户晚上停止输入后锁屏，采集端把 `loginwindow`
当成前台应用记录了整夜，第二天早上解锁时才补出一条 12.5 小时的 active 事件，
于是锁屏时间被算成了工作。现在的行为是：

1. **Idle 状态区间**（`idle.state`）：
   `timestamp` 是区间开始，`data.duration_seconds` 是区间长度，缺省表示区间未结束。
   `idle` 区间的起点取**最后一次输入**的时刻，`locked` 区间起点是锁屏时刻。

2. **锁屏 / idle 后停止窗口计时**：
   Idle 传感器在状态变化时通知 Window 传感器挂起（`suspend`），并结算当前活动段；
   恢复 `active` 后才重新开始计时。Window 传感器自身每次轮询也会探测锁屏，
   作为第二层防御（即使 Idle 传感器被关掉也不会漏）。

3. **系统伪应用永不计数**：
   `loginwindow`、`ScreenSaverEngine`、`SecurityAgent`、`CoreAuthUI` 等
   出现在前台不代表用户在工作。清单在 `sensors/macos.py`，必须与服务端
   `server/internal/events/validate.go` 保持一致。

4. **系统睡眠不算活跃**：
   墙钟在睡眠期间照常前进、单调钟不会，两者之差超过阈值就判定进程被挂起过；
   此时 `active` 区间只算到最后一次观测时刻，不会把睡眠算进去。

## 模块

```text
sensors/ → event/ → privacy/ → storage/ → sync/
```

传感器不得直接访问网络；device_token 存 macOS Keychain。

## 安装与运行

```bash
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt

.venv/bin/python -m lumen_desktop init-config        # 生成 ~/.lumen/config.json
.venv/bin/python -m lumen_desktop doctor             # 环境自检
.venv/bin/python -m lumen_desktop register --token <enrollment-token>
.venv/bin/python -m lumen_desktop run                # 前台运行
```

## CLI 命令

```text
lumen-desktop run          运行采集（Ctrl-C 退出）
lumen-desktop status       查看状态（--json 输出结构化结果）
lumen-desktop pause        暂停采集（不产生新的工作事件）
lumen-desktop resume       恢复采集
lumen-desktop sync-now     立即同步一次
lumen-desktop register     注册设备（--token 提供一次性令牌）
lumen-desktop init-config  生成配置样例（不含任何密钥）
lumen-desktop events       查看最近事件（自查实际会上传什么）
lumen-desktop doctor       环境自检（依赖、权限、连通性）
lumen-desktop reset-token  删除本地 device_token
```

`pause` / `resume` 通过 SIGUSR1 / SIGUSR2 通知运行中的进程，无需重启。

## 配置

配置文件默认位于 `~/.lumen/config.json`，权限 0600。**该文件不保存任何密钥**，
device_token 存放在 macOS 钥匙串（service 名为 `com.lumen.desktop`）。

关键配置项：

| 配置 | 说明 |
| --- | --- |
| `server_url` | 服务端地址 |
| `repo_roots` | Git 扫描目录，只扫描这些目录，绝不递归整盘 |
| `app_blacklist` | 黑名单应用只记录 idle，不产生 window 事件 |
| `project_keywords` | 项目白名单；只有命中关键词的窗口标题才会提取出项目名 |
| `idle_threshold_seconds` | 进入 idle 的阈值，默认 300 秒；达到后停止窗口计时 |
| `window_checkpoint_seconds` | 窗口活动的落盘间隔，默认 60 秒 |
| `queue_soft_limit_mb` / `queue_hard_limit_mb` | 队列告警与暂停采集阈值 |

## launchd 自启示例

保存为 `~/Library/LaunchAgents/com.lumen.desktop.plist`，把 `YOUR_NAME` 换成实际用户名：

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>com.lumen.desktop</string>
    <key>ProgramArguments</key>
    <array>
        <string>/Users/YOUR_NAME/Documents/Project/lumen/desktop/.venv/bin/python</string>
        <string>-m</string><string>lumen_desktop</string><string>run</string>
    </array>
    <key>WorkingDirectory</key>
    <string>/Users/YOUR_NAME/Documents/Project/lumen/desktop</string>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>StandardOutPath</key><string>/Users/YOUR_NAME/.lumen/lumen.out.log</string>
    <key>StandardErrorPath</key><string>/Users/YOUR_NAME/.lumen/lumen.err.log</string>
</dict>
</plist>
```

加载：`launchctl load ~/Library/LaunchAgents/com.lumen.desktop.plist`

## 权限说明

- **辅助功能权限**（系统设置 → 隐私与安全性）：用于读取窗口标题。未授权时自动降级为只记录应用名，不影响其他功能；
- **钥匙串**：保存 device_token，首次写入可能弹出授权提示。

## 验收

真实运行 4 小时；断网 30 分钟后补传；同一事件重复上报不重复入库；空闲 CPU < 1%；连续 7 天无崩溃和持续内存增长。
