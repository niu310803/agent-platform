#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
version=$(sed 's/^v//' VERSION)
mkdir -p build/webapp-image
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/webapp-image/agent-platform ./cmd/agent-platform
docker build --platform linux/amd64 -f deploy/webapp/Dockerfile -t "agent-platform-webapp-base:$version" "$@" .
