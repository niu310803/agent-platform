import fs from 'node:fs/promises';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { generateKeyPairSync, createHash } from 'node:crypto';
import { renderNginx } from './nginx.mjs';

export function validateImageManifest(m) {
  m.frontend.routeConfig ??= {};
  m.frontend.routeConfig.backendPrefixes ??= [];
  const a = m.authentication;
  if (m.hosting !== 'platform' || !a || !['sso', 'custom', 'none'].includes(a.mode)) throw Error('image requires explicit authentication.mode: sso/custom/none');
  if (a.mode === 'sso' && (a.protocol !== 'oidc' || !/^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$/.test(a.providerRef))) throw Error('sso requires oidc and providerRef');
  const safe = p => /^\/[A-Za-z0-9_./-]+$/.test(p) && p !== '/' && !p.includes('//') && !p.split('/').some(x => x === '.' || x === '..') && !/^\/__(webapp|auth|internal|desktop)(\/|$)/.test(p);
  if (!safe('/'+m.frontend.index) || (m.frontend.routeConfig.navigationFallback && !safe('/'+m.frontend.routeConfig.navigationFallback))) throw Error('image frontend paths must be normalized');
  for (const p of m.frontend.routeConfig.backendPrefixes) if (!safe(p) || p.endsWith('/')) throw Error('unsafe backend prefix');
  if (m.backend && (m.backend.command.type !== 'node' || !/^backend\/[A-Za-z0-9_./-]+$/.test(m.backend.command.entry))) throw Error('image supports Node backend only');
  if (a.mode === 'custom') {
    const pub = [a.loginPath, ...(a.publicPaths || [])];
    if (!m.backend || !safe(a.checkPath) || a.checkPath.endsWith('/') || pub.some(p => !safe(p)) || pub.some(p => p === a.checkPath || p.endsWith('/') && a.checkPath.startsWith(p))) throw Error('invalid custom auth paths');
  }
  return m;
}
export function providerConfig(manifest, providers) {
  if (manifest.authentication.mode !== 'sso') return null;
  const p = providers?.[manifest.authentication.providerRef];
  if (!p?.issuer || !p?.clientId || !p?.publicOrigin) throw Error('missing OIDC provider configuration');
  const origin = new URL(p.publicOrigin); const issuer = new URL(p.issuer);
  if (origin.origin !== p.publicOrigin || issuer.hash || issuer.search || issuer.username || issuer.password) throw Error('invalid OIDC URL');
  const local = u => ['localhost','127.0.0.1','[::1]'].includes(u.hostname);
  if (origin.protocol !== 'https:' && !(origin.protocol === 'http:' && local(origin) && p.allowInsecureLocalhost === true)) throw Error('OIDC publicOrigin requires HTTPS');
  if (issuer.protocol !== 'https:' && !(issuer.protocol === 'http:' && local(issuer) && p.allowInsecureLocalhost === true)) throw Error('OIDC issuer requires HTTPS');
  if (p.clientSecret !== undefined && typeof p.clientSecret !== 'string') throw Error('invalid OIDC secret');
  if (p.tokenEndpointAuthMethod && !['client_secret_basic','client_secret_post'].includes(p.tokenEndpointAuthMethod)) throw Error('unsupported OIDC client authentication');
  return p;
}
async function json(file, value) { await fs.writeFile(file, JSON.stringify(value,null,2)+'\n',{mode:0o600}); }
async function missing(file) { try { await fs.lstat(file); return false; } catch(e) { if(e.code === 'ENOENT') return true; throw e; } }
// Preserve ordinary runtime sections; the image owns these three policies.
// This is a block projection, not a second YAML parser: Platform still validates
// the resulting YAML (including duplicate or unsupported keys) before listening.
export function runtimeConfig(base, service) {
  let skip=false;const kept=[];
  for(const line of base.split('\n')) {
    if(/^[^\s#]/.test(line))skip=/^(auth|automation|memory)\s*:/.test(line);
    if(!skip)kept.push(line);
  }
  const proxy=/^http-proxy\s*:/m.test(base)?'':'http-proxy:\n  mode: direct\n';
  return kept.join('\n')+`\nauth:\n  enabled: true\n  issuer: ${service.issuer}\n  audience: ${service.audience}\n  allowed-subjects: [${service.subject}]\n  require-expiry: true\nautomation:\n  enabled: false\nmemory:\n  worker:\n    enabled: false\n`+proxy;
}
// Package defaults never overwrite instance configuration or credentials.
async function defaults(source, target) {
  if (await missing(source)) return;
  await fs.mkdir(target,{recursive:true});
  for (const e of await fs.readdir(source,{withFileTypes:true})) {
    const s=path.join(source,e.name), t=path.join(target,e.name);
    if(e.isSymbolicLink()) throw Error('symlink in package');
    if(e.isDirectory()) await defaults(s,t); else if(await missing(t)) await fs.copyFile(s,t);
  }
}
export async function prepare(source, runtime, update=false, binary='/usr/local/bin/agent-platform') {
  await fs.mkdir(runtime,{recursive:true});
  // Caller must stop all roles before prepare. A lock rejects concurrent installers.
  const lock=path.join(runtime,'.prepare-lock'); await fs.mkdir(lock);
  let stage;
  try {
    stage=await fs.mkdtemp(path.join(runtime,'.prepare-'));
    const app=path.join(stage,'app');
    execFileSync(binary,['webapp','unpack','--source',source,'--output',app],{stdio:'pipe'});
    const manifest=validateImageManifest(JSON.parse(await fs.readFile(path.join(app,'webapp.json'),'utf8')));
    const provider=providerConfig(manifest,JSON.parse(await fs.readFile(path.join(runtime,'config/auth.json'),'utf8').catch(e=>{if(e.code==='ENOENT')return '{}';throw e;})));
    const hash=createHash('sha256');
    async function digest(dir){for(const e of (await fs.readdir(dir,{withFileTypes:true})).sort((a,b)=>Buffer.compare(Buffer.from(a.name),Buffer.from(b.name)))){const f=path.join(dir,e.name),stat=await fs.stat(f);hash.update(JSON.stringify([path.relative(app,f),e.isDirectory(),stat.mode&0o777,e.isDirectory()?0:stat.size])+'\0');if(e.isDirectory())await digest(f);else hash.update(await fs.readFile(f));}}
    await digest(app); const revision=hash.digest('hex');
    const stateFile=path.join(runtime,'run/installation.json');
    const old=JSON.parse(await fs.readFile(stateFile,'utf8').catch(e=>{if(e.code==='ENOENT')return 'null';throw e;}));
    if(old && old.appId!==manifest.id) throw Error('runtime belongs to another app');
    if(old && old.revision!==revision && !update) throw Error('package changed: stop services and set WEBAPP_UPDATE=true');
    for(const dir of ['config','run','logs','chats','webapp-data','platform-runtime','.state/platform','host/public','host/nginx','host/backend','host/auth','host/platform/configs']) await fs.mkdir(path.join(runtime,dir),{recursive:true});
    const platform=path.join(app,'platform');
    for(const dir of ['agents','skills-center','connectors-center']) {
      const dest=path.join(runtime,dir); const src=path.join(platform,dir);
      // Only the package-declared resources are replaced; unrelated instance data stays.
      if((!old || old.revision!==revision) && !(await missing(src))) for(const e of await fs.readdir(src)) {
        await fs.rm(path.join(dest,e),{recursive:true,force:true});
        await fs.mkdir(dest,{recursive:true}); await fs.cp(path.join(src,e),path.join(dest,e),{recursive:true});
      }
    }
    await defaults(path.join(platform,'registries'),path.join(runtime,'registries'));
    await defaults(path.join(platform,'configs'),path.join(runtime,'config/platform'));
    const configRoot=path.join(runtime,'host/platform/configs');
    await fs.rm(configRoot,{recursive:true,force:true});
    await fs.cp(path.join(runtime,'config/platform'),configRoot,{recursive:true});
    const {privateKey,publicKey}=generateKeyPairSync('rsa',{modulusLength:2048,publicKeyEncoding:{type:'spki',format:'pem'},privateKeyEncoding:{type:'pkcs8',format:'pem'}});
    await fs.writeFile(path.join(runtime,'host/backend/platform-key.pem'),privateKey,{mode:0o600});
    await fs.writeFile(path.join(configRoot,'local-public-key.pem'),publicKey,{mode:0o644});
    const service={issuer:'webapp-image',audience:manifest.id,subject:'webapp-backend:'+manifest.id};
    await json(path.join(runtime,'host/backend/service.json'),service);
    const baseRuntime=await fs.readFile(path.join(runtime,'config/platform/runtime.yml'),'utf8').catch(e=>{if(e.code==='ENOENT')return '';throw e;});
    await fs.writeFile(path.join(configRoot,'runtime.yml'),runtimeConfig(baseRuntime,service));
    await json(path.join(runtime,'host/backend/app.json'),{...manifest,skills:manifest.copilot?.references.map(x=>({skillId:x.id,name:x.id}))||[]});
    await json(path.join(runtime,'host/auth/config.json'),{mode:manifest.authentication.mode,provider,backendHealth:manifest.backend?.health.type==='http'?manifest.backend.health.path:null});
    await json(path.join(runtime,'host/public/bootstrap.json'),{id:manifest.id,label:manifest.label,version:manifest.version,authentication:{mode:manifest.authentication.mode,loginPath:manifest.authentication.mode==='sso'?'/__auth/login':manifest.authentication.loginPath||null}});
    await fs.copyFile(new URL('./client.js',import.meta.url),path.join(runtime,'host/public/client.js'));
    await fs.writeFile(path.join(runtime,'host/nginx/nginx.conf'),renderNginx(manifest));
    await fs.rm(path.join(runtime,'app'),{recursive:true,force:true}); await fs.rename(app,path.join(runtime,'app'));
    await json(stateFile,{appId:manifest.id,revision,mode:manifest.authentication.mode,preparedAt:new Date().toISOString()});
    console.log(JSON.stringify({event:'prepared',appId:manifest.id,revision,authentication:manifest.authentication.mode}));
  } finally {if(stage)await fs.rm(stage,{recursive:true,force:true});await fs.rm(lock,{recursive:true,force:true});}
}
