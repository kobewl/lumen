import tempfile
import unittest
from lumen.core import Store,Actions
from lumen.agent import Agent
from test_lumen import ScriptModel

class PlansTest(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.store=Store(self.tmp.name+'/test.db');self.a=Actions(self.store)
    def tearDown(self):self.store.db.close();self.tmp.cleanup()
    def plan(self):return self.a.execute('propose_plan',{'title':'完成课程','goal':'学会数据库','steps':['第一课','第二课']},actor='agent')
    def test_draft_has_no_side_effect_until_user_confirmation(self):
        plan=self.plan();self.assertEqual(self.store.state()['todos'],[])
        with self.assertRaises(ValueError):self.a.execute('apply_plan',{'id':plan['id']},actor='agent')
        self.a.execute('apply_plan',{'id':plan['id']});self.a.execute('apply_plan',{'id':plan['id']})
        state=self.store.state();self.assertEqual(len(state['todos']),2);self.assertEqual(len(state['projects']),1)
        self.assertTrue(all(t['project_id']==state['projects'][0]['id'] for t in state['todos']))
    def test_cancel_and_edit(self):
        plan=self.plan();self.a.execute('propose_plan',{'id':plan['id'],'title':'课程','steps':['预习']})
        self.a.execute('reject_plan',{'id':plan['id']})
        with self.assertRaises(ValueError):self.a.execute('apply_plan',{'id':plan['id']})
        self.assertEqual(self.store.state()['todos'],[])
    def test_feishu_compatible_commands_confirm_without_model(self):
        plan=self.plan();model=ScriptModel([]);agent=Agent(self.store,self.a,model)
        self.assertIn('完成课程',agent.reply('/plans'))
        self.assertIn('已创建',agent.reply('/approve '+plan['id'][:8]));self.assertEqual(model.inputs,[])
        self.assertEqual(len(self.store.state()['todos']),2)
