#!/usr/bin/env python3
"""300 loaded definitions: 50 independent Go mockmcp connectors, 6 tools each."""
import json,os,pathlib,socket,subprocess,time
from run import ROOT,OUT,run,rss,stop
results={}
for kind,port,binary in [('go',8990,OUT/'brokerd-go'),('rust',8991,ROOT/'rust/target/release/brokerd-rust')]:
    with socket.socket() as probe:probe.bind(('127.0.0.1',port))
    root=OUT/f'shape-{kind}-{time.time_ns()}';root.mkdir(mode=0o700);(root/'home').mkdir()
    (root/'connectors.yaml').write_text('connectors:\n'+''.join(f'  mock{i:02d}:\n    command: {OUT / "mockmcp"}\n    pin: false\n' for i in range(50)))
    token=root/'owner.token'
    if kind=='go':run([OUT/'client','seed',root])
    else:token.write_text('comparison-fixture-owner-token');token.chmod(0o600)
    env=dict(os.environ,HOME=str(root/'home'),BROKERD_DATA_DIR=str(root),BROKERD_PORT=str(port),BROKERD_HOST='127.0.0.1')
    args=[binary] if kind=='go' else [binary,root,token,str(port)]
    with (root/'daemon.log').open('w') as log:
        start=time.monotonic();p=subprocess.Popen([str(a) for a in args],cwd=ROOT,env=env,stdout=log,stderr=log)
        try:
            end=time.monotonic()+90
            while time.monotonic()<end:
                ready=subprocess.run([str(OUT/'client'),'ready',f'http://127.0.0.1:{port}/mcp',str(token)],capture_output=True,text=True)
                if ready.returncode==0 and json.loads(ready.stdout)['tools']==300:break
                if p.poll() is not None:raise RuntimeError(log.name)
                time.sleep(.1)
            else:raise RuntimeError('300 tools did not become ready: '+log.name)
            elapsed=(time.monotonic()-start)*1000
            results[kind]={'tools':300,'connectors':50,'ready_ms':elapsed,'rss_kib':rss(p.pid),'fixture':str(root)}
        finally:stop(p)
(OUT/'shape.json').write_text(json.dumps(results,indent=2)+'\n');print(json.dumps(results,indent=2))
