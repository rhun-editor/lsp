#!/usr/bin/env python3
"""Real host/server callbacks, settings reload and progress through the native UI."""
import json
import errno
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[1]
RHUN = Path(sys.argv.pop(1) if len(sys.argv)>1 and not sys.argv[1].startswith('-') else shutil.which('rhun') or Path.home()/'.local/bin/rhun').resolve()
TARGET = 'windows-x64' if os.name=='nt' else 'macos-arm64' if sys.platform=='darwin' else 'linux-x86_64'


class Callbacks(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.build = tempfile.TemporaryDirectory(prefix='rhun-lsp-callback-build-')
        cls.extension = Path(cls.build.name)/('lsp.exe' if os.name=='nt' else 'lsp')
        cls.server = Path(cls.build.name)/('gopls.exe' if os.name=='nt' else 'gopls')
        subprocess.run(['go','build','-o',str(cls.extension),'.'],cwd=ROOT,check=True)
        subprocess.run(['go','build','-o',str(cls.server),'./tests/callbackserver'],cwd=ROOT,check=True)

    @classmethod
    def tearDownClass(cls):
        cls.build.cleanup()

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='rhun-lsp-callback-')
        self.work = Path(self.tmp.name).resolve()
        self.state = self.work/'state/rhun/extensions/rhun-editor--lsp'
        self.state.mkdir(parents=True)
        self.project = self.work/'project'
        self.project.mkdir()
        (self.project/'main.go').write_text('package main\n')
        config = self.work/'config/rhun'
        config.mkdir(parents=True)
        (config/'config').write_text('[files]\nrestore_session = false\nrestore_project = false\n[updates]\ncheck = false\n[git]\nenabled = false\n')
        extension = self.work/'extensions/rhun-editor/lsp'
        (extension/'bin').mkdir(parents=True)
        manifest = (ROOT/'extension.ini').read_text()
        manifest = manifest.replace(f'bin/lsp-{TARGET}'+('.exe' if os.name=='nt' else ''),'bin/'+self.extension.name)
        (extension/'extension.ini').write_text(manifest)
        shutil.copy2(self.extension,extension/'bin'/self.extension.name)
        self.bindir = self.work/'bin'
        self.bindir.mkdir()
        shutil.copy2(self.server,self.bindir/self.server.name)
        (self.state/'lsp.ini').write_text('[servers]\nGo = gopls\n')

    def tearDown(self):
        # The host can exit just before its extension finishes flushing state.
        # Retry only a directory repopulated during cleanup, and keep a bound.
        deadline = time.monotonic() + 2
        while True:
            try:
                self.tmp.cleanup()
                break
            except OSError as error:
                if error.errno != errno.ENOTEMPTY or time.monotonic() >= deadline:
                    raise
                time.sleep(.05)

    def run_editor(self, actions, invalid=False, dynamic=False, extra_options=None, initial_wait=1300):
        options={'from':'initial'}
        if dynamic: options['dynamicCapabilities']=True
        options.update(extra_options or {})
        (self.state/'settings.json').write_text(json.dumps({'defaults':{'settings':{'gopls':{'mode':'old'},'invalidRestart':invalid},'initializationOptions':options}}))
        script=self.work/'actions.txt'
        script.write_text('\n'.join(['open '+str(self.project/'main.go'),f'wait {initial_wait}',*actions,'quit'])+'\n')
        env=dict(os.environ,HOME=str(self.work),XDG_CONFIG_HOME=str(self.work/'config'),XDG_STATE_HOME=str(self.work/'state'),PATH=str(self.bindir)+os.pathsep+os.environ['PATH'])
        result=subprocess.run([str(RHUN),str(self.project),'--headless','1000x600','--extensions',str(self.work/'extensions'),'--script',str(script)],env=env,capture_output=True,timeout=30)
        self.assertEqual(result.returncode,0,result.stderr.decode(errors='replace'))
        trace=self.state/'callbacks.jsonl'
        self.assertTrue(trace.exists(),result.stderr.decode(errors='replace')+result.stdout.decode(errors='replace'))
        return result.stdout.decode(),[json.loads(x) for x in trace.read_text().splitlines()]

    def test_configuration_reload_and_progress(self):
        out,trace=self.run_editor(['print-extensions','cmd rhun-editor/lsp.reload_configuration','wait 500','cmd rhun-editor/lsp.cancel_progress','wait 500','print-extensions'])
        self.assertIn('status=Go: Indexing files (40%)',out)
        initial=next(x for x in trace if x.get('method')=='initialize')['params']
        self.assertEqual(initial['initializationOptions'],{'from':'initial'})
        self.assertTrue(initial['capabilities']['workspace']['configuration'])
        self.assertTrue(initial['capabilities']['window']['workDoneProgress'])
        config=next(x for x in trace if x.get('id')=='config' and 'result' in x)['result']
        self.assertEqual(config,[{'mode':'old'},None])
        folders=next(x for x in trace if x.get('id')=='folders' and 'result' in x)['result']
        self.assertEqual(len(folders),1)
        for identity in ('unknown','registration'):
            error=next(x for x in trace if x.get('id')==identity and 'error' in x)['error']
            self.assertEqual(error['code'],-32601)
        pushes=[x['params']['settings']['gopls']['mode'] for x in trace if x.get('method')=='workspace/didChangeConfiguration']
        self.assertEqual(pushes,['old','new'])
        self.assertTrue(any(x.get('method')=='window/workDoneProgress/cancel' for x in trace),trace)
        self.assertIn('status=gopls',out.splitlines()[-1])

    def test_dynamic_definition_registration_and_removal(self):
        out,trace=self.run_editor(['cmd definition','wait 500','cmd definition','wait 500'],dynamic=True)
        initial=next(x for x in trace if x.get('method')=='initialize')['params']
        self.assertTrue(initial['capabilities']['textDocument']['definition']['dynamicRegistration'])
        for identity in ('dynamic-register','dynamic-unregister'):
            reply=next(x for x in trace if x.get('id')==identity and 'method' not in x)
            self.assertNotIn('error',reply)
            self.assertIn('result',reply)
        queries=[x for x in trace if x.get('method')=='textDocument/definition']
        self.assertEqual(len(queries),1,trace)

    def test_full_sync_and_save_include_current_text(self):
        out,trace=self.run_editor(['type é','wait 200','cmd save','wait 300'],extra_options={'syncKind':1,'syncOpenClose':True,'syncSave':True})
        changes=[x for x in trace if x.get('method')=='textDocument/didChange']
        self.assertTrue(changes,trace)
        change=changes[-1]['params']['contentChanges'][0]
        self.assertNotIn('range',change)
        self.assertIn('é',change['text'])
        save=next(x for x in trace if x.get('method')=='textDocument/didSave')
        self.assertEqual(save['params']['text'],change['text'])
        self.assertLess(trace.index(changes[-1]),trace.index(save))

    def test_delayed_initialize_opens_latest_text(self):
        out,trace=self.run_editor(['type changed','wait 1200'],extra_options={'initializeDelayMs':700},initial_wait=200)
        opened=[x for x in trace if x.get('method')=='textDocument/didOpen']
        self.assertEqual(len(opened),1,trace)
        self.assertIn('changed',opened[0]['params']['textDocument']['text'])
        self.assertFalse(any(x.get('method')=='textDocument/didChange' for x in trace),trace)

    def test_server_can_disable_document_synchronization(self):
        out,trace=self.run_editor(['type changed','cmd save','wait 300'],extra_options={'syncKind':0,'syncOpenClose':False,'syncSave':False})
        forbidden={'textDocument/didOpen','textDocument/didChange','textDocument/didSave','textDocument/didClose'}
        self.assertFalse(any(x.get('method') in forbidden for x in trace),trace)

    def test_invalid_restart_keeps_current_server(self):
        out,trace=self.run_editor(['print-extensions','cmd rhun-editor/lsp.restart','wait 500','print-toast','print-extensions'],invalid=True)
        self.assertIn('configuration: lsp.ini',out)
        pids=re.findall(r'pid=(\d+)',out)
        self.assertEqual(len(pids),2,out)
        self.assertEqual(pids[0],pids[1],out)
        self.assertEqual(sum(x.get('method')=='initialize' for x in trace),1)

if __name__=='__main__':
    unittest.main()
