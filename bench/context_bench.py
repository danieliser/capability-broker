"""Context cost of MCP tools per chat: no servers vs every broker connector preloaded vs every connector on demand.
Measures the input tokens a trivial prompt costs in Claude Code (Haiku 5.5) and Codex (GPT-6 Luna, low)."""
import json, os, subprocess, tempfile, sys
TOK = os.environ.get('BROKER_TOKEN') or open(os.path.expanduser('~/.brokerd/clients/claude-code.token')).read().strip()
BASE = 'http://127.0.0.1:8890/mcp?prefix=none'
SETUPS = {'none': None, 'preloaded': BASE, 'on-demand': BASE + '&ondemand=all'}
PROMPT = 'Reply with the single word OK.'
WS = os.environ.get('BENCH_DIR', os.getcwd())
# An isolated CODEX_HOME (auth.json only) keeps personal skills, apps and memories out of the measurement.
CODEX_HOME = os.environ.get('BENCH_CODEX_HOME', os.path.expanduser('~/.codex'))

def claude(url):
    cfg = {'mcpServers': {}}
    if url:
        cfg['mcpServers']['broker'] = {'type': 'http', 'url': url, 'headers': {'Authorization': 'Bearer ' + TOK}}
    with tempfile.NamedTemporaryFile('w', suffix='.json', dir=WS, delete=False) as f:
        json.dump(cfg, f)
    try:
        out = subprocess.run(['claude', '-p', PROMPT, '--model', 'claude-haiku-5-5', '--strict-mcp-config', '--mcp-config', f.name,
                              '--output-format', 'json'], capture_output=True, text=True, stdin=subprocess.DEVNULL, timeout=240, cwd=WS).stdout
    finally:
        os.remove(f.name)
    u = json.loads(out)['usage']
    return u.get('input_tokens', 0) + u.get('cache_creation_input_tokens', 0) + u.get('cache_read_input_tokens', 0)

def codex(url):
    args = ['codex', 'exec', '--json', '--skip-git-repo-check', '--ignore-user-config', '--ignore-rules', '--ephemeral', '-s', 'read-only',
            '--model', 'gpt-6-luna', '-c', 'model_reasoning_effort="low"', '--disable', 'apps', '--disable', 'plugins',
            '--disable', 'memories', '--disable', 'hooks', '-c', 'skills.include_instructions=false', '-c', 'skills.bundled.enabled=false',
            '-c', 'project_doc_max_bytes=0']
    if url:
        args += ['-c', f'mcp_servers.broker.url={json.dumps(url)}', '-c', 'mcp_servers.broker.bearer_token_env_var="BROKER_TOK"']
    out = subprocess.run(args + ['--', PROMPT], capture_output=True, text=True, stdin=subprocess.DEVNULL, timeout=240, cwd=WS,
                         env={**os.environ, 'BROKER_TOK': TOK, 'CODEX_HOME': CODEX_HOME}).stdout
    tokens = 0
    for line in out.splitlines():
        try: e = json.loads(line)
        except ValueError: continue
        if e.get('type') == 'turn.completed':
            tokens = e['usage'].get('input_tokens', 0)
    return tokens

rows = []
for client, fn in (('claude-code', claude), ('codex', codex)):
    for setup, url in SETUPS.items():
        vals = [fn(url) for _ in range(2)]
        rows.append({'client': client, 'setup': setup, 'input_tokens': vals})
        print(client, setup, vals, flush=True)
json.dump(rows, open(os.path.join(WS, 'context-results.json'), 'w'), indent=1)
