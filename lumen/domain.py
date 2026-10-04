"""Personal records, validation, and transactional change receipts."""
import json
from .contracts import S, identifier, stamp, tool
from .privacy import blocked_memory, require_safe_memory

EMPTY = {'type': 'string', 'minLength': 0}
TABLES = {'memory': 'memories', 'todo': 'todos', 'note': 'notes', 'project': 'projects', 'schedule': 'schedules', 'plan':'plans'}
DOMAIN_TOOLS = [
    tool('get_pending_actions','读取用户待确认的删除请求；这些请求尚未执行。',{}),
    tool('get_activity','读取近期操作回执，不含历史私密内容。实际撤销需要用户发送 /undo 或使用面板。',{}),
    tool('propose_plan', '为用户目标拟定计划草稿，不创建任务。用户必须在网页确认或发送 /approve ID 才会落地。id 可选，修改未确认草稿。', {'id':S,'title':S,'goal':EMPTY,'steps':{'type':'array','items':{'type':'string'},'minItems':1,'maxItems':20}}, ['title','steps']),
    tool('search_records', '关键词搜索个人记录，仅搜索已确认未过期记忆，结果包含真实 ID。',
         {'query': {**S, 'maxLength': 200}, 'scope': {'type': 'string', 'enum': ['all', 'memories', 'todos', 'notes', 'projects','plans']}}, ['query']),
    tool('get_record', '按真实 ID 读取完整记录。', {'type': {'type': 'string', 'enum': list(TABLES)}, 'id': S}, ['type', 'id']),
    tool('save_note', '保存想法、资料或随手记，提供 id 时修改原笔记。笔记不是人格记忆。',
         {'id': S, 'title': S, 'content': {**S, 'maxLength': 12000}, 'tags': EMPTY}, ['title', 'content']),
    tool('delete_note', '删除用户指定笔记。', {'id': S}, ['id']),
    tool('save_project', '保存或修改目标项目，id 可选。', {'id': S, 'title': S, 'goal': EMPTY,
         'status': {'type': 'string', 'enum': ['active', 'completed', 'paused']}}, ['title']),
    tool('delete_project', '删除项目并解除任务归属，任务保留。', {'id': S}, ['id']),
]
USER_ACTIONS = [tool('undo_change','用户撤销一项事务操作，id 可选默认最近操作。',{'id':S}),tool('retry_delivery','用户重试飞书发送。',{'id':S},['id']),tool('confirm_memory', '用户确认候选记忆。', {'id': S}, ['id']),
    tool('accept_revision','用户采纳捕获器对已有记忆提出的修改。',{'id':S},['id']),
    tool('dismiss_revision','用户忽略捕获器对已有记忆提出的修改。',{'id':S},['id']),
    tool('set_memory_policy','用户设置某个记忆目录的写入权限。auto 直接生效，confirm 需要用户确认。',
         {'catalog':{'type':'string','enum':['soul','daily']},'operation':{'type':'string','enum':['capture','revise']},'mode':{'type':'string','enum':['auto','confirm']}},
         ['catalog','operation','mode']),
    tool('approve_action','用户确认并执行待确认删除，记录变化时拒绝执行。',{'id':S},['id']),
    tool('reject_action','用户取消待确认删除。',{'id':S},['id']),
    tool('set_action_policy','用户设置删除权限。auto 允许模型直接删除，confirm 先生成请求。',
         {'scope':{'type':'string','enum':['memory','todo','note','schedule','project']},'mode':{'type':'string','enum':['auto','confirm']}},['scope','mode']),
    tool('apply_plan','用户确认计划并创建项目和任务。',{'id':S},['id']),
    tool('reject_plan','用户取消待确认计划。',{'id':S},['id'])]


class Changes:
    def __init__(self, db):
        self.db, self.before = db, {}

    def touch(self, table, record_id):
        if (table, record_id) not in self.before:
            row = self.db.execute(f'SELECT * FROM {table} WHERE id=?', (record_id,)).fetchone()
            self.before[(table, record_id)] = dict(row) if row else None

    def put(self, table, record_id, values):
        self.touch(table, record_id)
        exists = self.db.execute(f'SELECT 1 FROM {table} WHERE id=?', (record_id,)).fetchone()
        if exists:
            self.db.execute(f"UPDATE {table} SET " + ','.join(f'{key}=?' for key in values) + ' WHERE id=?', (*values.values(), record_id))
        else:
            record = {'id': record_id, **values}
            self.db.execute(f"INSERT INTO {table} ({','.join(record)}) VALUES ({','.join('?' for _ in record)})", tuple(record.values()))

    def delete(self, table, record_id):
        self.touch(table, record_id)
        self.db.execute(f'DELETE FROM {table} WHERE id=?', (record_id,))

    def finish(self, action, record_id, actor):
        changes = []
        for (table, item_id), before in self.before.items():
            row = self.db.execute(f'SELECT * FROM {table} WHERE id=?', (item_id,)).fetchone()
            after = dict(row) if row else None
            if before != after:
                changes.append({'table': table, 'id': item_id, 'before': before, 'after': after})
        if not changes:
            return None
        receipt = identifier()
        self.db.execute('INSERT INTO changes VALUES (?,?,?,?,?,0,?)',
                        (receipt, action, record_id, json.dumps(changes, ensure_ascii=False), stamp(), actor))
        return receipt


