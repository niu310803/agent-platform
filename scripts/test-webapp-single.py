#!/usr/bin/env python3
"""Disposable macOS/Windows single-host acceptance using a supplied Node 24 launcher.
No real model or credentials are used. Runtime and logs remain in the printed
scratch directory. Run with --platform <binary> --node <Desktop bin/node>.
"""
import argparse,os,json,tempfile,subprocess,time,urllib.request,urllib.parse,pathlib,signal,http.server,threading
parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--platform',type=pathlib.Path,required=True)
parser.add_argument('--node',type=pathlib.Path,required=True)
args=parser.parse_args()
binary=str(args.platform.resolve());node=str(args.node.resolve())
version=subprocess.check_output([binary,'--version'],text=True).strip()
root=pathlib.Path(tempfile.mkdtemp(prefix='platform-single-')).resolve()
class Model(http.server.BaseHTTPRequestHandler):
 def log_message(self,*args):pass
 def do_POST(self):
  self.rfile.read(int(self.headers['Content-Length']))
  self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
  for delta,finish in [({'role':'assistant','content':'single-agent-ok'},None),({},'stop')]:
   self.wfile.write(('data: '+json.dumps({'id':'smoke','object':'chat.completion.chunk','model':'mock','choices':[{'index':0,'delta':delta,'finish_reason':finish}]})+'\n\n').encode())
  self.wfile.write(b'data: [DONE]\n\n')
model=http.server.ThreadingHTTPServer(('127.0.0.1',0),Model)
threading.Thread(target=model.serve_forever,daemon=True).start()

