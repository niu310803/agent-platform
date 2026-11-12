// Only validated, normalized paths are interpolated. No manifest-supplied Nginx text.
export function renderNginx(m) {
  const a=m.authentication, sso=a.mode==='sso', custom=a.mode==='custom';
  const gate=a.mode==='none'?'':`set $auth_method $request_method;
      auth_request /__internal/auth/check;
      auth_request_set $app_user $upstream_http_x_webapp_user;
      auth_request_set $app_name $upstream_http_x_webapp_name;`;
  const identity=`proxy_set_header X-Webapp-User ${a.mode==='none'?'""':'$app_user'};
      proxy_set_header X-Webapp-Name ${a.mode==='none'?'""':'$app_name'};
      proxy_set_header X-Webapp-Auth-Mode ${a.mode};
      proxy_set_header Remote-User "";
      proxy_set_header X-Remote-User "";
      proxy_set_header X-Forwarded-User "";
      proxy_set_header X-Auth-Request-User "";
      proxy_set_header X-Webapp-Roles "";
      proxy_set_header X-Webapp-Email "";
      proxy_set_header X-Webapp-Subject "";
      proxy_set_header X-Webapp-Issuer "";
      proxy_set_header X-Forwarded-Host $host;
      proxy_set_header X-Forwarded-Proto $scheme;
      proxy_set_header X-Forwarded-For $remote_addr;
      ${sso?'proxy_set_header Cookie ""; proxy_set_header Authorization ""; proxy_hide_header Set-Cookie;':''}`;
  const proxy=`proxy_pass http://backend:8081; proxy_http_version 1.1;
      proxy_set_header Host $host;
      ${identity}
      proxy_read_timeout 300s; proxy_buffering off;`;
  const staticFiles=`try_files $uri $uri/ ${m.frontend.routeConfig.navigationFallback?'/'+m.frontend.routeConfig.navigationFallback:'=404'};`;
  const protectedLocations=[];
  if(m.backend) for(const prefix of m.frontend.routeConfig.backendPrefixes) {
    protectedLocations.push(`location = ${prefix} { ${gate} ${proxy} }`,`location ^~ ${prefix}/ { ${gate} ${proxy} }`);
  }
  const publicLocations=[];
  if(custom) for(const p of new Set([a.loginPath,...(a.publicPaths||[])])) {
    const backend=m.frontend.routeConfig.backendPrefixes.some(x=>p===x||p.startsWith(x+'/'));
    publicLocations.push(`location ${p.endsWith('/')?'^~':'='} ${p} { auth_request off; ${backend?proxy:staticFiles} }`);
  }
  return `worker_processes auto;
pid /tmp/nginx.pid;
error_log /logs/nginx-error.log warn;
events { worker_connections 512; }
http {
  include /etc/nginx/mime.types;
  default_type application/octet-stream;
  log_format webapp '$remote_addr [$time_local] "$request_method $uri $server_protocol" $status $body_bytes_sent';
  access_log /logs/nginx-access.log webapp;
  server_tokens off;
  client_body_temp_path /tmp/client-body;
  proxy_temp_path /tmp/proxy;
  fastcgi_temp_path /tmp/fastcgi;
  uwsgi_temp_path /tmp/uwsgi;
  scgi_temp_path /tmp/scgi;
  map $request_uri $auth_original_uri { default $request_uri; }
  server {
    listen 8080;
    root /frontend;
    index ${m.frontend.index};
    client_max_body_size 128k;
    add_header X-Content-Type-Options nosniff always;
    add_header Referrer-Policy same-origin always;
    location = /__webapp/bootstrap.json { alias /public/bootstrap.json; add_header Cache-Control no-store; }
    location = /__webapp/client.js { alias /public/client.js; add_header Cache-Control no-store; }
    location = /__webapp { return 404; }
    location ^~ /__webapp/ { return 404; }
    location = /__desktop { return 404; }
    location ^~ /__desktop/ { return 404; }
    ${a.mode==='none'?'':`location = /__internal/auth/check {
      internal; proxy_pass http://${custom?'backend:8081'+a.checkPath:'auth:8082/__internal/auth/check'};
      proxy_pass_request_body off; proxy_set_header Content-Length "";
      proxy_set_header X-Original-Method $auth_method; proxy_set_header X-Original-URI $auth_original_uri;
      proxy_set_header X-Original-Origin $http_origin; proxy_set_header X-Webapp-CSRF $http_x_webapp_csrf;
      proxy_set_header Cookie $http_cookie; proxy_set_header Authorization $http_authorization;
      proxy_set_header X-Webapp-User ""; proxy_set_header X-Webapp-Name "";
      proxy_set_header X-Webapp-Roles ""; proxy_set_header X-Webapp-Email "";
      proxy_set_header X-Webapp-Subject ""; proxy_set_header X-Webapp-Issuer "";
      proxy_set_header Remote-User ""; proxy_set_header X-Remote-User "";
      proxy_set_header X-Forwarded-User ""; proxy_set_header X-Auth-Request-User "";
      proxy_set_header X-Webapp-Auth-Mode custom;
    }`}
    location = /__internal { return 404; }
    location ^~ /__internal/ { return 404; }
    location = /__auth { return 404; }
    ${sso?`location ^~ /__auth/ {
      error_log /logs/nginx-error.log crit;
      proxy_pass http://auth:8082; proxy_set_header Host $host;
      proxy_set_header X-Webapp-User ""; proxy_set_header X-Webapp-Name "";
      proxy_set_header X-Forwarded-Proto $scheme;
    }`:'location ^~ /__auth/ { return 404; }'}
    ${custom?`location = ${a.checkPath} { return 404; }`:''}
    ${publicLocations.join('\n    ')}
    ${protectedLocations.join('\n    ')}
    location / { ${gate} ${a.mode==='none'?'':`error_page 401 =302 ${sso?'/__auth/login':a.loginPath};`} ${staticFiles} }
  }
}
`;
}
