import json
import sqlite3
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from lumen.core import Store,Actions,stamp
from lumen.agent import Agent,Model,ModelError
from lumen.operations import activity,backup,restore,export_data,ProcessLock
from test_lumen import ScriptModel,call

class OperationsTest(unittest.TestCase):
    def setUp(self):self.tmp=tempfile.TemporaryDirectory();self.path=self.tmp.name+'/db';self.store=Store(self.path);self.a=Actions(self.store)
    def tearDown(self):self.store.db.close();self.tmp.cleanup()
    def test_undo_restores_deleted_note_and_refuses_later_conflict(self):
        note=self.a.execute('save_note',{'title':'想法','content':'keep-me'})
        deleted=self.a.execute('delete_note',{'id':note['id']})
        self.a.execute('undo_change',{'id':deleted['receipt_id']})
        self.assertEqual(self.store.state()['notes'][0]['content'],'keep-me')
        with self.assertRaises(ValueError):self.a.execute('undo_change',{'id':deleted['receipt_id']})
        self.a.execute('save_note',{'id':note['id'],'title':'新想法','content':'later-change'})
        with self.assertRaises(ValueError):self.a.execute('undo_change',{'id':note['receipt_id']})
        self.assertEqual(self.store.state()['notes'][0]['content'],'later-change')
    def test_plan_approval_can_be_undone_as_one_transaction(self):
        plan=self.a.execute('propose_plan',{'title':'学习','steps':['一','二']})
        approval=self.a.execute('apply_plan',{'id':plan['id']})
        self.a.execute('undo_change',{'id':approval['receipt_id']})
        state=self.store.state();self.assertEqual(state['todos'],[]);self.assertEqual(state['projects'],[])
        self.assertEqual(state['plans'][0]['status'],'pending')
    def test_backup_restores_all_data_and_saves_previous_database(self):
        self.a.execute('save_memory',{'key':'风格','content':'简洁'})
        saved=backup(self.store)
        self.a.execute('add_todo',{'title':'later'})
        self.store.db.close();restore(saved,self.path);self.store=Store(self.path);self.a=Actions(self.store)
        self.assertEqual(self.store.state()['todos'],[]);self.assertEqual(self.store.state()['memories'][0]['content'],'简洁')
        self.assertTrue(list(Path(self.tmp.name+'/backups').glob('lumen-pre-restore-*.db')))
        self.assertEqual(self.store.query('PRAGMA integrity_check')[0]['integrity_check'],'ok')
    def test_invalid_restore_does_not_overwrite_current_database(self):
        bad=Path(self.tmp.name+'/wrong.db');sqlite3.connect(bad).close()
        self.a.execute('add_todo',{'title':'keep'})
        with self.assertRaises(ValueError):restore(bad,self.path)
        self.assertEqual(self.store.state()['todos'][0]['title'],'keep')
    def test_export_has_personal_records_and_no_environment_secrets(self):
        self.a.execute('save_note',{'title':'private-note','content':'content'})
        with patch.dict('os.environ',{'LUMEN_MODEL_API_KEY':'never-export-this'}):
            data=export_data(self.store)
        self.assertEqual(data['tables']['notes'][0]['title'],'private-note')
        self.assertNotIn('never-export-this',json.dumps(data))
    def test_process_lock_refuses_second_instance(self):
        first=ProcessLock(self.path)
        try:
            with self.assertRaises(ValueError):ProcessLock(self.path)
        finally:first.close()
    def test_receipts_exclude_record_contents(self):
        self.a.execute('save_memory',{'key':'代号','content':'private-code-name'})
        self.assertNotIn('private-code-name',str(activity(self.store)))
    def test_request_id_replay_returns_original_reply_without_duplicate_writes(self):
        model=ScriptModel([call('add_todo',{'title':'once'}),{'role':'assistant','content':'已记录'}])
        agent=Agent(self.store,self.a,model)
        self.assertEqual(agent.reply('帮我记 once',request_id='request-1'),'已记录')
        self.assertEqual(agent.reply('帮我记 once',request_id='request-1'),'已记录')
        self.assertEqual(len(model.inputs),2);self.assertEqual(len(self.store.state()['todos']),1)
        with self.assertRaises(ModelError):agent.reply('different',request_id='request-1')
    def test_budget_stops_model_before_network_request(self):
        with self.store.transaction() as db:db.execute('INSERT INTO usage (id,model,input_tokens,output_tokens,created_at) VALUES (?,?,?,?,?)',('u','test',10,20,stamp()))
        with patch.dict('os.environ',{'LUMEN_MODEL_API_KEY':'test','LUMEN_MODEL_DAILY_CALL_LIMIT':'1'}):
            model=Model();model.store=self.store
            with self.assertRaisesRegex(ModelError,'预算'):model.complete([{'role':'user','content':'hello'}])
    def test_model_cannot_confirm_or_undo_user_records(self):
        with self.assertRaises(ValueError):self.a.execute('undo_change',{},actor='agent')
    def test_automatic_backup_runs_once_daily_and_retains_manual_snapshots(self):
        from lumen.maintenance import backup_loop
        directory=Path(self.tmp.name)/'backups';directory.mkdir()
        for number in range(10):
            (directory/f'lumen-auto-20000101-{number:06d}.db').write_bytes(b'old')
        manual=backup(self.store,directory)
        class Stop:
            count=0
            def is_set(self):return self.count>=2
            def wait(self,seconds):self.count+=1
        with patch.dict('os.environ',{'LUMEN_BACKUP_DIR':str(directory)}):
            backup_loop(self.store,self.a.zone,Stop())
        self.assertEqual(len(list(directory.glob('lumen-auto-*.db'))),7)
        self.assertTrue(manual.exists())
        self.assertTrue(self.store.query("SELECT value FROM settings WHERE key='last_backup'"))
        newest=max(directory.glob('lumen-auto-*.db'))
        with sqlite3.connect(newest) as db:
            self.assertEqual(db.execute('PRAGMA integrity_check').fetchone()[0],'ok')
    def test_failed_automatic_backup_does_not_claim_success(self):
        from lumen.maintenance import backup_loop
        class Stop:
            stopped=False
            def is_set(self):return self.stopped
            def wait(self,seconds):self.stopped=True
        with patch('lumen.maintenance.backup',side_effect=OSError('disk full')),patch('logging.exception') as log:
            backup_loop(self.store,self.a.zone,Stop())
        log.assert_called_once()
        self.assertEqual(self.store.query("SELECT value FROM settings WHERE key='backup_date'"),[])
