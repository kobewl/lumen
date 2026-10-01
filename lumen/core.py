from __future__ import annotations

import os
import sqlite3
import threading
import uuid
from contextlib import contextmanager
from datetime import datetime, timedelta, timezone
from pathlib import Path
from zoneinfo import ZoneInfo


def now():
    return datetime.now(timezone.utc)


def stamp(value=None):
    return (value or now()).isoformat()


def identifier():
    return uuid.uuid4().hex


class Store:
    def __init__(self, path):
        Path(path).parent.mkdir(parents=True, exist_ok=True)
        self.lock = threading.RLock()
        self.db = sqlite3.connect(path, check_same_thread=False)
        self.db.row_factory = sqlite3.Row
        self.db.executescript('''
            PRAGMA journal_mode=WAL;
            CREATE TABLE IF NOT EXISTS settings (
                key TEXT PRIMARY KEY, value TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS messages (
                id TEXT PRIMARY KEY, role TEXT NOT NULL, content TEXT NOT NULL,
                created_at TEXT NOT NULL, source TEXT NOT NULL DEFAULT 'chat');
            CREATE TABLE IF NOT EXISTS memories (
                id TEXT PRIMARY KEY, key TEXT NOT NULL UNIQUE, content TEXT NOT NULL,
                updated_at TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS todos (
                id TEXT PRIMARY KEY, title TEXT NOT NULL, due_at TEXT,
                done INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS schedules (
                id TEXT PRIMARY KEY, title TEXT NOT NULL, prompt TEXT NOT NULL,
                kind TEXT NOT NULL, run_at TEXT NOT NULL, repeat TEXT NOT NULL,
                enabled INTEGER NOT NULL DEFAULT 1, status TEXT NOT NULL DEFAULT 'pending',
                last_error TEXT, created_at TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS feishu_inbox (
                id TEXT PRIMARY KEY, status TEXT NOT NULL, created_at TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS deliveries (
                id TEXT PRIMARY KEY, event_key TEXT NOT NULL UNIQUE, recipient TEXT NOT NULL,
                content TEXT NOT NULL, status TEXT NOT NULL, attempts INTEGER NOT NULL,
                next_at TEXT NOT NULL, last_error TEXT);
            CREATE TABLE IF NOT EXISTS runs (
                id TEXT PRIMARY KEY, schedule_id TEXT NOT NULL, scheduled_at TEXT NOT NULL,
                status TEXT NOT NULL, result TEXT NOT NULL, created_at TEXT NOT NULL);
        ''')
        # Do not silently repeat potentially completed writes after a crash.
        self.db.execute("UPDATE schedules SET status='failed', enabled=0, last_error='执行中断；请检查结果后重新启用' WHERE status='running'")
        self.db.execute("UPDATE feishu_inbox SET status='interrupted' WHERE status='processing'")
        self.db.commit()
        os.chmod(path, 0o600)

    @contextmanager
    def transaction(self):
        with self.lock, self.db:
            yield self.db

    def query(self, sql, values=()):
        with self.lock:
            return [dict(row) for row in self.db.execute(sql, values).fetchall()]

    def message(self, role, content, source='chat'):
        with self.transaction() as db:
            db.execute('INSERT INTO messages VALUES (?,?,?,?,?)',
                       (identifier(), role, content, stamp(), source))

    def context_cutoff(self):
        rows = self.query("SELECT value FROM settings WHERE key='context_cutoff'")
        return int(rows[0]['value']) if rows else 0

    def reset_context(self, db, new_conversation=False):
        cutoff = db.execute('SELECT COALESCE(MAX(rowid),0) FROM messages').fetchone()[0]
        db.execute("INSERT INTO settings VALUES ('context_cutoff',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", (str(cutoff),))
        if new_conversation:
            db.execute("INSERT INTO settings VALUES ('chat_view_cutoff',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", (str(cutoff),))

    def history(self, max_chars=24000):
        rows = self.query("SELECT role,content FROM messages WHERE source='chat' AND rowid>? ORDER BY rowid DESC LIMIT 30", (self.context_cutoff(),))
        selected, size = [], 0
        for row in rows:
            if size + len(row['content']) > max_chars:
                break
            selected.append(row)
            size += len(row['content'])
        selected.reverse()
        while selected and selected[0]['role'] != 'user':
            selected.pop(0)
        return selected

    def state(self):
        cutoff = self.query("SELECT value FROM settings WHERE key='chat_view_cutoff'")
        visible_after = int(cutoff[0]['value']) if cutoff else 0
        return {
            'messages': list(reversed(self.query('SELECT * FROM messages WHERE rowid>? ORDER BY rowid DESC LIMIT 200', (visible_after,)))),
            'memories': self.query('SELECT * FROM memories ORDER BY updated_at DESC'),
            'todos': self.query('SELECT * FROM todos ORDER BY done, created_at DESC'),
            'schedules': self.query('SELECT * FROM schedules ORDER BY run_at'),
            'deliveries': self.query('SELECT id,status,attempts,last_error FROM deliveries ORDER BY rowid DESC LIMIT 30'),
            'runs': self.query('SELECT * FROM runs ORDER BY rowid DESC LIMIT 30'),
        }


