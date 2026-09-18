"""lumen-desktop 命令行入口。

命令（与设计文档一致）：
  lumen-desktop run          后台运行采集
  lumen-desktop status       查看运行状态
  lumen-desktop pause        暂停采集
  lumen-desktop resume       恢复采集
  lumen-desktop sync-now     立即同步
  lumen-desktop register     用一次性 enrollment token 注册设备
  lumen-desktop init-config  生成配置样例
  lumen-desktop events       查看最近事件（自查上传内容）
  lumen-desktop doctor       环境自检
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import signal
import subprocess
import sys
from pathlib import Path

from .config import DEFAULT_CONFIG_PATH, Config, default_config_template
from .keychain import delete_token, has_token, load_token, store_token
from .runtime import Runtime
from .storage import Storage

# 状态文件用于让 pause/resume 在进程之外也能生效。
STATE_DIR = Path.home() / ".lumen"
STATE_FILE = STATE_DIR / "state.json"


def _setup_logging(level: str) -> None:
    """配置日志：默认输出到 stderr，格式简洁。

    注意：日志只记录事件 id/type/status，绝不记录完整 data。
    """
    logging.basicConfig(
        level=getattr(logging, level.upper(), logging.INFO),
        format="%(asctime)s %(levelname)s [%(name)s] %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )


def _read_state() -> dict:
    if not STATE_FILE.exists():
        return {}
    try:
        return json.loads(STATE_FILE.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return {}


def _write_state(state: dict) -> None:
    """原子写入状态文件。

    用"写临时文件 + 替换"而不是直接覆盖：直接写入时如果进程被杀死在写入中途，
    会留下一个损坏的 JSON，之后所有读取都会失败。os.replace 在同一文件系统上是
    原子操作，不会出现半截内容。
    """
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    tmp = STATE_FILE.with_suffix(".json.tmp")
    tmp.write_text(json.dumps(state, ensure_ascii=False, indent=2), encoding="utf-8")
    os.chmod(tmp, 0o600)
    os.replace(tmp, STATE_FILE)


def _clear_state_if_owner(pid: int, stopped_at: str | None) -> None:
    """退出时清空状态，但只在状态里的 pid 仍是自己时才清。

    这是个真实的竞态：旧进程收到 SIGTERM 后要花几秒收尾，如果期间新进程已经
    启动并写入了自己的 pid，旧进程无条件清空就会把新进程的记录抹掉，
    导致 status 显示"未运行"而进程其实活着。
    """
    current = _read_state()
    if current.get("pid") in (None, pid):
        _write_state({"pid": None, "stopped_at": stopped_at})


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except (OSError, ProcessLookupError):
        return False
    return True


def _find_running_pids() -> list[int]:
    """扫描进程列表，找出真实在运行的采集进程。

    作为 state 文件之外的独立事实来源：状态文件可能因为异常退出而失准，
    而进程列表不会骗人。
    """
    try:
        result = subprocess.run(
            ["pgrep", "-f", "lumen_desktop run"],
            capture_output=True, text=True, timeout=5, check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return []
    pids = []
    for line in result.stdout.split():
        try:
            pid = int(line)
        except ValueError:
            continue
        if pid != os.getpid() and _pid_alive(pid):
            pids.append(pid)
    return pids


# ---- 各子命令 ----


def cmd_run(args: argparse.Namespace) -> int:
    """运行采集进程。"""
    config = Config.load(args.config)
    config.ensure_dirs()
    _setup_logging(config.log_level)

    # 命令行参数覆盖配置，便于临时调试。
    if getattr(args, "device_id", None):
        config.device_id = args.device_id
    if getattr(args, "server_url", None):
        config.server_url = args.server_url
    if getattr(args, "repo_root", None):
        config.repo_roots.extend(args.repo_root)

    storage = Storage(config.db_path)
    # 把关键设置同步到数据库，供其他进程读取。
    storage.set_setting("device_id", config.device_id)
    storage.set_setting("server_url", config.server_url)

    token = load_token()
    if not token:
        logging.warning("未找到 device_token，事件会先缓存在本地。请运行 lumen-desktop register 完成注册")

    runtime = Runtime(config, storage, token=token)
    pid = os.getpid()
    _write_state({"pid": pid, "started_at": runtime.status()["now"], "paused": config.paused})

    logging.info("配置摘要: %s", json.dumps({
        "device_id": config.device_id,
        "server_url": config.server_url,
        "db_path": config.db_path,
        "sensors": {
            "window": config.enable_window_sensor,
            "idle": config.enable_idle_sensor,
            "git": config.enable_git_sensor,
        },
        "repo_roots": config.repo_roots,
        "paused": config.paused,
    }, ensure_ascii=False))

    try:
        runtime.run_forever()
    finally:
        # 顺序很重要：status() 会查询数据库，必须在 close() 之前调用。
        # 之前这里先 close 再取状态，导致退出时抛 ProgrammingError，
        # 进程以非零码退出，看起来像崩溃。
        try:
            final_status = runtime.status()
            storage.set_setting("paused", final_status["paused"])
            stopped_at = final_status["now"]
        except Exception:
            logging.getLogger(__name__).exception("退出时保存状态失败")
            stopped_at = None
        finally:
            storage.close()

        _clear_state_if_owner(pid, stopped_at)
    return 0


def cmd_status(args: argparse.Namespace) -> int:
    """显示运行状态。"""
    config = Config.load(args.config)
    storage = Storage(config.db_path)

    status = {
        "device_id": config.device_id,
        "server_url": config.server_url,
        "db_path": config.db_path,
        "registered": has_token(),
        "paused": bool(storage.get_setting("paused", config.paused)),
        "counts": storage.counts(),
        "queue_mb": round(storage.queue_size_bytes() / 1024 / 1024, 3),
        "last_sync_at": storage.get_meta("last_sync_at"),
        "last_sync_error": storage.get_meta("last_sync_error"),
        "sensors": {
            "window": config.enable_window_sensor,
            "idle": config.enable_idle_sensor,
            "git": config.enable_git_sensor,
        },
    }
    # 判断是否在运行：状态文件是首选，但不能只信它——
    # 异常退出、竞态覆盖都会让它失准。用进程列表交叉验证，避免"进程在跑却显示未运行"。
    state = _read_state()
    pid = state.get("pid")
    running_pids = _find_running_pids()

    if pid and _pid_alive(int(pid)):
        status["running"] = True
        status["pid"] = pid
    elif running_pids:
        status["running"] = True
        status["pid"] = running_pids[0]
        if len(running_pids) > 1:
            # 多个实例会重复采集同一批活动，应当告警。
            status["warning"] = f"检测到 {len(running_pids)} 个采集进程，可能重复采集"
    else:
        status["running"] = False
        status["pid"] = None

    if args.json:
        print(json.dumps(status, ensure_ascii=False, indent=2))
    else:
        print(f"设备 ID      : {status['device_id']}")
        print(f"服务端       : {status['server_url']}")
        print(f"已注册       : {'是' if status['registered'] else '否（请运行 register）'}")
        print(f"运行中       : {'是' if status['running'] else '否'}")
        print(f"暂停采集     : {'是' if status['paused'] else '否'}")
        print(f"传感器       : window={status['sensors']['window']} idle={status['sensors']['idle']} git={status['sensors']['git']}")
        print(f"事件统计     : {status['counts']}")
        print(f"待同步体积   : {status['queue_mb']} MB")
        print(f"上次同步     : {status['last_sync_at'] or '（尚未同步）'}")
        if status["last_sync_error"]:
            print(f"上次错误     : {status['last_sync_error']}")

    storage.close()
    return 0


def _send_signal(sig: signal.Signals, config: Config, action: str) -> int:
    """向运行中的进程发送信号。"""
    state = _read_state()
    pid = state.get("pid")
    if not pid or not _pid_alive(int(pid)):
        print("Lumen 采集进程当前未运行。")
        return 1
    try:
        os.kill(int(pid), sig)
    except OSError as exc:
        print(f"发送信号失败: {exc}")
        return 1
    print(f"已{action}（pid={pid}）")
    return 0


def cmd_pause(args: argparse.Namespace) -> int:
    """暂停采集。运行中的进程会收到 SIGUSR1。"""
    config = Config.load(args.config)
    storage = Storage(config.db_path)
    storage.set_setting("paused", True)
    storage.close()
    return _send_signal(signal.SIGUSR1, config, "暂停采集")


def cmd_resume(args: argparse.Namespace) -> int:
    """恢复采集。运行中的进程会收到 SIGUSR2。"""
    config = Config.load(args.config)
    storage = Storage(config.db_path)
    storage.set_setting("paused", False)
    storage.close()
    return _send_signal(signal.SIGUSR2, config, "恢复采集")


def cmd_sync_now(args: argparse.Namespace) -> int:
    """立即同步一次（独立进程执行，不需要 run 在运行）。"""
    config = Config.load(args.config)
    _setup_logging(config.log_level)
    storage = Storage(config.db_path)
    token = load_token()
    if not token:
        print("未注册设备：请先运行 lumen-desktop register --token <enrollment_token>")
        storage.close()
        return 1

    runtime = Runtime(config, storage, token=token)
    result = runtime.sync_now()
    if args.json:
        print(json.dumps(result, ensure_ascii=False, indent=2))
    else:
        print(result["message"])
        if result["rejected"]:
            print(f"被拒绝 {result['rejected']} 条，详情见日志")
    storage.close()
    return 0 if result["rejected"] == 0 else 2


def cmd_register(args: argparse.Namespace) -> int:
    """使用一次性 enrollment token 注册设备。"""
    config = Config.load(args.config)
    if not args.token:
        print("请通过 --token 提供一次性 enrollment token（由服务端 LUMEN_ENROLLMENT_TOKEN 配置）")
        return 1

    import urllib.error
    import urllib.request

    url = config.server_url.rstrip("/") + "/api/v1/devices/register"
    body = json.dumps({
        "enrollment_token": args.token,
        "device_name": config.device_id,
    }).encode("utf-8")
    request = urllib.request.Request(
        url, data=body, method="POST",
        headers={"Content-Type": "application/json", "User-Agent": "lumen-desktop/0.1.0"},
    )

    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            payload = json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        try:
            detail = json.loads(exc.read().decode("utf-8"))
            print(f"注册失败({exc.code}): {detail.get('message', '')}")
        except Exception:
            print(f"注册失败: HTTP {exc.code}")
        return 1
    except (urllib.error.URLError, OSError) as exc:
        print(f"无法连接服务端: {exc}")
        return 1

    device_id = payload.get("device_id", "")
    token = payload.get("device_token", "")
    if not token:
        print("服务端未返回 device_token")
        return 1

    if store_token(token):
        print(f"注册成功，device_id={device_id}")
        print("device_token 已保存到 macOS 钥匙串，不会写入配置文件。")
    else:
        print("注册成功，但写入钥匙串失败。请检查钥匙串访问权限。")
        return 1

    # 设备 ID 可能与请求时的名称不同，同步到配置与数据库。
    if device_id and device_id != config.device_id:
        config.device_id = device_id
        config.save(args.config)
    storage = Storage(config.db_path)
    storage.set_setting("device_id", device_id)
    storage.set_setting("server_url", config.server_url)
    storage.close()
    return 0


def cmd_init_config(args: argparse.Namespace) -> int:
    """生成配置样例文件（不含任何密钥）。"""
    path = Path(args.config) if args.config else DEFAULT_CONFIG_PATH
    if path.exists() and not args.force:
        print(f"配置文件已存在: {path}（使用 --force 覆盖）")
        return 1

    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(default_config_template(), ensure_ascii=False, indent=2),
        encoding="utf-8",
    )
    os.chmod(path, 0o600)
    print(f"已生成配置样例: {path}")
    print("该文件不包含任何密钥；device_token 保存在 macOS 钥匙串中。")
    return 0


def cmd_events(args: argparse.Namespace) -> int:
    """查看最近事件，用于自查“我到底上传了什么”。"""
    config = Config.load(args.config)
    storage = Storage(config.db_path)
    events = storage.recent_events(limit=args.limit)
    storage.close()

    if args.json:
        print(json.dumps(events, ensure_ascii=False, indent=2))
        return 0
    if not events:
        print("本地还没有事件。")
        return 0

    print(f"最近 {len(events)} 条事件：")
    for item in events:
        context = ", ".join(f"{k}={v}" for k, v in item["context"].items())
        data = ", ".join(f"{k}={v}" for k, v in item["data"].items())
        print(f"  [{item['status']:>13}] {item['timestamp']} {item['type']:<16} {context} | {data}")
    print("\n提示：窗口标题不会出现在上面的内容中；只有命中项目白名单的项目名会上传。")
    return 0


def cmd_doctor(args: argparse.Namespace) -> int:
    """环境自检：依赖、权限、连通性。"""
    config = Config.load(args.config)
    checks: list[tuple[str, bool, str]] = []

    # 1. Python 版本
    py_ok = sys.version_info >= (3, 11)
    checks.append(("Python 版本 >= 3.11", py_ok, sys.version.split()[0]))

    # 2. pyobjc 可用性
    try:
        import AppKit  # noqa: F401
        checks.append(("pyobjc (AppKit)", True, "已安装"))
    except ImportError:
        checks.append(("pyobjc (AppKit)", False, "未安装，Window/Idle 传感器不可用"))

    try:
        import Quartz  # noqa: F401
        checks.append(("pyobjc (Quartz)", True, "已安装"))
    except ImportError:
        checks.append(("pyobjc (Quartz)", False, "未安装，无法读取空闲状态"))

    # 3. 钥匙串
    checks.append(("device_token 已注册", has_token(), "运行 register 完成注册" if not has_token() else "已存在"))

    # 4. 数据库可写
    try:
        storage = Storage(config.db_path)
        checks.append(("本地数据库可写", True, config.db_path))
        storage.close()
    except Exception as exc:
        checks.append(("本地数据库可写", False, str(exc)))

    # 5. repo_roots 存在
    existing = [r for r in config.repo_roots if Path(r).expanduser().is_dir()]
    checks.append(("repo_roots 可访问", bool(existing), f"{len(existing)}/{len(config.repo_roots)} 个目录存在"))

    # 6. 服务端连通性
    import urllib.error
    import urllib.request
    try:
        with urllib.request.urlopen(config.server_url.rstrip("/") + "/api/v1/healthz", timeout=5) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
            checks.append(("服务端可访问", True, f"version={payload.get('version')}"))
    except Exception as exc:
        checks.append(("服务端可访问", False, f"{exc}"))

    print("Lumen 桌面端自检：")
    for name, ok, detail in checks:
        mark = "✅" if ok else "❌"
        print(f"  {mark} {name}: {detail}")

    failed = [c for c in checks if not c[1]]
    if failed:
        print(f"\n有 {len(failed)} 项需要处理。")
        return 1
    print("\n全部检查通过。")
    return 0


def cmd_reset(args: argparse.Namespace) -> int:
    """删除本地 token（紧急响应使用）。"""
    if not args.yes:
        print("该操作会删除钥匙串中的 device_token。确认请加 --yes")
        return 1
    delete_token()
    print("已删除 device_token。如需继续上报，请重新注册设备。")
    return 0


# ---- 参数解析 ----


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="lumen-desktop",
        description="Lumen macOS 采集端：低打扰记录工作元数据并同步到你的服务器。",
    )
    parser.add_argument("--config", help=f"配置文件路径（默认 {DEFAULT_CONFIG_PATH}）")
    sub = parser.add_subparsers(dest="command", required=True)

    p_run = sub.add_parser("run", help="运行采集进程")
    p_run.add_argument("--device-id", help="覆盖设备 ID")
    p_run.add_argument("--server-url", help="覆盖服务端地址")
    p_run.add_argument("--repo-root", action="append", help="追加 Git 扫描目录（可多次指定）")
    p_run.set_defaults(func=cmd_run)

    p_status = sub.add_parser("status", help="查看运行状态")
    p_status.add_argument("--json", action="store_true", help="以 JSON 输出")
    p_status.set_defaults(func=cmd_status)

    p_pause = sub.add_parser("pause", help="暂停采集")
    p_pause.set_defaults(func=cmd_pause)

    p_resume = sub.add_parser("resume", help="恢复采集")
    p_resume.set_defaults(func=cmd_resume)

    p_sync = sub.add_parser("sync-now", help="立即同步")
    p_sync.add_argument("--json", action="store_true", help="以 JSON 输出")
    p_sync.set_defaults(func=cmd_sync_now)

    p_reg = sub.add_parser("register", help="注册设备（使用一次性 enrollment token）")
    p_reg.add_argument("--token", required=True, help="服务端 LUMEN_ENROLLMENT_TOKEN 的值")
    p_reg.set_defaults(func=cmd_register)

    p_init = sub.add_parser("init-config", help="生成配置样例")
    p_init.add_argument("--force", action="store_true", help="覆盖已存在的配置")
    p_init.set_defaults(func=cmd_init_config)

    p_events = sub.add_parser("events", help="查看最近事件（自查上传内容）")
    p_events.add_argument("--limit", type=int, default=20, help="显示条数")
    p_events.add_argument("--json", action="store_true", help="以 JSON 输出")
    p_events.set_defaults(func=cmd_events)

    p_doctor = sub.add_parser("doctor", help="环境自检")
    p_doctor.set_defaults(func=cmd_doctor)

    p_reset = sub.add_parser("reset-token", help="删除本地 device_token")
    p_reset.add_argument("--yes", action="store_true", help="确认删除")
    p_reset.set_defaults(func=cmd_reset)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
