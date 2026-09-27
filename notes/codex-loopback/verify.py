#!/usr/bin/env python3
"""Synthetic Codex client→router→Exchange probe; never uses real credentials."""

import argparse
import http.client
import json
import os
from pathlib import Path
import select
import shutil
import socket
import subprocess
import sys
import tempfile
import time

TOKEN = 'RR_SYNTHETIC.eyJleHAiOjQxMDI0NDQ4MDB9.sig'
REQUEST = json.dumps({'model': 'claude-opus-5', 'max_tokens': 16,
    'messages': [{'role': 'user', 'content': 'RR_SYNTHETIC_SAFE_PROMPT'}]})
SCENARIOS = {
    'on': {'transient': (429, '2'), 'quota': (429, None),
           'unknown': (429, None), 'no-code-429': (429, None),
           'mapped-503': (502, None)},
    'off': {'transient': (429, '2'), 'quota': (429, None),
            'unknown': (429, None), 'no-code-429': (429, None),
            'mapped-503': (429, None)},
}
ERRORS = {
    'on': {
        'transient': ('rate_limit_error', 'The upstream service reported a rate limit.'),
        'quota': ('rate_limit_error', 'The upstream service reported a usage or spending limit.'),
        'unknown': ('rate_limit_error', 'The upstream service reported a limit.'),
        'no-code-429': ('rate_limit_error', 'The upstream service reported a limit.'),
        'mapped-503': ('api_error', 'privacy: upstream response rejected'),
    },
    'off': {
        'transient': ('rate_limit_error', 'The upstream service reported a rate limit.'),
        'quota': ('rate_limit_error', 'The upstream service reported a usage or spending limit.'),
        'unknown': ('rate_limit_error', 'The upstream service reported a limit.'),
        'no-code-429': ('rate_limit_error', 'The upstream service reported a limit.'),
        'mapped-503': ('api_error', 'The upstream service failed.'),
    },
}


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def call(port, path, body=None, headers=None):
    conn = http.client.HTTPConnection('127.0.0.1', port, timeout=15)
    conn.request('GET' if body is None else 'POST', path, body,
                 headers or {'Content-Type': 'application/json'})
    response = conn.getresponse()
    result = response.status, dict(response.getheaders()), response.read(16384)
    conn.close()
    return result


def ready(process, port, log):
    deadline = time.monotonic() + 8
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise AssertionError(f'router exited with {process.returncode}: {log.read_text()}')
        try:
            if call(port, '/healthz')[0] == 200:
                return
        except (OSError, TimeoutError):
            pass
        time.sleep(0.05)
    raise AssertionError(f'router did not start: {log.read_text()}')


def stop(process):
    process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


