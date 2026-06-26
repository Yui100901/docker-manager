#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
OUTPUT=""
VERSION="${VERSION:-dev}"
COMMIT="${COMMIT:-$(git -C "${ROOT_DIR}" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
BUILD_DATE="${BUILD_DATE:-$(date -u +"%Y-%m-%dT%H:%M:%SZ")}"
RUN_TESTS=1
RUN_VET=0
RACE=0
EXTRA_GOFLAGS="${GOFLAGS:-}"

usage() {
  cat <<'EOF'
Usage: scripts/dev-build.sh [options]

Build a local development dm binary for the current Go platform.

Options:
  --output PATH       Output binary path. Default: bin/dev/dm(.exe)
  --version VALUE     Version injected into dm version. Default: VERSION or dev
  --commit VALUE      Commit injected into dm version. Default: git short HEAD
  --build-date VALUE  Build date injected into dm version. Default: current UTC time
  --no-test           Skip go test ./...
  --vet               Run go vet ./...
  --race              Build with -race
  --goflags VALUE     Extra GOFLAGS for test/vet/build
  -h, --help          Show this help
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --output)
      OUTPUT=${2:?missing value for --output}
      shift 2
      ;;
    --version)
      VERSION=${2:?missing value for --version}
      shift 2
      ;;
    --commit)
      COMMIT=${2:?missing value for --commit}
      shift 2
      ;;
    --build-date)
      BUILD_DATE=${2:?missing value for --build-date}
      shift 2
      ;;
    --no-test)
      RUN_TESTS=0
      shift
      ;;
    --vet)
      RUN_VET=1
      shift
      ;;
    --race)
      RACE=1
      shift
      ;;
    --goflags)
      EXTRA_GOFLAGS=${2:?missing value for --goflags}
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

GOOS_VALUE=$(go env GOOS)
GOARCH_VALUE=$(go env GOARCH)
if [ -z "${OUTPUT}" ]; then
  suffix=""
  if [ "${GOOS_VALUE}" = "windows" ]; then
    suffix=".exe"
  fi
  OUTPUT="${ROOT_DIR}/bin/dev/dm${suffix}"
fi

LDFLAGS="-X docker-manager/internal/version.version=${VERSION} -X docker-manager/internal/version.commit=${COMMIT} -X docker-manager/internal/version.buildDate=${BUILD_DATE}"
BUILD_ARGS=(-trimpath -ldflags "${LDFLAGS}")
if [ "${RACE}" = "1" ]; then
  BUILD_ARGS=(-race "${BUILD_ARGS[@]}")
fi

mkdir -p "$(dirname "${OUTPUT}")"

if [ "${RUN_TESTS}" = "1" ]; then
  echo "==> go test ./..."
  (cd "${ROOT_DIR}" && GOFLAGS="${EXTRA_GOFLAGS}" go test ./...)
fi

if [ "${RUN_VET}" = "1" ]; then
  echo "==> go vet ./..."
  (cd "${ROOT_DIR}" && GOFLAGS="${EXTRA_GOFLAGS}" go vet ./...)
fi

echo "==> build ${GOOS_VALUE}/${GOARCH_VALUE} ${VERSION} ${COMMIT}"
(cd "${ROOT_DIR}" && GOFLAGS="${EXTRA_GOFLAGS}" go build "${BUILD_ARGS[@]}" -o "${OUTPUT}" .)

echo "Built: ${OUTPUT}"
"${OUTPUT}" version
