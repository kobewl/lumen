from __future__ import annotations

import os
import sqlite3
import threading
import uuid
from contextlib import contextmanager
from datetime import datetime, timedelta, timezone
from pathlib import Path
from zoneinfo import ZoneInfo


from .contracts import S, now, stamp, identifier, tool


class Store:
    def __init__(self, path):
        self.path = str(path)
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
        from .migrations import migrate
        migrate(self.db)
        # Do not silently repeat potentially completed writes after a crash.
        self.db.execute("UPDATE schedules SET status='failed', enabled=0, last_error='执行中断；请检查结果后重新启用' WHERE status='running'")
        self.db.execute("UPDATE feishu_inbox SET status='interrupted' WHERE status='processing'")
        self.db.execute("UPDATE chat_requests SET status='interrupted' WHERE status='processing'")
        self.db.commit()
        for filename in (str(path),str(path)+'-wal',str(path)+'-shm'):
            if Path(filename).exists():os.chmod(filename,0o600)

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
            'notes': self.query('SELECT * FROM notes ORDER BY updated_at DESC'),
            'projects': self.query('SELECT * FROM projects ORDER BY created_at DESC'),
            'plans': self.query('SELECT * FROM plans ORDER BY created_at DESC'),
            'policies': self.query('SELECT id,catalog,operation,mode FROM memory_policies ORDER BY catalog,operation'),
        }


TOOLS = [
    tool('get_state', '读取个人记忆、Todo 和定时任务及其真实 ID。', {}),
    tool('save_memory', '保存用户明确要求记住或更正的事实。catalog=soul 是个人 Soul（称呼、价值观、稳定偏好），catalog=daily 是日常（行程、近况、临时状态）。更正已有事实时必须传真实 id；同一 key 覆盖旧值。随口事实由捕获器处理，不要重复保存。',
         {'id': S, 'key': S, 'content': S, 'catalog': {'type':'string','enum':['soul','daily']}, 'category': {'type':'string','enum':['personal','preference','work','temporary']}, 'status': {'type':'string','enum':['confirmed','pending']}, 'expires_at': {'type':'string','minLength':0}}, ['key', 'content']),
    tool('delete_memory', '按真实 ID 删除用户要求忘记的信息，同时重置短期模型上下文，避免从旧聊天重新读到。', {'id': S}, ['id']),
    tool('add_todo', '记录待办。due_at 可选，必须是带时区的 ISO8601；截止日期不会自动创建提醒。',
         {'title': S, 'due_at': {'type':'string','minLength':0}, 'priority': {'type':'string','enum':['high','normal','low']}, 'project_id': {'type':'string','minLength':0}, 'notes': {'type':'string','minLength':0}, 'remind_at': S}, ['title']),
    tool('update_todo', '修改真实 ID 对应的待办标题或完成状态。',
         {'id': S, 'title': S, 'done': {'type': 'boolean'}, 'due_at': {'type':'string','minLength':0}, 'priority': {'type':'string','enum':['high','normal','low']}, 'project_id': {'type':'string','minLength':0}, 'notes': {'type':'string','minLength':0}, 'remind_at': {'type':'string','minLength':0}}, ['id']),
    tool('delete_todo', '删除指定待办。', {'id': S}, ['id']),
    tool('create_schedule', '创建未来定时任务。reminder 到时发送 prompt 原文；agent 到时执行 prompt。repeat 支持 none/daily/weekly/weekdays/monthly，按用户时区重复。',
         {'title': S, 'prompt': S, 'run_at': S,
          'kind': {'type': 'string', 'enum': ['reminder', 'agent','briefing']},
          'repeat': {'type': 'string', 'enum': ['none', 'daily', 'weekly','weekdays','monthly']}},
         ['title', 'prompt', 'run_at', 'kind', 'repeat']),
    tool('update_schedule', '修改任务内容、执行时间或重复规则；可暂停、启用。重新启用过期任务需提供未来 run_at。',
         {'id':S,'enabled':{'type':'boolean'},'run_at':S,'title':S,'prompt':S,'kind':{'type':'string','enum':['reminder','agent','briefing']},'repeat':{'type':'string','enum':['none','daily','weekly','weekdays','monthly']}}, ['id']),
    tool('snooze_schedule','将指定提醒推迟若干分钟，保持原重复节奏。',{'id':S,'minutes':{'type':'integer','minimum':1,'maximum':10080}},['id','minutes']),
    tool('get_today','获取今日简报或近七天复盘，基于真实任务，不产生写入。',{'mode':{'type':'string','enum':['today','weekly']}}),
    tool('delete_schedule', '删除定时任务，历史执行结果仍保留。', {'id': S}, ['id']),
]
from .domain import DOMAIN_TOOLS, USER_ACTIONS
TOOLS.extend(DOMAIN_TOOLS)
CATALOG = {t['function']['name']: t['function']['parameters'] for t in [*TOOLS, *USER_ACTIONS]}


def __getattr__(name):
    if name == 'Actions':
        from .actions import Actions
        return Actions
    raise AttributeError(name)


from .scheduler import Scheduler
