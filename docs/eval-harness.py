#!/usr/bin/env python3
"""Broker tool-passthrough eval: low-effort models, three client setups, six tasks, graded against live data
and the broker's own receipts. Usage: eval.py [--reps N] [--models haiku,luna] [--setups ...] [--tasks ...] [--jobs N]"""
import argparse, concurrent.futures as cf, json, os, subprocess, tempfile, time, uuid, urllib.request, base64, sys

HOME = os.path.expanduser('~')
WS = os.path.dirname(os.path.abspath(__file__))
BROKER = 'http://127.0.0.1:8890'
TOKEN = open(f'{HOME}/.brokerd/clients/eval.token').read().strip()
CONNECTORS = ['clickhouse', 'memory', 'wp-mcp-router']

def ch(sql):
    pw = json.load(open(f'{HOME}/.brokerd/secrets.json'))['clickhouse_password']
    r = urllib.request.Request('http://localhost:18123/', data=(sql + ' FORMAT TSV').encode(),
                               headers={'Authorization': 'Basic ' + base64.b64encode(f'broker_reader:{pw}'.encode()).decode()})
    return urllib.request.urlopen(r, timeout=30).read().decode().strip()

LIVE = {'orders': ch('SELECT count() FROM companykit.wp_edd_orders'),
        'charges': ch('SELECT count() FROM companykit.stripe_charges')}

def num_in(text, n):
    return n in text.replace(',', '').replace(' ', '').replace(' ', '')

TASKS = {
    'orders': ('How many EDD orders are in the companykit warehouse? Give the exact number.',
               lambda ans, r: num_in(ans, LIVE['orders'])),
    'stripe_table': ('Which table in the companykit ClickHouse database holds Stripe charges? Give its exact name.',
                     lambda ans, r: 'stripe_charges' in ans),
    'recall': ('What do you remember about the capability broker smoke test? Answer from memory, briefly.',
               lambda ans, r: any(k in ans.lower() for k in ('steward', 'namespace', 'safe to delete', 'brokerd'))),
    'store': ('Please save this to long-term memory for later: "Broker eval marker {rid} is green."',
              lambda ans, r: any(x['tool'] == 'store_memory' and x['state'] == 'completed' and r in x.get('args_summary', '') for x in RECEIPTS())),
    'sites': ('Which WordPress sites can you manage? List their site ids.',
              lambda ans, r: 'wppopupmaker' in ans and 'contentcontrolplugin' in ans),
    'compose': ('Find out how many Stripe charges are in the companykit warehouse, then save that number to long-term memory together with the marker {rid}.',
                lambda ans, r: any(x['tool'] == 'store_memory' and x['state'] == 'completed' and r in x.get('args_summary', '')
                                   and num_in(x.get('args_summary', ''), LIVE['charges']) for x in RECEIPTS())),
}

_receipts_cache = {'t': 0, 'v': []}
def RECEIPTS():
    if time.time() - _receipts_cache['t'] > 2:
        out = subprocess.run([f'{HOME}/.local/bin/brokerd', 'receipts', '--after', str(START_ID), '--limit', '1000'], capture_output=True, text=True).stdout
        _receipts_cache['v'] = json.loads(out)['receipts'] if out else []
        _receipts_cache['t'] = time.time()
    return _receipts_cache['v']

NOINST = os.environ.get('EVAL_NO_INSTRUCTIONS') == '1'
HINT = os.environ.get('EVAL_HINT') == '1'
CHARTER = os.environ.get('EVAL_CHARTER', '')
def _q(u):
    if NOINST: u = u + ('&' if '?' in u else '?') + 'instructions=none'
    if HINT: u = u + ('&' if '?' in u else '?') + 'hint=live'
    return u
SETUPS_RAW = {
    'servers': {**{c: f'{BROKER}/mcp/{c}' for c in CONNECTORS}, 'broker': f'{BROKER}/mcp?exclude=' + ','.join(CONNECTORS)},
    'single_prefixed': {'broker': f'{BROKER}/mcp'},
    'single_plain': {'broker': f'{BROKER}/mcp?prefix=none'},
}
SETUPS = {k: {n: _q(u) for n, u in v.items()} for k, v in SETUPS_RAW.items()}

def run_claude(setup, prompt, d):
    cfg = {'mcpServers': {n: {'type': 'http', 'url': u, 'headers': {'Authorization': f'Bearer {TOKEN}'}} for n, u in SETUPS[setup].items()}}
    p = os.path.join(d, 'mcp.json'); open(p, 'w').write(json.dumps(cfg)); os.chmod(p, 0o600)
    t = time.time()
    out = subprocess.run(['claude', '-p', '--model', 'claude-haiku-5-5', '--effort', 'low', '--strict-mcp-config', '--mcp-config', p,
                          '--setting-sources', '', '--tools', '', '--allowedTools', ','.join('mcp__' + n for n in SETUPS[setup]), '--output-format', 'stream-json', '--verbose', prompt],
                         capture_output=True, text=True, cwd=d, timeout=300, stdin=subprocess.DEVNULL).stdout
    calls, errors, answer, tokens = [], 0, '', 0
    for line in out.splitlines():
        try: e = json.loads(line)
        except Exception: continue
        if e.get('type') == 'assistant':
            for c in e['message'].get('content', []):
                if c.get('type') == 'tool_use': calls.append(c['name'])
        if e.get('type') == 'user':
            for c in e['message'].get('content', []) if isinstance(e['message'].get('content'), list) else []:
                if c.get('type') == 'tool_result' and (c.get('is_error') or 'BROKER_' in json.dumps(c.get('content', ''))): errors += 1
        if e.get('type') == 'result':
            answer = e.get('result') or ''
            u = e.get('usage', {}); tokens = u.get('input_tokens', 0) + u.get('cache_read_input_tokens', 0) + u.get('cache_creation_input_tokens', 0)
    return calls, errors, answer, tokens, time.time() - t, 0, out

