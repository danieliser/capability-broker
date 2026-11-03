"""Resident memory of every MCP server brokerd runs, against one copy per app that had it configured
(read from ~/.brokerd/clients/adopted.json and the install backups). Run while brokerd is up."""
import json, os, subprocess
H = os.path.expanduser('~')
procs = []
for line in subprocess.run(['ps', '-axo', 'pid=,ppid=,rss=,command='], capture_output=True, text=True).stdout.splitlines():
    f = line.split(None, 3)
    if len(f) == 4:
        procs.append((int(f[0]), int(f[1]), int(f[2]), f[3]))
root = next(p for p, _, _, c in procs if c.split()[0].endswith('/brokerd'))
kids = {}
for p, pp, r, c in procs:
    kids.setdefault(pp, []).append((p, r, c))
def tree_rss(p):
    return sum(r + tree_rss(k) for k, r, _ in kids.get(p, []))
import yaml  # pip install pyyaml
specs = (yaml.safe_load(open(H + '/.brokerd/connectors.yaml')) or {}).get('connectors', {})
def connector_of(cmd):
    for name, sp in specs.items():
        if f'/runtimes/{name}/' in cmd:
            return name
    for name, sp in specs.items():
        args = [a for a in (sp or {}).get('args', []) if a not in ('-y', 'run', 'mcp')]
        if sp and sp.get('command') and args and all(a in cmd for a in args):
            return name
        if sp and sp.get('command') and not args and os.path.basename(sp['command']) in cmd:
            return name
    return None
# How many apps ran their own copy: recorded by `brokerd client adopt` in adopted.json; for servers configured
# before adopt existed, pass --copies name=N (e.g. --copies clickhouse=4 memory=5).
import sys
counts = {}
adopted = json.load(open(H + '/.brokerd/clients/adopted.json')) if os.path.exists(H + '/.brokerd/clients/adopted.json') else {}
for orig, rec in adopted.items():
    name = rec.get('connector') or orig.lower()
    counts[name] = counts.get(name, 0) + len(rec['entries'])
if '--copies' in sys.argv:
    for kv in sys.argv[sys.argv.index('--copies') + 1:]:
        k, v = kv.split('=')
        counts[k] = int(v)
before = after = 0
print(f'{"child process":60} {"MB":>5} {"apps":>4}')
for k, r, c in kids.get(root, []):
    mb = round((r + tree_rss(k)) / 1024)
    name = connector_of(c)
    n = counts.get(name, 1)
    before, after = before + mb * n, after + mb
    print(f'{(name or c[:40]):60} {mb:5d} {n:4d}')
broker = round(next(r for p, _, r, _ in procs if p == root) / 1024)
print(f'one copy per app: {before} MB; brokerd: {after} MB of servers + {broker} MB broker')
