#!/usr/bin/env python3
"""Executable owner-front/lifecycle contract against Go cmd/mockmcp. Scratch only."""
import json, os, pathlib, signal, socket, sqlite3, subprocess, time, urllib.request, urllib.error
ROOT=pathlib.Path(__file__).resolve().parents[2]; OUT=ROOT/'rust/.bench'
def rpc(path,method,params=None,token='fixture-owner'):
    params=dict(params or {},_meta={'io.modelcontextprotocol/protocolVersion':'2026-07-28','io.modelcontextprotocol/clientCapabilities':{},'io.modelcontextprotocol/clientInfo':{'name':'smoke','version':'1'}})
    req=urllib.request.Request('http://127.0.0.1:8995'+path,data=json.dumps(dict(jsonrpc='2.0',id=1,method=method,params=params)).encode(),headers={'Authorization':'Bearer '+token,'Content-Type':'application/json','Accept':'application/json, text/event-stream','Mcp-Protocol-Version':'2026-07-28','Mcp-Method':method,**({'Mcp-Name':params.get('name',params.get('uri'))} if params and ('name' in params or 'uri' in params) else {})})
    try:
        with urllib.request.urlopen(req,timeout=10) as r:return r.status,json.load(r)
    except urllib.error.HTTPError as e:return e.code,{}
def result(path,method,params=None):
    code,r=rpc(path,method,params);assert code==200,(code,r);assert 'error' not in r,r;return r['result']
def wait(check):
    end=time.monotonic()+20
    while time.monotonic()<end:
        try:
            if check():return
        except (OSError,KeyError,AssertionError):pass
        time.sleep(.1)
    raise AssertionError('condition did not become true')
