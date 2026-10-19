#!/usr/bin/env python3
"""Native host watcher through the SDK and LSP dynamic registration lifecycle."""
import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
import json
import threading
import time

spec = importlib.util.spec_from_file_location('callbacks', Path(__file__).with_name('callbacks.py'))
callbacks = importlib.util.module_from_spec(spec)
spec.loader.exec_module(callbacks)


class WatchedFiles(callbacks.Callbacks):
    @classmethod
    def setUpClass(cls):
        cls.build = tempfile.TemporaryDirectory(prefix='rhun-lsp-watch-build-')
        cls.extension = Path(cls.build.name) / ('lsp.exe' if callbacks.os.name == 'nt' else 'lsp')
        cls.server = Path(cls.build.name) / ('gopls.exe' if callbacks.os.name == 'nt' else 'gopls')
        subprocess.run(['go', 'build', '-o', str(cls.extension), '.'], cwd=callbacks.ROOT, check=True)
        subprocess.run(['go', 'build', '-o', str(cls.server), './tests/watchserver'], cwd=callbacks.ROOT, check=True)

    def test_native_invalidation_and_dynamic_sync(self):
        stop = threading.Event()
        mutations = []
        def external_writer():
            target = self.project / '.hidden' / 'created.watch'
            log = self.state / 'callbacks.jsonl'
            phase = 0
            deadline = time.monotonic() + 12
            while not stop.wait(0.01) and time.monotonic() < deadline:
                try:
                    messages = [json.loads(line) for line in log.read_text().splitlines()]
                except (FileNotFoundError, json.JSONDecodeError):
                    continue
                if phase == 0 and any(item.get('method') == 'textDocument/didSave' for item in messages):
                    target.parent.mkdir()
                    target.write_text('created')
                    phase = 1
                    mutations.append('create')
                if phase == 1 and any(item.get('id') == 'watch-remove' and 'method' not in item for item in messages):
                    target.write_text('after unregister')
                    mutations.append('after unregister')
                    return
        writer = threading.Thread(target=external_writer, daemon=True)
        writer.start()
        try:
            _, trace = self.run_editor(['type changed', 'wait 200', 'cmd save', 'wait 3200',
                                        'cmd close_tab', 'wait 200'])
        finally:
            stop.set()
            writer.join(timeout=1)
        self.assertEqual(mutations, ['create', 'after unregister'], trace)
        initial = next(item for item in trace if item.get('method') == 'initialize')['params']['capabilities']
        self.assertTrue(initial['workspace']['didChangeWatchedFiles']['dynamicRegistration'])
        self.assertTrue(initial['workspace']['didChangeWatchedFiles']['relativePatternSupport'])
        self.assertTrue(initial['textDocument']['synchronization']['dynamicRegistration'])
        for identity in ('watch-add', 'watch-remove'):
            reply = next(item for item in trace if item.get('id') == identity and 'method' not in item)
            self.assertIn('result', reply, trace)
            self.assertNotIn('error', reply, trace)
        opened = [item for item in trace if item.get('method') == 'textDocument/didOpen']
        self.assertEqual(len(opened), 1, trace)
        saved = next(item for item in trace if item.get('method') == 'textDocument/didSave')
        self.assertIn('changed', saved['params']['text'])
        watched = [item for item in trace if item.get('method') == 'workspace/didChangeWatchedFiles']
        self.assertEqual(len(watched), 1, trace)
        events = watched[0]['params']['changes']
        self.assertEqual(len(events), 1, trace)
        self.assertEqual(events[0]['type'], 1)
        self.assertTrue(events[0]['uri'].endswith('/.hidden/created.watch'))
        self.assertTrue(any(item.get('method') == 'textDocument/didClose' for item in trace), trace)


def load_tests(loader, tests, pattern):
    return unittest.TestSuite([WatchedFiles('test_native_invalidation_and_dynamic_sync')])


if __name__ == '__main__':
    unittest.main()