def run(binary, fixtures):
    info = subprocess.run(['go', 'version', '-m', str(binary)],
                          check=True, capture_output=True, text=True).stdout
    if not any(line.strip() == 'build\t-tags=router_codex_loopback'
               for line in info.splitlines()):
        raise AssertionError('refusing to run a binary without the router_codex_loopback build tag')
    with tempfile.TemporaryDirectory(prefix='router-codex-loopback-') as temp:
        temp = Path(temp)
        with (temp / 'stub.log').open('w') as stub_log:
            stub = subprocess.Popen([sys.executable, str(fixtures / 'stub.py')],
                stdout=subprocess.PIPE, stderr=stub_log, text=True,
                env={'HOME': str(temp), 'PATH': os.defpath,
                     'PYTHONNOUSERSITE': '1', 'PYTHONDONTWRITEBYTECODE': '1'})
            try:
                if not select.select([stub.stdout], [], [], 5)[0]:
                    raise AssertionError('stub did not print a loopback port')
                stub_port = int(stub.stdout.readline().strip())
                if call(stub_port, '/_state')[0] != 200:
                    raise AssertionError('stub is not responding')
                for privacy, expected in SCENARIOS.items():
                    home = temp / privacy
                    router_home = home / 'router'
                    router_home.mkdir(parents=True)
                    (home / 'home').mkdir()
                    (home / 'codex-home').mkdir()
                    providers = router_home / 'providers.json'
                    providers.write_text(json.dumps({'providers': [{'name': 'codex',
                        'type': 'codex', 'base_url': 'https://chatgpt.com/backend-api/codex'}],
                        'models': [{'provider': 'codex', 'model': 'gpt-5'}]}))
                    (router_home / 'providers.json.active-profile').write_text('"default"\n')
                    (router_home / 'providers.json.profiles').mkdir()
                    (router_home / 'providers.json.profiles/default.json').write_text(json.dumps({
                        'family_routes': {'opus': {'default': {'mode': 'model',
                            'model': 'codex/gpt-5'}}}, 'routes': {}, 'model_pools': {}}))
                    auth_path = router_home / 'codex-auth.json'
                    auth_path.write_text(json.dumps({
                        'auth_mode': 'chatgpt', 'tokens': {'id_token': 'RR_SYNTHETIC_ID',
                        'access_token': TOKEN, 'refresh_token': 'RR_SYNTHETIC_REFRESH',
                        'account_id': 'RR_SYNTHETIC_ACCOUNT'}}))
                    auth_path.chmod(0o600)
                    shutil.copyfile(fixtures / f'privacy-{privacy}.json',
                                    router_home / 'privacy-profiles.json')
                    api, ui = free_port(), free_port()
                    env = {'PATH': os.defpath, 'HOME': str(home / 'home'),
                        'CODEX_HOME': str(home / 'codex-home'),
                        'ROUTER_HOME': str(router_home),
                        'ROUTER_ENV_FILE': str(router_home / 'missing-env'),
                        'ROUTER_SLOT': '',
                        'ROUTER_PROVIDERS_FILE': str(providers),
                        'ROUTER_CODEX_AUTH_FILE': str(auth_path),
                        'ROUTER_UI_HISTORY_FILE': str(router_home / 'history.jsonl'),
                        'ROUTER_STATE_FILE': str(router_home / 'state.json'),
                        'ROUTER_ANTHROPIC_LIMITS_FILE': str(router_home / 'limits.json'),
                        'ROUTER_ACTIVE_SLOT_FILE': str(router_home / 'active-slot'),
                        'ROUTER_CODEX_TEST_STUB_URL': f'http://127.0.0.1:{stub_port}',
                        'ROUTER_UPSTREAM_URL': f'http://127.0.0.1:{stub_port}',
                        'ROUTER_LISTEN': f'127.0.0.1:{api}',
                        'ROUTER_UI_LISTEN': f'127.0.0.1:{ui}',
                        'ROUTER_LOCAL_FAILOVER': '0',
                        'HTTPS_PROXY': 'http://127.0.0.1:1',
                        'HTTP_PROXY': 'http://127.0.0.1:1'}
                    log = home / 'router.log'
                    with log.open('w') as output:
                        router = subprocess.Popen([str(binary), 'serve'],
                            env=env, stdout=output, stderr=subprocess.STDOUT)
                        try:
                            ready(router, api, log)
                            for mode, wanted in expected.items():
                                if call(stub_port, '/_control', json.dumps({'mode': mode}))[0] != 200:
                                    raise AssertionError(f'stub rejected {mode}')
                                status, headers, content = call(api, '/v1/messages', REQUEST,
                                    {'Content-Type': 'application/json',
                                     'X-Api-Key': 'RR_SYNTHETIC_CLIENT_KEY',
                                     'Anthropic-Version': '2023-06-01'})
                                state = json.loads(call(stub_port, '/_state')[2])
                                if (status, headers.get('Retry-After')) != wanted:
                                    raise AssertionError(f'{privacy}/{mode}: unexpected HTTP status or Retry-After')
                                if state['calls'] != 1 or state['paths'] != ['/backend-api/codex/responses']:
                                    raise AssertionError(f'{privacy}/{mode}: wrong Codex route: {state}')
                                if (b'RR_SYNTHETIC_UPSTREAM_ERROR_CANARY' in content or
                                        b'RR_SYNTHETIC.' in content or
                                        'RR_SYNTHETIC_UPSTREAM_ERROR_CANARY' in str(headers) or
                                        TOKEN in str(headers)):
                                    raise AssertionError(f'{privacy}/{mode}: unsafe error response')
                                if (headers.get('Content-Type', '').split(';', 1)[0].strip().lower() != 'application/json' or
                                        headers.get('Cache-Control') != 'no-store'):
                                    raise AssertionError(f'{privacy}/{mode}: missing JSON/no-store response headers')
                                try:
                                    envelope = json.loads(content)
                                except (UnicodeError, ValueError) as exc:
                                    raise AssertionError(f'{privacy}/{mode}: invalid public error JSON') from exc
                                kind, message = ERRORS[privacy][mode]
                                if (not isinstance(envelope, dict) or set(envelope) != {'type', 'error'} or
                                        envelope['type'] != 'error' or not isinstance(envelope['error'], dict) or
                                        envelope['error'] != {'type': kind, 'message': message}):
                                    raise AssertionError(f'{privacy}/{mode}: unexpected public error envelope')
                                print(f'{privacy}/{mode}: HTTP {status}, Codex Exchange once')
                        finally:
                            stop(router)
            finally:
                stop(stub)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--fixture-dir', type=Path, required=True)
    args = parser.parse_args()
    run(args.binary.resolve(), args.fixture_dir.resolve())
