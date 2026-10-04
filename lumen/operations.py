"""User-triggered undo, data export, consistent backup, and safe offline restore."""
import json
import os
import sqlite3
import tempfile
from pathlib import Path
from .contracts import now, stamp
from .domain import Changes, require

RECOVERABLE = {'memories','todos','notes','projects','schedules','plans','memory_policies'}
EXPORT_TABLES = (*sorted(RECOVERABLE),'messages','changes','runs','deliveries','feishu_inbox','settings','usage','chat_requests','capture_events')
LABELS = {'expire_memory':'临时记忆到期','save_memory':'记住或更正信息','delete_memory':'忘记信息','confirm_memory':'确认记忆',
          'set_memory_policy':'调整记忆权限','accept_revision':'采纳记忆修改','dismiss_revision':'忽略记忆修改',
          'add_todo':'添加任务','update_todo':'修改任务','delete_todo':'删除任务','save_note':'保存笔记','delete_note':'删除笔记',
          'save_project':'保存项目','delete_project':'删除项目','propose_plan':'拟定计划','apply_plan':'确认计划','reject_plan':'取消计划',
          'create_schedule':'创建提醒','update_schedule':'修改提醒','delete_schedule':'删除提醒','snooze_schedule':'稍后提醒','undo_change':'撤销操作'}


def activity(store, limit=30):
    rows=store.query('SELECT id,action,record_id,created_at,undone,actor FROM changes ORDER BY rowid DESC LIMIT ?', (limit,))
    for row in rows:row['label']=LABELS.get(row['action'],'更新个人事务')
    return rows


def undo(actions, record_id=None):
    with actions.store.transaction() as db:
        if record_id:
            row=require(db,'changes',record_id)
        else:
            item=db.execute("SELECT * FROM changes WHERE undone=0 AND action!='undo_change' ORDER BY rowid DESC LIMIT 1").fetchone()
            if not item:raise ValueError('没有可以撤销的操作')
            row=dict(item)
        if row['undone'] or row['action']=='undo_change':raise ValueError('这项操作已经撤销或不支持撤销')
        entries=json.loads(row['changes'])
        for entry in entries:
            if entry['table'] not in RECOVERABLE:raise ValueError('操作记录不支持恢复')
            if entry['table']=='memories' and entry['before'] is not None:
                from .privacy import blocked_memory
                if blocked_memory(entry['before']):raise ValueError('撤销会恢复敏感记忆，已拒绝；请直接修改当前记录')
            current=db.execute(f'SELECT * FROM {entry["table"]} WHERE id=?',(entry['id'],)).fetchone()
            if (dict(current) if current else None)!=entry['after']:
                raise ValueError('记录已经被后续操作或执行改变，无法安全撤销；请直接修改当前记录')
        changes=Changes(db)
        for entry in reversed(entries):
            if entry['before'] is None:changes.delete(entry['table'],entry['id'])
            else:changes.put(entry['table'],entry['id'],{key:value for key,value in entry['before'].items() if key!='id'})
        if any(entry['table']=='memories' for entry in entries):actions.store.reset_context(db)
        db.execute('UPDATE changes SET undone=1 WHERE id=?',(row['id'],))
        receipt=changes.finish('undo_change',row['id'],'user')
        return {'ok':True,'id':row['id'],'action':'undo_change','receipt_id':receipt}


def export_data(store):
    from . import __version__
    with store.transaction() as db:
        return {'format':'lumen-personal-data','version':__version__,'exported_at':stamp(),
                'tables':{table:[dict(row) for row in db.execute(f'SELECT * FROM {table}')] for table in EXPORT_TABLES}}


def backup(store, directory=None):
    directory=Path(directory or Path(store.path).parent/'backups')
    directory.mkdir(parents=True,exist_ok=True)
    os.chmod(directory,0o700)
    path=directory/('lumen-'+now().strftime('%Y%m%d-%H%M%S-%f')+'.db')
    with store.lock:
        target=sqlite3.connect(path)
        try:store.db.backup(target)
        finally:target.close()
    os.chmod(path,0o600)
    return path


def restore(source, destination):
    source,destination=Path(source).resolve(),Path(destination).resolve()
    if source==destination:raise ValueError('备份不能与目标数据库相同')
    if not source.is_file():raise ValueError('备份文件不存在')
    original=sqlite3.connect(source.as_uri()+'?mode=ro',uri=True)
    staged=None
    try:
        if original.execute('PRAGMA integrity_check').fetchone()[0]!='ok':raise ValueError('备份完整性检查失败')
        tables={row[0] for row in original.execute("SELECT name FROM sqlite_master WHERE type='table'")}
        if not {'messages','memories','todos','schedules'}.issubset(tables):raise ValueError('文件不是 Lumen 备份')
        destination.parent.mkdir(parents=True,exist_ok=True)
        handle,name=tempfile.mkstemp(prefix='.lumen-restore-',suffix='.db',dir=destination.parent)
        os.close(handle);staged=Path(name)
        target=sqlite3.connect(staged)
        try:original.backup(target)
        finally:target.close()
        if destination.exists():
            previous=sqlite3.connect(destination)
            try:
                if previous.execute('PRAGMA wal_checkpoint(TRUNCATE)').fetchone()[0]:raise ValueError('数据库仍在使用，请先停止服务')
                folder=destination.parent/'backups';folder.mkdir(exist_ok=True);os.chmod(folder,0o700)
                saved=folder/('lumen-pre-restore-'+now().strftime('%Y%m%d-%H%M%S-%f')+'.db')
                target=sqlite3.connect(saved)
                try:previous.backup(target)
                finally:target.close()
                os.chmod(saved,0o600)
            finally:previous.close()
        os.chmod(staged,0o600)
        staged.replace(destination)
        staged=None
    finally:
        original.close()
        if staged and staged.exists():staged.unlink()


class ProcessLock:
    def __init__(self,path):
        path=Path(path).expanduser().resolve()
        Path(path).parent.mkdir(parents=True,exist_ok=True)
        self.handle=open(str(path)+'.lock','a+b')
        os.chmod(str(path)+'.lock',0o600)
        try:
            if os.name=='nt':
                import msvcrt
                self.handle.seek(0);self.handle.write(b'0');self.handle.flush();self.handle.seek(0)
                msvcrt.locking(self.handle.fileno(),msvcrt.LK_NBLCK,1)
            else:
                import fcntl
                fcntl.flock(self.handle.fileno(),fcntl.LOCK_EX|fcntl.LOCK_NB)
        except OSError:
            self.handle.close();raise ValueError('数据库已有运行中的 Lumen 进程，请先停止它') from None
    def close(self):self.handle.close()
