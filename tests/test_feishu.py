import json
import tempfile
import unittest
from datetime import timedelta
from types import SimpleNamespace

from lumen.agent import Agent
from lumen.core import Actions, Scheduler, Store, now, stamp
from lumen.feishu import Feishu
from test_lumen import ScriptModel


class FeishuTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.store = Store(self.tmp.name + '/test.db')
        self.actions = Actions(self.store)
        self.model = ScriptModel([{'role': 'assistant', 'content': '你好，主人'}])
        self.agent = Agent(self.store, self.actions, self.model)
        self.sent = []
        self.bot = Feishu(self.store, self.agent, 'owner', lambda *args:self.sent.append(args))

    def tearDown(self):
        self.store.db.close()
        self.tmp.cleanup()

    def test_whitelist_group_rejection_and_durable_deduplication(self):
        self.bot.handle('m0', 'stranger', 'p2p', 'text', '{"text":"hello"}')
        self.bot.handle('m1', 'owner', 'group', 'text', '{"text":"hello"}')
        self.assertEqual(self.store.state()['messages'], [])
        self.bot.handle('m2', 'owner', 'p2p', 'text', '{"text":"hello"}')
        self.bot.handle('m2', 'owner', 'p2p', 'text', '{"text":"hello"}')
        self.assertEqual(len(self.model.inputs), 1)
        self.assertEqual(len(self.store.state()['deliveries']), 1)
        self.bot.deliver()
        self.bot.deliver()
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.sent[0][0], 'owner')
        self.assertEqual(self.store.state()['deliveries'][0]['status'], 'sent')

    def test_task_result_queued_atomically_and_delivery_retries(self):
        at = now()+timedelta(seconds=1)
        self.actions.execute('create_schedule', {'title':'提醒','prompt':'休息', 'kind':'reminder', 'repeat':'none', 'run_at':stamp(at)})
        Scheduler(self.store, self.actions, self.agent, 'owner').tick(at+timedelta(seconds=1))
        self.assertEqual(len(self.store.state()['runs']), 1)
        self.assertEqual(len(self.store.state()['deliveries']), 1)
        def fail(*args):
            raise RuntimeError('offline')
        self.bot.sender=fail
        self.bot.deliver()
        delivery=self.store.state()['deliveries'][0]
        self.assertEqual(delivery['status'], 'pending')
        self.assertEqual(delivery['attempts'], 1)
        self.bot.sender=lambda *args:self.sent.append(args)
        with self.store.transaction() as db:
            db.execute('UPDATE deliveries SET next_at=?', (stamp(now()-timedelta(seconds=1)),))
        self.bot.deliver()
        self.assertIn('休息', self.sent[0][1])
        self.assertEqual(self.store.state()['deliveries'][0]['status'], 'sent')

    def test_new_conversation_command_does_not_call_model(self):
        self.store.message('user', 'old-topic')
        self.actions.execute('save_memory', {'key': '风格', 'content': '简洁'})
        self.bot.handle('new-command', 'owner', 'p2p', 'text', '{"text":"/new"}')
        self.assertEqual(self.model.inputs, [])
        self.assertEqual(len(self.store.state()['memories']), 1)
        self.assertNotIn('old-topic', str(self.store.state()['messages']))
        self.bot.deliver()
        self.assertIn('已开始新对话', self.sent[0][1])

    def test_sdk_event_adapter(self):
        message=SimpleNamespace(message_id='sdk-m',chat_type='p2p',message_type='text',content=json.dumps({'text':'hi'}))
        event=SimpleNamespace(event=SimpleNamespace(message=message,sender=SimpleNamespace(sender_id=SimpleNamespace(open_id='owner'))))
        self.bot.event(event)
        self.bot.handle(*self.bot.incoming.get_nowait())
        self.assertEqual(len(self.store.state()['deliveries']), 1)


if __name__=='__main__':
    unittest.main()
