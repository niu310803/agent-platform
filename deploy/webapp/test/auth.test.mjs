import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { generateKeyPairSync, sign, createHash } from 'node:crypto';
import { createAuthServer } from '../host/auth.mjs';
import { providerConfig, validateImageManifest } from '../host/config.mjs';
import { renderNginx } from '../host/nginx.mjs';
const listen=server=>new Promise(r=>server.listen(0,'127.0.0.1',()=>r('http://127.0.0.1:'+server.address().port)));
const close=server=>new Promise(r=>server.close(r));
const manifest=authentication=>({hosting:'platform',authentication,frontend:{index:'index.html',routeConfig:{backendPrefixes:['/api']}},backend:{command:{type:'node',entry:'backend/server.mjs'}}});
test('explicit auth mode, OIDC-only identity, normalized routes, no silent fallback',()=>{
  for(const a of [undefined,{mode:'gateway'},{mode:'sso',protocol:'oauth2',providerRef:'x'},{mode:'custom',checkPath:'/api/check',loginPath:'/login',publicPaths:['/api/']}])assert.throws(()=>validateImageManifest(manifest(a)));
  for(const a of [{mode:'none'},{mode:'sso',protocol:'oidc',providerRef:'company'},{mode:'custom',checkPath:'/api/check',loginPath:'/login',publicPaths:['/assets/']}])assert.equal(validateImageManifest(manifest(a)).authentication,a);
  assert.throws(()=>providerConfig(manifest({mode:'sso',protocol:'oidc',providerRef:'missing'}),{}));
  assert.throws(()=>providerConfig(manifest({mode:'sso',protocol:'oidc',providerRef:'x'}),{x:{issuer:'http://public.test',clientId:'app',publicOrigin:'https://app.test'}}));
});
test('rendered policy contains only backend routes and blocks legacy/private namespaces',()=>{
  for(const a of [{mode:'none'},{mode:'sso',protocol:'oidc',providerRef:'x'},{mode:'custom',checkPath:'/api/check',loginPath:'/login',publicPaths:['/assets/']}]){
    const text=renderNginx(manifest(a));assert.doesNotMatch(text,/proxy_pass http:\/\/platform/);assert.match(text,/location \^~ \/__desktop\/ \{ return 404/);
    assert.match(text,/location \^~ \/__internal\/ \{ return 404/);
    assert.match(text,/proxy_set_header X-Webapp-Auth-Mode/);
    if(a.mode==='none')assert.doesNotMatch(text,/auth_request \/__internal/);else assert.match(text,/internal; proxy_pass http:\/\//);
  }
});
test('none/custom expose no SSO endpoints',async()=>{
  for(const mode of ['none','custom']){const s=await createAuthServer({mode});const base=await listen(s);try{assert.equal((await fetch(base+'/__auth/login')).status,404);assert.equal((await fetch(base+'/__internal/health/ready')).status,200);}finally{await close(s);}}
});
test('OIDC code + PKCE + state/nonce, JWT verification, CSRF and logout',async()=>{
  const pair=generateKeyPairSync('rsa',{modulusLength:2048});const jwk=pair.publicKey.export({format:'jwk'});jwk.kid='test';let issuer,authBase,claimsOverride={},badSignature=false,last,tokenCalls=0;
  const idp=http.createServer(async(req,res)=>{
    res.setHeader('Content-Type','application/json');
    if(req.url==='/.well-known/openid-configuration')return res.end(JSON.stringify({issuer,authorization_endpoint:issuer+'/authorize',token_endpoint:issuer+'/token',jwks_uri:issuer+'/jwks',code_challenge_methods_supported:['S256']}));
    if(req.url==='/jwks')return res.end(JSON.stringify({keys:[jwk]}));
    if(req.url==='/token'){
      tokenCalls++;const body=new URLSearchParams(await new Promise(r=>{let s='';req.on('data',x=>s+=x);req.on('end',()=>r(s));}));
      assert.equal(createHash('sha256').update(body.get('code_verifier')).digest('base64url'),last.searchParams.get('code_challenge'));
      assert.equal(body.get('redirect_uri'),authBase+'/__auth/callback');
      const now=Math.floor(Date.now()/1000), header=Buffer.from(JSON.stringify({alg:'RS256',kid:'test'})).toString('base64url');
      const claims=Buffer.from(JSON.stringify({iss:issuer,aud:'client',sub:'user-1',name:'测试用户',iat:now,exp:now+600,nonce:last.searchParams.get('nonce'),...claimsOverride})).toString('base64url');
      const data=header+'.'+claims;const signature=badSignature?Buffer.alloc(256):sign('RSA-SHA256',Buffer.from(data),pair.privateKey);return res.end(JSON.stringify({id_token:data+'.'+signature.toString('base64url')}));
    }res.writeHead(404);res.end('{}');
  });
  issuer=await listen(idp);
  const auth=await createAuthServer({mode:'sso',provider:{issuer,clientId:'client',publicOrigin:'http://localhost:1',allowInsecureLocalhost:true}});
  // Use a fixed configured origin, independent of the hostile Host request header.
  authBase='http://localhost:1';const base=await listen(auth);
  async function login(){const r=await fetch(base+'/__auth/login',{redirect:'manual'});last=new URL(r.headers.get('location'));assert.equal(last.searchParams.get('code_challenge_method'),'S256');return {state:last.searchParams.get('state'),cookie:r.headers.get('set-cookie').split(';')[0]};}
  async function callback(tx, cookie=tx.cookie){return fetch(base+'/__auth/callback?code=ok&state='+tx.state,{redirect:'manual',headers:{Cookie:cookie}});}
  try{
    let tx=await login();const count=tokenCalls;assert.equal((await callback(tx,'webapp-login=forged')).status,400);assert.equal(tokenCalls,count);
    for(const override of [{nonce:'bad'},{iss:'https://wrong'},{aud:'wrong'},{exp:1},{iat:Math.floor(Date.now()/1000)+1000},{aud:['client','other'],azp:'wrong'}]){claimsOverride=override;tx=await login();assert.equal((await callback(tx)).status,502);}
    claimsOverride={};badSignature=true;tx=await login();assert.equal((await callback(tx)).status,502);badSignature=false;
    tx=await login();const r=await callback(tx);assert.equal(r.status,302);const session=r.headers.get('set-cookie').split(';')[0];
    assert.equal((await callback(tx)).status,400);
    const me=await (await fetch(base+'/__auth/me',{headers:{Cookie:session}})).json();assert.equal(me.name,'测试用户');
    const csrf=(await (await fetch(base+'/__auth/csrf',{headers:{Cookie:session}})).json()).csrf;
    const check=headers=>fetch(base+'/__internal/auth/check',{headers:{Cookie:session,...headers}});
    assert.equal((await check({'X-Original-Method':'POST'})).status,403);
    assert.equal((await check({'X-Original-Method':'POST','X-Original-Origin':authBase,'X-Webapp-CSRF':csrf})).status,204);
    assert.equal((await check({'X-Original-Method':'POST','X-Original-Origin':'https://evil.test','X-Webapp-CSRF':csrf})).status,403);
    assert.equal((await fetch(base+'/__auth/logout',{method:'POST',headers:{Cookie:session,Origin:authBase,'X-Webapp-CSRF':csrf}})).status,200);
    assert.equal((await check({})).status,401);
  }finally{await close(auth);await close(idp);}
});

test('runtime configuration preserves operator tuning and fixes service auth',async()=>{
 const {runtimeConfig}=await import('../host/config.mjs');
 const yaml=runtimeConfig('budget:\n  timeout: 42\nauth:\n  enabled: false\n  jwks-uri: https://wrong\nhttp-proxy:\n  mode: fixed\n  url: http://proxy:8001\nmemory:\n  worker:\n    enabled: true\n',{issuer:'image',audience:'app',subject:'backend'});
 assert.match(yaml,/timeout: 42/);assert.match(yaml,/mode: fixed/);assert.doesNotMatch(yaml,/jwks-uri/);assert.equal((yaml.match(/^auth:/gm)||[]).length,1);assert.match(yaml,/require-expiry: true/);assert.match(yaml,/worker:\n    enabled: false/);
});
