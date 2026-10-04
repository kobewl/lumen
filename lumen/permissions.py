"""Deletion policies, durable approval requests, and conflict-checked execution."""
import json
from .contracts import identifier,stamp
from .domain import Changes,require

DELETE_ACTIONS={'delete_memory':'memory','delete_todo':'todo','delete_note':'note','delete_schedule':'schedule','delete_project':'project'}
TABLES={'memory':'memories','todo':'todos','note':'notes','schedule':'schedules','project':'projects'}
LABELS={'memory':'忘记记忆','todo':'删除待办','note':'删除笔记','schedule':'删除提醒','project':'删除项目'}


def ensure(db):
    for scope in TABLES:
        db.execute('INSERT OR IGNORE INTO action_policies VALUES (?,?,?,?)',(scope+':delete',scope,'delete','confirm'))


def snapshot(db,action,record_id):
    scope=DELETE_ACTIONS[action];table=TABLES[scope]
    result={table:[require(db,table,record_id)]}
    if scope=='todo':
        result['schedules']=[dict(row) for row in db.execute('SELECT * FROM schedules WHERE todo_id=? ORDER BY id',(record_id,))]
    elif scope=='project':
        result['todos']=[dict(row) for row in db.execute('SELECT * FROM todos WHERE project_id=? ORDER BY id',(record_id,))]
    return result


def pending(store):
    rows=store.query("SELECT id,action,record_id,status,created_at FROM action_requests WHERE status='pending' ORDER BY rowid DESC LIMIT 30")
    for row in rows:
        scope=DELETE_ACTIONS[row['action']];table=TABLES[scope]
        item=store.query(f'SELECT * FROM {table} WHERE id=?',(row['record_id'],))
        row['title']=item[0].get('title',item[0].get('key','记录')) if item else '记录已不存在'
        row['label']=LABELS[scope]
        row['detail']='关联任务保留，解除项目归属' if scope=='project' else '关联提醒会停止' if scope=='todo' else ''
    return rows


def request_delete(actions,name,args):
    with actions.store.transaction() as db:
        scope=DELETE_ACTIONS[name]
        policy=db.execute('SELECT mode FROM action_policies WHERE scope=? AND operation=?',(scope,'delete')).fetchone()
        if policy and policy['mode']=='auto':
            changes=Changes(db)
            delete(actions,db,changes,name,args['id'])
            receipt=changes.finish(name,args['id'],'agent')
            return {'ok':True,'effect':'deleted','action':name,'id':args['id'],'receipt_id':receipt}
        current=snapshot(db,name,args['id'])
        payload=json.dumps(current,sort_keys=True,ensure_ascii=False)
        existing=db.execute("SELECT * FROM action_requests WHERE action=? AND record_id=? AND status='pending' ORDER BY rowid DESC LIMIT 1",(name,args['id'])).fetchone()
        if existing and existing['snapshot']==payload:
            request_id=existing['id'];receipt=None
        else:
            changes=Changes(db)
            if existing:changes.put('action_requests',existing['id'],{'status':'superseded','completed_at':stamp()})
            request_id=identifier()
            changes.put('action_requests',request_id,{'action':name,'record_id':args['id'],'snapshot':payload,'status':'pending','created_at':stamp()})
            receipt=changes.finish('request_delete',args['id'],'agent')
    return {'ok':True,'effect':'pending','action':name,'id':args['id'],'request_id':request_id,
            'requires_confirmation':True,'receipt_id':receipt,'message':'尚未删除，请在面板确认或发送 /confirm '+request_id[:8]}


def delete(actions,db,changes,name,record_id):
    scope=DELETE_ACTIONS[name];table=TABLES[scope]
    row=require(db,table,record_id)
    if scope=='schedule' and row['status']=='running':raise ValueError('任务正在执行，请完成后再删除')
    if scope=='todo':
        for item in db.execute('SELECT id,status FROM schedules WHERE todo_id=?',(record_id,)).fetchall():
            if item['status']=='running':raise ValueError('关联提醒正在执行，请稍后删除任务')
            changes.put('schedules',item['id'],{'enabled':0,'todo_id':None})
    if scope=='project':
        for item in db.execute('SELECT id FROM todos WHERE project_id=?',(record_id,)).fetchall():
            changes.put('todos',item['id'],{'project_id':None})
    changes.delete(table,record_id)
    if scope=='memory':actions.store.reset_context(db)


def approve(actions,args):
    with actions.store.transaction() as db:
        row=require(db,'action_requests',args['id'])
        if row['status']=='approved':
            return {'ok':True,'effect':'already_applied','id':row['record_id'],'action':'approve_action','request_id':row['id']}
        if row['status']!='pending':raise ValueError('这项请求已经取消或被替代')
        current=snapshot(db,row['action'],row['record_id'])
        if json.dumps(current,sort_keys=True,ensure_ascii=False)!=row['snapshot']:
            raise ValueError('记录或关联项已变化，请取消这项请求，检查当前内容后重新发起删除')
        changes=Changes(db)
        delete(actions,db,changes,row['action'],row['record_id'])
        changes.put('action_requests',row['id'],{'status':'approved','completed_at':stamp()})
        receipt=changes.finish('approve_action',row['record_id'],'user')
    return {'ok':True,'effect':'deleted','id':row['record_id'],'action':'approve_action','request_id':row['id'],'receipt_id':receipt}


def reject(actions,args):
    with actions.store.transaction() as db:
        row=require(db,'action_requests',args['id'])
        if row['status'] not in ('pending','rejected'):raise ValueError('这项请求已经执行或被替代')
        if row['status']=='rejected':
            return {'ok':True,'effect':'already_rejected','id':row['record_id'],'action':'reject_action','request_id':row['id']}
        changes=Changes(db)
        changes.put('action_requests',row['id'],{'status':'rejected','completed_at':stamp()})
        receipt=changes.finish('reject_action',row['record_id'],'user')
    return {'ok':True,'effect':'rejected','id':row['record_id'],'action':'reject_action','request_id':row['id'],'receipt_id':receipt}


def set_policy(actions,args):
    with actions.store.transaction() as db:
        record_id=args['scope']+':delete';require(db,'action_policies',record_id)
        changes=Changes(db);changes.put('action_policies',record_id,{'mode':args['mode']})
        receipt=changes.finish('set_action_policy',record_id,'user')
    return {'ok':True,'action':'set_action_policy','id':record_id,'receipt_id':receipt}
