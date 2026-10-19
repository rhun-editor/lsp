#!/usr/bin/env python3
"""Source actions through the real SDK, native save host and a scripted LSP server."""
import json
from pathlib import Path
import subprocess
import threading
import time
import unittest

host=__import__('callbacks')

class SourceActions(host.Callbacks):
    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        subprocess.run(['go','build','-o',str(cls.server),'./tests/saveserver'],cwd=host.ROOT,check=True)

    def run_source(self,mode='sequential',formatting=False,kinds='source.fixAll',actions=None,timeout=2000,final_newline=True):
        config=self.work/'config/rhun/config'
        config.write_text(config.read_text()+f'\n[files]\ncode_actions_on_save = {kinds}\ncode_actions_on_save_timeout = {timeout}\nformat_on_save = {str(formatting).lower()}\nfinal_newline = {str(final_newline).lower()}\n')
        return self.run_editor(actions or ['type y','cmd save','wait 1200','print-doc','print-state'],extra_options={'sourceMode':mode})

    def text(self):return (self.project/'main.go').read_text()

    def test_sequential_edits_requery_fresh_text_before_formatting(self):
        out,trace=self.run_source(formatting=True)
        self.assertEqual(self.text(),'F[A]ypackage main\n[B]\n',out)
        queries=[m['params'] for m in trace if m.get('method')=='textDocument/codeAction']
        self.assertEqual(len(queries),3)
        self.assertTrue(all(q['context']=={'diagnostics':[],'only':['source.fixAll'],'triggerKind':2} for q in queries))
        self.assertEqual(queries[0]['range']['start'],{'line':0,'character':0})
        self.assertTrue(any(m.get('method')=='textDocument/didChange' for m in trace))

    def test_resolved_action_preserves_original_kind(self):
        out,trace=self.run_source('resolve')
        self.assertEqual(self.text(),'[R]ypackage main\n',out)
        self.assertEqual(sum(m.get('method')=='codeAction/resolve' for m in trace),1)

    def test_command_edits_drain_even_after_execute_reply(self):
        out,trace=self.run_source('command',formatting=True)
        self.assertEqual(self.text(),'F[C]ypackage main\n',out)
        self.assertGreaterEqual(len({m['serverPID'] for m in trace if m.get('method')=='initialize'}),2)

    def test_command_edits_refresh_and_synchronize_before_acknowledgment(self):
        out,trace=self.run_source('command-two')
        self.assertEqual(self.text(),'[1]ypackage main\n[2]\n',out)
        for identity in ('apply1','apply2'):
            reply=next(m for m in trace if m.get('id')==identity and 'result' in m)
            self.assertTrue(reply['result']['applied'],trace)

    def test_duplicate_edit_only_actions_merge_atomically(self):
        out,trace=self.run_source('duplicate-edit',final_newline=False,actions=['type y','cmd save','wait 900','cmd undo','print-doc'])
        self.assertEqual(self.text(),'[A]ypackage main\n[B]',out)
        self.assertIn('ypackage main\n',out)
        self.assertNotIn('[A]',out)

    def test_duplicate_commands_are_refused(self):
        out,trace=self.run_source('duplicate-command')
        self.assertEqual(self.text(),'ypackage main\n',out)
        self.assertFalse(any(m.get('method')=='workspace/executeCommand' for m in trace))

    def test_stale_lsp_version_is_rejected(self):
        out,trace=self.run_source('stale-version')
        self.assertEqual(self.text(),'ypackage main\n',out)

    def test_foreign_group_is_rejected_without_opening_other_file(self):
        (self.project/'other.go').write_text('other\n')
        out,trace=self.run_source('foreign',actions=['type y','cmd save','wait 900','print-tabs'])
        self.assertEqual(self.text(),'ypackage main\n',out)
        self.assertNotIn('other.go',out)
        self.assertEqual((self.project/'other.go').read_text(),'other\n')

    def test_cancel_command_does_not_write(self):
        out,trace=self.run_source('cancel-command',actions=['type y','cmd save','wait 150','cmd cancel_save','wait 1000','print-doc'])
        self.assertEqual(self.text(),'package main\n',out)
        self.assertNotIn('[C]',out)

    def test_late_post_reply_command_edit_is_not_applied(self):
        out,trace=self.run_source('late-command')
        self.assertEqual(self.text(),'ypackage main\n',out)

    def test_typing_during_source_action_skips_further_transforms(self):
        out,trace=self.run_source('delayed',formatting=True,actions=['type y','cmd save','wait 150','type z','wait 1000'])
        self.assertEqual(self.text(),'yzpackage main\n',out)
        self.assertFalse(any(m.get('method')=='textDocument/formatting' for m in trace))

    def test_two_source_kinds_run_in_configured_order(self):
        out,trace=self.run_source('sequential',kinds='source.organizeImports source.fixAll')
        self.assertEqual(self.text(),'[A][O]ypackage main\n[B]\n',out)
        kinds=[m['params']['context']['only'][0] for m in trace if m.get('method')=='textDocument/codeAction']
        self.assertEqual(kinds[:2],['source.organizeImports','source.organizeImports'])
        self.assertTrue(all(k=='source.fixAll' for k in kinds[2:]),trace)


    def test_unsupported_resolution_falls_back_to_formatting(self):
        out,trace=self.run_source('unsupported-resolve',formatting=True)
        self.assertEqual(self.text(),'Fypackage main\n',out)
        self.assertFalse(any(m.get('method')=='codeAction/resolve' for m in trace))

    def test_source_timeout_cancels_query_and_saves_latest(self):
        out,trace=self.run_source('delayed',timeout=250)
        self.assertEqual(self.text(),'ypackage main\n',out)
        self.assertTrue(any(m.get('method')=='$/cancelRequest' for m in trace),trace)


    def test_save_retires_a_session_that_previously_ran_an_explicit_command(self):
        out,trace=self.run_source('command',actions=['cmd code_actions','wait 300','key Return','wait 700','cmd save','wait 900','print-doc'])
        self.assertEqual(self.text(),'[C]package main\n',out)
        self.assertEqual(sum(m.get('method')=='workspace/executeCommand' for m in trace),1,trace)
        self.assertEqual(len({m['serverPID'] for m in trace if m.get('method')=='initialize'}),2,trace)


    def test_deferred_source_action_does_not_require_data(self):
        for mode in ('resolve-no-data','resolve-null-data','resolve-title-only'):
            with self.subTest(mode=mode):
                (self.project/'main.go').write_text('package main\n')
                trace_file=self.state/'callbacks.jsonl'
                if trace_file.exists():trace_file.unlink()
                out,trace=self.run_source(mode)
                self.assertEqual(self.text(),'[R]ypackage main\n',out)
                self.assertTrue(any(m.get('method')=='codeAction/resolve' for m in trace))

    def test_interactive_deferred_action_remains_available(self):
        out,trace=self.run_source('resolve',actions=['cmd code_actions','wait 200','key Return','wait 500','cmd save_without_formatting','wait 200','print-doc'])
        self.assertEqual(self.text(),'[R]package main\n',out)
        self.assertTrue(any(m.get('method')=='codeAction/resolve' for m in trace))


    def test_typing_during_interactive_resolution_rejects_self_and_foreign_edits(self):
        for mode in ('resolve-delayed','resolve-foreign-delayed'):
            with self.subTest(mode=mode):
                (self.project/'main.go').write_text('package main\n')
                (self.project/'other.go').write_text('other\n')
                trace_file=self.state/'callbacks.jsonl'
                if trace_file.exists():trace_file.unlink()
                out,trace=self.run_source(mode,actions=['cmd code_actions','wait 200','key Return','wait 150','type z','wait 800','cmd save_without_formatting','wait 200','print-tabs','print-doc'])
                self.assertEqual(self.text(),'zpackage main\n',out)
                self.assertNotIn('[R]',out)
                self.assertNotIn('other.go',out)
                self.assertEqual((self.project/'other.go').read_text(),'other\n')

    def test_rejected_command_edit_blocks_following_valid_edit(self):
        (self.project/'other.go').write_text('other\n')
        out,trace=self.run_source('command-foreign-then-valid',formatting=True)
        self.assertEqual(self.text(),'Fypackage main\n',out)
        for identity in ('apply1','apply2'):
            reply=next(m for m in trace if m.get('id')==identity and 'result' in m)
            self.assertFalse(reply['result']['applied'],trace)
        self.assertEqual((self.project/'other.go').read_text(),'other\n')

    def test_typing_in_foreign_target_during_resolution_preserves_text(self):
        other=self.project/'other.go'
        other.write_text('other\n')
        out,trace=self.run_source('resolve-foreign-replace-delayed',actions=[
            'open '+str(other),'wait 200','open '+str(self.project/'main.go'),
            'cmd code_actions','wait 200','key Return','wait 150',
            'open '+str(other),'type z','wait 800','cmd save_without_formatting',
            'wait 200','print-doc'])
        self.assertEqual(other.read_text(),'zother\n',out)
        self.assertNotIn('[R]',out)

    def test_unchanged_foreign_target_can_receive_resolved_edit(self):
        other=self.project/'other.go'
        other.write_text('other\n')
        out,trace=self.run_source('resolve-foreign-replace-delayed',actions=[
            'open '+str(other),'wait 200','open '+str(self.project/'main.go'),
            'cmd code_actions','wait 200','key Return','wait 850',
            'open '+str(other),'cmd save_without_formatting','wait 200','print-doc'])
        self.assertEqual(other.read_text(),'[R]\n',out)

    def test_deferred_edit_without_prior_target_snapshot_is_rejected(self):
        other=self.project/'other.go'
        other.write_text('other\n')
        out,trace=self.run_source('resolve-foreign-replace-delayed',actions=[
            'cmd code_actions','wait 200','key Return','wait 850','print-tabs'])
        self.assertEqual(other.read_text(),'other\n',out)
        self.assertNotIn('other.go',out)

    def test_typing_during_explicit_command_preserves_text(self):
        out,trace=self.run_source('explicit-stale-command',actions=[
            'cmd code_actions','wait 200','key Return','wait 150','type z',
            'wait 1000','cmd save_without_formatting','wait 200','print-doc'])
        self.assertEqual(self.text(),'zpackage main\n',out)
        reply=next(m for m in trace if m.get('id')=='apply1' and 'result' in m)
        self.assertFalse(reply['result']['applied'],trace)

    def test_explicit_command_can_apply_sequential_credited_edits(self):
        out,trace=self.run_source('command-two',actions=[
            'cmd code_actions','wait 200','key Return','wait 800',
            'cmd save_without_formatting','wait 200','print-doc'])
        self.assertEqual(self.text(),'[1]package main\n[2]\n',out)
        for identity in ('apply1','apply2'):
            reply=next(m for m in trace if m.get('id')==identity and 'result' in m)
            self.assertTrue(reply['result']['applied'],trace)

    def test_command_refresh_accepts_target_without_language_server(self):
        target=self.project/'go.mod'
        target.write_text('module example\n')
        out,trace=self.run_source('command-no-server-two',actions=[
            'open '+str(target),'wait 250','open '+str(self.project/'main.go'),
            'cmd code_actions','wait 200','key Return','wait 800',
            'open '+str(target),'cmd save_without_formatting','wait 200','print-doc'])
        self.assertEqual(target.read_text(),'[2][1]module example\n',out)
        for identity in ('apply1','apply2'):
            reply=next(m for m in trace if m.get('id')==identity and 'result' in m)
            self.assertTrue(reply['result']['applied'],trace)

    def test_numeric_version_from_another_server_is_rejected_after_refresh(self):
        target=self.project/'other.rs'
        target.write_text('other\n')
        (self.state/'lsp.ini').write_text('[servers]\nGo = gopls\nRust = gopls\n')
        out,trace=self.run_source('command-foreign-server-two',actions=[
            'open '+str(target),'wait 350','open '+str(self.project/'main.go'),
            'cmd code_actions','wait 200','key Return','wait 800',
            'open '+str(target),'cmd save_without_formatting','wait 200','print-doc'])
        self.assertEqual(target.read_text(),'[1]other\n',out)
        first=next(m for m in trace if m.get('id')=='apply1' and 'result' in m)
        second=next(m for m in trace if m.get('id')=='apply2' and 'result' in m)
        self.assertTrue(first['result']['applied'],trace)
        self.assertFalse(second['result']['applied'],trace)

    def test_typing_during_initial_action_request_preserves_foreign_text(self):
        for mode in ('resolve-initial-foreign-delayed','inline-foreign-delayed'):
            with self.subTest(mode=mode):
                other=self.project/'other.go'
                other.write_text('other\n')
                trace_file=self.state/'callbacks.jsonl'
                if trace_file.exists():trace_file.unlink()
                out,trace=self.run_source(mode,actions=[
                    'open '+str(other),'wait 200','open '+str(self.project/'main.go'),
                    'cmd code_actions','wait 150','open '+str(other),'type z',
                    'open '+str(self.project/'main.go'),'wait 700','key Return','wait 700',
                    'open '+str(other),'cmd save_without_formatting','wait 200','print-doc'])
                self.assertEqual(other.read_text(),'zother\n',out)
                self.assertNotIn('[R]',out)

    def test_initial_edit_can_be_selected_when_foreign_target_is_unchanged(self):
        other=self.project/'other.go'
        other.write_text('other\n')
        out,trace=self.run_source('resolve-initial-foreign-delayed',actions=[
            'open '+str(other),'wait 200','open '+str(self.project/'main.go'),
            'cmd code_actions','wait 800','key Return','wait 400',
            'open '+str(other),'cmd save_without_formatting','wait 200','print-doc'])
        self.assertEqual(other.read_text(),'[R]\n',out)

    def test_reopened_target_does_not_receive_previous_document_edit(self):
        other=self.project/'other.go'
        other.write_text('other\n')
        marker=self.work/'target-closed.ppm'
        stop=threading.Event()
        written=threading.Event()
        def replace_closed_file():
            deadline=time.monotonic()+10
            while not stop.is_set() and time.monotonic()<deadline:
                if marker.exists():
                    other.write_text('fresh\n')
                    written.set()
                    return
                time.sleep(.005)
        writer=threading.Thread(target=replace_closed_file)
        writer.start()
        try:
            out,trace=self.run_source('resolve-foreign-replace-delayed',actions=[
                'open '+str(other),'wait 200','open '+str(self.project/'main.go'),
                'cmd code_actions','wait 200','key Return','wait 100',
                'open '+str(other),'cmd close_tab','shot '+str(marker),'wait 150',
                'open '+str(other),'wait 800','cmd save_without_formatting',
                'wait 200','print-doc'])
        finally:
            stop.set()
            writer.join(1)
        self.assertTrue(written.is_set(),'fixture did not rewrite the closed target')
        self.assertEqual(other.read_text(),'fresh\n',out)
        self.assertNotIn('[R]',out)

    def test_inline_edit_and_command_share_fresh_credited_snapshot(self):
        out,trace=self.run_source('inline-edit-command',actions=[
            'cmd code_actions','wait 200','key Return','wait 800',
            'cmd save_without_formatting','wait 200','print-doc'])
        self.assertEqual(self.text(),'[C][I]package main\n',out)
        reply=next(m for m in trace if m.get('id')=='apply1' and 'result' in m)
        self.assertTrue(reply['result']['applied'],trace)
        self.assertFalse(any(m.get('method')=='codeAction/resolve' for m in trace))

for name in dir(host.Callbacks):
    if name.startswith('test_'):setattr(SourceActions,name,None)

if __name__=='__main__':unittest.main()