def main():
    with socket.socket() as probe:
        probe.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);probe.bind(('127.0.0.1',8995))
    OUT.mkdir(exist_ok=True)
    subprocess.run(['go','build','-o',str(OUT/'mockmcp'),'./cmd/mockmcp'],cwd=ROOT,check=True)
    root=OUT/f'full-smoke-{time.time_ns()}';root.mkdir(mode=0o700);(root/'home').mkdir()
    (root/'owner.token').write_text('fixture-owner');(root/'owner.token').chmod(0o600)
    base=f'connectors:\n  mock:\n    command: {OUT / "mockmcp"}\n    pin: false\n'
    config=root/'connectors.yaml';config.write_text(base+'    on_demand: true\n    idle_stop: 1s\n')
    env=dict(os.environ,HOME=str(root/'home'))
    with (root/'daemon.log').open('w') as log:
        p=subprocess.Popen([str(ROOT/'rust/target/release/brokerd-rust'),str(root),str(root/'owner.token'),'8995'],env=env,stdout=log,stderr=log)
        try:
            wait(lambda:len(result('/mcp/mock','tools/list')['tools'])==6)
            assert rpc('/mcp/mock','tools/list',token='wrong')[0]==401
            init=result('/mcp/mock','initialize',{'protocolVersion':'2026-07-28','capabilities':{},'clientInfo':{'name':'fixture','version':'1'}})
            assert init['instructions'].startswith('mockmcp: test server')
            tools=result('/mcp/mock','tools/list')['tools'];echo=next(t for t in tools if t['name']=='echo')
            assert echo['title']=='Echo' and echo['annotations']['readOnlyHint'] and echo['outputSchema'] and echo['_meta']['mock/kind']=='echo'
            assert len(result('/mcp/mock','prompts/list')['prompts'])==1
            assert result('/mcp/mock','prompts/get',{'name':'greet','arguments':{'name':'Sir'}})['messages'][0]['content']['text']=='Say hello to Sir.'
            assert len(result('/mcp/mock','resources/list')['resources'])==1
            assert len(result('/mcp/mock','resources/templates/list')['resourceTemplates'])==1
            assert result('/mcp/mock','resources/read',{'uri':'mock://items/7'})['contents'][0]['text']=='{"id":"7"}'
            assert result('/mcp/mock','tools/call',{'name':'echo','arguments':{'text':'hello'}})['structuredContent']=={'echo':'hello','length':5}
            assert result('/mcp/mock','tools/call',{'name':'fail'})['isError']
            db=sqlite3.connect(root/'broker.db')
            db.execute("CREATE TRIGGER deny_fixture BEFORE INSERT ON receipts WHEN NEW.phase='intent' BEGIN SELECT RAISE(ABORT,'denied'); END");db.commit()
            assert 'error' in rpc('/mcp/mock','tools/call',{'name':'grow'})[1]
            assert len(result('/mcp/mock','tools/list')['tools'])==6
            db.execute('DROP TRIGGER deny_fixture');db.commit()
            result('/mcp/mock','tools/call',{'name':'grow'})
            wait(lambda:any(t['name']=='added_1' for t in result('/mcp/mock','tools/list')['tools']))
            wait(lambda:len(result('/mcp/mock','prompts/list')['prompts'])==2)
            assert (root/'cache/mock.json').exists()
            # The old child must actually exit, not merely report a stopped state.
            wait(lambda:not subprocess.run(['pgrep','-P',str(p.pid)],capture_output=True,text=True).stdout.strip())
            assert len(result('/mcp/mock','tools/list')['tools'])==7
            assert result('/mcp/mock','tools/call',{'name':'echo','arguments':{'text':'wake'}})['structuredContent']['echo']=='wake'
            assert subprocess.run(['pgrep','-P',str(p.pid)],capture_output=True,text=True).stdout.strip()
            config.write_text('connectors: [invalid');p.send_signal(signal.SIGHUP);time.sleep(.3)
            assert result('/mcp/mock','tools/list')['tools']
            config.write_text(base+'    expose: broker\n    tools: {exclude: [fail]}\n    strip: [annotations, meta, prompts]\n    response: {drop_keys: [debug, _links], max_chars: 200, tools: [noisy]}\n')
            wait(lambda:{t['name'] for t in result('/mcp','tools/list')['tools']}=={'search_tools','describe_tool','call_tool'})
            hits=result('/mcp','tools/call',{'name':'search_tools','arguments':{'query':'echo text'}})
            assert '"tool":"echo"' in hits['content'][0]['text']
            assert result('/mcp','tools/call',{'name':'call_tool','arguments':{'connector':'mock','tool':'echo','arguments':{'text':'via broker'}}})['structuredContent']['echo']=='via broker'
            assert all(t['name']!='fail' for t in result('/mcp/mock','tools/list')['tools'])
            assert 'error' in rpc('/mcp/mock','tools/call',{'name':'fail'})[1]
            assert result('/mcp/mock','prompts/list')['prompts']==[]
            noisy=result('/mcp/mock','tools/call',{'name':'noisy'})['content'][0]['text']
            assert 'debug' not in noisy and '_links' not in noisy and 'more characters cut by the broker' in noisy
            assert result('/mcp?exclude=mock','tools/list')['tools']==[]
            config.write_text('connectors: {}\n');p.send_signal(signal.SIGHUP)
            wait(lambda:rpc('/mcp/mock','tools/list')[0]==404)
            db.close()
        finally:
            if p.poll() is None:p.send_signal(signal.SIGINT)
            try:p.wait(timeout=10)
            except subprocess.TimeoutExpired:p.kill();p.wait()
    report={'owner_front':True,'prompt_resource_template_passthrough':True,'runtime_discovery':True,'idle_process_exit_and_restart':True,'invalid_reload_preserves_state':True,'poll_and_sighup':True,'discovery_tools':True,'filter_and_strip':True,'failed_intent_denies_upstream':True,'fixture':str(root)}
    (OUT/'full-smoke.json').write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report,indent=2))
if __name__=='__main__':main()
