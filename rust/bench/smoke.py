#!/usr/bin/env python3
"""Focused MVP behavior and authority-boundary smoke, entirely in rust/.bench."""
import json, os, pathlib, signal, sqlite3, subprocess, time, urllib.request, urllib.error
ROOT=pathlib.Path(__file__).resolve().parents[2]; OUT=ROOT/'rust/.bench'
def rpc(path,method,params=None,token='fixture-owner'):
    data=json.dumps(dict(jsonrpc='2.0',id=1,method=method,params=dict(params or {}, _meta={'io.modelcontextprotocol/protocolVersion':'2026-07-28','io.modelcontextprotocol/clientCapabilities':{},'io.modelcontextprotocol/clientInfo':{'name':'smoke','version':'1'}}))).encode()
    req=urllib.request.Request('http://127.0.0.1:8991'+path,data=data,headers={'Authorization':'Bearer '+token,'Content-Type':'application/json','Accept':'application/json, text/event-stream','MCP-Protocol-Version':'2026-07-28','Mcp-Method':method, **({'Mcp-Name':params['name']} if params and 'name' in params else {})})
    try:
        with urllib.request.urlopen(req,timeout=5) as r:
            assert not r.headers.get('Mcp-Session-Id')
            return r.status,json.load(r)
    except urllib.error.HTTPError as e: return e.code,e.read().decode()

def main():
    root=OUT/f'smoke-{time.time_ns()}';root.mkdir(mode=0o700);(root/'home').mkdir()
    (root/'owner.token').write_text('fixture-owner');(root/'secrets.json').write_text('{"TEST":"secret-resolved"}')
    wrapper=root/'upstream.py'
    wrapper.write_text('import os\nassert os.environ["CHECK"]=="prefix-secret-resolved-suffix"\nassert "BROKERD_TEST_LEAK" not in os.environ\nos.execv('+repr(str(OUT/'mockmcp'))+', ["mockmcp"])\n')
    import sys
    (root/'connectors.yaml').write_text(f'''connectors:
  a:
    command: {sys.executable}
    args: [{wrapper}]
    env: {{CHECK: "prefix-${{secret:TEST}}-suffix"}}
  b:
    command: {OUT / 'mockmcp'}
  remote:
    url: https://example.invalid/mcp
    oauth: {{}}
''')
    env=dict(os.environ,HOME=str(root/'home'),BROKERD_TEST_LEAK='must-not-reach-child')
    binary=ROOT/'rust/target/release/brokerd-rust'
    with (root/'daemon.log').open('w') as log:
        p=subprocess.Popen([str(binary),str(root),str(root/'owner.token'),'8991'],env=env,stdout=log,stderr=log)
        try:
            for _ in range(300):
                try:
                    status,data=rpc('/mcp/a','tools/list')
                    if status==200: break
                except OSError: pass
                time.sleep(.01)
            else: raise RuntimeError(f'not ready: {status}, {data}')
            assert len(data['result']['tools'])==6
            for path in ['/mcp','/mcp/a','/mcp/b']:
                assert rpc(path,'tools/list',token='')[0]==401
                assert rpc(path,'tools/list',token='wrong')[0]==401
            assert rpc('/mcp/remote','tools/list')[0]==404
            status,aggregate=rpc('/mcp','tools/list')
            names={x['name'] for x in aggregate['result']['tools']}
            assert len(names)==12 and 'a__echo' in names and 'b__echo' in names and 'echo' not in names
            status,result=rpc('/mcp','tools/call',{'name':'a__echo','arguments':{'text':'hello'}})
            assert result['result']['structuredContent']=={'echo':'hello','length':5}
            db=sqlite3.connect(root/'broker.db')
            assert db.execute('select phase,state from receipts order by id').fetchall()==[('intent','intent'),('outcome','completed')]
            db.execute("CREATE TRIGGER fail_intent BEFORE INSERT ON receipts WHEN NEW.phase='intent' BEGIN SELECT RAISE(ABORT,'fixture-deny'); END")
            db.commit()
            _,denied=rpc('/mcp/a','tools/call',{'name':'grow'})
            assert 'error' in denied
            db.execute('DROP TRIGGER fail_intent');db.commit()
            _,grow=rpc('/mcp/a','tools/call',{'name':'grow'})
            # If denied call reached the child this would say added_2.
            assert grow['result']['content'][0]['text']=='added added_1'
            assert db.execute('select count(*) from receipts').fetchone()[0]==4
            db.close()
        finally:
            p.send_signal(signal.SIGINT);p.wait(timeout=10)
    # Missing secrets fail at startup, rather than passing unresolved values to a child.
    (root/'secrets.json').write_text('{}')
    failed=subprocess.run([str(binary),str(root),str(root/'owner.token'),'8991'],env=env,capture_output=True,text=True,timeout=10)
    assert failed.returncode!=0 and 'missing secret TEST' in failed.stderr
    print('PASS: auth, stateless HTTP, exact passthrough (SDK client smoke separately), aggregate clash routing, args/env/secret resolution, env isolation, skipped remote, two receipt phases, fail-closed intent, missing-secret startup')
if __name__=='__main__': main()
