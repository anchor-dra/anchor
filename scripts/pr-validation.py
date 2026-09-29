#!/usr/bin/env python3
"""Record PR validation and reuse it only for an identical merged tree."""
import io
import json
import os
from pathlib import Path
import subprocess
import sys
from urllib.parse import urlencode
import zipfile

ARTIFACT = 'pr-validation-evidence'


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def api(path):
    return json.loads(command('gh', 'api', path))


def record():
    event = json.loads(Path(os.environ['GITHUB_EVENT_PATH']).read_text())
    evidence = dict(schema=1, tree=command('git', 'rev-parse', 'HEAD^{tree}'),
                    commit=command('git', 'rev-parse', 'HEAD'),
                    head_sha=event['pull_request']['head']['sha'],
                    base=event['pull_request']['base']['ref'],
                    run_id=int(os.environ['GITHUB_RUN_ID']),
                    attempt=int(os.environ['GITHUB_RUN_ATTEMPT']))
    Path('.work').mkdir(exist_ok=True)
    Path('.work/pr-validation.json').write_text(json.dumps(evidence) + '\n')


def download(artifact):
    raw = subprocess.check_output(['gh', 'api', artifact['archive_download_url']])
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        info = archive.getinfo('pr-validation.json')
        if info.file_size > 16384:
            raise ValueError('Oversized validation evidence')
        return json.loads(archive.read(info))


def matches(evidence, run, tree, tested_commit):
    return (evidence.get('schema') == 1 and evidence.get('base') == 'develop'
            and evidence.get('run_id') == run['id']
            and evidence.get('attempt') == run['run_attempt']
            and evidence.get('head_sha') == run['head_sha']
            and evidence.get('tree') == tree
            and tested_commit['tree']['sha'] == tree
            and (evidence.get('commit') == run['head_sha']
                 or run['head_sha'] in [p['sha'] for p in tested_commit['parents']]))


def reusable(repository):
    sha = command('git', 'rev-parse', 'HEAD')
    tree = command('git', 'rev-parse', 'HEAD^{tree}')
    prefix = f'repos/{repository}'
    prs = api(f'{prefix}/commits/{sha}/pulls')
    for pr in prs:
        if (not pr.get('merged_at') or pr['base']['ref'] != 'develop'
                or pr['merge_commit_sha'] != sha):
            continue
        query = urlencode(dict(event='pull_request', head_sha=pr['head']['sha'],
                               status='success', per_page=100))
        runs = api(f'{prefix}/actions/workflows/ci.yaml/runs?{query}')['workflow_runs']
        for run in runs:
            if (run['event'] != 'pull_request' or run['status'] != 'completed'
                    or run['conclusion'] != 'success' or run['head_sha'] != pr['head']['sha']):
                continue
            artifacts = api(f'{prefix}/actions/runs/{run["id"]}/artifacts?per_page=100')['artifacts']
            for artifact in artifacts:
                if artifact['name'] != ARTIFACT or artifact['expired']:
                    continue
                evidence = download(artifact)
                commit = evidence.get('commit', '')
                if len(commit) != 40 or any(c not in '0123456789abcdef' for c in commit):
                    continue
                tested = api(f'{prefix}/git/commits/{commit}')
                if matches(evidence, run, tree, tested):
                    print(f'Reusing successful PR #{pr["number"]} validation: {run["html_url"]}')
                    return True
    return False


def main():
    if sys.argv[1] == 'record':
        record()
        return
    reuse = False
    try:
        reuse = reusable(os.environ['GITHUB_REPOSITORY'])
    except (subprocess.CalledProcessError, ValueError, KeyError, OSError,
            zipfile.BadZipFile, TypeError, AttributeError) as error:
        print(f'Validation evidence unavailable: {error}')
    if not reuse:
        print('Running full validation: no successful PR evidence for this exact tree')
    with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
        output.write(f'reuse={str(reuse).lower()}\n')


if __name__ == '__main__':
    main()
