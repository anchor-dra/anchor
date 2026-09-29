#!/usr/bin/env python3
"""Allow main to reuse validation only for a successfully published develop RC."""
import json
import os
import re
import subprocess
import sys
from urllib.parse import urlencode


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def api(path):
    return json.loads(command('gh', 'api', path))


def verify(repository):
    tree = command('git', 'rev-parse', 'HEAD^{tree}')
    tags = command('git', 'tag', '--list', 'v*-rc.*', '--sort=-v:refname').splitlines()
    for tag in tags:
        if not re.fullmatch(r'v\d+\.\d+\.\d+-rc\.\d+', tag):
            continue
        if command('git', 'rev-parse', tag + '^{tree}') != tree:
            continue
        sha = command('git', 'rev-parse', tag + '^{commit}')
        query = urlencode({'branch': 'develop', 'event': 'push', 'head_sha': sha,
                           'status': 'success', 'per_page': 100})
        runs = api(f'repos/{repository}/actions/workflows/ci.yaml/runs?{query}')
        if not any(run['head_sha'] == sha and run['head_branch'] == 'develop'
                   and run['event'] == 'push' and run['status'] == 'completed'
                   and run['conclusion'] == 'success'
                   for run in runs['workflow_runs']):
            raise ValueError(f'{tag} has no successful develop release workflow')
        release = api(f'repos/{repository}/releases/tags/{tag}')
        required = {f'anchor-{tag[1:]}.tgz', 'release.yaml', 'SHA256SUMS'}
        assets = {asset['name'] for asset in release['assets']
                  if asset['size'] > 0 and asset['state'] == 'uploaded'}
        if (release['tag_name'] != tag or release['draft'] or not release['prerelease']
                or not release['published_at'] or not required <= assets):
            raise ValueError(f'{tag} is not a complete published RC')
        # Check both OCI artifacts still exist, not only the GitHub release record.
        owner = repository.split('/')[0].lower()
        for artifact in (f'ghcr.io/{repository.lower()}:{tag[1:]}',
                         f'ghcr.io/{owner}/charts/anchor:{tag[1:]}'):
            command('docker', 'buildx', 'imagetools', 'inspect', artifact)
        return tag
    raise ValueError('main must exactly match a successfully published develop RC')


if __name__ == '__main__':
    try:
        print('Verified promotion of ' + verify(os.environ['GITHUB_REPOSITORY']))
    except (ValueError, KeyError, subprocess.CalledProcessError) as error:
        print(f'Promotion rejected: {error}', file=sys.stderr)
        sys.exit(1)
