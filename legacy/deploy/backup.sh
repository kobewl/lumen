#!/usr/bin/env bash
#
# Lumen 数据库备份脚本
#
# 作用：生成 SQLite 一致性快照、清理旧备份、可选地加密并复制到异地。
#
# 设计要点：
#   - 使用 sqlite3 的 .backup 命令（等价于在线备份 API），运行中也能安全备份；
#   - 加密密钥从环境变量读取，绝不写在脚本里或命令行参数中（避免进入 ps）；
#   - 只保留最近 N 份，避免磁盘被占满。
#
# 用法：
#   sudo -u lumen ./backup.sh
#
# 环境变量（可从 /etc/lumen/lumen.env 读取）：
#   LUMEN_DB_PATH            数据库路径，默认 /var/lib/lumen/lumen.db
#   LUMEN_BACKUP_DIR         备份目录，默认 /var/lib/lumen/backups
#   LUMEN_BACKUP_KEEP        本地保留份数，默认 7
#   LUMEN_BACKUP_REMOTE      异地目标（rsync 目标，可选）
#   LUMEN_BACKUP_PASSPHRASE  加密口令（可选；设置后用 openssl 加密副本）

set -euo pipefail

DB_PATH="${LUMEN_DB_PATH:-/var/lib/lumen/lumen.db}"
BACKUP_DIR="${LUMEN_BACKUP_DIR:-/var/lib/lumen/backups}"
KEEP="${LUMEN_BACKUP_KEEP:-7}"
REMOTE="${LUMEN_BACKUP_REMOTE:-}"
PASSPHRASE="${LUMEN_BACKUP_PASSPHRASE:-}"

timestamp() { date +%Y%m%d-%H%M%S; }

log() { printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"; }

die() { printf '错误: %s\n' "$*" >&2; exit 1; }

[ -f "$DB_PATH" ] || die "数据库不存在: $DB_PATH"
command -v sqlite3 >/dev/null 2>&1 || die "未找到 sqlite3，请先安装"

mkdir -p "$BACKUP_DIR"
chmod 0700 "$BACKUP_DIR"

STAMP="$(timestamp)"
TARGET="${BACKUP_DIR}/lumen-${STAMP}.db"

log "开始备份 ${DB_PATH} -> ${TARGET}"

# .backup 是 SQLite 的在线备份机制：即使服务正在写入也能得到一致快照。
# 备份完成后立刻关闭 WAL 模式，避免留下空的 -wal/-shm 附属文件。
sqlite3 "$DB_PATH" ".backup '${TARGET}'"
sqlite3 "$TARGET" "PRAGMA journal_mode=DELETE;" >/dev/null
rm -f "${TARGET}-wal" "${TARGET}-shm"

# 校验快照可读，避免留下损坏的备份。
if ! sqlite3 "$TARGET" "PRAGMA integrity_check;" | head -1 | grep -q '^ok$'; then
    rm -f "$TARGET"
    die "备份完整性校验失败，已删除损坏文件"
fi

chmod 0600 "$TARGET"
SIZE="$(du -h "$TARGET" | cut -f1)"
log "备份完成，大小 ${SIZE}"

# 清理旧备份：按修改时间排序，保留最近的 KEEP 份。
#
# 注意这里刻意不用 mapfile：macOS 自带 bash 3.2 没有该内建命令，
# 而这个脚本需要在开发机（macOS）和服务器（Linux）上都能运行。
OLD_LIST="$(ls -1t "${BACKUP_DIR}"/lumen-*.db 2>/dev/null | tail -n "+$((KEEP + 1))" || true)"
if [ -n "$OLD_LIST" ]; then
    OLD_COUNT="$(printf '%s\n' "$OLD_LIST" | grep -c . || true)"
    log "清理 ${OLD_COUNT} 份旧备份"
    printf '%s\n' "$OLD_LIST" | while IFS= read -r old_file; do
        [ -n "$old_file" ] && rm -f "$old_file"
    done
fi

# 加密异地副本。仅在同一台 VPS 上保留副本不算完整备份。
if [ -n "$REMOTE" ]; then
    if [ -n "$PASSPHRASE" ]; then
        # 通过环境变量把口令传给 openssl，避免出现在命令行参数中。
        ENC_TARGET="${TARGET}.enc"
        if ! LUMEN_BACKUP_PASSPHRASE="$PASSPHRASE" openssl enc -aes-256-cbc -salt -pbkdf2 \
            -in "$TARGET" -out "$ENC_TARGET" -pass env:LUMEN_BACKUP_PASSPHRASE; then
            die "加密失败"
        fi
        chmod 0600 "$ENC_TARGET"
        log "已加密: ${ENC_TARGET}"
        rsync -az --quiet "$ENC_TARGET" "$REMOTE" && log "已复制到异地: ${REMOTE}"
        rm -f "$ENC_TARGET"
    else
        log "警告: 设置了 LUMEN_BACKUP_REMOTE 但未设置 LUMEN_BACKUP_PASSPHRASE，跳过加密异地备份"
        log "      异地备份必须加密，请配置口令后重试"
    fi
else
    log "提示: 未配置 LUMEN_BACKUP_REMOTE，仅保留本地备份（不构成完整备份策略）"
fi

log "备份流程结束"
