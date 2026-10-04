"""Synthetic examples only: verify all routes that can persist or send a secret."""
import json
import tempfile
import unittest
from unittest.mock import patch
from lumen.agent import Agent,Model
from lumen.core import Store,Actions,stamp
from lumen.capture import file_utterance
from lumen.privacy import reasons,require_safe_memory,redact
from test_lumen import ScriptModel,call

PASSWORD='synthetic-password-only'
PRIVATE_HEADER='-----BEGIN '+'PRIVATE KEY-----'
PRIVATE_FOOTER='-----END '+'PRIVATE KEY-----'
DOCUMENT='110101199001010018'
CARD='4111'+'1111'*3  # Industry test number, never an account.

class PrivacyTest(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.store=Store(self.tmp.name+'/db');self.a=Actions(self.store)
    def tearDown(self):self.store.db.close();self.tmp.cleanup()
    def old_memory(self,key,content,proposal=None):
        with self.store.transaction() as db:
            db.execute('INSERT INTO memories (id,key,content,updated_at,proposed_content) VALUES (?,?,?,?,?)',('old',key,content,stamp(),proposal))
    def test_password_document_card_tokens_and_obfuscation(self):
        for text in ('我的密码是 '+PASSWORD,'验证码：123456','身份证号：'+DOCUMENT,DOCUMENT,
                     '银行卡号 '+CARD,' '.join(CARD[i:i+4] for i in range(0,16,4)),
                     'sk-'+'a'*32,PRIVATE_HEADER+'\nsynthetic\n'+PRIVATE_FOOTER,
                     '我的密\u200b码：'+PASSWORD,'身份证号：１１０１０１１９９００１０１００１８',
                     '护照号：P00000001','https://' + 'user' + ':' + 'synthetic' + '@example.invalid'):
            with self.subTest(text_kind=text[:4]):self.assertTrue(reasons(text))
    def test_json_and_english_assignment_are_not_forwarded(self):
        for text in (json.dumps({'password':PASSWORD}), 'my password is '+PASSWORD, json.dumps({'护照号':'P00000001'})):
            self.assertTrue(reasons(text))
            self.assertNotIn(PASSWORD,redact(text))
    def test_ordinary_facts_and_security_discussion_are_not_secrets(self):
        for text in ('我讨厌香菜','我用密码管理器','我忘记密码了','银行卡丢了怎么办','我在上海出差','今天写了120行代码'):
            self.assertEqual(reasons(text),())
            require_safe_memory('近况',text)
    def test_every_memory_actor_and_pending_status_cannot_store_sensitive_data(self):
        for actor in ('user','agent','capture'):
            for key,content in (('密码',PASSWORD),('身份信息',DOCUMENT),('支付信息',CARD)):
                for status in ('confirmed','pending'):
                    with self.subTest(actor=actor,key=key,status=status),self.assertRaisesRegex(ValueError,'敏感'):
                        self.a.execute('save_memory',{'key':key,'content':content,'status':status},actor=actor)
        self.assertEqual(self.store.state()['memories'],[])
        self.assertEqual(self.store.query('SELECT * FROM changes'),[])
    def test_sensitive_revision_is_rejected_without_overwriting_original(self):
        original=self.a.execute('save_memory',{'key':'近况','content':'上海'})
        for actor in ('agent','capture','user'):
            with self.assertRaises(ValueError):self.a.execute('save_memory',{'id':original['id'],'key':'近况','content':'密码：'+PASSWORD},actor=actor)
        self.assertEqual(self.store.state()['memories'][0]['content'],'上海')
        self.assertIsNone(self.store.state()['memories'][0]['proposed_content'])
    def test_secret_utterance_never_reaches_classifier(self):
        class Finder:
            def classify(self,*args):raise AssertionError('secret reached classifier')
        self.assertEqual(file_utterance(Finder(),self.a,'记住我的密码是 '+PASSWORD),[])
    def test_classifier_output_is_checked_even_if_input_is_safe(self):
        class Finder:
            def classify(self,*args):return json.dumps({'items':[{'catalog':'soul','key':'密码','content':PASSWORD}]})
        filed=file_utterance(Finder(),self.a,'我喜欢简洁回答')
        self.assertFalse(filed[0]['ok']);self.assertNotIn(PASSWORD,str(filed));self.assertEqual(self.store.state()['memories'],[])
    def test_old_sensitive_memory_hidden_from_context_search_and_direct_read(self):
        self.old_memory('密码',PASSWORD)
        self.assertEqual(self.a.context()['memories'],[])
        self.assertEqual(self.a.execute('search_records',{'query':'密码'})['records'],[])
        with self.assertRaises(ValueError):self.a.execute('get_record',{'type':'memory','id':'old'})
        row=self.store.state()['memories'][0]
        self.assertTrue(row['sensitive_blocked']);self.assertEqual(row['content'],PASSWORD)
        self.a.execute('save_memory',{'id':'old','key':'偏好','content':'简洁'})
        self.assertFalse(self.store.state()['memories'][0]['sensitive_blocked'])
        self.assertEqual(self.a.context()['memories'][0]['content'],'简洁')
    def test_accepting_or_confirming_old_sensitive_data_is_blocked(self):
        self.old_memory('近况','上海','密码是 '+PASSWORD)
        with self.assertRaises(ValueError):self.a.execute('accept_revision',{'id':'old'})
        with self.store.transaction() as db:db.execute("UPDATE memories SET key='密码',content=?,proposed_content=NULL,status='pending'",(PASSWORD,))
        with self.assertRaises(ValueError):self.a.execute('confirm_memory',{'id':'old'})
        self.assertEqual(self.store.state()['memories'][0]['status'],'pending')
    def test_history_current_input_and_tool_results_redacted_before_chat_model(self):
        self.store.message('user','密码是 '+PASSWORD)
        note=self.a.execute('save_note',{'title':'本地资料','content':'银行卡号：'+CARD})
        model=ScriptModel([call('get_record',{'type':'note','id':note['id']}),{'role':'assistant','content':'敏感信息不能保存为记忆。'}])
        Agent(self.store,self.a,model).reply('身份证号：'+DOCUMENT)
        payload=json.dumps(model.inputs,ensure_ascii=False)
        for value in (PASSWORD,CARD,DOCUMENT):self.assertNotIn(value,payload)
        self.assertIn(PASSWORD,str(self.store.state()['messages']))  # Local history is preserved.
    def test_private_key_body_and_multiline_assignments_are_fully_redacted(self):
        self.assertNotIn('synthetic-key-body',redact(PRIVATE_HEADER+'\nsynthetic-key-body\n'+PRIVATE_FOOTER))
        self.assertNotIn(PASSWORD,redact('密码\n是 '+PASSWORD))
    def test_real_http_payload_is_sanitized_without_mutating_caller_data(self):
        class Response:
            def __enter__(self):return self
            def __exit__(self,*args):pass
            def read(self,*args):return json.dumps({'choices':[{'message':{'role':'assistant','content':'ok'}}]}).encode()
        messages=[{'role':'user','content':'password: '+PASSWORD}]
        with patch.dict('os.environ',{'LUMEN_MODEL_API_KEY':'test-only'}),patch('urllib.request.urlopen',return_value=Response()) as request:
            Model().complete(messages)
        self.assertNotIn(PASSWORD,request.call_args.args[0].data.decode())
        self.assertIn(PASSWORD,messages[0]['content'])
    def test_undo_cannot_restore_sensitive_legacy_memory(self):
        self.old_memory('密码',PASSWORD)
        deletion=self.a.execute('delete_memory',{'id':'old'})
        with self.assertRaises(ValueError):self.a.execute('undo_change',{'id':deletion['receipt_id']})
        self.assertEqual(self.store.state()['memories'],[])
