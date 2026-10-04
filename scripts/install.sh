#!/usr/bin/env bash
# Install/update a checkout without sourcing or printing local credentials.
set -Eeuo pipefail
umask 077
repo=https://github.com/kobewl/lumen.git
if [[ -f "$PWD/lumen/core.py" ]]; then default_dir=$PWD; else default_dir="$HOME/lumen"; fi
dir=${LUMEN_INSTALL_DIR:-$default_dir}
python=${LUMEN_PYTHON:-python3}
service=${LUMEN_SERVICE:-}
command -v git >/dev/null || { echo '需要安装 git'; exit 1; }
"$python" -c 'import sys; assert sys.version_info >= (3,11), "需要 Python 3.11+"'
mkdir -p "$dir"
dir=$(cd "$dir" && pwd)
if [[ -d "$dir/.git" ]]; then
  [[ -z $(git -C "$dir" status --porcelain -- . ':!.venvs' ':!.venv-next' ':!.lumen-install.lock') ]] || { echo '目录有未提交修改，更新已停止。请先保存修改。'; exit 1; }
  origin=$(git -C "$dir" remote get-url origin)
  [[ "$origin" == "$repo" || "$origin" == https://github.com/kobewl/lumen || "$origin" == git@github.com:kobewl/lumen.git ]] || { echo '目录不是 Lumen 官方仓库'; exit 1; }
  git -C "$dir" fetch origin main
else
  [[ -z $(ls -A "$dir") ]] || { echo '安装目录非空且不是 Git 仓库'; exit 1; }
  git clone --branch main "$repo" "$dir"
fi
old=$(git -C "$dir" rev-parse HEAD)
new=$(git -C "$dir" rev-parse origin/main)
lock="$dir/.lumen-install.lock"
mkdir "$lock" 2>/dev/null || { echo '安装锁已存在，请检查是否有另一个更新进程'; exit 1; }
requirements=''
cleanup() { if [[ -n "$requirements" ]]; then rm -f "$requirements"; fi; rmdir "$lock" 2>/dev/null || true; }
trap cleanup EXIT
# Prepare dependencies before stopping the running service or changing its code.
mkdir -p "$dir/.venvs"
venv="$dir/.venvs/$new"
requirements=$(mktemp)
git -C "$dir" show "$new:requirements.txt" > "$requirements"
if [[ ! -x "$venv/bin/python" ]]; then "$python" -m venv "$venv"; fi
"$venv/bin/python" -m pip install -r "$requirements"
# Only public code/dependencies are made readable; config and databases keep 0600.
chmod a+rx "$dir/.venvs"
chmod -R a+rX "$venv"
restart=false
finish() {
  result=$?
  cleanup
  if [[ "$result" != 0 ]]; then
    echo '更新失败。若服务已停止，请检查错误后手动启动；配置和备份保留。' >&2
    exit "$result"
  fi
  if [[ "$restart" == true ]]; then
    systemctl start "$service" || { echo '服务启动失败，请检查 journalctl'; exit 1; }
  fi
  exit "$result"
}
trap finish EXIT
if [[ -n "$service" ]]; then
  [[ $(systemctl show "$service" -p WorkingDirectory --value) == "$dir" ]] || { echo '服务工作目录与安装目录不一致'; exit 1; }
  if systemctl is-active --quiet "$service"; then
    systemctl stop "$service"
    restart=true
  fi
fi
# Consistent backups also work for a manually running service. Never open Store here.
"$python" - "$dir" <<'PY'
import os,sqlite3,sys
from datetime import datetime,timezone
from pathlib import Path
root=Path(sys.argv[1]);paths=set(root.glob('data/*.db'))
if os.getenv('LUMEN_DB_PATH'):paths.add(Path(os.environ['LUMEN_DB_PATH']))
if root==Path('/opt/lumen') and Path('/var/lib/lumen/lumen-v001.db').exists():paths.add(Path('/var/lib/lumen/lumen-v001.db'))
for path in paths:
    if not path.is_file():continue
    folder=path.parent/'backups';folder.mkdir(exist_ok=True);os.chmod(folder,0o700)
    saved=folder/('lumen-upgrade-'+datetime.now(timezone.utc).strftime('%Y%m%d-%H%M%S-%f')+'.db')
    with sqlite3.connect(path.resolve().as_uri()+'?mode=ro',uri=True) as source,sqlite3.connect(saved) as target:
        source.backup(target)
        if target.execute('PRAGMA integrity_check').fetchone()[0]!='ok':raise RuntimeError('备份完整性检查失败')
    os.chmod(saved,0o600)
print('数据库备份检查完成；配置文件保持原样。')
PY
cd "$dir"
git merge --ff-only origin/main
"$python" - "$dir" <<'PY'
import os,subprocess,sys
from pathlib import Path
root=Path(sys.argv[1])
for name in subprocess.check_output(['git','ls-files','-z']).decode().split('\0'):
    if not name:continue
    path=root/name
    if path.is_symlink():continue
    os.chmod(path,path.stat().st_mode|0o444)
    for parent in (path.parent,*path.parent.parents):
        if parent==root.parent:break
        os.chmod(parent,parent.stat().st_mode|0o555)
PY
if [[ -d .venv && ! -L .venv ]]; then
  mv .venv ".venvs/previous-$(date +%s)"
fi
"$python" - "$dir" "$venv" <<'PY'
import os,sys
from pathlib import Path
root=Path(sys.argv[1]);staged=root/'.venv-next'
if staged.is_symlink():staged.unlink()
elif staged.exists():raise RuntimeError('切换路径已有非链接文件，请先检查 .venv-next')
staged.symlink_to(sys.argv[2],target_is_directory=True)
os.replace(staged,root/'.venv')
PY
.venv/bin/python -c 'from lumen import __version__; print("已安装 Lumen "+__version__)'
echo "运行目录：$dir"
if [[ "$restart" == false ]]; then echo '安装/更新完成。请用现有配置启动或重启服务：.venv/bin/python -m lumen'; fi
echo "更新前代码：$old（升级备份位于数据库旁 backups/）"
