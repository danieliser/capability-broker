#!/usr/bin/env python3
"""Local HTTP/1.1 upstream fixture: custom headers and initialize compatibility."""
import http.server,json,os,socket,subprocess,threading,time
from full_smoke import ROOT,OUT,result,wait
seen=[]
class Upstream(http.server.BaseHTTPRequestHandler):
    protocol_version='HTTP/1.1'
    def log_message(self,*args):pass
    def do_POST(self):
        req=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        seen.append((req['method'],dict(self.headers),self.request_version))
        m=req['method']
        if m=='initialize':data={'protocolVersion':'2025-11-25','capabilities':{'tools':{}},'serverInfo':{'name':'http-fixture','version':'1'}}
        elif m=='tools/list':data={'tools':[{'name':'echo','inputSchema':{'type':'object'}}]}
        elif m=='tools/call':data={'content':[{'type':'text','text':'remote ok'}]}
        elif 'id' not in req:
            self.send_response(202);self.send_header('Content-Length','0');self.end_headers();return
        else:data=None
        body=json.dumps({'jsonrpc':'2.0','id':req.get('id'),**({'result':data} if data is not None else {'error':{'code':-32601,'message':'method not found'}})}).encode()
        self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)
    def do_GET(self):self.send_error(405)
def main():
    server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Upstream);server.daemon_threads=True
    thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
    root=OUT/f'remote-{time.time_ns()}';root.mkdir(mode=0o700);(root/'home').mkdir();(root/'owner.token').write_text('fixture-owner')
    (root/'secrets.json').write_text('{"TEST":"fixture-header"}');(root/'secrets.json').chmod(0o600)
    (root/'connectors.yaml').write_text(f'connectors:\n  mock:\n    url: http://127.0.0.1:{server.server_port}/mcp\n    headers: {{X-Fixture: "${{secret:TEST}}"}}\n')
    with (root/'daemon.log').open('w') as log:
        p=subprocess.Popen([str(ROOT/'rust/target/release/brokerd-rust'),str(root),str(root/'owner.token'),'8995'],env=dict(os.environ,HOME=str(root/'home')),stdout=log,stderr=log)
        try:
            wait(lambda:len(result('/mcp/mock','tools/list')['tools'])==1)
            assert result('/mcp/mock','tools/call',{'name':'echo'})['content'][0]['text']=='remote ok'
            init=next(h for m,h,v in seen if m=='initialize')
            assert any(k.lower()=='mcp-protocol-version' for k in init),init
            assert all(v=='HTTP/1.1' and any(k.lower()=='user-agent' and val.startswith('brokerd/') for k,val in h.items()) and any(k.lower()=='x-fixture' and val=='fixture-header' for k,val in h.items()) for m,h,v in seen)
            report={'http1':True,'broker_user_agent':True,'protocol_header_initialize':True,'secret_header':True,'remote_echo':True,'fixture':str(root)}
            (OUT/'remote-smoke.json').write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report,indent=2))
        finally:
            p.terminate()
            try:p.wait(timeout=5)
            except subprocess.TimeoutExpired:p.kill();p.wait()
            server.shutdown();server.server_close()
if __name__=='__main__':main()
