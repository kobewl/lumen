import json
import sqlite3
import tempfile
import unittest
from datetime import timedelta

from lumen.agent import Agent
from lumen.actions import Actions
from lumen.capture import file_utterance, parse_items
from lumen.core import Store, now, stamp
from test_lumen import ScriptModel


class CaptureTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.store = Store(self.tmp.name + '/test.db')
        self.actions = Actions(self.store)

    def tearDown(self):
        self.store.db.close()
        self.tmp.cleanup()

    def test_parse_keeps_two_catalogs_and_drops_junk(self):
        raw = '说明\n{"items":[{"catalog":"soul","key":"口味","content":"少糖"},{"catalog":"daily","key":"所在地","content":"在上海出差","expires_at":""},{"catalog":"secret","key":"x","content":"y"},{"key":"缺目录","content":"无"}]}'
        self.assertEqual(parse_items(raw), [
            {'catalog': 'soul', 'key': '口味', 'content': '少糖'},
            {'catalog': 'daily', 'key': '所在地', 'content': '在上海出差'},
        ])
        self.assertEqual(parse_items('没有json'), [])

    def test_capture_files_new_facts_without_a_second_confirmation(self):
        class Finder:
            def classify(self, text, memories):
                self.seen = (text, memories)
                return json.dumps({'items': [
                    {'catalog': 'soul', 'key': '口味', 'content': '讨厌香菜'},
                    {'catalog': 'daily', 'key': '所在地', 'content': '在上海出差'},
                ]})
        finder = Finder()
        filed = file_utterance(finder, self.actions, '我在上海出差，而且我讨厌香菜')
        self.assertEqual(finder.seen[0], '我在上海出差，而且我讨厌香菜')
        self.assertEqual({item['effect'] for item in filed}, {'stored'})
        rows = {row['key']: row for row in self.store.state()['memories']}
        self.assertEqual(rows['口味']['catalog'], 'soul')
        self.assertEqual(rows['口味']['status'], 'confirmed')
        self.assertEqual(rows['所在地']['catalog'], 'daily')
        self.assertEqual({row['content'] for row in self.actions.context()['memories']}, {'讨厌香菜', '在上海出差'})

    def test_soul_revision_stays_a_proposal_until_the_user_accepts(self):
        self.actions.execute('save_memory', {'key': '称呼', 'content': '小林', 'catalog': 'soul'})
        result = self.actions.execute('save_memory', {'key': '称呼', 'content': '阿林', 'catalog': 'soul'}, actor='capture')
        self.assertEqual(result['effect'], 'proposed')
        row = self.store.state()['memories'][0]
        self.assertEqual(row['content'], '小林')
        self.assertEqual(row['proposed_content'], '阿林')
        self.assertEqual(self.actions.context()['memories'][0]['content'], '小林')
        self.assertNotIn('proposed_content', self.actions.context()['memories'][0])
        self.assertEqual(self.actions.context()['pending_revisions'][0]['proposed_content'], '阿林')
        with self.assertRaises(ValueError):
            self.actions.execute('accept_revision', {'id': row['id']}, actor='agent')
        self.actions.execute('accept_revision', {'id': row['id']})
        self.assertEqual(self.store.state()['memories'][0]['content'], '阿林')
        self.assertIsNone(self.store.state()['memories'][0]['proposed_content'])

    def test_daily_revision_applies_immediately(self):
        self.actions.execute('save_memory', {'key': '所在地', 'content': '上海', 'catalog': 'daily'})
        result = self.actions.execute('save_memory', {'key': '所在地', 'content': '杭州', 'catalog': 'daily'}, actor='capture')
        self.assertEqual(result['effect'], 'stored')
        self.assertEqual(self.store.state()['memories'][0]['content'], '杭州')

    def test_confirm_grant_holds_new_soul_facts_for_the_user(self):
        self.actions.execute('set_memory_policy', {'catalog': 'soul', 'operation': 'capture', 'mode': 'confirm'})
        with self.assertRaises(ValueError):
            self.actions.execute('set_memory_policy', {'catalog': 'soul', 'operation': 'capture', 'mode': 'auto'}, actor='agent')
        captured = self.actions.execute('save_memory', {'key': '口味', 'content': '少糖', 'catalog': 'soul'}, actor='capture')
        self.assertEqual(captured['effect'], 'pending')
        self.assertEqual(self.actions.context()['memories'], [])
        self.assertEqual(self.actions.context()['pending_memories'][0]['key'], '口味')
        asked = self.actions.execute('save_memory', {'key': '称呼', 'content': '王栋', 'catalog': 'soul'}, actor='agent')
        self.assertEqual(asked['status'], 'pending')
        self.actions.execute('save_memory', {'key': '城市', 'content': '上海', 'catalog': 'daily'})
        city = next(row for row in self.store.state()['memories'] if row['key'] == '城市')
        self.assertEqual(city['status'], 'confirmed')
        self.assertEqual(city['catalog'], 'daily')

    def test_explicit_chat_correction_still_updates_soul(self):
        memory = self.actions.execute('save_memory', {'key': '称呼', 'content': '小林'})
        updated = self.actions.execute('save_memory', {'id': memory['id'], 'key': '称呼', 'content': '阿林'}, actor='agent')
        self.assertEqual(updated['effect'], 'stored')
        self.assertEqual(self.store.state()['memories'][0]['content'], '阿林')
        self.assertIsNone(self.store.state()['memories'][0]['proposed_content'])

    def test_reply_captures_before_the_chat_model_and_skips_scheduled_runs(self):
        class Both(ScriptModel):
            def __init__(self, responses):
                super().__init__(responses)
                self.calls = 0

            def classify(self, text, memories):
                self.calls += 1
                return json.dumps({'items': [{'catalog': 'daily', 'key': '所在地', 'content': '上海'}]})

        scheduled = Both([{'role': 'assistant', 'content': '定时结果'}])
        Agent(self.store, self.actions, scheduled).reply('我现在在上海', scheduled=True)
        self.assertEqual(scheduled.calls, 0)
        self.assertEqual(self.store.state()['memories'], [])
        live = Both([{'role': 'assistant', 'content': '记下了，你在上海。'}])
        self.assertEqual(Agent(self.store, self.actions, live).reply('我现在在上海'), '记下了，你在上海。')
        self.assertEqual(live.calls, 1)
        self.assertEqual(self.store.state()['memories'][0]['catalog'], 'daily')
        self.assertIn('本轮捕获器已经处理', live.inputs[0][2]['content'])
        self.assertIn('"effect": "stored"', live.inputs[0][2]['content'])

    def test_same_fact_with_new_expiry_updates_it(self):
        first = stamp(now() + timedelta(hours=1))
        later = stamp(now() + timedelta(days=3))
        self.actions.execute('save_memory', {'key': '所在地', 'content': '上海', 'catalog': 'daily', 'expires_at': first})
        result = self.actions.execute('save_memory', {'key': '所在地', 'content': '上海', 'catalog': 'daily', 'expires_at': later}, actor='capture')
        self.assertEqual(result['effect'], 'stored')
        self.assertEqual(self.store.state()['memories'][0]['expires_at'], later)
        again = self.actions.execute('save_memory', {'key': '所在地', 'content': '上海', 'catalog': 'daily', 'expires_at': later}, actor='capture')
        self.assertEqual(again['effect'], 'unchanged')
        self.assertEqual(len(self.store.query('SELECT id FROM changes')), 2)

    def test_expired_fact_mentioned_again_comes_back(self):
        memory = self.actions.execute('save_memory', {'key': '所在地', 'content': '上海', 'catalog': 'daily',
                                                       'expires_at': stamp(now() + timedelta(hours=1))})
        with self.store.transaction() as db:
            db.execute('UPDATE memories SET expires_at=? WHERE id=?', (stamp(now() - timedelta(hours=1)), memory['id']))
        self.assertEqual(self.actions.context()['memories'], [])
        self.assertEqual(self.store.state()['memories'][0]['status'], 'expired')
        renewed = stamp(now() + timedelta(days=2))
        result = self.actions.execute('save_memory', {'key': '所在地', 'content': '上海', 'catalog': 'daily', 'expires_at': renewed}, actor='capture')
        self.assertEqual(result['effect'], 'stored')
        row = self.store.state()['memories'][0]
        self.assertEqual((row['status'], row['expires_at']), ('confirmed', renewed))
        self.assertEqual(self.actions.context()['memories'][0]['content'], '上海')

    def _lapse_without_sweep(self, key='所在地', content='上海'):
        memory = self.actions.execute('save_memory', {'key': key, 'content': content, 'catalog': 'daily',
                                                       'expires_at': stamp(now() + timedelta(hours=1))})
        with self.store.transaction() as db:
            db.execute('UPDATE memories SET expires_at=? WHERE id=?', (stamp(now() - timedelta(hours=1)), memory['id']))
        row = self.store.state()['memories'][0]
        self.assertEqual(row['status'], 'confirmed')  # lapsed, but nobody has swept it yet
        return memory['id']

    def _chat_after_lapse(self, with_expiry):
        class Capturing(ScriptModel):
            def classify(self, text, memories):
                item = {'catalog': 'daily', 'key': '所在地', 'content': '上海'}
                if with_expiry:
                    item['expires_at'] = stamp(now() + timedelta(days=2))
                return json.dumps({'items': [item]})

        self._lapse_without_sweep()
        model = Capturing([{'role': 'assistant', 'content': '好的。'}])
        Agent(self.store, self.actions, model).reply('我还在上海')
        row = self.store.state()['memories'][0]
        self.assertEqual(row['status'], 'confirmed')
        self.assertEqual(self.actions.context()['memories'][0]['content'], '上海')
        self.assertIn('"effect": "stored"', model.inputs[0][2]['content'])
        return row

    def test_chat_right_after_lapse_revives_the_fact_before_the_sweep(self):
        row = self._chat_after_lapse(with_expiry=False)
        self.assertIsNone(row['expires_at'])

    def test_chat_right_after_lapse_with_new_expiry(self):
        row = self._chat_after_lapse(with_expiry=True)
        self.assertGreater(row['expires_at'], stamp())

    def test_save_memory_treats_a_lapsed_confirmed_row_as_expired(self):
        self._lapse_without_sweep()
        result = self.actions.execute('save_memory', {'key': '所在地', 'content': '上海', 'catalog': 'daily'}, actor='capture')
        self.assertEqual(result['effect'], 'stored')
        row = self.store.state()['memories'][0]
        self.assertEqual((row['status'], row['expires_at']), ('confirmed', None))
        self.assertEqual(self.actions.context()['memories'][0]['content'], '上海')

    def test_lapsed_soul_row_is_revived_not_proposed(self):
        memory = self.actions.execute('save_memory', {'key': '出差', 'content': '上海', 'catalog': 'soul',
                                                       'expires_at': stamp(now() + timedelta(hours=1))})
        with self.store.transaction() as db:
            db.execute('UPDATE memories SET expires_at=? WHERE id=?', (stamp(now() - timedelta(hours=1)), memory['id']))
        result = self.actions.execute('save_memory', {'key': '出差', 'content': '杭州', 'catalog': 'soul'}, actor='capture')
        self.assertEqual(result['effect'], 'stored')
        self.assertEqual(self.store.state()['memories'][0]['content'], '杭州')

    def test_expired_fact_without_new_expiry_comes_back_without_one(self):
        memory = self.actions.execute('save_memory', {'key': '所在地', 'content': '上海', 'catalog': 'daily',
                                                       'expires_at': stamp(now() + timedelta(hours=1))})
        with self.store.transaction() as db:
            db.execute('UPDATE memories SET expires_at=? WHERE id=?', (stamp(now() - timedelta(hours=1)), memory['id']))
        self.actions.context()
        result = self.actions.execute('save_memory', {'key': '所在地', 'content': '上海', 'catalog': 'daily'}, actor='capture')
        self.assertEqual(result['effect'], 'stored')
        row = self.store.state()['memories'][0]
        self.assertEqual((row['status'], row['expires_at']), ('confirmed', None))

    def test_proposal_keeps_new_expiry_and_catalog_until_accepted(self):
        self.actions.execute('save_memory', {'key': '住处', 'content': '上海', 'catalog': 'soul'})
        until = stamp(now() + timedelta(days=5))
        result = self.actions.execute('save_memory', {'key': '住处', 'content': '杭州', 'catalog': 'daily', 'expires_at': until}, actor='capture')
        self.assertEqual(result['effect'], 'proposed')
        row = self.store.state()['memories'][0]
        self.assertEqual((row['content'], row['catalog'], row['expires_at']), ('上海', 'soul', None))
        pending = self.actions.context()['pending_revisions'][0]
        self.assertEqual((pending['proposed_catalog'], pending['proposed_expires_at']), ('daily', until))
        self.assertNotIn('proposed_expires_at', self.actions.context()['memories'][0])
        self.actions.execute('accept_revision', {'id': row['id']})
        row = self.store.state()['memories'][0]
        self.assertEqual((row['content'], row['catalog'], row['expires_at'], row['status']), ('杭州', 'daily', until, 'confirmed'))
        for field in ('proposed_content', 'proposed_key', 'proposed_catalog', 'proposed_expires_at'):
            self.assertIsNone(row[field])

    def test_expiry_only_proposal_can_be_accepted_and_dismissed(self):
        self.actions.execute('save_memory', {'key': '称呼', 'content': '小林', 'catalog': 'soul'})
        until = stamp(now() + timedelta(days=1))
        result = self.actions.execute('save_memory', {'key': '称呼', 'content': '小林', 'catalog': 'soul', 'expires_at': until}, actor='capture')
        self.assertEqual(result['effect'], 'proposed')
        row_id = self.store.state()['memories'][0]['id']
        self.actions.execute('dismiss_revision', {'id': row_id})
        row = self.store.state()['memories'][0]
        self.assertIsNone(row['expires_at'])
        self.assertIsNone(row['proposed_expires_at'])
        self.actions.execute('save_memory', {'key': '称呼', 'content': '小林', 'catalog': 'soul', 'expires_at': until}, actor='capture')
        self.actions.execute('accept_revision', {'id': row_id})
        self.assertEqual(self.store.state()['memories'][0]['expires_at'], until)

    def test_stale_proposal_expiry_is_rejected_on_accept(self):
        self.actions.execute('save_memory', {'key': '称呼', 'content': '小林', 'catalog': 'soul'})
        self.actions.execute('save_memory', {'key': '称呼', 'content': '阿林', 'catalog': 'soul',
                                             'expires_at': stamp(now() + timedelta(hours=1))}, actor='capture')
        row_id = self.store.state()['memories'][0]['id']
        with self.store.transaction() as db:
            db.execute('UPDATE memories SET proposed_expires_at=? WHERE id=?', (stamp(now() - timedelta(minutes=1)), row_id))
        with self.assertRaises(ValueError):
            self.actions.execute('accept_revision', {'id': row_id})
        self.assertEqual(self.store.state()['memories'][0]['content'], '小林')

    def test_old_work_memory_lands_in_daily_on_upgrade(self):
        path = self.tmp.name + '/old.db'
        db = sqlite3.connect(path)
        db.execute('''CREATE TABLE memories (
            id TEXT PRIMARY KEY, key TEXT NOT NULL UNIQUE, content TEXT NOT NULL,
            updated_at TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'confirmed',
            category TEXT NOT NULL DEFAULT 'personal')''')
        db.execute("INSERT INTO memories (id,key,content,updated_at,category) VALUES ('a','地点','上海',?,'work')", (stamp(),))
        db.execute("INSERT INTO memories (id,key,content,updated_at,category) VALUES ('b','称呼','小林',?,'personal')", (stamp(),))
        db.commit()
        db.close()
        store = Store(path)
        try:
            rows = {row['key']: row['catalog'] for row in store.state()['memories']}
            self.assertEqual(rows, {'地点': 'daily', '称呼': 'soul'})
            self.assertEqual(len(store.state()['policies']), 4)
        finally:
            store.db.close()
