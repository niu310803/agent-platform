FROM alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2
RUN test "$(apk --print-arch)" = x86_64 \
    && apk add --no-cache nodejs~24 nginx ca-certificates tini \
    && test "$(node -p 'process.versions.node.split(".")[0]')" = 24 \
    && nginx -V 2>&1 | grep -q -- --with-http_auth_request_module \
    && addgroup -g 10001 webapp && adduser -D -u 10001 -G webapp webapp
COPY build/webapp-image/agent-platform /usr/local/bin/agent-platform
COPY deploy/webapp/host /opt/webapp-host
USER 10001:10001
ENTRYPOINT ["/sbin/tini", "--", "node", "/opt/webapp-host/main.mjs"]
