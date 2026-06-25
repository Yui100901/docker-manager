#!/bin/bash
set -euo pipefail

VERSION=${VERSION:-dev}
COMMIT=${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}
BUILD_DATE=${BUILD_DATE:-$(date -u +"%Y-%m-%dT%H:%M:%SZ")}
LDFLAGS="-s -w -X docker-manager/internal/version.version=${VERSION} -X docker-manager/internal/version.commit=${COMMIT} -X docker-manager/internal/version.buildDate=${BUILD_DATE}"

mkdir -p ./bin/linux ./bin/windows

echo "Build for Linux ${VERSION} ${COMMIT}"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o ./bin/linux/dm .

echo "Build for Windows ${VERSION} ${COMMIT}"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o ./bin/windows/dm.exe .

echo "Build completed"