def tool(name, description, properties, required=()):
    return {'type': 'function', 'function': {
        'name': name, 'description': description,
        'parameters': {'type': 'object', 'properties': properties,
                       'required': list(required), 'additionalProperties': False}}}


S = {'type': 'string'}
TOOLS = [
    tool('get_state', '读取个人记忆、Todo 和定时任务及其真实 ID。', {}),
    tool('save_memory', '保存用户明确要求记住的个人事实。更正已有事实时必须传真实 id，可更改 key；同一 key 覆盖旧值，不创建同义重复条目。',
         {'id': S, 'key': S, 'content': S}, ['key', 'content']),
    tool('delete_memory', '按真实 ID 删除用户要求忘记的信息，同时重置短期模型上下文，避免从旧聊天重新读到。', {'id': S}, ['id']),
    tool('add_todo', '记录待办。due_at 可选，必须是带时区的 ISO8601；截止日期不会自动创建提醒。',
         {'title': S, 'due_at': S}, ['title']),
    tool('update_todo', '修改真实 ID 对应的待办标题或完成状态。',
         {'id': S, 'title': S, 'done': {'type': 'boolean'}}, ['id']),
    tool('delete_todo', '删除指定待办。', {'id': S}, ['id']),
    tool('create_schedule', '创建未来定时任务。reminder 到时发送 prompt 原文；agent 到时执行 prompt。repeat 支持 none/daily/weekly，按用户时区重复。',
         {'title': S, 'prompt': S, 'run_at': S,
          'kind': {'type': 'string', 'enum': ['reminder', 'agent']},
          'repeat': {'type': 'string', 'enum': ['none', 'daily', 'weekly']}},
         ['title', 'prompt', 'run_at', 'kind', 'repeat']),
    tool('update_schedule', '暂停或重新启用真实 ID 对应任务。重新启用需要提供未来 run_at。',
         {'id': S, 'enabled': {'type': 'boolean'}, 'run_at': S}, ['id', 'enabled']),
    tool('delete_schedule', '删除定时任务，历史执行结果仍保留。', {'id': S}, ['id']),
]
CATALOG = {t['function']['name']: t['function']['parameters'] for t in TOOLS}


