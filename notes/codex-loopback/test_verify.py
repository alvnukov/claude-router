"""No-network regression checks for the synthetic Codex verifier."""

import contextlib
import io
import json
import os
from pathlib import Path
import tempfile
import types
import unittest
from unittest import mock

import verify


EXPECTED = {
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


class FakeProcess:
    def __init__(self):
        self.stdout = io.StringIO('12345\n')
        self.returncode = None

    def poll(self):
        return self.returncode

    def terminate(self):
        self.returncode = 0

    def wait(self, timeout=None):
        return self.returncode


class VerifyTest(unittest.TestCase):
    def invoke(self, error=None, response_override=None, stop_at_router=False, captures=None):
        environments = []
        stub_environment = []
        if captures is not None:
            captures['router'], captures['stub'] = environments, stub_environment
        selected = {'mode': 'transient'}

        def popen(command, **kwargs):
            if command[-1] == 'serve':
                environments.append(kwargs['env'])
                if stop_at_router:
                    raise RuntimeError('router environment captured')
            else:
                stub_environment.append(kwargs['env'])
            return FakeProcess()

        def call(port, path, body=None, headers=None):
            if path == '/healthz' or path == '/_control':
                if path == '/_control':
                    selected['mode'] = json.loads(body)['mode']
                return 200, {}, b'{}'
            if path == '/_state':
                count = 1 if environments and not stop_at_router else 0
                return 200, {}, json.dumps({'calls': count,
                    'paths': ['/backend-api/codex/responses'] if count else []}).encode()
            privacy = Path(environments[-1]['ROUTER_HOME']).parent.name
            status, retry = verify.SCENARIOS[privacy][selected['mode']]
            response_headers = {'Content-Type': 'application/json', 'Cache-Control': 'no-store'}
            if retry is not None:
                response_headers['Retry-After'] = retry
            if response_override:
                response_headers.update(response_override)
            kind, message = EXPECTED[privacy][selected['mode']]
            payload = error if error is not None else {
                'type': 'error', 'error': {'type': kind, 'message': message}}
            return status, response_headers, json.dumps(payload).encode()

        with tempfile.TemporaryDirectory() as fixtures:
            fixtures = Path(fixtures)
            for privacy in ('on', 'off'):
                (fixtures / f'privacy-{privacy}.json').write_text('{}')
            with (mock.patch.object(verify.subprocess, 'run',
                    return_value=types.SimpleNamespace(stdout='build\t-tags=router_codex_loopback\n')),
                  mock.patch.object(verify.subprocess, 'Popen', side_effect=popen),
                  mock.patch.object(verify.select, 'select', return_value=([FakeProcess().stdout], [], [])),
                  mock.patch.object(verify, 'free_port', return_value=12346),
                  mock.patch.object(verify, 'call', side_effect=call),
                  contextlib.redirect_stdout(io.StringIO())):
                verify.run(Path('/synthetic/tagged-binary'), fixtures)
        return environments, stub_environment

    def test_router_and_stub_do_not_inherit_live_paths_or_python_code(self):
        with mock.patch.dict(os.environ, {
            'ROUTER_UI_HISTORY_FILE': '/synthetic/poison/history.jsonl',
            'ROUTER_STATE_FILE': '/synthetic/poison/state.json',
            'ROUTER_ANTHROPIC_LIMITS_FILE': '/synthetic/poison/limits.json',
            'ROUTER_ACTIVE_SLOT_FILE': '/synthetic/poison/active-slot',
            'ROUTER_MODELS_STATE': '/synthetic/poison/models.json',
            'PYTHONPATH': '/synthetic/poison/module',
            'ALL_PROXY': 'http://synthetic.invalid:12345',
        }):
            captured = {}
            with self.assertRaisesRegex(RuntimeError, 'router environment captured'):
                self.invoke(stop_at_router=True, captures=captured)
            router, stub = captured['router'][0], captured['stub'][0]
            home = Path(router['ROUTER_HOME'])
            for key in ('ROUTER_UI_HISTORY_FILE', 'ROUTER_STATE_FILE',
                        'ROUTER_ANTHROPIC_LIMITS_FILE', 'ROUTER_ACTIVE_SLOT_FILE'):
                self.assertEqual(Path(router[key]).parent, home)
            self.assertNotIn('ROUTER_MODELS_STATE', router)
            for environment in (router, stub):
                self.assertNotIn('PYTHONPATH', environment)
                self.assertNotIn('ALL_PROXY', environment)
                self.assertNotIn('/synthetic/poison', repr(environment))

    def test_accepts_fixed_error_matrix(self):
        environments, stub_environments = self.invoke()
        self.assertEqual(len(environments), 2)
        self.assertEqual(len(stub_environments), 1)

    def test_rejects_missing_or_wrong_public_error_envelope(self):
        for payload in ({}, {'type': 'error', 'error': {'type': 'api_error',
                       'message': 'The upstream service reported a rate limit.'}},
                        {'type': 'error', 'error': {'type': 'rate_limit_error', 'message': ''}},
                        {'type': 'error', 'error': {'type': 'rate_limit_error',
                         'message': 'unverified success'}}):
            with self.subTest(payload=payload):
                with self.assertRaises(AssertionError):
                    self.invoke(error=payload)

    def test_rejects_missing_no_store_or_json_content_type(self):
        for header in ({'Cache-Control': None}, {'Cache-Control': 'public'},
                       {'Content-Type': 'text/plain'}):
            with self.subTest(header=header):
                with self.assertRaises(AssertionError):
                    self.invoke(response_override=header)


if __name__ == '__main__':
    unittest.main()
