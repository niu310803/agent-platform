import fs from 'node:fs/promises';
import { createWriteStream } from 'node:fs';
import { spawn, execFileSync } from 'node:child_process';
import { sign } from 'node:crypto';
import { prepare } from './config.mjs';
import { createAuthServer } from './auth.mjs';

const role=process.argv[2];
function child(command,args,env=process.env) {
  const log=createWriteStream('/logs/'+role+'.log',{flags:'a',mode:0o600});
  const p=spawn(command,args,{env,stdio:['inherit','pipe','pipe']});
  for(const stream of [p.stdout,p.stderr])stream.on('data',data=>{log.write(data);process.stdout.write(data);});
  for(const signal of ['SIGTERM','SIGINT'])process.on(signal,()=>p.kill(signal));
  p.on('error',()=>process.exit(1));p.on('exit',(code)=>process.exit(code??1));return p;
}
if(role==='prepare')await prepare(process.env.WEBAPP_SOURCE||'/webapp',process.env.WEBAPP_RUNTIME||'/runtime',process.env.WEBAPP_UPDATE==='true');
else if(role==='platform')child('/usr/local/bin/agent-platform',['--config-dir','/platform-config','--listen','0.0.0.0:8083'],{...process.env,AP_RUNTIME_DIR:'/runtime',AP_RUNTIME_REGISTRIES_DIR:'/runtime/registries'});
else if(role==='auth') {
  const server=await createAuthServer(JSON.parse(await fs.readFile('/auth-config/config.json','utf8')));
  server.listen(8082,'0.0.0.0');process.on('SIGTERM',()=>server.close(()=>process.exit(0)));
} else if(role==='backend') {
  const m=JSON.parse(await fs.readFile('/backend-config/app.json','utf8'));
  if(!m.backend)throw Error('no backend declared');
  const key=await fs.readFile('/backend-config/platform-key.pem');const service=JSON.parse(await fs.readFile('/backend-config/service.json','utf8'));
  async function rotate(){const now=Math.floor(Date.now()/1000), header=Buffer.from(JSON.stringify({alg:'RS256',typ:'JWT'})).toString('base64url'),claims=Buffer.from(JSON.stringify({iss:service.issuer,aud:service.audience,sub:service.subject,iat:now,exp:now+600})).toString('base64url');const text=header+'.'+claims;await fs.writeFile('/tmp/platform-token.tmp',text+'.'+sign('RSA-SHA256',Buffer.from(text),key).toString('base64url'),{mode:0o600});await fs.rename('/tmp/platform-token.tmp','/tmp/platform-token');}
  await rotate();setInterval(()=>rotate().catch(()=>process.exit(1)),300000).unref();
  child('node',['--max-old-space-size=256','/app/'+m.backend.command.entry,...(m.backend.args||[])],{...process.env,...m.backend.env,HOST:'0.0.0.0',PORT:'8081',WEBAPP_DATA_DIR:'/data',WEBAPP_CONFIG_FILE:'/backend-config/app.json',WEBAPP_PLATFORM_URL:'http://platform:8083',WEBAPP_PLATFORM_TOKEN_FILE:'/tmp/platform-token'});
} else if(role==='nginx') {
  const probe=async()=>{const r=await fetch('http://auth:8082/__internal/health/ready',{signal:AbortSignal.timeout(3000)});if(!r.ok)throw Error('dependencies are not ready');};
  // No public listener exists until the effective policy and dependencies pass.
  await probe();execFileSync('nginx',['-t','-e','/logs/nginx-error.log','-c','/nginx/nginx.conf'],{stdio:'inherit'});
  child('nginx',['-e','/logs/nginx-error.log','-c','/nginx/nginx.conf','-g','daemon off;']);
} else if(role==='health') {
  const target=process.argv[3];
  try{const r=await fetch(target,{signal:AbortSignal.timeout(3000)});if(!r.ok)process.exit(1);}catch{process.exit(1);}
} else throw Error('role must be prepare/platform/backend/auth/nginx/health');