runtime=root/'runtime'; runtime.mkdir();(runtime/'frontend').mkdir();(runtime/'backend').mkdir()
(runtime/'frontend/index.html').write_text('<html>single application</html>')
(runtime/'webapp.json').write_text(json.dumps(dict(schemaVersion=2,id='webapp-0123456789abcdef',key='smoke',label='Smoke',version='1.0.0',target='any',hosting='platform',minPlatformVersion=version,copilot=dict(agentKey='smoke-agent',references=[dict(type='skill',id='smoke-skill')]),frontend=dict(root='frontend',index='index.html',routeConfig=dict(backendPrefixes=['/api'])),backend=dict(command=dict(type='node',entry='backend/server.mjs'),health=dict(type='http',path='/api/health')))))
(runtime/'backend/server.mjs').write_text('''import http from 'node:http';import fs from 'node:fs';import cp from 'node:child_process';
const denied=[]; for (const p of [process.env.WEBAPP_DIR+'/.state/webapp-owner/owner.json',process.env.WEBAPP_DIR+'/chats',process.env.WEBAPP_DIR+'/registries']) {try {fs.readdirSync(p);throw new Error('unexpected read')}catch(e){if(e.code!=='ERR_ACCESS_DENIED')throw e;denied.push(p)}}
try{fs.writeFileSync(process.env.WEBAPP_DIR+'/frontend/hack','x');throw new Error('unexpected write')}catch(e){if(e.code!=='ERR_ACCESS_DENIED')throw e}
try{cp.spawnSync('echo',['bad']);throw new Error('unexpected spawn')}catch(e){if(e.code!=='ERR_ACCESS_DENIED')throw e}
fs.writeFileSync(process.env.WEBAPP_DATA_DIR+'/ok',JSON.stringify({denied,pid:process.pid}));
http.createServer((q,r)=>{r.end('healthy')}).listen(Number(process.env.PORT),process.env.HOST);
''')
(runtime/'skills-center/smoke-skill').mkdir(parents=True)
(runtime/'skills-center/smoke-skill/SKILL.md').write_text('---\nname: smoke-skill\ndescription: smoke test instructions\n---\nRespond briefly.\n')
(runtime/'agents/smoke-agent').mkdir(parents=True)
(runtime/'agents/smoke-agent/agent.yml').write_text('key: smoke-agent\nname: Smoke Agent\nmode: GENERAL\nmodelConfig:\n  modelKey: mock\n')
registries=root/'registries'
(registries/'providers').mkdir(parents=True);(registries/'models').mkdir()
(registries/'providers/mock.yml').write_text('key: mock\nbaseUrl: http://127.0.0.1:'+str(model.server_port)+'\napiKey: test-key\n')
(registries/'models/mock.yml').write_text('key: mock\nprovider: mock\nprotocol: OPENAI\nmodelId: mock\n')
registry_before={str(p.relative_to(registries)):p.read_bytes() for p in registries.rglob('*') if p.is_file()}
config=root/'config';config.mkdir();(config/'configs').mkdir();(config/'configs/tools.yml').write_text('bash:\n  git-bash:\n    enabled: false\n')
log=open(root/'log','w');env=dict(os.environ,AP_RUNTIME_DIR=str(runtime));
for k in ['AP_RUNTIME_REGISTRIES_DIR','AP_RUNTIME_CHATS_DIR','AP_RUNTIME_MEMORY_DIR','AP_RUNTIME_STATE_DIR','AP_RUNTIME_PAN_DIR']:env.pop(k,None)
env['AP_RUNTIME_REGISTRIES_DIR']=str(registries)
parent=subprocess.Popen([os.sys.executable,'-c','import time;time.sleep(600)'])
command=[binary,'--webapp-host=single','--listen=127.0.0.1:0','--config-dir='+str(config),'--ready-file='+str(root/'ready.json'),'--webapp-node='+node,'--parent-pid='+str(parent.pid)]
p=subprocess.Popen(command,env=env,stdout=log,stderr=log)
try:
 for i in range(500):
  if (root/'ready.json').exists():break
  if p.poll() is not None:raise RuntimeError((root/'log').read_text())
  time.sleep(.1)
 else:raise RuntimeError((root/'log').read_text())
 ready=json.loads((root/'ready.json').read_text());token=json.loads((runtime/'.state/webapp-owner/owner.json').read_text())['token']
 def request(path,method='GET',data=None):
  req=urllib.request.Request(ready['apiUrl']+path,data=json.dumps(data).encode() if data is not None else None,method=method,headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'});return json.load(urllib.request.urlopen(req))
 launch=request('/api/admin/webapps/webapp-0123456789abcdef/launch-ticket','POST',{'returnPath':'/'})['data']
 class NoRedirect(urllib.request.HTTPRedirectHandler):
  def redirect_request(self,*args):return None
 opener=urllib.request.build_opener(NoRedirect())
 req=urllib.request.Request(launch['action'],data=urllib.parse.urlencode({'ticket':launch['ticket']}).encode(),headers={'Origin':ready['apiUrl'],'Content-Type':'application/x-www-form-urlencoded'})
 try:opener.open(req)
 except urllib.error.HTTPError as e:
  assert e.code==303,e.code; cookie=e.headers['Set-Cookie'].split(';')[0]
 req=urllib.request.Request(ready['appUrl']+'/',headers={'Cookie':cookie});assert b'single application' in urllib.request.urlopen(req).read()
 bootstrap=json.load(urllib.request.urlopen(urllib.request.Request(ready['appUrl']+'/__desktop/bootstrap.json',headers={'Cookie':cookie})))
 call=urllib.request.Request(ready['appUrl']+'/__desktop/actions/call',data=json.dumps({'action':'assistant.chat','args':{'message':'Respond briefly'}}).encode(),headers={'Cookie':cookie,'Origin':ready['appUrl'],'Content-Type':'application/json','X-WebApp-CSRF':bootstrap['csrfToken']})
 result=json.load(urllib.request.urlopen(call,timeout=30));assert result['ok'],result;assert 'single-agent-ok' in result['result']['text'],result
 status=request('/api/webapp/status')['data'];assert status['idle'],status
 assert registry_before=={str(p.relative_to(registries)):p.read_bytes() for p in registries.rglob('*') if p.is_file()},'registries changed'
 # A hard Platform crash must close the watchdog pipe and release its lease.
 if os.name!='nt':
  backend=json.loads((runtime/'webapp-data/ok').read_text())['pid']
  p.kill();p.wait()
  for _ in range(100):
   try:os.kill(backend,0)
   except ProcessLookupError:break
   time.sleep(.1)
  else:raise AssertionError('backend survived Platform SIGKILL')
  p=subprocess.Popen(command,env=env,stdout=log,stderr=log)
  for _ in range(500):
   try:
    candidate=json.loads((root/'ready.json').read_text())
    if candidate['pid']==p.pid:ready=candidate;break
   except (FileNotFoundError,json.JSONDecodeError):pass
   if p.poll() is not None:raise RuntimeError((root/'log').read_text())
   time.sleep(.1)
  else:raise AssertionError('restart did not become ready')
 parent.terminate();parent.wait();p.wait(timeout=15)
 assert not (root/'ready.json').exists(),'ready file survived normal parent death'
 backend=json.loads((runtime/'webapp-data/ok').read_text())['pid']
 if os.name!='nt':
  try:os.kill(backend,0)
  except ProcessLookupError:pass
  else:raise AssertionError('backend survived parent death')
 for name in ['automations','kbases','ru-kbases','mail.db']:
  assert not (runtime/name).exists(),name
 assert not (runtime/'chats/mail.db').exists()
 print(json.dumps({'root':str(root),'ready':ready,'status':status,'nodePermission':json.loads((runtime/'webapp-data/ok').read_text())}))
finally:
 model.shutdown()
 if parent.poll() is None:parent.terminate();parent.wait()
 if p.poll() is None:p.terminate()
 try:p.wait(timeout=12)
 except: p.kill();p.wait()
 print('exit',p.returncode)
