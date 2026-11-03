#!/usr/bin/env python3
"""Actually link each requested target; save status and complete diagnostics."""
import json, os, pathlib, subprocess, time
root=pathlib.Path(__file__).resolve().parents[2];out=root/'rust/.bench';results=[]
for goos,goarch,target in [('linux','amd64','x86_64-unknown-linux-gnu'),('linux','arm64','aarch64-unknown-linux-gnu'),('windows','amd64','x86_64-pc-windows-gnu'),('darwin','amd64','x86_64-apple-darwin')]:
 for lang in ['go','rust']:
  env=dict(os.environ)
  if lang=='go':
   env.update(CGO_ENABLED='0',GOOS=goos,GOARCH=goarch)
   cmd=['go','build','-trimpath','-ldflags=-s -w','-o',str(out/f'go-{goos}-{goarch}'),'./cmd/brokerd']
  else: cmd=['cargo','build','--release','--locked','--manifest-path','rust/Cargo.toml','--target',target]
  start=time.monotonic();p=subprocess.run(cmd,cwd=root,env=env,capture_output=True,text=True)
  (out/f'cross-{lang}-{goos}-{goarch}.log').write_text(p.stdout+p.stderr)
  results.append(dict(language=lang,target=f'{goos}/{goarch}',rust_target=target,ok=p.returncode==0,seconds=round(time.monotonic()-start,2),diagnostic=(p.stdout+p.stderr)[-3500:]))
  (out/'cross.json').write_text(json.dumps(results,indent=2)+'\n')
  print(lang,goos,goarch,p.returncode,flush=True)
