#!/usr/bin/env python3
"""Code lens commands and inline completion through the real SDK against the fake server: a click
on a lens runs its server command (workspace/executeCommand, whose edit comes back as
workspace/applyEdit), and textDocument/inlineCompletion shows as ghost text that Tab takes.
usage: tests/presentation.py [RHUN]"""
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
RHUN = Path(sys.argv.pop(1) if len(sys.argv) > 1 and not sys.argv[1].startswith('-')
            else shutil.which('rhun') or Path.home() / '.local/bin/rhun').resolve()
TARGET = 'windows-x64' if os.name == 'nt' else 'macos-arm64' if sys.platform == 'darwin' else 'linux-x86_64'
EXE = '.exe' if os.name == 'nt' else ''
MAIN = 'package main\n\n// LENS\nfunc main() {\n\t//\n}\n'


class Presentation(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.build = tempfile.TemporaryDirectory(prefix='rhun-lsp-pres-build-')
        cls.extension = Path(cls.build.name) / ('lsp' + EXE)
        cls.server = Path(cls.build.name) / ('gopls' + EXE)
        env = dict(os.environ, CGO_ENABLED='0')
        subprocess.run(['go', 'build', '-o', str(cls.extension), '.'], cwd=ROOT, check=True, env=env)
        subprocess.run(['go', 'build', '-o', str(cls.server), './tests/fakeserver'], cwd=ROOT, check=True, env=env)

    @classmethod
    def tearDownClass(cls):
        cls.build.cleanup()

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='rhun-lsp-pres-')
        self.work = Path(self.tmp.name).resolve()
        self.project = self.work / 'project'
        self.project.mkdir()
        (self.project / 'main.go').write_text(MAIN)
        config = self.work / 'config/rhun'
        config.mkdir(parents=True)
        (config / 'config').write_text('[files]\nrestore_session = false\nrestore_project = false\n[updates]\n'
                                       'check = false\n[git]\nenabled = false\n[ui]\nsidebar = false\n'
                                       'agents_panel = false\n[editor]\ncursor_blink = false\n'
                                       'inline_complete = false\n')
        extension = self.work / 'extensions/rhun-editor/lsp'
        (extension / 'bin').mkdir(parents=True)
        manifest = (ROOT / 'extension.ini').read_text().replace(f'bin/lsp-{TARGET}' + EXE, 'bin/' + self.extension.name)
        (extension / 'extension.ini').write_text(manifest)
        shutil.copy2(self.extension, extension / 'bin' / self.extension.name)
        self.bindir = self.work / 'bin'
        self.bindir.mkdir()
        shutil.copy2(self.server, self.bindir / self.server.name)

    def tearDown(self):
        self.tmp.cleanup()

    def run_editor(self, actions):
        script = self.work / 'actions.txt'
        script.write_text('\n'.join(['wait 100', 'open ' + str(self.project / 'main.go'), 'wait-ext lsp running',
                                     'wait 1500', *actions, 'quit']) + '\n')
        env = dict(os.environ, HOME=str(self.work), XDG_CONFIG_HOME=str(self.work / 'config'),
                   XDG_STATE_HOME=str(self.work / 'state'), PATH=str(self.bindir) + os.pathsep + os.environ['PATH'])
        r = subprocess.run([str(RHUN), str(self.project), '--headless', '1000x600', '--scale', '1',
                            '--extensions', str(self.work / 'extensions'), '--script', str(script)],
                           env=env, capture_output=True, timeout=60)
        self.assertEqual(r.returncode, 0, r.stderr.decode(errors='replace'))
        return r.stdout.decode(errors='replace').splitlines()

    def test_a_lens_click_runs_its_server_command(self):
        out = self.run_editor(['print-present lenses', 'print-xy 3 1'])
        self.assertIn('lens 3 Add a comment +command', out)
        x, y = map(int, out[-1].split())
        out = self.run_editor([f'click {x + 6} {y - 21}', 'wait 1500', 'print-doc'])
        self.assertIn('// added by a command', out)

    def test_inline_completion_shows_and_tab_takes_it(self):
        out = self.run_editor(['key ctrl+Home'] + ['key Down'] * 4 + ['key End', 'cmd inline_complete', 'wait 1200',
                                                                    'print-present ghost', 'key Tab', 'print-doc'])
        self.assertIn('ghost 5:4  more x }', out)
        self.assertIn('\t// more x }', out)


if __name__ == '__main__':
    unittest.main()
