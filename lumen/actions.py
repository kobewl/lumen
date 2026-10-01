"""Validated tools and atomic personal-data mutations."""
from datetime import datetime, timezone
from zoneinfo import ZoneInfo

from .core import CATALOG, USER_ACTIONS, identifier, now, stamp
from .domain import Changes, TABLES, execute, require, search


class Actions:
    def __init__(self, store, timezone_name='Asia/Shanghai'):
        self.store, self.zone = store, ZoneInfo(timezone_name)

    def date(self, value, future=False):
        dt = datetime.fromisoformat(value.replace('Z', '+00:00'))
        if dt.tzinfo is None:
            raise ValueError('时间必须包含时区，例如 2026-10-01T09:00:00+08:00')
        if future and dt <= now():
            raise ValueError('执行时间必须在未来')
        return stamp(dt.astimezone(timezone.utc))

    def context(self, text=''):
        with self.store.transaction() as db:
            expired = db.execute("SELECT id FROM memories WHERE status!='expired' AND expires_at IS NOT NULL AND expires_at<=?", (stamp(),)).fetchall()
            if expired:
                for item in expired:
                    db.execute("UPDATE memories SET status='expired' WHERE id=?", (item['id'],))
                self.store.reset_context(db)
        memories = self.store.query("SELECT * FROM memories WHERE status='confirmed' AND (expires_at IS NULL OR expires_at>?) ORDER BY updated_at DESC LIMIT 40", (stamp(),))
        todos = self.store.query("SELECT * FROM todos WHERE done=0 ORDER BY CASE priority WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END,due_at IS NULL,due_at,created_at DESC LIMIT 30")
        schedules = self.store.query("SELECT * FROM schedules WHERE enabled=1 ORDER BY run_at LIMIT 15")
        notes = self.store.query('SELECT id,title,tags,updated_at FROM notes ORDER BY updated_at DESC LIMIT 15')
        projects = self.store.query("SELECT * FROM projects WHERE status='active' ORDER BY created_at DESC LIMIT 15")
        counts = {table: self.store.query(f'SELECT COUNT(*) AS n FROM {table}')[0]['n'] for table in ('memories','todos','notes','projects','schedules')}
        for rows in (memories,todos,schedules,notes,projects):
            for row in rows:
                for key,value in list(row.items()):
                    if isinstance(value,str):
                        row[key] = value[:(800 if key=='content' else 200 if key in ('key','title') else 400)]
        plans = self.store.query("SELECT * FROM plans WHERE status='pending' ORDER BY created_at DESC LIMIT 3")
        for item in plans:
            import json
            item['steps'] = [step[:120] for step in json.loads(item['steps'])]
        return {'plans':plans,'memories':memories,'todos':todos,'schedules':schedules,'notes':notes,'projects':projects,
                'counts':counts,'hint':'这是有限上下文，其他记录使用 search_records 或 get_record 查询。'}

    def execute(self, name, args, scheduled=False, actor='user'):
        if not isinstance(name,str) or name not in CATALOG or not isinstance(args,dict):
            raise ValueError('未知工具或无效参数')
        schema = CATALOG[name]
        if set(args)-set(schema['properties']) or set(schema['required'])-set(args):
            raise ValueError('缺少必要参数或存在未知参数')
        for key,value in args.items():
            prop = schema['properties'][key]
            if prop['type']=='boolean':
                valid = type(value) is bool
            elif prop['type']=='integer':
                valid = type(value) is int and prop.get('minimum',0)<=value<=prop.get('maximum',10000)
            elif prop['type']=='array':
                valid = isinstance(value,list) and prop.get('minItems',0)<=len(value)<=prop.get('maxItems',20) and all(isinstance(v,str) and v.strip() and len(v)<=1000 for v in value)
            else:
                valid = isinstance(value,str) and len(value.strip())>=prop.get('minLength',1) and len(value)<=prop.get('maxLength',4000)
            if not valid or ('enum' in prop and value not in prop['enum']):
                raise ValueError(f'{key} 取值无效')
        if actor=='agent' and name in {t['function']['name'] for t in USER_ACTIONS}:
            raise ValueError('此操作需要用户明确确认，请使用管理面板或快捷命令')
        if scheduled and name in ('create_schedule','update_schedule','delete_schedule','save_memory','delete_memory','confirm_memory','apply_plan','reject_plan','propose_plan'):
            raise ValueError('定时执行不能修改记忆或管理其他定时任务')
        if name=='get_state':
            return self.context()
        if name=='search_records':
            return search(self,args['query'],args.get('scope','all'))
        if name=='get_record':
            table = TABLES[args['type']]
            with self.store.transaction() as db:
                row = require(db,table,args['id'])
            if table=='memories' and (row['status']!='confirmed' or (row['expires_at'] and row['expires_at']<=stamp())):
                raise ValueError('记忆未确认或已过期')
            return row
        record_id = args.get('id',identifier())
        with self.store.transaction() as db:
            changes = Changes(db)
            if name=='save_memory':
                key = args['key'].strip()
                row = require(db,'memories',record_id) if 'id' in args else db.execute('SELECT * FROM memories WHERE key=?',(key,)).fetchone()
                row = dict(row) if row else {}
                if row:
                    record_id = row['id']
                if db.execute('SELECT id FROM memories WHERE key=? AND id!=?',(key,record_id)).fetchone():
                    raise ValueError('这个名称属于另一条记忆，请先确认要修改哪一条')
                if actor=='agent' and row.get('status')=='confirmed' and args.get('status')=='pending':
                    raise ValueError('已有确认记忆；候选更正请先询问用户，不覆盖原事实')
                expiry = self.date(args['expires_at'],future=True) if args.get('expires_at') else (None if 'expires_at' in args else row.get('expires_at'))
                changes.put('memories',record_id,{'key':key,'content':args['content'].strip(),'updated_at':stamp(),
                    'category':args.get('category',row.get('category','personal')),'status':args.get('status',row.get('status','confirmed')),
                    'expires_at':expiry,'source':actor})
            elif name=='delete_memory':
                require(db,'memories',record_id)
                changes.delete('memories',record_id)
                self.store.reset_context(db)
            elif name in ('add_todo','update_todo'):
                row = require(db,'todos',record_id) if name=='update_todo' else {}
                if name=='update_todo' and len(args)==1:
                    raise ValueError('需要提供要修改的字段')
                project = args.get('project_id',row.get('project_id')) or None
                if project:
                    require(db,'projects',project)
                due = self.date(args['due_at']) if args.get('due_at') else (None if 'due_at' in args else row.get('due_at'))
                done = int(args.get('done',row.get('done',0)))
                changes.put('todos',record_id,{'title':args.get('title',row.get('title','')).strip(),'due_at':due,
                    'done':done,'created_at':row.get('created_at',stamp()),'priority':args.get('priority',row.get('priority','normal')),
                    'project_id':project,'notes':args.get('notes',row.get('notes','')),
                    'completed_at':(row.get('completed_at') or stamp()) if done else None})
                linked = db.execute('SELECT id FROM schedules WHERE todo_id=?',(record_id,)).fetchall()
                if done or 'remind_at' in args:
                    for item in linked:
                        if require(db,'schedules',item['id'])['status']=='running':
                            raise ValueError('关联提醒正在执行，请稍后修改任务')
                        changes.put('schedules',item['id'],{'enabled':0})
                if args.get('remind_at') and not done:
                    item_id = identifier()
                    changes.put('schedules',item_id,{'title':'待办提醒：'+args.get('title',row.get('title','')),
                        'prompt':args.get('title',row.get('title','')),'kind':'reminder','repeat':'none','run_at':self.date(args['remind_at'],future=True),
                        'created_at':stamp(),'todo_id':record_id})
            elif name=='delete_todo':
                require(db,'todos',record_id)
                for item in db.execute('SELECT id,status FROM schedules WHERE todo_id=?',(record_id,)).fetchall():
                    if item['status']=='running':
                        raise ValueError('关联提醒正在执行，请稍后删除任务')
                    changes.put('schedules',item['id'],{'enabled':0,'todo_id':None})
                changes.delete('todos',record_id)
            elif name=='create_schedule':
                changes.put('schedules',record_id,{'title':args['title'],'prompt':args['prompt'],'kind':args['kind'],
                    'run_at':self.date(args['run_at'],future=True),'repeat':args['repeat'],'created_at':stamp()})
            elif name in ('delete_schedule','update_schedule'):
                row = require(db,'schedules',record_id)
                if row['status']=='running':
                    raise ValueError('任务正在执行，请完成后再修改')
                if name=='delete_schedule':
                    changes.delete('schedules',record_id)
                else:
                    if args['enabled'] and not args.get('run_at'):
                        raise ValueError('重新启用任务需要提供未来 run_at')
                    run_at = self.date(args['run_at'],future=True) if args['enabled'] else row['run_at']
                    changes.put('schedules',record_id,{'enabled':int(args['enabled']),'run_at':run_at,'status':'pending','last_error':None})
            else:
                record_id = execute(self,db,changes,name,args,record_id)
            receipt = changes.finish(name,record_id,actor)
        return {'ok':True,'id':record_id,'action':name,'receipt_id':receipt}
