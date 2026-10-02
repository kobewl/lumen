"""Validated tools and atomic personal-data mutations."""
from datetime import datetime, timezone, timedelta
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
                expired_changes=Changes(db)
                for item in expired:
                    expired_changes.touch('memories',item['id'])
                    db.execute("UPDATE memories SET status='expired' WHERE id=?", (item['id'],))
                self.store.reset_context(db)
                expired_changes.finish('expire_memory',expired[0]['id'],'system')
        memories = self.store.query("SELECT * FROM memories WHERE status='confirmed' AND (expires_at IS NULL OR expires_at>?) ORDER BY CASE catalog WHEN 'soul' THEN 0 ELSE 1 END, updated_at DESC LIMIT 40", (stamp(),))
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
        revisions = []
        for row in memories:
            if row.get('proposed_content'):
                revisions.append({'id': row['id'], 'catalog': row.get('catalog'), 'key': row['key'],
                                  'proposed_key': row.get('proposed_key'), 'proposed_content': row['proposed_content'][:400],
                                  'proposed_catalog': row.get('proposed_catalog'), 'proposed_expires_at': row.get('proposed_expires_at')})
            for field in ('proposed_content', 'proposed_key', 'proposed_catalog', 'proposed_expires_at'):
                row.pop(field, None)
        pending = self.store.query("SELECT id,catalog,key FROM memories WHERE status='pending' ORDER BY updated_at DESC LIMIT 8")
        policies = self.store.query('SELECT catalog,operation,mode FROM memory_policies ORDER BY catalog,operation')
        return {'plans':plans,'memories':memories,'pending_memories':pending,'pending_revisions':revisions,
                'memory_policy':policies,'todos':todos,'schedules':schedules,'notes':notes,'projects':projects,
                'counts':counts,'hint':'memories 才是已生效事实，按 catalog 分为 soul 与 daily。pending_memories 和 pending_revisions 还没有生效。'}

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
        if scheduled and name in ('create_schedule','update_schedule','delete_schedule','save_memory','delete_memory','confirm_memory','apply_plan','reject_plan','propose_plan','snooze_schedule'):
            raise ValueError('定时执行不能修改记忆或管理其他定时任务')
        if name=='set_memory_policy':
            with self.store.transaction() as db:
                row = db.execute('SELECT * FROM memory_policies WHERE catalog=? AND operation=?', (args['catalog'], args['operation'])).fetchone()
                if row is None:
                    raise ValueError('未知的记忆权限')
                changes = Changes(db)
                changes.put('memory_policies', row['id'], {'mode': args['mode']})
                receipt = changes.finish(name, row['id'], actor)
            return {'ok': True, 'id': row['id'], 'action': name, 'receipt_id': receipt, 'catalog': args['catalog'], 'operation': args['operation'], 'mode': args['mode']}
        if name=='undo_change':
            from .operations import undo
            return undo(self,args.get('id'))
        if name=='get_activity':
            from .operations import activity
            return {'activity':activity(self.store)}
        if name=='retry_delivery':
            with self.store.transaction() as db:
                require(db,'deliveries',args['id'])
                db.execute("UPDATE deliveries SET status='pending',next_at=?,last_error=NULL WHERE id=?",(stamp(),args['id']))
            return {'ok':True,'id':args['id'],'action':name}
        if name=='get_today':
            from .briefing import briefing
            return briefing(self,args.get('mode','today'))
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
        extra = {}
        with self.store.transaction() as db:
            changes = Changes(db)
            if name=='save_memory':
                record_id, extra = self._save_memory(db, changes, args, record_id, actor)
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
                run_at=self.date(args['run_at'],future=True)
                local=datetime.fromisoformat(run_at).astimezone(self.zone)
                if args['repeat']=='weekdays':
                    while local.weekday()>=5: local+=timedelta(days=1)
                    run_at=stamp(local.astimezone(timezone.utc))
                if args['kind']=='briefing' and args['prompt'] not in ('today','weekly'):
                    raise ValueError('简报任务 prompt 必须为 today 或 weekly')
                changes.put('schedules',record_id,{'title':args['title'],'prompt':args['prompt'],'kind':args['kind'],
                    'run_at':run_at,'repeat':args['repeat'],'created_at':stamp(),'month_day':local.day,'wall_time':local.strftime('%H:%M:%S'),'zone_name':self.zone.key})
            elif name in ('delete_schedule','update_schedule','snooze_schedule'):
                row = require(db,'schedules',record_id)
                if row['status']=='running':
                    raise ValueError('任务正在执行，请完成后再修改')
                if name=='delete_schedule':
                    changes.delete('schedules',record_id)
                elif name=='snooze_schedule':
                    changes.put('schedules',record_id,{'run_at':stamp(now()+timedelta(minutes=args['minutes'])),'enabled':1,'status':'pending','last_error':None})
                else:
                    if len(args)==1:
                        raise ValueError('需要提供修改字段')
                    enabled=int(args.get('enabled',row['enabled']))
                    run_at=self.date(args['run_at'],future=True) if 'run_at' in args else row['run_at']
                    if enabled and datetime.fromisoformat(run_at)<=now():
                        raise ValueError('启用已过期任务需要提供未来 run_at')
                    values={key:args[key] for key in ('title','prompt','repeat','kind') if key in args}
                    if args.get('kind',row['kind'])=='briefing' and args.get('prompt',row['prompt']) not in ('today','weekly'):
                        raise ValueError('简报任务 prompt 必须为 today 或 weekly')
                    if 'run_at' in args:
                        local=datetime.fromisoformat(run_at).astimezone(self.zone)
                        values.update(month_day=local.day,wall_time=local.strftime('%H:%M:%S'),zone_name=self.zone.key)
                    if 'run_at' in args or 'enabled' in args:
                        values.update(enabled=enabled,run_at=run_at,status='pending',last_error=None)
                    changes.put('schedules',record_id,values)

            else:
                record_id = execute(self,db,changes,name,args,record_id)
            receipt = changes.finish(name,record_id,actor)
        result = {'ok':True,'id':record_id,'action':name,'receipt_id':receipt}
        result.update(extra)
        return result

    def _save_memory(self, db, changes, args, record_id, actor):
        from .policy import catalog_for, grant
        key = args['key'].strip()
        content = args['content'].strip()
        row = require(db,'memories',record_id) if 'id' in args else db.execute('SELECT * FROM memories WHERE key=?',(key,)).fetchone()
        row = dict(row) if row else {}
        if row:
            record_id = row['id']
        if row.get('status')=='confirmed' and row.get('expires_at') and row['expires_at']<=stamp():
            row['status'] = 'expired'  # Lapsed but not swept yet; treat it as already gone.
        if db.execute('SELECT id FROM memories WHERE key=? AND id!=?',(key,record_id)).fetchone():
            raise ValueError('这个名称属于另一条记忆，请先确认要修改哪一条')
        if actor=='agent' and row.get('status')=='confirmed' and args.get('status')=='pending':
            raise ValueError('已有确认记忆；候选更正请先询问用户，不覆盖原事实')
        catalog = catalog_for(args, row)
        expiry = self.date(args['expires_at'],future=True) if args.get('expires_at') else (None if 'expires_at' in args else None if row.get('status')=='expired' else row.get('expires_at'))
        if 'category' in args:
            category = args['category']
        elif row.get('category'):
            category = row['category']
        elif catalog=='daily':
            category = 'temporary' if expiry else 'work'
        else:
            category = 'personal'
        # A new period only counts when the speaker actually gave one; revived facts are never "unchanged".
        new_expiry = expiry if args.get('expires_at') else None
        changed = bool(row) and (row.get('content')!=content or row.get('key')!=key or row.get('catalog')!=catalog
                                 or (new_expiry is not None and new_expiry!=row.get('expires_at'))
                                 or row.get('status')=='expired')
        write_key, write_content, write_catalog = key, content, catalog
        proposed_content = proposed_key = proposed_catalog = proposed_expires_at = None
        status = args.get('status', 'confirmed' if row.get('status')=='expired' else row.get('status','confirmed'))
        effect = 'stored'
        if actor=='capture':
            operation = 'revise' if row.get('status')=='confirmed' and changed else 'capture'
            if row and not changed:
                return record_id, {'effect':'unchanged','catalog':row.get('catalog'),'status':row.get('status')}
            governed = row.get('catalog') if operation=='revise' and row.get('catalog') else catalog
            if grant(self.store, governed, operation)=='confirm' and operation=='revise':
                write_key, write_content, write_catalog = row['key'], row['content'], row.get('catalog') or catalog
                category, status, expiry = row.get('category', category), row['status'], row.get('expires_at')
                proposed_content, proposed_key = content, key
                proposed_catalog, proposed_expires_at = catalog, new_expiry
                effect = 'proposed'
            elif grant(self.store, catalog, 'capture')=='confirm' and operation=='capture':
                status, effect = 'pending', 'pending'
            else:
                status, effect = 'confirmed', 'stored'
        elif actor=='agent':
            if (not row or row.get('status')!='confirmed') and (args.get('status')=='pending' or grant(self.store, catalog, 'capture')=='confirm'):
                status, effect = 'pending', 'pending'
            elif args.get('status')=='pending':
                status, effect = 'pending', 'pending'
            else:
                status, effect = 'confirmed', 'stored'
        elif args.get('status')=='pending':
            status, effect = 'pending', 'pending'
        else:
            status = 'confirmed' if row.get('status')=='expired' or 'status' not in args else args.get('status', status)
            if status!='pending':
                status, effect = 'confirmed', 'stored'
        if effect=='unchanged':
            return record_id, {'effect':effect,'catalog':write_catalog,'status':status}
        changes.put('memories',record_id,{'key':write_key,'content':write_content,'updated_at':stamp(),
            'category':category,'status':status,'expires_at':expiry,'source':actor,'catalog':write_catalog,
            'proposed_content':proposed_content,'proposed_key':proposed_key,
            'proposed_catalog':proposed_catalog,'proposed_expires_at':proposed_expires_at})
        return record_id, {'effect':effect,'catalog':write_catalog,'status':status}
