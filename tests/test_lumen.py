import json
import os
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from datetime import timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

from lumen.__main__ import create_server
from lumen.agent import Agent, Model, ModelError
from lumen.core import Actions, Scheduler, Store, now, stamp


class ScriptModel:
    key = 'test'

    def __init__(self, responses):
        self.responses = iter(responses)
        self.inputs = []

    def complete(self, messages):
        self.inputs.append(json.loads(json.dumps(messages)))
        result = next(self.responses)
        if isinstance(result, Exception):
            raise result
        return result


def call(name, args):
    return {'role': 'assistant', 'content': None, 'tool_calls': [
        {'id': 'c1', 'type': 'function', 'function': {'name': name, 'arguments': json.dumps(args)}}]}


class LumenTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = self.tmp.name + '/test.db'
        self.store = Store(self.path)
        self.actions = Actions(self.store)

    def tearDown(self):
        self.store.db.close()
        self.tmp.cleanup()

    def test_persistence_memory_overwrite_and_forget(self):
        first = self.actions.execute('save_memory', {'key': '称呼', 'content': '小林'})
        second = self.actions.execute('save_memory', {'key': '称呼', 'content': '阿林'})
        self.assertEqual(first['id'], second['id'])
        todo = self.actions.execute('add_todo', {'title': '买牛奶'})
        self.actions.execute('update_todo', {'id': todo['id'], 'done': True})
        self.store.db.close()
        self.store = Store(self.path)
        self.actions = Actions(self.store)
        self.assertEqual(self.store.state()['memories'][0]['content'], '阿林')
        self.assertEqual(self.store.state()['todos'][0]['done'], 1)
        self.actions.execute('delete_memory', {'id': first['id']})
        self.assertEqual(self.store.state()['memories'], [])

    def test_tool_validation(self):
        for name, args in [('shell', {}), ('add_todo', {'title': ''}),
                           ('add_todo', {'title': 'x', 'due_at': '2026-10-01T09:00'}),
                           ('update_todo', {'id': 'fake', 'done': 'true'}),
                           ('add_todo', {'title': 'x', 'unexpected': 'x'})]:
            with self.subTest(name=name, args=args), self.assertRaises(ValueError):
                self.actions.execute(name, args)
        self.assertEqual(self.store.state()['todos'], [])

    def schedule(self, repeat='none', kind='reminder'):
        run_at = now() + timedelta(seconds=10)
        result = self.actions.execute('create_schedule', {'title': '喝水', 'prompt': '该喝水了',
            'run_at': stamp(run_at), 'repeat': repeat, 'kind': kind})
        return result['id'], run_at

    def test_reminder_exactly_one_run_and_repeat_catchup(self):
        _, at = self.schedule()
        scheduler = Scheduler(self.store, self.actions, None)
        scheduler.tick(at + timedelta(seconds=1))
        scheduler.tick(at + timedelta(seconds=2))
        self.assertEqual(len(self.store.state()['runs']), 1)
        self.assertEqual(self.store.state()['messages'][0]['source'], 'schedule')
        self.assertEqual(self.store.state()['schedules'][0]['enabled'], 0)
        repeated, at = self.schedule('daily')
        later = at + timedelta(days=3, seconds=2)
        scheduler.tick(later)
        row = next(s for s in self.store.state()['schedules'] if s['id'] == repeated)
        self.assertGreater(row['run_at'], stamp(later))
        self.assertEqual(row['enabled'], 1)
        self.assertEqual(len(self.store.state()['runs']), 2)

    def test_scheduled_agent_failure_and_restart(self):
        task, at = self.schedule('daily', 'agent')
        model = ScriptModel([ModelError('offline')])
        scheduler = Scheduler(self.store, self.actions, Agent(self.store, self.actions, model))
        scheduler.tick(at + timedelta(seconds=1))
        self.assertEqual(self.store.state()['schedules'][0]['status'], 'failed')
        self.assertEqual(self.store.state()['schedules'][0]['enabled'], 0)
        self.assertIn('offline', self.store.state()['runs'][0]['result'])
        task2, _ = self.schedule()
        with self.store.transaction() as db:
            db.execute("UPDATE schedules SET status='running' WHERE id=?", (task2,))
        self.store.db.close()
        self.store = Store(self.path)
        row = next(s for s in self.store.state()['schedules'] if s['id'] == task2)
        self.assertEqual(row['status'], 'failed')
        self.assertIn('中断', row['last_error'])

    def test_agent_tool_loop_memory_context_and_partial_failure(self):
        model = ScriptModel([call('save_memory', {'key': '口味', 'content': '少糖'}),
                             {'role': 'assistant', 'content': '记住了。'}])
        agent = Agent(self.store, self.actions, model)
        self.assertEqual(agent.reply('记住我喜欢少糖'), '记住了。')
        self.assertEqual(model.inputs[1][-1]['role'], 'tool')
        self.assertTrue(json.loads(model.inputs[1][-1]['content'])['ok'])
        model2 = ScriptModel([call('add_todo', {'title': '买茶'}), ModelError('timeout')])
        with self.assertRaisesRegex(ModelError, '已成功执行'):
            Agent(self.store, self.actions, model2).reply('帮我记买茶')
        self.assertIn('少糖', model2.inputs[0][0]['content'])
        self.assertEqual(self.store.state()['todos'][0]['title'], '买茶')
        self.assertEqual(self.store.state()['messages'][-1]['source'], 'error')

    def test_scheduled_agent_can_update_todos_but_not_spawn_tasks(self):
        args = {'title': 'x', 'prompt': 'x', 'run_at': stamp(now()+timedelta(hours=1)), 'kind': 'agent', 'repeat': 'daily'}
        with self.assertRaises(ValueError):
            self.actions.execute('create_schedule', args, scheduled=True)
        task, at = self.schedule(kind='agent')
        model = ScriptModel([call('add_todo', {'title': '整理今日计划'}), {'role': 'assistant', 'content': '计划已记录'}])
        Scheduler(self.store, self.actions, Agent(self.store, self.actions, model)).tick(at+timedelta(seconds=1))
        self.assertEqual(self.store.state()['todos'][0]['title'], '整理今日计划')
        self.assertEqual(self.store.state()['runs'][0]['status'], 'success')

    def test_api_auth_origin_and_chat(self):
        agent = Agent(self.store, self.actions, ScriptModel([{'role': 'assistant', 'content': '你好'}]))
        server = create_server('127.0.0.1', 0, self.store, self.actions, agent, 'private-token')
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        base = 'http://127.0.0.1:' + str(server.server_port)
        def request(path, data=None, token='private-token', origin=None):
            headers = {'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'}
            if origin:
                headers['Origin'] = origin
            req = urllib.request.Request(base+path, data=json.dumps(data).encode() if data is not None else None, headers=headers)
            with urllib.request.urlopen(req) as response:
                return json.load(response)
        try:
            with self.assertRaises(urllib.error.HTTPError) as e:
                request('/api/state', token='bad')
            self.assertEqual(e.exception.code, 401)
            with self.assertRaises(urllib.error.HTTPError) as e:
                request('/api/action', {'name': 'add_todo', 'args': {'title': 'bad'}}, origin='https://evil.example')
            self.assertEqual(e.exception.code, 403)
            request('/api/action', {'name': 'add_todo', 'args': {'title': '读书'}})
            self.assertEqual(request('/api/chat', {'message': '你好'})['reply'], '你好')
            self.assertEqual(len(request('/api/state')['messages']), 2)
            self.assertEqual(request('/api/state')['todos'][0]['title'], '读书')
            with urllib.request.urlopen(base) as response:
                self.assertIn('Lumen'.encode(), response.read())
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_real_openai_compatible_http_contract(self):
        received = []
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_POST(self):
                received.append((self.path, json.loads(self.rfile.read(int(self.headers['Content-Length'])))))
                self.send_response(200)
                self.end_headers()
                self.wfile.write(json.dumps({'choices': [{'finish_reason': 'stop', 'message': {'role': 'assistant', 'content': '你好'}}]}).encode())
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with patch.dict(os.environ, {'LUMEN_MODEL_API_KEY': 'test', 'LUMEN_MODEL_BASE_URL': f'http://127.0.0.1:{server.server_port}/v1'}):
                model = Model()
                self.assertEqual(model.complete([{'role': 'user', 'content': 'hi'}])['content'], '你好')
            self.assertEqual(received[0][0], '/v1/chat/completions')
            self.assertEqual(received[0][1]['tools'][0]['type'], 'function')
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == '__main__':
    unittest.main()
