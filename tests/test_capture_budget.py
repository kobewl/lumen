import importlib.util
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from lumen.core import Store,Actions,stamp
from lumen.agent import Agent,Model
from lumen.capture import file_utterance,parse_items
from lumen.capture_gate import screen
from lumen.diagnostics import status
from test_lumen import ScriptModel

class Finder(ScriptModel):
    def __init__(self):
        super().__init__([{'role':'assistant','content':'好的'}]*10);self.classifications=0
    def classify(self,text,memories):
        self.classifications+=1
        return json.dumps({'items':[{'catalog':'daily','key':'地点','content':'上海'}]})

class CaptureBudgetTest(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.store=Store(self.tmp.name+'/db');self.a=Actions(self.store)
    def tearDown(self):self.store.db.close();self.tmp.cleanup()
    def test_nonfacts_explicit_writes_and_questions_use_no_classifier(self):
        model=Finder();agent=Agent(self.store,self.a,model)
        for text in ('你好','谢谢','帮我整理任务','记住我喜欢简洁','我在哪里？','翻译：我喜欢吃辣'):
            agent.reply(text)
        self.assertEqual(model.classifications,0);self.assertEqual(len(model.inputs),6)
    def test_fact_prescreen_handles_chinese_english_and_mixed_questions(self):
        for text in ('我这周在上海','我最近在学数据库','我不吃海鲜','I prefer concise answers','我在上海出差，推荐咖啡馆？'):
            self.assertEqual(screen(text),'candidate')
    def test_separate_capture_limit_does_not_stop_chat(self):
        model=Finder();agent=Agent(self.store,self.a,model)
        with patch.dict('os.environ',{'LUMEN_CAPTURE_DAILY_CALL_LIMIT':'1'}):
            agent.reply('我现在在上海');agent.reply('我最近在杭州')
        self.assertEqual(model.classifications,1);self.assertEqual(len(model.inputs),2)
        self.assertEqual(status(self.store,self.a,model)['capture']['attempts'],1)
        self.assertEqual(self.store.query("SELECT reason FROM capture_events ORDER BY rowid DESC LIMIT 1")[0]['reason'],'capture_budget')
    def test_global_budget_reserves_remaining_chat_calls(self):
        with self.store.transaction() as db:
            db.execute('INSERT INTO usage (id,model,input_tokens,output_tokens,created_at) VALUES (?,?,?,?,?)',('u','model',0,0,stamp()))
        model=Finder()
        with patch.dict('os.environ',{'LUMEN_MODEL_DAILY_CALL_LIMIT':'2','LUMEN_CAPTURE_CHAT_RESERVE':'1'}):
            Agent(self.store,self.a,model).reply('我在上海')
        self.assertEqual(model.classifications,0);self.assertEqual(len(model.inputs),1)
        self.assertEqual(self.store.query('SELECT reason FROM capture_events')[0]['reason'],'chat_reserve')
    def test_failed_classification_consumes_attempt_limit_and_chat_continues(self):
        class Broken(Finder):
            def classify(self,*args):self.classifications+=1;raise RuntimeError('provider failure')
        model=Broken()
        with patch.dict('os.environ',{'LUMEN_CAPTURE_DAILY_CALL_LIMIT':'1'}),patch('logging.warning'):
            Agent(self.store,self.a,model).reply('我在上海');Agent(self.store,self.a,model).reply('我在杭州')
        self.assertEqual(model.classifications,1);self.assertEqual(len(model.inputs),2)
        self.assertEqual(self.store.query('SELECT reason FROM capture_events')[0]['reason'],'classifier_error')
    def test_disable_long_messages_and_sensitive_input_skip_classification(self):
        model=Finder()
        with patch.dict('os.environ',{'LUMEN_CAPTURE_ENABLED':'false'}):file_utterance(model,self.a,'我在上海')
        with patch.dict('os.environ',{'LUMEN_CAPTURE_MAX_CHARS':'5'}):file_utterance(model,self.a,'我在上海出差')
        file_utterance(model,self.a,'密码是 synthetic-only')
        self.assertEqual(model.classifications,0)
        self.assertNotIn('synthetic-only',str(self.store.query('SELECT * FROM capture_events')))
    def test_classifier_context_excludes_sensitive_legacy_memory(self):
        with self.store.transaction() as db:db.execute('INSERT INTO memories (id,key,content,updated_at) VALUES (?,?,?,?)',('old','密码','local-only',stamp()))
        class Inspect(Finder):
            def classify(self,text,memories):self.seen=memories;return '{"items":[]}'
        model=Inspect();file_utterance(model,self.a,'我在上海')
        self.assertEqual(model.seen,[])
    def test_capture_parser_rejects_oversized_values_without_truncating(self):
        for item in ({'catalog':'soul','key':'k'*41,'content':'safe'}, {'catalog':'daily','key':'k','content':'x'*401}):
            self.assertEqual(parse_items(json.dumps({'items':[item]})),[])
    def test_model_records_capture_and_chat_usage_separately(self):
        class Response:
            def __enter__(self):return self
            def __exit__(self,*args):pass
            def read(self,*args):return json.dumps({'choices':[{'message':{'role':'assistant','content':'{"items":[]}'}}],'usage':{'prompt_tokens':1,'completion_tokens':2}}).encode()
        with patch.dict('os.environ',{'LUMEN_MODEL_API_KEY':'test-only'}),patch('urllib.request.urlopen',return_value=Response()):
            model=Model();model.store=self.store;model.classify('我在上海',[]);model.complete([{'role':'user','content':'你好'}])
        self.assertEqual([row['purpose'] for row in self.store.query('SELECT purpose FROM usage')],['capture','chat'])
    def test_dry_evaluation_never_reports_stub_results_as_model_quality(self):
        result=subprocess.run([os.sys.executable,'scripts/evaluate_capture.py'],capture_output=True,text=True)
        self.assertEqual(result.returncode,0,result.stderr);report=json.loads(result.stdout)
        self.assertEqual(report['api_attempts'],0);self.assertIsNone(report['classification'])
        self.assertEqual(report['screen_missed_fact_ids'],[])
    def test_live_evaluation_without_key_fails_before_network(self):
        env={key:value for key,value in os.environ.items() if key not in ('LUMEN_MODEL_API_KEY','LUMEN_DEEPSEEK_API_KEY')}
        result=subprocess.run([os.sys.executable,'scripts/evaluate_capture.py','--live'],env=env,capture_output=True,text=True)
        self.assertEqual(result.returncode,2);self.assertIn('未发送请求',result.stderr)
    def test_evaluation_scoring_detects_hallucinations_missing_facts_and_duplicate_keys(self):
        spec=importlib.util.spec_from_file_location('evaluation','scripts/evaluate_capture.py');module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
        expected=[{'catalog':'soul','contains':['香菜']}]
        actual=[{'catalog':'soul','key':'口味','content':'不吃香菜'},{'catalog':'daily','key':'地点','content':'上海'}]
        self.assertEqual(module.score(expected,actual),{'tp':1,'fp':1,'fn':0})
        self.assertEqual(module.score(expected,actual,'其他名称'),{'tp':0,'fp':2,'fn':1})
