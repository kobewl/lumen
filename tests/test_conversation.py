import json
import tempfile
import unittest

from lumen.agent import Agent, ModelError
from lumen.core import Actions, Store
from test_lumen import ScriptModel, call


class ConversationTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = self.tmp.name + '/test.db'
        self.store = Store(self.path)
        self.actions = Actions(self.store)

    def tearDown(self):
        self.store.db.close()
        self.tmp.cleanup()

    def test_upgrade_adds_context_settings_without_changing_existing_data(self):
        self.store.message('user', '保留的原始聊天')
        self.actions.execute('save_memory', {'key': '称呼', 'content': '小林'})
        self.actions.execute('add_todo', {'title': '读书'})
        before = self.store.state()
        with self.store.transaction() as db:
            db.execute('DROP TABLE settings')
        self.store.db.close()
        self.store = Store(self.path)
        self.actions = Actions(self.store)
        after = self.store.state()
        for table in ('messages', 'memories', 'todos'):
            self.assertEqual(before[table], after[table])
        self.assertEqual(self.store.context_cutoff(), 0)
        self.assertEqual(self.store.query('PRAGMA integrity_check')[0]['integrity_check'], 'ok')

    def test_memory_correction_renames_same_id_and_rejects_conflicts(self):
        memory = self.actions.execute('save_memory', {'key': ' 称呼 ', 'content': '小林'})
        updated = self.actions.execute('save_memory', {'id': memory['id'], 'key': '姓名', 'content': '阿林'})
        self.assertEqual(updated['id'], memory['id'])
        self.assertEqual(len(self.store.state()['memories']), 1)
        self.assertEqual(self.store.state()['memories'][0]['key'], '姓名')
        other = self.actions.execute('save_memory', {'key': '语言', 'content': '中文'})
        for args in ({'id': 'missing', 'key': '姓名', 'content': '错误'},
                     {'id': other['id'], 'key': '姓名', 'content': '错误'}):
            with self.assertRaises(ValueError):
                self.actions.execute('save_memory', args)
        self.assertEqual(self.store.state()['memories'][0]['content'], '中文')

    def test_forgetting_removes_old_fact_from_current_and_future_model_context(self):
        self.store.message('user', '请记住我的代号是 old-private-fact')
        self.store.message('assistant', '记住了 old-private-fact')
        memory = self.actions.execute('save_memory', {'key': '代号', 'content': 'old-private-fact'})
        # The earlier get_state result must also disappear after forgetting.
        model = ScriptModel([call('get_state', {}), call('delete_memory', {'id': memory['id']}),
                             {'role': 'assistant', 'content': '已忘记。'}])
        Agent(self.store, self.actions, model).reply('忘记我的代号')
        self.assertNotIn('old-private-fact', json.dumps(model.inputs[2]))
        self.store.db.close()
        self.store = Store(self.path)
        self.actions = Actions(self.store)
        follow = ScriptModel([{'role': 'assistant', 'content': '没有保存你的代号。'}])
        Agent(self.store, self.actions, follow).reply('我的代号是什么？')
        self.assertNotIn('old-private-fact', json.dumps(follow.inputs[0]))
        self.assertTrue(any('old-private-fact' in m['content'] for m in self.store.state()['messages']))

    def test_correction_refreshes_state_before_final_model_response(self):
        memory = self.actions.execute('save_memory', {'key': '称呼', 'content': 'old-name'})
        model = ScriptModel([call('save_memory', {'id': memory['id'], 'key': '称呼', 'content': 'new-name'}),
                             {'role': 'assistant', 'content': '已更新。'}])
        Agent(self.store, self.actions, model).reply('把称呼改成 new-name')
        self.assertNotIn('old-name', model.inputs[1][0]['content'])
        self.assertIn('new-name', model.inputs[1][0]['content'])
        self.assertEqual(len(self.store.state()['memories']), 1)

    def test_new_conversation_works_without_model_and_keeps_personal_data(self):
        self.store.message('user', 'previous-thread-content')
        self.actions.execute('save_memory', {'key': '风格', 'content': '简洁'})
        self.actions.execute('add_todo', {'title': '读书'})
        model = ScriptModel([])
        agent = Agent(self.store, self.actions, model)
        self.assertIn('个人记忆', agent.reply('/new'))
        self.assertEqual(model.inputs, [])
        self.assertNotIn('previous-thread-content', str(self.store.state()['messages']))
        self.assertEqual(len(self.store.state()['memories']), 1)
        self.assertEqual(len(self.store.state()['todos']), 1)
        model = ScriptModel([{'role': 'assistant', 'content': '你好'}])
        Agent(self.store, self.actions, model).reply('你好')
        self.assertNotIn('previous-thread-content', str(model.inputs))
        self.assertIn('简洁', model.inputs[0][0]['content'])

    def test_duplicate_model_write_in_same_turn_creates_one_todo(self):
        model = ScriptModel([call('add_todo', {'title': '买茶'}), call('add_todo', {'title': '买茶'}),
                             {'role': 'assistant', 'content': '已记录。'}])
        Agent(self.store, self.actions, model).reply('帮我记买茶')
        self.assertEqual(len(self.store.state()['todos']), 1)
        self.assertEqual(json.loads(model.inputs[1][-1]['content'])['id'],
                         json.loads(model.inputs[2][-1]['content'])['id'])

    def test_state_updates_can_revert_to_an_earlier_value_in_same_turn(self):
        todo = self.actions.execute('add_todo', {'title': '买茶'})
        model = ScriptModel([call('update_todo', {'id': todo['id'], 'done': False}),
                             call('update_todo', {'id': todo['id'], 'done': True}),
                             call('update_todo', {'id': todo['id'], 'done': False}),
                             {'role': 'assistant', 'content': '保持未完成。'}])
        Agent(self.store, self.actions, model).reply('最后保持未完成')
        self.assertEqual(self.store.state()['todos'][0]['done'], 0)

    def test_history_budget_keeps_recent_contiguous_turns(self):
        for i in range(8):
            self.store.message('user', f'{i}-'+('a'*2000))
            self.store.message('assistant', f'{i}-'+('b'*2000))
        history = self.store.history(max_chars=10000)
        self.assertLessEqual(sum(len(row['content']) for row in history), 10000)
        self.assertTrue(history[0]['content'].startswith('6-'))
        self.assertTrue(history[-1]['content'].startswith('7-'))
        self.assertEqual(history[0]['role'], 'user')

    def test_malformed_tool_batch_fails_before_any_write(self):
        batch = call('add_todo', {'title': 'should-not-exist'})
        batch['tool_calls'].append({'id': 'invalid', 'type': 'function', 'function': {'name': 'add_todo'}})
        with self.assertRaisesRegex(ModelError, '无效工具调用'):
            Agent(self.store, self.actions, ScriptModel([batch])).reply('记待办')
        self.assertEqual(self.store.state()['todos'], [])


if __name__ == '__main__':
    unittest.main()
