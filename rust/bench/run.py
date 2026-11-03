#!/usr/bin/env python3
"""Isolated benchmark. Builds only local artifacts; never uses the installed broker."""
import json, os, pathlib, signal, socket, sqlite3, subprocess, time
ROOT = pathlib.Path(__file__).resolve().parents[2]
OUT = ROOT / 'rust/.bench'
OUT.mkdir(exist_ok=True)
def run(args, **kw):
    p = subprocess.run([str(x) for x in args], capture_output=True, text=True, **kw)
    if p.returncode: raise RuntimeError(p.stderr)
    return p
def rss(pid):
    return int(run(['ps', '-o', 'rss=', '-p', str(pid)]).stdout.strip())
def stop(p):
    if p.poll() is None:
        p.send_signal(signal.SIGINT)
        try: p.wait(timeout=12)
        except subprocess.TimeoutExpired: p.kill(); p.wait()
def main():
    for port in [8990, 8991]:
        with socket.socket() as probe:
            probe.bind(('127.0.0.1', port))  # Refuse to contact an existing listener.
    env = dict(os.environ, CGO_ENABLED='0')
    run(['go','build','-trimpath','-ldflags=-s -w','-o',OUT/'brokerd-go','./cmd/brokerd'],cwd=ROOT,env=env)
    run(['go','build','-o',OUT/'mockmcp','./cmd/mockmcp'],cwd=ROOT)
    run(['go','build','-o',OUT/'client','rust/bench/client.go'],cwd=ROOT)
    data = {}
    for kind, port, binary in [('go',8990,OUT/'brokerd-go'),('rust',8991,ROOT/'rust/target/release/brokerd-rust')]:
        cold=[]; measurements=[]
        for trial in range(3):
            root=OUT/f'{kind}-{time.time_ns()}'
            root.mkdir(mode=0o700);(root/'home').mkdir()
            (root/'connectors.yaml').write_text(f'connectors:\n  mock:\n    command: {OUT / "mockmcp"}\n    pin: false\n')
            token=root/'owner.token'
            if kind=='go': run([OUT/'client','seed',root])
            else: token.write_text('comparison-fixture-owner-token'); token.chmod(0o600)
            isolated=dict(os.environ, HOME=str(root/'home'), BROKERD_DATA_DIR=str(root),BROKERD_HOST='127.0.0.1',BROKERD_PORT=str(port))
            args=[binary] if kind=='go' else [binary,root,token,str(port)]
            with (root/'daemon.log').open('w') as log:
                started=time.perf_counter_ns()
                p=subprocess.Popen([str(x) for x in args],cwd=ROOT,env=isolated,stdout=log,stderr=log)
                try:
                    deadline=time.monotonic()+30
                    while True:
                        ready=subprocess.run([str(OUT/'client'),'ready',f'http://127.0.0.1:{port}/mcp/mock',str(token)],capture_output=True,text=True)
                        if p.poll() is not None: raise RuntimeError('benchmark daemon exited: '+log.name)
                        if ready.returncode==0: break
                        if p.poll() is not None or time.monotonic()>deadline: raise RuntimeError(log.name+': '+ready.stderr)
                        time.sleep(.005)
                    cold.append((time.perf_counter_ns()-started)/1e6)
                    idle=rss(p.pid)
                    # Only one benchmark per daemon kind; cold-start trials 2/3 do no calls.
                    if trial==0:
                        b=subprocess.Popen([str(OUT/'client'),'bench',f'http://127.0.0.1:{port}/mcp/mock',str(token)],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
                        assert b.stderr.readline().strip()=='RSS_READY'
                        after=rss(p.pid)
                        stdout,stderr=b.communicate(timeout=90)
                        if b.returncode: raise RuntimeError(stderr)
                        measurements=json.loads(stdout)
                        smoke=run([OUT/'client','smoke',f'http://127.0.0.1:{port}/mcp/mock',token,OUT/'mockmcp'])
                        (OUT/f'{kind}-smoke.json').write_text(smoke.stdout)
                        if kind=='rust':
                            aggregate=run([OUT/'client','smoke',f'http://127.0.0.1:{port}/mcp',token,OUT/'mockmcp'])
                            (OUT/'rust-aggregate-smoke.json').write_text(aggregate.stdout)
                        db=sqlite3.connect(root/'broker.db')
                        receipts=dict(db.execute('select phase,count(*) from receipts group by phase'))
                        expected=6010 if kind=='rust' else 6005
                        assert receipts=={'intent':expected,'outcome':expected},receipts
                        db.close()
                        data[kind]={'runs':measurements,'idle_rss_kib':idle,'rss_after_2000_kib':after,'receipts':receipts,'binary_bytes':binary.stat().st_size,'fixture_dir':str(root)}
                finally: stop(p)
        data[kind]['cold_start_ms']=cold
    (OUT/'results.json').write_text(json.dumps(data,indent=2)+'\n')
    print(json.dumps(data,indent=2))
if __name__=='__main__': main()
