// Public same-origin client: no Platform URL, ticket or service token.
export const bootstrap = await fetch('/__webapp/bootstrap.json',{cache:'no-store'}).then(r=>{if(!r.ok)throw Error('WebApp unavailable');return r.json();});
export async function request(url, options={}) {
  if(!url.startsWith('/')||url.startsWith('//'))throw Error('same-origin path required');
  const headers=new Headers(options.headers);
  if(bootstrap.authentication.mode==='sso'&&!['GET','HEAD'].includes((options.method||'GET').toUpperCase())) {
    const r=await fetch('/__auth/csrf',{cache:'no-store'});
    if(!r.ok)throw Error('请先登录');
    headers.set('X-Webapp-CSRF',(await r.json()).csrf);
  }
  const response=await fetch(url,{...options,headers,credentials:'same-origin'});
  if(response.status===401 && bootstrap.authentication.loginPath) {
    location.assign(bootstrap.authentication.loginPath); throw Error('需要登录');
  }
  const value=await response.json();
  if(!response.ok)throw Error(value.error||value.msg||`请求失败：${response.status}`);
  return value;
}
