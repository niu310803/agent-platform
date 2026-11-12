// Opt-in Docker integration: creates only temporary directories and one test project.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {execFileSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {renderNginx} from '../host/nginx.mjs';
const root=await fs.mkdtemp('/tmp/webapp-nginx-policy-'),fixture=fileURLToPath(new URL('./fixture.mjs',import.meta.url)),image='agent-platform-webapp-base:0.5.47',base='http://127.0.0.1:19393';
const common={image,platform:'linux/amd64',user:`${os.userInfo().uid}:${os.userInfo().gid}`,read_only:true,cap_drop:['ALL'],security_opt:['no-new-privileges:true'],tmpfs:['/tmp:rw,nosuid,nodev,size=32m,mode=1777']};
function docker(args){return execFileSync('docker',['compose','-p','webapp-nginx-policy-test','-f',root+'/compose.json',...args],{stdio:'inherit'});}
const get=(url,options={})=>fetch(base+url,{redirect:'manual',...options});
try{
 for(const mode of ['none','custom','sso']){
  for(const d of ['frontend','logs','public','nginx'])await fs.mkdir(path.join(root,d),{recursive:true});
  await fs.writeFile(root+'/frontend/index.html','<main>protected</main>');await fs.writeFile(root+'/frontend/login','<main>login</main>');await fs.writeFile(root+'/public/bootstrap.json',JSON.stringify({authentication:{mode}}));await fs.writeFile(root+'/public/client.js','export const ok=true');
  const authentication=mode==='custom'?{mode,checkPath:'/api/auth/check',loginPath:'/login',publicPaths:['/api/auth/login']}:{mode,protocol:'oidc',providerRef:'fixture'};
  const manifest={authentication,frontend:{index:'index.html',routeConfig:{backendPrefixes:['/api']}},backend:{command:{type:'node',entry:'backend/server.mjs'}}};
  await fs.writeFile(root+'/nginx/nginx.conf',renderNginx(manifest));
  const fixtureService={...common,entrypoint:['/sbin/tini','--','node','/fixture.mjs'],volumes:[fixture+':/fixture.mjs:ro']};
  const health=url=>({test:['CMD','node','/opt/webapp-host/main.mjs','health',url],interval:'1s',timeout:'3s',retries:30});
  const compose={services:{backend:{...fixtureService,environment:{FIXTURE_ROLE:'backend'},healthcheck:health('http://127.0.0.1:8081/api/health')},auth:{...fixtureService,environment:{FIXTURE_ROLE:'auth',MODE:mode},ports:mode==='sso'?['127.0.0.1:19394:8084']:[],depends_on:{backend:{condition:'service_healthy'}},healthcheck:health('http://127.0.0.1:8082/__internal/health/ready')},webapp:{...common,command:['nginx'],ports:['127.0.0.1:19393:8080'],volumes:['frontend:/frontend:ro','public:/public:ro','nginx:/nginx:ro','logs:/logs'].map(x=>root+'/'+x),depends_on:{auth:{condition:'service_healthy'}},healthcheck:health('http://127.0.0.1:8080/__webapp/bootstrap.json')}}};
  await fs.writeFile(root+'/compose.json',JSON.stringify(compose));docker(['up','-d','--wait','--wait-timeout','60']);
  for(const p of ['/__desktop','/__desktop/bridge.js','/__internal/auth/check','/__internal/health/ready'])assert.equal((await get(p)).status,404,p);
  assert.equal((await get('/__webapp/bootstrap.json')).status,200);
  if(mode==='none'){
   assert.equal((await get('/__auth/login')).status,404);
   const r=await get('/api/echo',{headers:{'X-Webapp-User':'forged','Remote-User':'forged','X-Webapp-Roles':'admin'}}),h=(await r.json()).headers;
   assert.equal(h['x-webapp-user'],undefined);assert.equal(h['remote-user'],undefined);assert.equal(h['x-webapp-roles'],undefined);assert.equal(h['x-webapp-auth-mode'],'none');
  } else if(mode==='custom'){
   assert.equal((await get('/')).status,302);assert.equal((await get('/login')).status,200);assert.equal((await get('/api/auth/check')).status,404);
   assert.equal((await get('/api/echo',{headers:{'X-Webapp-User':'forged'}})).status,401);
   assert.equal((await get('/api/auth/login',{method:'POST'})).headers.get('set-cookie').split(';')[0],'custom=valid');
   let r=await get('/api/echo',{headers:{Cookie:'custom=valid',Authorization:'Bearer business','X-Webapp-User':'forged'}});const h=(await r.json()).headers;
   assert.equal(h['x-webapp-user'],'custom-user');assert.equal(h.cookie,'custom=valid');assert.equal(h.authorization,'Bearer business');assert.ok(r.headers.get('set-cookie'));
   assert.equal((await get('/api/echo',{method:'POST',headers:{Cookie:'custom=valid'}})).status,403);
   assert.equal((await get('/api/echo',{method:'POST',headers:{Cookie:'custom=valid','X-Webapp-CSRF':'custom-csrf'}})).status,200);
   assert.equal((await get('/__auth/me')).status,404);
  } else {
   assert.equal((await get('/')).status,302);assert.equal((await get('/api/echo')).status,401);
   const login=await get('/__auth/login'),authorization=new URL(login.headers.get('location')),state=authorization.searchParams.get('state'),binding=login.headers.get('set-cookie').split(';')[0];
   const code=(await(await fetch('http://127.0.0.1:19394/authorize'+authorization.search)).json()).code;
   const callback=await get('/__auth/callback?state='+state+'&code='+code,{headers:{Cookie:binding}});assert.equal(callback.status,302);const session=callback.headers.get('set-cookie').split(';')[0];
   const r=await get('/api/echo',{headers:{Cookie:session,Authorization:'Bearer browser','X-Webapp-User':'forged'}}),h=(await r.json()).headers;
   assert.match(h['x-webapp-user'],/^[a-f0-9]{64}$/);assert.equal(h.cookie,undefined);assert.equal(h.authorization,undefined);assert.equal(r.headers.get('set-cookie'),null);
   assert.equal((await get('/api/echo',{method:'POST',headers:{Cookie:session}})).status,403);
   const csrf=(await(await get('/__auth/csrf',{headers:{Cookie:session}})).json()).csrf;
   assert.equal((await get('/api/echo',{method:'POST',headers:{Cookie:session,Origin:base,'X-Webapp-CSRF':csrf}})).status,200);
   assert.equal((await get('/api/echo',{method:'POST',headers:{Cookie:session,Origin:'https://evil.test','X-Webapp-CSRF':csrf}})).status,403);
   const log=await fs.readFile(root+'/logs/nginx-access.log','utf8');assert.ok(!log.includes('state=')&&!log.includes('code='));
  }
  console.log('PASS Nginx policy:',mode);docker(['down']);
 }
}finally{try{docker(['down']);}catch{}await fs.rm(root,{recursive:true,force:true});}
