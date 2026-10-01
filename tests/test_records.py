import tempfile
import unittest
from datetime import timedelta
from lumen.core import Actions, Store, now, stamp


class RecordsTest(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory()
        self.store=Store(self.tmp.name+'/test.db')
        self.actions=Actions(self.store)
    def tearDown(self):
        self.store.db.close();self.tmp.cleanup()
    def test_candidate_memory_requires_user_confirmation(self):
        memory=self.actions.execute('save_memory',{'key':'偏好','content':'少糖','status':'pending'},actor='agent')
        self.assertEqual(self.actions.context()['memories'],[])
        with self.assertRaises(ValueError):self.actions.execute('confirm_memory',{'id':memory['id']},actor='agent')
        self.actions.execute('confirm_memory',{'id':memory['id']})
        self.assertEqual(self.actions.context()['memories'][0]['content'],'少糖')
    def test_expiry_resets_old_history(self):
        self.store.message('user','temporary-private-fact')
        memory=self.actions.execute('save_memory',{'key':'临时地点','content':'temporary-private-fact','expires_at':stamp(now()+timedelta(hours=1))})
        with self.store.transaction() as db:db.execute('UPDATE memories SET expires_at=? WHERE id=?',(stamp(now()-timedelta(hours=1)),memory['id']))
        self.assertEqual(self.actions.context()['memories'],[])
        self.assertEqual(self.store.history(),[])
    def test_task_reminder_completion_and_atomic_failure(self):
        project=self.actions.execute('save_project',{'title':'学习','goal':'完成课程'})
        todo=self.actions.execute('add_todo',{'title':'第一课','priority':'high','project_id':project['id'],'remind_at':stamp(now()+timedelta(hours=1))})
        self.assertEqual(len(self.store.state()['schedules']),1)
        self.actions.execute('update_todo',{'id':todo['id'],'done':True})
        self.assertEqual(self.store.state()['schedules'][0]['enabled'],0)
        with self.assertRaises(ValueError):self.actions.execute('add_todo',{'title':'invalid','project_id':'missing'})
        self.assertEqual(len(self.store.state()['todos']),1)
    def test_note_search_reads_full_record_and_literal_wildcards(self):
        note=self.actions.execute('save_note',{'title':'学习资料','content':'SQLite 的事务保证','tags':'数据库'})
        result=self.actions.execute('search_records',{'query':'事务','scope':'notes'})
        self.assertEqual(result['records'][0]['id'],note['id'])
        self.assertEqual(self.actions.execute('search_records',{'query':'%','scope':'notes'})['records'],[])
        self.assertEqual(self.actions.execute('get_record',{'type':'note','id':note['id']})['content'],'SQLite 的事务保证')
    def test_context_is_bounded_with_many_records(self):
        for i in range(50):self.actions.execute('save_memory',{'key':str(i),'content':'x'*4000})
        self.assertEqual(len(self.actions.context()['memories']),40)
        self.assertLess(len(str(self.actions.context())),80000)
    def test_every_mutation_has_a_receipt(self):
        note=self.actions.execute('save_note',{'title':'想法','content':'做一个好助手'})
        self.assertTrue(note['receipt_id'])
        self.assertEqual(self.store.query('SELECT action FROM changes')[0]['action'],'save_note')