def run_codex(setup, prompt, d):
    args = ['codex', 'exec', '--json', '--skip-git-repo-check', '--ignore-user-config', '--ignore-rules', '--ephemeral', '-s', 'read-only',
            '--model', 'gpt-6-luna', '-c', 'model_reasoning_effort="low"',
            '--disable', 'apps', '--disable', 'plugins', '--disable', 'memories', '--disable', 'hooks',
            '-c', 'skills.include_instructions=false', '-c', 'skills.bundled.enabled=false', '-c', 'project_doc_max_bytes=0']
    if CHARTER:
        args += ['-c', f'developer_instructions={json.dumps(CHARTER)}']
    for n, u in SETUPS[setup].items():
        args += ['-c', f'mcp_servers.{n}.url={json.dumps(u)}', '-c', f'mcp_servers.{n}.bearer_token_env_var="BROKER_TOK"',
                 '-c', f'mcp_servers.{n}.default_tools_approval_mode="approve"']
    args += ['--', prompt]
    t = time.time()
    out = subprocess.run(args, capture_output=True, text=True, cwd=d, timeout=300, stdin=subprocess.DEVNULL,
                         env={**os.environ, 'BROKER_TOK': TOKEN, 'CODEX_HOME': os.path.join(WS, 'codex-home')}).stdout
    calls, errors, answer, tokens, shell = [], 0, '', 0, 0
    for line in out.splitlines():
        try: e = json.loads(line)
        except Exception: continue
        it = e.get('item', {})
        if e.get('type') == 'item.completed':
            if it.get('type') == 'mcp_tool_call':
                calls.append(f"{it.get('server')}/{it.get('tool')}")
                if it.get('status') != 'completed' or 'BROKER_' in json.dumps(it.get('result') or it.get('error') or ''): errors += 1
            elif it.get('type') == 'command_execution': shell += 1
            elif it.get('type') == 'agent_message': answer = it.get('text', '')
        if e.get('type') == 'turn.completed':
            tokens += e.get('usage', {}).get('input_tokens', 0)
    return calls, errors, answer, tokens, time.time() - t, shell, out

def one(model, setup, task, rep):
    rid = 'ev' + uuid.uuid4().hex[:8]
    prompt = TASKS[task][0].format(rid=rid)
    d = tempfile.mkdtemp(prefix='beval-')
    try:
        calls, errors, answer, tokens, secs, shell, raw = (run_claude if model == 'haiku' else run_codex)(setup, prompt, d)
    except subprocess.TimeoutExpired:
        calls, errors, answer, tokens, secs, shell, raw = [], 0, 'TIMEOUT', 0, 300, 0, ''
    time.sleep(3 if task in ('store', 'compose') else 0)
    _receipts_cache['t'] = 0
    ok = bool(TASKS[task][1](answer, rid))
    rec = dict(model=model, setup=setup, task=task, rep=rep, rid=rid, ok=ok, tool_calls=len(calls), calls=calls, errors=errors,
               shell=shell, input_tokens=tokens, seconds=round(secs, 1), answer=answer[:600])
    open(os.path.join(WS, 'runs', f'{model}-{setup}-{task}-{rep}-{rid}.jsonl'), 'w').write(raw)
    return rec

if __name__ == '__main__':
    ap = argparse.ArgumentParser()
    ap.add_argument('--reps', type=int, default=3); ap.add_argument('--jobs', type=int, default=4)
    ap.add_argument('--models', default='haiku,luna'); ap.add_argument('--setups', default=','.join(SETUPS)); ap.add_argument('--tasks', default=','.join(TASKS))
    ap.add_argument('--out', default=os.path.join(WS, 'results.jsonl'))
    a = ap.parse_args()
    START_ID = max([0] + [x['id'] for x in json.loads(subprocess.run([f'{HOME}/.local/bin/brokerd', 'receipts', '--after', '0', '--limit', '1000'], capture_output=True, text=True).stdout)['receipts']])
    # receipts are paged; walk to the end
    while True:
        page = json.loads(subprocess.run([f'{HOME}/.local/bin/brokerd', 'receipts', '--after', str(START_ID), '--limit', '1000'], capture_output=True, text=True).stdout)['receipts']
        if not page: break
        START_ID = page[-1]['id']
    print('live:', LIVE, 'receipts after', START_ID, flush=True)
    jobs = [(m, s, t, r) for r in range(a.reps) for m in a.models.split(',') for s in a.setups.split(',') for t in a.tasks.split(',')]
    with open(a.out, 'a') as f, cf.ThreadPoolExecutor(a.jobs) as ex:
        for rec in ex.map(lambda j: one(*j), jobs):
            f.write(json.dumps(rec) + '\n'); f.flush()
            print(f"{rec['model']:5} {rec['setup']:16} {rec['task']:12} ok={rec['ok']!s:5} calls={rec['tool_calls']} err={rec['errors']} shell={rec['shell']} tok={rec['input_tokens']} {rec['seconds']}s", flush=True)