def require(db, table, record_id):
    row = db.execute(f'SELECT * FROM {table} WHERE id=?', (record_id,)).fetchone()
    if row is None:
        raise ValueError('记录不存在，请先读取真实 ID')
    return dict(row)


def search(actions, query, scope='all'):
    escaped = query.replace('\\', '\\\\').replace('%', '\\%').replace('_', '\\_')
    pattern = '%' + escaped + '%'
    results = []
    for table, fields in [('memories', ('key', 'content')), ('todos', ('title', 'notes')),
                          ('notes', ('title', 'content', 'tags')), ('projects', ('title', 'goal')),('plans',('title','goal','steps'))]:
        if scope not in ('all', table):
            continue
        where = ' OR '.join(f"{field} LIKE ? ESCAPE '\\'" for field in fields)
        params = [pattern] * len(fields)
        if table == 'memories':
            where = '(' + where + ") AND status='confirmed' AND (expires_at IS NULL OR expires_at>?)"
            params.append(stamp())
        rows = actions.store.query(f'SELECT * FROM {table} WHERE {where} ORDER BY rowid DESC LIMIT 20', params)
        if table=='memories':
            rows = [row for row in rows if not blocked_memory(row)]
        results.extend({'type': table, **row} for row in rows)
    for record in results:
        for key in ('content','notes','goal','steps'):
            if isinstance(record.get(key),str) and len(record[key])>1000:
                record[key]=record[key][:1000]
                record['truncated']=True
        for key in ('title','key'):
            if key in record:record[key]=record[key][:200]
    return {'records': results[:20], 'query': query, 'limit':20,'hint':'完整内容请用 get_record 查询'}


def execute(actions, db, changes, name, args, record_id):
    if name == 'propose_plan':
        row = require(db,'plans',record_id) if 'id' in args else {}
        if row and row['status']!='pending':
            raise ValueError('已处理的计划不可修改，请新建草稿')
        changes.put('plans',record_id,{'title':args['title'],'goal':args.get('goal',row.get('goal','')),
            'steps':json.dumps(args['steps'],ensure_ascii=False),'status':'pending','created_at':row.get('created_at',stamp())})
    elif name in ('apply_plan','reject_plan'):
        row = require(db,'plans',record_id)
        if row['status']=='applied' and name=='apply_plan':
            return record_id
        if row['status']!='pending':
            raise ValueError('计划已经处理')
        if name=='reject_plan':
            changes.put('plans',record_id,{'status':'rejected'})
        else:
            project = identifier()
            changes.put('projects',project,{'title':row['title'],'goal':row['goal'],'created_at':stamp()})
            for step in json.loads(row['steps']):
                changes.put('todos',identifier(),{'title':step,'project_id':project,'created_at':stamp()})
            changes.put('plans',record_id,{'status':'applied','project_id':project})
    elif name == 'save_note':
        if 'id' in args:
            require(db, 'notes', record_id)
        changes.put('notes', record_id, {'title': args['title'].strip(), 'content': args['content'].strip(), 'tags': args.get('tags', ''), 'updated_at': stamp()})
    elif name == 'save_project':
        row = require(db, 'projects', record_id) if 'id' in args else {}
        changes.put('projects', record_id, {'title': args['title'].strip(), 'goal': args.get('goal', row.get('goal', '')),
            'status': args.get('status', row.get('status', 'active')), 'created_at': row.get('created_at', stamp())})
    elif name == 'confirm_memory':
        row = require(db, 'memories', record_id)
        require_safe_memory(row['key'], row['content'])
        if row['expires_at'] and row['expires_at'] <= stamp():
            raise ValueError('记忆已过期，请先更新有效期')
        changes.put('memories', record_id, {'status': 'confirmed', 'updated_at': stamp()})
    elif name == 'accept_revision':
        row = require(db, 'memories', record_id)
        if not row.get('proposed_content'):
            raise ValueError('没有待采纳的修改')
        new_key = (row.get('proposed_key') or row['key']).strip()
        require_safe_memory(new_key, row['proposed_content'])
        if db.execute('SELECT id FROM memories WHERE key=? AND id!=?', (new_key, record_id)).fetchone():
            raise ValueError('这个名称属于另一条记忆，请先确认要修改哪一条')
        expires_at = row.get('proposed_expires_at') or row.get('expires_at')
        if expires_at and expires_at <= stamp():
            raise ValueError('建议里的有效期已经过去，请忽略这条修改或手动更新有效期')
        changes.put('memories', record_id, {'key': new_key, 'content': row['proposed_content'],
            'catalog': row.get('proposed_catalog') or row.get('catalog') or 'soul', 'expires_at': expires_at,
            'proposed_content': None, 'proposed_key': None, 'proposed_catalog': None, 'proposed_expires_at': None,
            'status': 'confirmed', 'updated_at': stamp()})
    elif name == 'dismiss_revision':
        row = require(db, 'memories', record_id)
        if not row.get('proposed_content'):
            raise ValueError('没有待忽略的修改')
        changes.put('memories', record_id, {'proposed_content': None, 'proposed_key': None,
            'proposed_catalog': None, 'proposed_expires_at': None, 'updated_at': stamp()})
    else:
        raise ValueError('未知工具')
    return record_id
