import http from 'node:http';
import { randomBytes, createHash, createPublicKey, verify, timingSafeEqual } from 'node:crypto';

const random=()=>randomBytes(32).toString('base64url');
const equal=(a,b)=>typeof a==='string'&&typeof b==='string'&&Buffer.byteLength(a)===Buffer.byteLength(b)&&timingSafeEqual(Buffer.from(a),Buffer.from(b));
const cookie=(req,name)=>req.headers.cookie?.split(';').map(x=>x.trim()).find(x=>x.startsWith(name+'='))?.slice(name.length+1);
const json=(res,status,value)=>{res.writeHead(status,{'Content-Type':'application/json','Cache-Control':'no-store'});res.end(JSON.stringify(value));};
async function fetchJSON(url, options={}) {
  const r=await fetch(url,{...options,redirect:'error',signal:AbortSignal.timeout(10000)});
  if(!r.ok)throw Error('OIDC upstream rejected request');
  const text=await r.text();if(text.length>1024*1024)throw Error('OIDC response too large');return JSON.parse(text);
}
export async function createAuthServer(config) {
  const {mode,provider:p}=config;
  let discovery;
  const sessions=new Map(), pending=new Map();
  const secure=p?.publicOrigin.startsWith('https:');
  const sessionCookie=secure?'__Host-webapp-session':'webapp-session';
  const txCookie=secure?'__Host-webapp-login':'webapp-login';
  const setCookie=(res,name,value,age)=>res.setHeader('Set-Cookie',`${name}=${value}; Path=/; HttpOnly; SameSite=Lax; Max-Age=${age}${secure?'; Secure':''}`);
  const prune=map=>{for(const[k,v]of map)if(v.expires<Date.now())map.delete(k);};
  function session(req){prune(sessions);return sessions.get(cookie(req,sessionCookie));}
  if(mode==='sso') {
    discovery=await fetchJSON(p.issuer.replace(/\/$/,'')+'/.well-known/openid-configuration');
    if(discovery.issuer!==p.issuer || !discovery.code_challenge_methods_supported?.includes('S256'))throw Error('OIDC discovery issuer/PKCE mismatch');
    for(const key of ['authorization_endpoint','token_endpoint','jwks_uri']) {
      const u=new URL(discovery[key]);
      if(u.username||u.password||u.hash||u.protocol!=='https:'&&!(p.allowInsecureLocalhost&&u.protocol==='http:'&&['localhost','127.0.0.1','[::1]'].includes(u.hostname)))throw Error('unsafe OIDC endpoint');
    }
  }
  async function identity(token, nonce) {
    const parts=token?.split('.');if(parts?.length!==3)throw Error('invalid ID token');
    const header=JSON.parse(Buffer.from(parts[0],'base64url'));
    if(header.alg!=='RS256'||header.crit||header.jku||header.x5u)throw Error('unsupported ID token algorithm');
    const jwks=await fetchJSON(discovery.jwks_uri);
    const keys=jwks.keys?.filter(k=>k.kty==='RSA'&&(!k.use||k.use==='sig')&&(!k.alg||k.alg==='RS256')&&(!header.kid||k.kid===header.kid));
    if(keys?.length!==1)throw Error('ambiguous ID token key');
    const key=createPublicKey({key:keys[0],format:'jwk'});
    if(!verify('RSA-SHA256',Buffer.from(parts[0]+'.'+parts[1]),key,Buffer.from(parts[2],'base64url')))throw Error('invalid ID token signature');
    const c=JSON.parse(Buffer.from(parts[1],'base64url')), now=Date.now()/1000;
    const aud=Array.isArray(c.aud)?c.aud:[c.aud];
    if(c.iss!==p.issuer||!aud.includes(p.clientId)||(aud.length>1&&c.azp!==p.clientId)||(c.azp&&c.azp!==p.clientId)||!Number.isFinite(c.exp)||c.exp<=now||!Number.isFinite(c.iat)||c.iat>now+30||c.nbf&&c.nbf>now+30||!equal(c.nonce,nonce)||typeof c.sub!=='string'||!c.sub||c.sub.length>256)throw Error('invalid ID token claims');
    return {user:createHash('sha256').update(c.iss+'\0'+c.sub).digest('hex'),name:encodeURIComponent(typeof c.name==='string'?c.name.slice(0,120):c.sub),expires:Math.min(c.exp*1000,Date.now()+8*3600000),csrf:random()};
  }
  return http.createServer(async(req,res)=>{
    const url=new URL(req.url,'http://auth');
    try {
      if(url.pathname==='/__internal/health/live')return json(res,200,{status:'live'});
      if(url.pathname==='/__internal/health/ready') {
        if(config.backendHealth){try{const r=await fetch('http://backend:8081'+config.backendHealth,{signal:AbortSignal.timeout(2000)});if(!r.ok)throw Error();}catch{return json(res,503,{status:'waiting_for_backend'});}}
        return json(res,200,{status:'ready',mode});
      }
      if(mode!=='sso')return json(res,404,{error:'not_found'});
      if(url.pathname==='/__internal/auth/check') {
        const s=session(req);if(!s)return json(res,401,{error:'login_required'});
        const method=req.headers['x-original-method']||'GET';
        if(!['GET','HEAD','OPTIONS'].includes(method) && (req.headers['x-original-origin']!==p.publicOrigin||!equal(req.headers['x-webapp-csrf'],s.csrf)))return json(res,403,{error:'csrf_rejected'});
        res.writeHead(204,{'X-Webapp-User':s.user,'X-Webapp-Name':s.name});return res.end();
      }
      if(req.method==='GET'&&url.pathname==='/__auth/login') {
        prune(pending);if(pending.size>=1000)return json(res,503,{error:'login_capacity'});
        const state=random(),binding=random(),verifier=random(),nonce=random();
        pending.set(state,{binding,verifier,nonce,expires:Date.now()+300000});
        setCookie(res,txCookie,binding,300);
        const target=new URL(discovery.authorization_endpoint);
        for(const[k,v]of Object.entries({response_type:'code',scope:'openid profile',client_id:p.clientId,redirect_uri:p.publicOrigin+'/__auth/callback',state,nonce,code_challenge:createHash('sha256').update(verifier).digest('base64url'),code_challenge_method:'S256'}))target.searchParams.set(k,v);
        res.writeHead(302,{Location:target.href,'Cache-Control':'no-store'});return res.end();
      }
      if(req.method==='GET'&&url.pathname==='/__auth/callback') {
        const state=url.searchParams.get('state'), tx=pending.get(state);pending.delete(state);
        setCookie(res,txCookie,'',0);
        if(!tx||tx.expires<Date.now()||!equal(cookie(req,txCookie),tx.binding)||!url.searchParams.get('code')||url.searchParams.get('error'))return json(res,400,{error:'invalid_login_state'});
        const params=new URLSearchParams({grant_type:'authorization_code',code:url.searchParams.get('code'),redirect_uri:p.publicOrigin+'/__auth/callback',client_id:p.clientId,code_verifier:tx.verifier});
        const headers={'Content-Type':'application/x-www-form-urlencoded'};
        if(p.clientSecret) {
          if((p.tokenEndpointAuthMethod||'client_secret_basic')==='client_secret_post')params.set('client_secret',p.clientSecret);
          else if(!p.tokenEndpointAuthMethod||p.tokenEndpointAuthMethod==='client_secret_basic')headers.Authorization='Basic '+Buffer.from(encodeURIComponent(p.clientId)+':'+encodeURIComponent(p.clientSecret)).toString('base64');
          else throw Error('unsupported client authentication');
        }
        const tokens=await fetchJSON(discovery.token_endpoint,{method:'POST',headers,body:params});
        const s=await identity(tokens.id_token,tx.nonce);prune(sessions);if(sessions.size>=10000)throw Error('session capacity');
        const id=random();sessions.set(id,s);setCookie(res,sessionCookie,id,Math.floor((s.expires-Date.now())/1000));
        res.writeHead(302,{Location:'/','Cache-Control':'no-store'});return res.end();
      }
      if(req.method==='GET'&&['/__auth/me','/__auth/csrf'].includes(url.pathname)) {
        const s=session(req);if(!s)return json(res,401,{error:'login_required'});
        return json(res,200,url.pathname.endsWith('/csrf')?{csrf:s.csrf}:{user:s.user,name:decodeURIComponent(s.name),expiresAt:s.expires});
      }
      if(req.method==='POST'&&url.pathname==='/__auth/logout') {
        const s=session(req);if(!s)return json(res,401,{error:'login_required'});
        if(req.headers.origin!==p.publicOrigin||!equal(req.headers['x-webapp-csrf'],s.csrf))return json(res,403,{error:'csrf_rejected'});
        sessions.delete(cookie(req,sessionCookie));setCookie(res,sessionCookie,'',0);return json(res,200,{loggedOut:true});
      }
      return json(res,404,{error:'not_found'});
    } catch {return json(res,502,{error:'authentication_failed'});}
  });
}
