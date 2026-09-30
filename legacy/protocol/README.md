# protocol — Desktop 与 Server 的唯一契约

V0.1 只需要两个 JSON Schema：

- `schemas/event.schema.json`：window.activity、idle.state、git.activity；
- `schemas/sync.schema.json`：batch 请求、逐事件 accepted/duplicate/rejected 响应。

`golden/` 下是不含任何真实数据的样例，两端测试都用它验证契约一致性：

```text
golden/events.json          三条事件样例（三种类型各一条）
golden/sync_request.json    批量上传请求样例
golden/sync_response.json   逐事件 ACK 响应样例（含一个 rejected）
```

硬性约定：

1. event_id 使用客户端生成的 ULID；
2. 时间使用 UTC RFC3339；
3. 投递语义为 at-least-once，服务端按 event_id 幂等；
4. ACK 必须逐事件返回，不能用最大 ULID cursor 确认整批；
5. payload 使用字段 allowlist；
6. 禁止 Clipboard、Screen、源代码、diff、终端输出和完整窗口标题；
7. 单批压缩前 ≤ 512KB，单事件 ≤ 64KB；
8. V0.1 不定义 command.schema.json 和 Server → Desktop 下行协议。

## 事件类型与必填字段

| 类型 | privacy | context 必填 | data 必填 | 说明 |
| --- | --- | --- | --- | --- |
| `window.activity` | P0 | `app` | `duration_seconds` | 前台应用活动段 |
| `idle.state` | P0 | — | `state` | active / idle / locked 状态区间 |
| `git.activity` | P1 | `repo` | `kind` | commit 或 workspace |

`context` 允许的字段：`app`、`bundle_id`、`project`、`repo`。

`data` 允许的字段：`duration_seconds`、`checkpoint`、`state`、`schema_version`、
`branch`、`head_commit`、`commit_message`、`changed_files_count`、`kind`。

## timestamp 的语义：一律是「区间开始」

这是两端必须一致的核心约定。历史上出过一次严重的账目错误：
采集端写的是「上一段状态 + 状态变化时刻」，锁屏 12 小时被记成了 active，
22:30 的总结因此把锁屏时间算成了工作。

**`window.activity`**：`timestamp` 是活动段开始时刻，`duration_seconds` 是长度。
区间 = `[timestamp, timestamp + duration_seconds)`。

**`idle.state`**：`timestamp` 是状态区间的开始时刻，`duration_seconds` 是长度；
**没有 `duration_seconds` 表示区间尚未结束**（开放区间）。区间 = `[timestamp, ...)`。

```json
// 从 08:05:20Z 起 idle 了 512 秒
{ "type": "idle.state", "timestamp": "2026-09-17T08:05:20Z",
  "data": { "state": "idle", "schema_version": 2, "duration_seconds": 512 } }

// 从 12:42:30Z 起锁屏，尚未解锁
{ "type": "idle.state", "timestamp": "2026-09-17T12:42:30Z",
  "data": { "state": "locked", "schema_version": 2 } }
```

`idle` 区间的起点必须是**最后一次输入的时刻**，而不是发现空闲的时刻；
锁屏区间同理，起点是锁屏时刻。服务端据此切断 Session，切断点就是用户
真正离开的时间。

**服务端切断规则**：`locked` 一律切断；`idle` 只有持续 ≥ 8 分钟才切断。
切断点取区间**开始**，且离开区间内的窗口活动一律被裁剪——锁屏期间不可能
有工作，无论事件是真是假。

**系统伪应用**：`loginwindow`、`ScreenSaverEngine`、`SecurityAgent` 这类
锁屏与认证窗口出现在前台不代表用户在工作。采集端不产出它们的事件，
服务端在重算时也会忽略历史数据里已有的同类事件（清单见
`server/internal/events/validate.go` 与 `desktop/lumen_desktop/sensors/macos.py`，
两处必须一致）。

### 历史数据：没有 `schema_version` 的 idle.state 是旧语义

2026-09-18 之前采集端写的是「上一段状态 + 状态变化时刻」，
例如真机数据 `{"state":"locked","duration_seconds":45094}`：
它的 `timestamp`（`2026-09-18T01:14:04Z`，本地 09:14 解锁）是**区间结束**，
区间实际是本地 2026-09-17 20:42:30 ~ 2026-09-18 09:14:04。

```text
旧语义区间 = [timestamp - duration_seconds, timestamp]
```

服务端按 `schema_version` 是否存在区分两种语义：

| `data.schema_version` | 语义 | 区间 |
| --- | --- | --- |
| `>= 2` | 新语义 | `[timestamp, timestamp + duration_seconds)` |
| 缺省 | 旧语义 | `[timestamp - duration_seconds, timestamp]` |

历史原始事件**不删除**：`sessions` 引擎在重算时按上表还原区间，并忽略
锁屏/登录窗口产生的假活动，因此重算结果不再受旧数据影响。
算法版本升到 `rules-v2`，Session ID 会随之重新派生，旧的脏 Session 自动被清理。

## 响应状态语义

| 状态 | 客户端动作 |
| --- | --- |
| `accepted` | 标记为已同步 |
| `duplicate` | 标记为已同步（**成功**，不是错误） |
| `rejected` | 记录错误码并停止重试；`code` 为 `storage_error` 等可重试错误除外 |

客户端**只**把 `accepted` 和 `duplicate` 标记为已同步。批次中某条没有出现在
响应里时，必须保持待同步并重试，不能假定成功。

## 时钟异常

服务端保存客户端的 `timestamp` 与自己的 `received_at`。当客户端时间比服务器
超前超过 `LUMEN_CLOCK_SKEW_TOLERANCE_SECONDS`（默认 300 秒）时：

- 该事件的响应带上 `clock_skew: true`；
- 服务端使用 `received_at` 参与 Session 聚合；
- 原始 `timestamp` 保留用于诊断，不丢弃。

## 修改协议时请注意

协议字段语义变化必须升级 API/schema 版本，并同步更新：

1. 两个 Schema 文件；
2. `golden/` 样例；
3. 服务端 `internal/events/validate.go` 的 allowlist；
4. 采集端 `lumen_desktop/event.py` 的 allowlist；
5. 两端对应的测试。
