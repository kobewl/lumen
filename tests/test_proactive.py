import json
import tempfile
import unittest
from datetime import datetime,timedelta,timezone
from zoneinfo import ZoneInfo
from lumen.core import Store,Actions,Scheduler,now,stamp
from lumen.calendar import next_occurrence
from lumen.feishu import Feishu,enqueue
from lumen.agent import Agent
from test_lumen import ScriptModel

class ProactiveTest(unittest.TestCase):
    def setUp(self):self.tmp=tempfile.TemporaryDirectory();self.store=Store(self.tmp.name+'/db');self.a=Actions(self.store)
    def tearDown(self):self.store.db.close();self.tmp.cleanup()
    def test_month_end_anchor_recovers_in_march(self):
        zone=ZoneInfo('Asia/Shanghai')
        first='2027-01-31T09:00:00+08:00'
        feb=next_occurrence(first,'monthly',zone,datetime.fromisoformat(first),31)
        self.assertEqual(datetime.fromisoformat(feb).astimezone(zone).day,28)
        march=next_occurrence(feb,'monthly',zone,datetime.fromisoformat(feb),31)
        self.assertEqual(datetime.fromisoformat(march).astimezone(zone).day,31)
    def test_weekdays_skip_weekend_and_dst_keeps_wall_clock(self):
        zone=ZoneInfo('Asia/Shanghai');fri=datetime.fromisoformat('2027-01-08T09:00:00+08:00')
        nxt=datetime.fromisoformat(next_occurrence(fri.isoformat(),'weekdays',zone,fri)).astimezone(zone)
        self.assertEqual(nxt.weekday(),0)
        zone=ZoneInfo('America/New_York');before=datetime.fromisoformat('2027-03-13T09:00:00-05:00')
        nxt=datetime.fromisoformat(next_occurrence(before.isoformat(),'daily',zone,before,wall_time='09:00:00')).astimezone(zone)
        self.assertEqual(nxt.hour,9);self.assertEqual(nxt.utcoffset(),timedelta(hours=-4))
    def test_briefing_uses_real_todos_without_model(self):
        todo=self.a.execute('add_todo',{'title':'交房租','due_at':stamp(now()-timedelta(hours=1))})
        result=self.a.execute('get_today',{})
        self.assertEqual(result['overdue'],1);self.assertIn(todo['id'],result['todo_ids'])
        at=now()+timedelta(seconds=1)
        self.a.execute('create_schedule',{'title':'早报','prompt':'today','kind':'briefing','repeat':'daily','run_at':stamp(at)})
        Scheduler(self.store,self.a,None).tick(at+timedelta(seconds=1))
        self.assertIn('交房租',self.store.state()['runs'][0]['result'])
        self.assertEqual(self.store.state()['runs'][0]['status'],'success')
    def test_snooze_and_edit_reminder(self):
        task=self.a.execute('create_schedule',{'title':'喝水','prompt':'喝水','kind':'reminder','repeat':'daily','run_at':stamp(now()+timedelta(hours=1))})
        self.a.execute('update_schedule',{'id':task['id'],'prompt':'休息'})
        self.a.execute('snooze_schedule',{'id':task['id'],'minutes':30})
        row=self.store.state()['schedules'][0]
        self.assertEqual(row['prompt'],'休息');self.assertEqual(row['repeat'],'daily')
        self.assertLess(datetime.fromisoformat(row['run_at']),now()+timedelta(minutes=31))
    def test_long_reply_chunks_remain_ordered_on_send_failure(self):
        sent=[]
        with self.store.transaction() as db:enqueue(db,'owner','a'*2500+'b'*100,'group')
        bot=Feishu(self.store,None,'owner',lambda *args:(_ for _ in ()).throw(RuntimeError()))
        bot.deliver();self.assertEqual(sum(d['attempts'] for d in self.store.state()['deliveries']),1)
        bot.sender=lambda recipient,text,uid:sent.append(text)
        bot.deliver();self.assertEqual(sent,[])
        with self.store.transaction() as db:db.execute('UPDATE deliveries SET next_at=?',(stamp(now()-timedelta(seconds=1)),))
        bot.deliver();self.assertEqual([text[0] for text in sent],['a','b'])
    def test_queued_inbound_survives_restart(self):
        with self.store.transaction() as db:db.execute('INSERT INTO feishu_inbox (id,status,created_at,sender,content) VALUES (?,?,?,?,?)',('m','queued',stamp(),'owner','{"text":"/help"}'))
        self.store.db.close();self.store=Store(self.tmp.name+'/db');self.a=Actions(self.store)
        bot=Feishu(self.store,Agent(self.store,self.a,ScriptModel([])),'owner',lambda *args:None)
        bot.handle('m','owner','p2p','text','{"text":"/help"}')
        self.assertEqual(self.store.query('SELECT status FROM feishu_inbox')[0]['status'],'done')
        self.assertEqual(len(self.store.state()['deliveries']),1)
