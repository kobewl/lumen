import tempfile
import unittest
from datetime import timedelta
from lumen.core import Store,Actions,now,stamp
from lumen.agent import Agent
from lumen.permissions import DELETE_ACTIONS,TABLES
from test_lumen import ScriptModel,call

class PermissionTest(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.path=self.tmp.name+'/db';self.store=Store(self.path);self.a=Actions(self.store)
    def tearDown(self):self.store.db.close();self.tmp.cleanup()
    def make(self,scope):
        if scope=='memory':return self.a.execute('save_memory',{'key':'偏好','content':'简洁'})['id']
        if scope=='todo':return self.a.execute('add_todo',{'title':'读书'})['id']
        if scope=='note':return self.a.execute('save_note',{'title':'资料','content':'原始正文'})['id']
        if scope=='project':return self.a.execute('save_project',{'title':'学习'})['id']
        return self.a.execute('create_schedule',{'title':'提醒','prompt':'喝水','kind':'reminder','repeat':'none','run_at':stamp(now()+timedelta(days=1))})['id']
    def request(self,scope,record_id):return self.a.execute('delete_'+scope,{'id':record_id},actor='agent')
    def exists(self,scope,record_id):return bool(self.store.query('SELECT id FROM '+TABLES[scope]+' WHERE id=?',(record_id,)))
    def test_every_model_deletion_requires_confirmation_by_default(self):
        for scope in TABLES:
            record_id=self.make(scope);result=self.request(scope,record_id)
            self.assertEqual(result['effect'],'pending');self.assertTrue(self.exists(scope,record_id))
            self.a.execute('approve_action',{'id':result['request_id']})
            self.assertFalse(self.exists(scope,record_id))
    def test_user_can_grant_auto_delete_per_scope_without_affecting_other_scopes(self):
        self.a.execute('set_action_policy',{'scope':'note','mode':'auto'})
        note=self.make('note');todo=self.make('todo')
        self.assertEqual(self.request('note',note)['effect'],'deleted');self.assertFalse(self.exists('note',note))
        self.assertEqual(self.request('todo',todo)['effect'],'pending');self.assertTrue(self.exists('todo',todo))
        self.store.db.close();self.store=Store(self.path);self.a=Actions(self.store)
        self.assertEqual(next(row for row in self.store.state()['action_policies'] if row['scope']=='note')['mode'],'auto')
    def test_missing_policy_fails_closed(self):
        with self.store.transaction() as db:db.execute("DELETE FROM action_policies WHERE scope='note'")
        note=self.make('note');self.assertEqual(self.request('note',note)['effect'],'pending')
    def test_model_and_capture_cannot_grant_permissions_or_confirm_requests(self):
        todo=self.make('todo');request=self.request('todo',todo)
        for actor in ('agent','capture'):
            for name,args in [('set_action_policy',{'scope':'todo','mode':'auto'}),('approve_action',{'id':request['request_id']}),('reject_action',{'id':request['request_id']})]:
                with self.assertRaises(ValueError):self.a.execute(name,args,actor=actor)
        self.assertTrue(self.exists('todo',todo))
    def test_identical_requests_reuse_pending_id(self):
        todo=self.make('todo');first=self.request('todo',todo);second=self.request('todo',todo)
        self.assertEqual(first['request_id'],second['request_id'])
        self.assertEqual(len(self.store.state()['pending_actions']),1)
    def test_duplicate_approval_does_not_execute_again(self):
        todo=self.make('todo');request=self.request('todo',todo)
        self.a.execute('approve_action',{'id':request['request_id']})
        before=len(self.store.query('SELECT id FROM changes'))
        self.assertEqual(self.a.execute('approve_action',{'id':request['request_id']})['effect'],'already_applied')
        self.assertEqual(len(self.store.query('SELECT id FROM changes')),before)
    def test_cancel_keeps_record_and_prevents_later_approval(self):
        todo=self.make('todo');request=self.request('todo',todo)
        self.a.execute('reject_action',{'id':request['request_id']})
        before=len(self.store.query('SELECT id FROM changes'))
        self.assertEqual(self.a.execute('reject_action',{'id':request['request_id']})['effect'],'already_rejected')
        self.assertEqual(len(self.store.query('SELECT id FROM changes')),before)
        with self.assertRaises(ValueError):self.a.execute('approve_action',{'id':request['request_id']})
        self.assertTrue(self.exists('todo',todo));self.assertEqual(self.store.state()['pending_actions'],[])
    def test_modified_record_rejects_old_approval_and_new_request_supersedes_it(self):
        note=self.make('note');first=self.request('note',note)
        self.a.execute('save_note',{'id':note,'title':'资料','content':'新的正文'})
        with self.assertRaisesRegex(ValueError,'变化'):self.a.execute('approve_action',{'id':first['request_id']})
        second=self.request('note',note)
        self.assertNotEqual(first['request_id'],second['request_id'])
        self.assertEqual(len(self.store.state()['pending_actions']),1)
        with self.assertRaises(ValueError):self.a.execute('approve_action',{'id':first['request_id']})
        self.a.execute('approve_action',{'id':second['request_id']});self.assertFalse(self.exists('note',note))
    def test_new_linked_reminder_rejects_old_todo_approval(self):
        todo=self.make('todo');request=self.request('todo',todo)
        self.a.execute('update_todo',{'id':todo,'remind_at':stamp(now()+timedelta(days=1))})
        with self.assertRaises(ValueError):self.a.execute('approve_action',{'id':request['request_id']})
        self.assertTrue(self.exists('todo',todo));self.assertEqual(self.store.state()['schedules'][0]['enabled'],1)
    def test_new_project_task_rejects_old_project_approval(self):
        project=self.make('project');request=self.request('project',project)
        self.a.execute('add_todo',{'title':'新任务','project_id':project})
        with self.assertRaises(ValueError):self.a.execute('approve_action',{'id':request['request_id']})
        self.assertTrue(self.exists('project',project));self.assertEqual(self.store.state()['todos'][0]['project_id'],project)
    def test_approval_cannot_delete_running_reminder(self):
        schedule=self.make('schedule')
        with self.store.transaction() as db:db.execute("UPDATE schedules SET status='running' WHERE id=?",(schedule,))
        request=self.request('schedule',schedule)
        with self.assertRaisesRegex(ValueError,'执行'):self.a.execute('approve_action',{'id':request['request_id']})
        self.assertTrue(self.exists('schedule',schedule));self.assertEqual(self.store.state()['pending_actions'][0]['id'],request['request_id'])
    def test_failed_todo_approval_rolls_back_linked_reminder_changes(self):
        todo=self.a.execute('add_todo',{'title':'读书','remind_at':stamp(now()+timedelta(days=1))})['id']
        other=self.make('schedule')
        with self.store.transaction() as db:db.execute("UPDATE schedules SET todo_id=?,status='running' WHERE id=?",(todo,other))
        request=self.request('todo',todo)
        before=self.store.query('SELECT * FROM schedules ORDER BY id')
        with self.assertRaises(ValueError):self.a.execute('approve_action',{'id':request['request_id']})
        self.assertEqual(self.store.query('SELECT * FROM schedules ORDER BY id'),before)
        self.assertTrue(self.exists('todo',todo))
    def test_undo_approved_todo_deletion_restores_linked_reminder_and_request(self):
        todo=self.a.execute('add_todo',{'title':'读书','remind_at':stamp(now()+timedelta(days=1))})['id']
        request=self.request('todo',todo);approved=self.a.execute('approve_action',{'id':request['request_id']})
        self.assertEqual(self.store.state()['schedules'][0]['enabled'],0)
        self.a.execute('undo_change',{'id':approved['receipt_id']})
        self.assertTrue(self.exists('todo',todo));self.assertEqual(self.store.state()['schedules'][0]['enabled'],1)
        self.assertEqual(self.store.state()['pending_actions'][0]['id'],request['request_id'])
    def test_pending_requests_survive_restart_and_commands_need_no_model(self):
        todo=self.make('todo');request=self.request('todo',todo)
        self.store.db.close();self.store=Store(self.path);self.a=Actions(self.store)
        agent=Agent(self.store,self.a,ScriptModel([]))
        self.assertIn('读书',agent.reply('/pending'))
        self.assertIn('已确认',agent.reply('/confirm '+request['request_id'][:8]))
        self.assertFalse(self.exists('todo',todo));self.assertEqual(agent.model.inputs,[])
    def test_pending_memory_delete_keeps_fact_until_confirmation_then_resets_context(self):
        memory=self.a.execute('save_memory',{'key':'代号','content':'original-fact'})['id']
        self.store.message('user','代号是 original-fact')
        model=ScriptModel([call('delete_memory',{'id':memory}),{'role':'assistant','content':'尚未删除，请确认。'}])
        agent=Agent(self.store,self.a,model);agent.reply('忘记代号')
        self.assertTrue(self.exists('memory',memory));self.assertIn('original-fact',model.inputs[1][0]['content'])
        request=self.store.state()['pending_actions'][0]
        agent.reply('/confirm '+request['id'][:8]);self.assertFalse(self.exists('memory',memory))
        follow=ScriptModel([{'role':'assistant','content':'没有记住代号。'}]);Agent(self.store,self.a,follow).reply('我的代号？')
        self.assertNotIn('original-fact',str(follow.inputs))
    def test_pending_metadata_does_not_disclose_snapshot_body(self):
        note=self.a.execute('save_note',{'title':'资料','content':'local-private-body'})['id'];self.request('note',note)
        self.assertNotIn('local-private-body',str(self.store.state()['pending_actions']))
        self.assertNotIn('local-private-body',str(self.a.execute('get_pending_actions',{})))
    def test_scheduled_run_cannot_request_memory_or_schedule_deletion(self):
        for scope in ('memory','schedule'):
            record_id=self.make(scope)
            with self.assertRaises(ValueError):self.a.execute('delete_'+scope,{'id':record_id},actor='agent',scheduled=True)
        self.assertEqual(self.store.state()['pending_actions'],[])
    def test_manual_delete_is_explicit_user_action_and_still_undoable(self):
        note=self.make('note');result=self.a.execute('delete_note',{'id':note})
        self.assertEqual(result['effect'],'deleted');self.assertFalse(self.exists('note',note))
        self.a.execute('undo_change',{'id':result['receipt_id']});self.assertTrue(self.exists('note',note))
