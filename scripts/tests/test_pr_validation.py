import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('pr_validation', Path(__file__).parents[1] / 'pr-validation.py')
validation = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validation)


class ValidationReuseTests(unittest.TestCase):
    def setUp(self):
        self.head = 'a' * 40
        self.commit = 'b' * 40
        self.run = dict(id=42, run_attempt=2, head_sha=self.head,
                        event='pull_request', status='completed', conclusion='success', html_url='run-url')
        self.pr = dict(number=9, merged_at='now', merge_commit_sha='merged',
                       base=dict(ref='develop'), head=dict(sha=self.head))
        self.artifact = dict(name=validation.ARTIFACT, expired=False)
        self.evidence = dict(schema=1, base='develop', run_id=42, attempt=2,
                             head_sha=self.head, tree='tree', commit=self.commit)
        self.tested = dict(tree=dict(sha='tree'), parents=[dict(sha=self.head)])
        self.downloads = []

    def api(self, path):
        if '/pulls' in path:
            return [self.pr]
        if '/workflows/' in path:
            return dict(workflow_runs=[self.run])
        if '/artifacts?' in path:
            return dict(artifacts=[self.artifact])
        return self.tested

    def reuse(self):
        with patch.object(validation, 'command', side_effect=lambda *args: 'tree' if args[-1].endswith('^{tree}') else 'merged'), \
                patch.object(validation, 'api', side_effect=self.api), \
                patch.object(validation, 'download', return_value=self.evidence) as download:
            result = validation.reusable('anchor-dra/anchor')
            self.downloads = download.call_args_list
            return result

    def test_exact_tree_reuses_successful_pr(self):
        self.assertTrue(self.reuse())

    def test_changed_tree_old_attempt_or_wrong_evidence_falls_back(self):
        for key, value in [('tree', 'different'), ('attempt', 1), ('head_sha', 'other'),
                           ('run_id', 7), ('base', 'main'), ('schema', 2), ('commit', 'invalid')]:
            with self.subTest(key=key):
                original = self.evidence[key]
                self.evidence[key] = value
                self.assertFalse(self.reuse())
                self.evidence[key] = original

    def test_unsuccessful_or_wrong_run_never_downloaded(self):
        for key, value in [('event', 'push'), ('conclusion', 'failure'),
                           ('status', 'in_progress'), ('head_sha', 'other')]:
            with self.subTest(key=key):
                original = self.run[key]
                self.run[key] = value
                self.assertFalse(self.reuse())
                self.assertFalse(self.downloads)
                self.run[key] = original

    def test_unrelated_or_unmerged_pr_rejected(self):
        for key, value in [('merge_commit_sha', 'other'), ('merged_at', None),
                           ('base', dict(ref='main'))]:
            with self.subTest(key=key):
                original = self.pr[key]
                self.pr[key] = value
                self.assertFalse(self.reuse())
                self.pr[key] = original

    def test_expired_or_missing_artifact_falls_back(self):
        self.artifact['expired'] = True
        self.assertFalse(self.reuse())
        self.artifact['expired'] = False
        self.artifact['name'] = 'other'
        self.assertFalse(self.reuse())

    def test_git_commit_must_back_up_evidence(self):
        self.tested['tree']['sha'] = 'different'
        self.assertFalse(self.reuse())
        self.tested['tree']['sha'] = 'tree'
        self.tested['parents'] = [dict(sha='unrelated')]
        self.assertFalse(self.reuse())

    def test_api_failure_runs_full_checks(self):
        with tempfile.TemporaryDirectory() as temp:
            output = Path(temp) / 'output'
            with patch.dict(os.environ, GITHUB_OUTPUT=str(output), GITHUB_REPOSITORY='anchor-dra/anchor'), \
                    patch('sys.argv', ['pr-validation.py', 'reuse']), \
                    patch.object(validation, 'reusable', side_effect=subprocess.CalledProcessError(1, 'gh')):
                validation.main()
            self.assertEqual(output.read_text(), 'reuse=false\n')

    def test_malformed_evidence_falls_back(self):
        with tempfile.TemporaryDirectory() as temp:
            output = Path(temp) / 'output'
            with patch.dict(os.environ, GITHUB_OUTPUT=str(output), GITHUB_REPOSITORY='anchor-dra/anchor'), \
                    patch('sys.argv', ['pr-validation.py', 'reuse']), \
                    patch.object(validation, 'reusable', side_effect=TypeError('Malformed evidence')):
                validation.main()
            self.assertEqual(output.read_text(), 'reuse=false\n')

    def test_record_captures_checkout_tree_and_run_attempt(self):
        with tempfile.TemporaryDirectory() as temp:
            event = Path(temp) / 'event.json'
            event.write_text(json.dumps(dict(pull_request=self.pr)))
            original = os.getcwd()
            try:
                os.chdir(temp)
                with patch.dict(os.environ, GITHUB_EVENT_PATH=str(event), GITHUB_RUN_ID='42', GITHUB_RUN_ATTEMPT='2'), \
                        patch.object(validation, 'command', side_effect=['tree', self.commit]):
                    validation.record()
                self.assertEqual(json.loads(Path('.work/pr-validation.json').read_text()), self.evidence)
            finally:
                os.chdir(original)


if __name__ == '__main__':
    unittest.main()
