import os
import shutil
import sqlite3
import subprocess
import tempfile
import unittest
from pathlib import Path

INSTALLER=Path(__file__).resolve().parents[1]/'scripts/install.sh'
class InstallerTest(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name);self.source=self.root/'source';self.source.mkdir();self.target=self.root/'installed';self.bin=self.root/'bin';self.bin.mkdir()
        self.git=shutil.which('git')
        self.run_git('init','-b','main')
        (self.source/'lumen').mkdir();(self.source/'lumen/__init__.py').write_text('__version__="fixture-1"\n');(self.source/'lumen/core.py').write_text('# fixture\n');(self.source/'requirements.txt').write_text('')
        (self.source/'.gitignore').write_text('.venv\n.venvs/\n.venv-next\ndata/\n.env\n__pycache__/\n')
        self.commit()
        wrapper=self.bin/'git';wrapper.write_text('''#!/bin/bash
if [[ "$1" == clone ]]; then exec "$FIXTURE_GIT" clone --branch main "$FIXTURE_SOURCE" "${@: -1}"; fi
if [[ "$3" == remote && "$4" == get-url ]]; then echo https://github.com/kobewl/lumen.git; exit; fi
if [[ ( "$1" == merge || "$3" == merge ) && "${FAIL_MERGE:-}" == true ]]; then exit 42; fi
exec "$FIXTURE_GIT" "$@"
''');wrapper.chmod(0o755)
        systemctl=self.bin/'systemctl';systemctl.write_text('''#!/bin/bash
case "$1" in
show) echo "$LUMEN_INSTALL_DIR";;
is-active) exit 0;;
stop) touch "$FIXTURE_ROOT/stopped";;
start) touch "$FIXTURE_ROOT/started";;
*) exit 1;;
esac
''');systemctl.chmod(0o755)
        self.env={**os.environ,'PATH':str(self.bin)+':'+os.environ['PATH'],'LUMEN_INSTALL_DIR':str(self.target),'LUMEN_SERVICE':'','FIXTURE_GIT':self.git,'FIXTURE_SOURCE':str(self.source),'FIXTURE_ROOT':str(self.root),'PIP_DISABLE_PIP_VERSION_CHECK':'1'}
        self.env.pop('LUMEN_DB_PATH',None)
    def tearDown(self):self.tmp.cleanup()
    def run_git(self,*args):return subprocess.run([self.git,'-C',str(self.source),*args],capture_output=True,text=True,check=True)
    def commit(self):
        self.run_git('add','-A');self.run_git('-c','user.name=Fixture','-c','user.email=fixture@example.invalid','commit','-m','fixture')
    def install(self,**env):return subprocess.run(['bash',str(INSTALLER)],env={**self.env,**env},capture_output=True,text=True)
    def test_first_install_then_update_preserves_configuration_and_backs_up_data(self):
        result=self.install();self.assertEqual(result.returncode,0,result.stderr)
        old=self.target.joinpath('.venv').resolve()
        (self.target/'.env').write_text('local-fixture-config');(self.target/'.env').chmod(0o600)
        (self.target/'data').mkdir();db=self.target/'data/personal.db'
        with sqlite3.connect(db) as connection:connection.execute('CREATE TABLE facts(content TEXT)');connection.execute("INSERT INTO facts VALUES ('keep-me')")
        (self.source/'lumen/__init__.py').write_text('__version__="fixture-2"\n');self.commit()
        result=self.install(LUMEN_SERVICE='lumen');self.assertEqual(result.returncode,0,result.stderr)
        self.assertTrue((self.root/'stopped').exists());self.assertTrue((self.root/'started').exists())
        self.assertEqual((self.target/'.env').stat().st_mode&0o777,0o600)
        self.assertEqual((self.target/'.venvs').stat().st_mode&0o005,0o005)
        self.assertNotEqual(self.target.joinpath('.venv').resolve(),old)
        self.assertTrue(old.exists());self.assertEqual((self.target/'.env').read_text(),'local-fixture-config')
        snapshots=list((self.target/'data/backups').glob('lumen-upgrade-*.db'));self.assertEqual(len(snapshots),1)
        with sqlite3.connect(snapshots[0]) as connection:self.assertEqual(connection.execute('SELECT content FROM facts').fetchone()[0],'keep-me')
        self.assertIn('fixture-2',result.stdout)
    def test_dirty_checkout_is_refused_without_stopping_service(self):
        self.assertEqual(self.install().returncode,0)
        (self.target/'lumen/core.py').write_text('# local change\n')
        result=self.install(LUMEN_SERVICE='lumen')
        self.assertNotEqual(result.returncode,0);self.assertFalse((self.root/'stopped').exists())
        self.assertEqual((self.target/'lumen/core.py').read_text(),'# local change\n')
    def test_failed_update_leaves_service_stopped_and_old_environment_selected(self):
        self.assertEqual(self.install().returncode,0);old=self.target.joinpath('.venv').resolve()
        result=self.install(LUMEN_SERVICE='lumen',FAIL_MERGE='true')
        self.assertNotEqual(result.returncode,0);self.assertTrue((self.root/'stopped').exists());self.assertFalse((self.root/'started').exists())
        self.assertEqual(self.target.joinpath('.venv').resolve(),old)
