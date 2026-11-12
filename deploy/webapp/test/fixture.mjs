// Test-only backend and local OIDC identity source; never copied into the image.
import http from 'node:http';
import {generateKeyPairSync,sign,createHash} from 'node:crypto';
import {createAuthServer} from '/opt/webapp-host/auth.mjs';
const json=(res,status,data)=>{res.writeHead(status,{'Content-Type':'application/json'});res.end(JSON.stringify(data));};
if(process.env.FIXTURE_ROLE==='backend') {
 http.createServer((req,res)=>{
  if(req.url==='/api/health')return json(res,200,{ready:true});
  if(req.url==='/api/auth/check') {
   if(!req.headers.cookie?.includes('custom=valid'))return json(res,401,{});
   if(req.headers['x-original-method']==='POST'&&req.headers['x-webapp-csrf']!=='custom-csrf')return json(res,403,{});
   res.writeHead(204,{'X-Webapp-User':'custom-user'});return res.end();
  }
  if(req.url==='/api/auth/login'){res.setHeader('Set-Cookie','custom=valid; HttpOnly; Path=/');return json(res,200,{login:true});}
  if(req.url==='/api/echo'){res.setHeader('Set-Cookie','business-cookie=yes');return json(res,200,{headers:req.headers});}
  return json(res,404,{});
 }).listen(8081,'0.0.0.0');
} else {
 const mode=process.env.MODE;
 if(mode==='sso') {
  const pair=generateKeyPairSync('rsa',{modulusLength:2048}), jwk=pair.publicKey.export({format:'jwk'});jwk.kid='fixture';const codes=new Map();
  const issuer='http://127.0.0.1:8084';
  const idp=http.createServer(async(req,res)=>{
   const u=new URL(req.url,issuer);res.setHeader('Content-Type','application/json');
   if(u.pathname==='/.well-known/openid-configuration')return res.end(JSON.stringify({issuer,authorization_endpoint:issuer+'/authorize',token_endpoint:issuer+'/token',jwks_uri:issuer+'/jwks',code_challenge_methods_supported:['S256']}));
   if(u.pathname==='/jwks')return res.end(JSON.stringify({keys:[jwk]}));
   if(u.pathname==='/authorize'){codes.set(u.searchParams.get('state'),{nonce:u.searchParams.get('nonce'),challenge:u.searchParams.get('code_challenge')});return res.end(JSON.stringify({code:u.searchParams.get('state')}));}
   if(u.pathname==='/token'){
    let text='';for await(const c of req)text+=c;const p=new URLSearchParams(text),tx=codes.get(p.get('code'));codes.delete(p.get('code'));
    if(!tx||createHash('sha256').update(p.get('code_verifier')).digest('base64url')!==tx.challenge)return json(res,400,{});
    const now=Math.floor(Date.now()/1000), data=Buffer.from(JSON.stringify({alg:'RS256',kid:'fixture'})).toString('base64url')+'.'+Buffer.from(JSON.stringify({iss:issuer,aud:'fixture',sub:'user',name:'Fixture',iat:now,exp:now+600,nonce:tx.nonce})).toString('base64url');return res.end(JSON.stringify({id_token:data+'.'+sign('RSA-SHA256',Buffer.from(data),pair.privateKey).toString('base64url')}));
   }
   return json(res,404,{});
  });await new Promise(r=>idp.listen(8084,'0.0.0.0',r));
 }
 const server=await createAuthServer({mode,backendHealth:'/api/health',provider:mode==='sso'?{issuer:'http://127.0.0.1:8084',clientId:'fixture',publicOrigin:'http://127.0.0.1:19393',allowInsecureLocalhost:true}:null});server.listen(8082,'0.0.0.0');
}
