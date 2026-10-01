import json
import sqlite3
import subprocess
import tempfile
import threading
import unittest
import urllib.request
from pathlib import Path
from lumen.core import Store,Actions,stamp
from lumen.agent import Agent
from lumen.__main__ import create_server
from test_lumen import ScriptModel

class ReleaseTest(unittest.TestCase):
    def test_original_v001_schema_upgrade_keeps_records(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=tmp+'/old.db';db=sqlite3.connect(path)
            db.executescript('''CREATE TABLE messages(id TEXT PRIMARY KEY,role TEXT,content TEXT,created_at TEXT,source TEXT);
            CREATE TABLE memories(id TEXT PRIMARY KEY,key TEXT UNIQUE,content TEXT,updated_at TEXT);
            CREATE TABLE todos(id TEXT PRIMARY KEY,title TEXT,due_at TEXT,done INTEGER,created_at TEXT);
            CREATE TABLE schedules(id TEXT PRIMARY KEY,title TEXT,prompt TEXT,kind TEXT,run_at TEXT,repeat TEXT,enabled INTEGER,status TEXT,last_error TEXT,created_at TEXT);
            CREATE TABLE feishu_inbox(id TEXT PRIMARY KEY,status TEXT,created_at TEXT);''')
            db.execute('INSERT INTO memories VALUES (?,?,?,?)',('memory','称呼','小林',stamp()))
            db.execute('INSERT INTO todos VALUES (?,?,NULL,0,?)',('todo','买茶',stamp()))
            db.commit();db.close();store=Store(path)
            try:
                self.assertEqual(store.state()['memories'][0]['status'],'confirmed')
                self.assertEqual(store.state()['todos'][0]['priority'],'normal')
                self.assertEqual(store.state()['todos'][0]['title'],'买茶')
                self.assertEqual(store.query('PRAGMA integrity_check')[0]['integrity_check'],'ok')
            finally:store.db.close()
    def test_authenticated_status_export_and_binary_backup(self):
        with tempfile.TemporaryDirectory() as tmp:
            store=Store(tmp+'/db');a=Actions(store);a.execute('save_note',{'title':'private','content':'export-content'})
            server=create_server('127.0.0.1',0,store,a,Agent(store,a,ScriptModel([])),'test-token');t=threading.Thread(target=server.serve_forever,daemon=True);t.start()
            try:
                base=f'http://127.0.0.1:{server.server_port}'
                for path in ('status','export','backup'):
                    request=urllib.request.Request(base+'/api/'+path,headers={'Authorization':'Bearer test-token'})
                    with urllib.request.urlopen(request) as response:
                        data=response.read()
                        if path=='status':self.assertEqual(json.loads(data)['version'],'1.0.0')
                        if path=='export':self.assertEqual(json.loads(data)['tables']['notes'][0]['title'],'private')
                        if path=='backup':self.assertTrue(data.startswith(b'SQLite format 3\x00'))
            finally:server.shutdown();server.server_close();t.join();store.db.close()
    def test_cli_check_backup_and_restore(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=tmp+'/db'
            result=subprocess.run(['python3','-m','lumen','--db',path,'--check'],capture_output=True,text=True)
            self.assertEqual(result.returncode,0,result.stderr);self.assertEqual(json.loads(result.stdout)['database'],'ok')
            result=subprocess.run(['python3','-m','lumen','--db',path,'--backup',tmp+'/saved'],capture_output=True,text=True)
            self.assertEqual(result.returncode,0,result.stderr)
            snapshot=next(Path(tmp+'/saved').glob('*.db'))
            result=subprocess.run(['python3','-m','lumen','--db',tmp+'/restored','--restore',str(snapshot)],capture_output=True,text=True)
            self.assertEqual(result.returncode,0,result.stderr)
            result=subprocess.run(['python3','-m','lumen','--db',tmp+'/restored','--check'],capture_output=True,text=True)
            self.assertEqual(json.loads(result.stdout)['database'],'ok')