class Actions:
    def __init__(self, store, timezone_name='Asia/Shanghai'):
        self.store = store
        self.zone = ZoneInfo(timezone_name)

    def date(self, value, future=False):
        dt = datetime.fromisoformat(value.replace('Z', '+00:00'))
        if dt.tzinfo is None:
            raise ValueError('时间必须包含时区，例如 2026-10-01T09:00:00+08:00')
        if future and dt <= now():
            raise ValueError('执行时间必须在未来')
        return stamp(dt.astimezone(timezone.utc))

    def execute(self, name, args, scheduled=False):
        if not isinstance(name, str):
            raise ValueError('工具名称必须是字符串')
        schema = CATALOG.get(name)
        if schema is None or not isinstance(args, dict):
            raise ValueError('未知工具或无效参数')
        if set(args) - set(schema['properties']) or set(schema['required']) - set(args):
            raise ValueError('缺少必要参数或存在未知参数')
        for key, value in args.items():
            prop = schema['properties'][key]
            if prop['type'] == 'boolean':
                if type(value) is not bool:
                    raise ValueError(f'{key} 必须为布尔值')
            elif not isinstance(value, str) or not value.strip() or len(value) > 4000:
                raise ValueError(f'{key} 必须是 1–4000 字符的文本')
            if 'enum' in prop and value not in prop['enum']:
                raise ValueError(f'{key} 取值无效')
        if scheduled and name in ('create_schedule', 'update_schedule', 'delete_schedule'):
            raise ValueError('定时执行期间不能创建或修改其他定时任务')
        if name == 'get_state':
            state = self.store.state()
            return {k: state[k] for k in ('memories', 'todos', 'schedules')}
        record_id = args.get('id', identifier())
        with self.store.transaction() as db:
            if name == 'save_memory':
                key, content = args['key'].strip(), args['content'].strip()
                if 'id' in args:
                    if not db.execute('SELECT 1 FROM memories WHERE id=?', (record_id,)).fetchone():
                        raise ValueError('记忆不存在，请先读取真实 ID')
                    conflict = db.execute('SELECT id FROM memories WHERE key=? AND id!=?', (key, record_id)).fetchone()
                    if conflict:
                        raise ValueError('这个名称属于另一条记忆，请先确认要修改哪一条')
                    db.execute('UPDATE memories SET key=?,content=?,updated_at=? WHERE id=?',
                               (key, content, stamp(), record_id))
                else:
                    db.execute('''INSERT INTO memories VALUES (?,?,?,?)
                        ON CONFLICT(key) DO UPDATE SET content=excluded.content, updated_at=excluded.updated_at''',
                               (record_id, key, content, stamp()))
                    record_id = db.execute('SELECT id FROM memories WHERE key=?', (key,)).fetchone()[0]
            elif name == 'add_todo':
                due = self.date(args['due_at']) if 'due_at' in args else None
                db.execute('INSERT INTO todos VALUES (?,?,?,0,?)', (record_id, args['title'], due, stamp()))
            elif name == 'create_schedule':
                run_at = self.date(args['run_at'], future=True)
                db.execute('INSERT INTO schedules (id,title,prompt,kind,run_at,repeat,created_at) VALUES (?,?,?,?,?,?,?)',
                           (record_id, args['title'], args['prompt'], args['kind'], run_at, args['repeat'], stamp()))
            else:
                table = 'memories' if name.endswith('memory') else 'todos' if name.endswith('todo') else 'schedules'
                row = db.execute(f'SELECT * FROM {table} WHERE id=?', (record_id,)).fetchone()
                if row is None:
                    raise ValueError('记录不存在，请先读取真实 ID')
                if table == 'schedules' and row['status'] == 'running':
                    raise ValueError('任务正在执行，请完成后再修改')
                if name.startswith('delete_'):
                    db.execute(f'DELETE FROM {table} WHERE id=?', (record_id,))
                    if name == 'delete_memory':
                        self.store.reset_context(db)
                elif name == 'update_todo':
                    if not ('title' in args or 'done' in args):
                        raise ValueError('需要提供 title 或 done')
                    db.execute('UPDATE todos SET title=?, done=? WHERE id=?',
                               (args.get('title', row['title']), int(args.get('done', row['done'])), record_id))
                elif name == 'update_schedule':
                    if args['enabled'] and 'run_at' not in args:
                        raise ValueError('重新启用任务需要提供未来 run_at')
                    run_at = self.date(args['run_at'], future=True) if args['enabled'] else row['run_at']
                    db.execute("UPDATE schedules SET enabled=?,run_at=?,status='pending',last_error=NULL WHERE id=?",
                               (int(args['enabled']), run_at, record_id))
        return {'ok': True, 'id': record_id, 'action': name}


class Scheduler:
    def __init__(self, store, actions, agent, notify_owner=None):
        self.store, self.actions, self.agent = store, actions, agent
        self.notify_owner = notify_owner
        self.stop = threading.Event()

    def tick(self, at=None):
        at = at or now()
        due = self.store.query("SELECT * FROM schedules WHERE enabled=1 AND status='pending' AND run_at<=? ORDER BY run_at", (stamp(at),))
        for item in due:
            if self.stop.is_set():
                break
            with self.store.transaction() as db:
                claimed = db.execute("UPDATE schedules SET status='running' WHERE id=? AND status='pending' AND enabled=1", (item['id'],)).rowcount
            if not claimed:
                continue
            status, result = 'success', item['prompt']
            try:
                if item['kind'] == 'agent':
                    result = self.agent.reply('执行定时任务：' + item['prompt'], scheduled=True)
            except Exception as exc:
                status, result = 'failed', f'任务「{item["title"]}」执行失败：{exc}'
            next_at = item['run_at']
            enabled = 0
            if item['repeat'] != 'none':
                local = datetime.fromisoformat(next_at).astimezone(self.actions.zone)
                delta = timedelta(days=1 if item['repeat'] == 'daily' else 7)
                while local.astimezone(timezone.utc) <= at:
                    local += delta
                next_at = stamp(local.astimezone(timezone.utc))
                enabled = int(status == 'success')
            with self.store.transaction() as db:
                db.execute('INSERT INTO runs VALUES (?,?,?,?,?,?)',
                           (identifier(), item['id'], item['run_at'], status, result, stamp()))
                db.execute('INSERT INTO messages VALUES (?,?,?,?,?)',
                           (identifier(), 'assistant', f'⏰ {item["title"]}\n{result}', stamp(), 'schedule'))
                if self.notify_owner:
                    from .feishu import enqueue
                    enqueue(db, self.notify_owner, f'⏰ {item["title"]}\n{result}', 'schedule:' + item['id'] + ':' + item['run_at'])
                db.execute('UPDATE schedules SET status=?,enabled=?,run_at=?,last_error=? WHERE id=?',
                           ('pending' if enabled else status, enabled, next_at, result if status == 'failed' else None, item['id']))

    def run(self):
        while not self.stop.is_set():
            try:
                self.tick()
            except Exception:
                import logging
                logging.exception('scheduler tick failed')
            self.stop.wait(1)
