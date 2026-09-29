import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('promotion', Path(__file__).parents[1] / 'verify-promotion.py')
promotion = importlib.util.module_from_spec(spec)
spec.loader.exec_module(promotion)


class PromotionTests(unittest.TestCase):
    def setUp(self):
        self.run = dict(head_sha='rc-commit', head_branch='develop', event='push',
                        status='completed', conclusion='success')
        self.release = dict(tag_name='v0.2.2-rc.1', draft=False, prerelease=True,
                            published_at='2026-09-29', assets=[
                                dict(name=name, size=100, state='uploaded') for name in
                                ['anchor-0.2.2-rc.1.tgz', 'release.yaml', 'SHA256SUMS']])
        self.rc_tree = 'same-tree'

    def command(self, *args):
        if args[:2] == ('git', 'tag'):
            return 'v0.2.2-rc.1'
        if args[:2] == ('git', 'rev-parse'):
            if args[2] == 'HEAD^{tree}':
                return 'same-tree'
            return 'rc-commit' if args[2].endswith('^{commit}') else self.rc_tree
        return 'OCI manifest'

    def api(self, path):
        return {'workflow_runs': [self.run]} if '/actions/' in path else self.release

    def verify(self):
        with patch.object(promotion, 'command', side_effect=self.command), \
                patch.object(promotion, 'api', side_effect=self.api):
            return promotion.verify('anchor-dra/anchor')

    def test_matching_successful_published_rc(self):
        self.assertEqual(self.verify(), 'v0.2.2-rc.1')

    def test_different_tree_rejected(self):
        self.rc_tree = 'changed-tree'
        with self.assertRaises(ValueError):
            self.verify()

    def test_failed_unrelated_or_unfinished_run_rejected(self):
        for field, value in [('conclusion', 'failure'), ('head_sha', 'other'),
                             ('event', 'pull_request'), ('head_branch', 'main'),
                             ('status', 'in_progress')]:
            with self.subTest(field=field):
                original = self.run[field]
                self.run[field] = value
                with self.assertRaises(ValueError):
                    self.verify()
                self.run[field] = original

    def test_incomplete_or_wrong_release_rejected(self):
        for field, value in [('draft', True), ('prerelease', False),
                             ('published_at', None), ('tag_name', 'v0.2.1-rc.1'),
                             ('assets', [])]:
            with self.subTest(field=field):
                original = self.release[field]
                self.release[field] = value
                with self.assertRaises(ValueError):
                    self.verify()
                self.release[field] = original

    def test_missing_registry_artifact_rejected(self):
        original = self.command
        def missing(*args):
            if args[0] == 'docker':
                raise subprocess.CalledProcessError(1, args)
            return original(*args)
        self.command = missing
        with self.assertRaises(subprocess.CalledProcessError):
            self.verify()


if __name__ == '__main__':
    unittest.main()
