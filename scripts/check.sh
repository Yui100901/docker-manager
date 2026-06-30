#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
RUN_RACE=0
RUN_SHELLCHECK=1

usage() {
  cat <<'EOF'
Usage: scripts/check.sh [options]

Run local static checks without creating build artifacts.

Options:
  --race           Run go test -race ./... with CGO_ENABLED=1
  --no-shellcheck  Skip shellcheck even when it is available
  -h, --help       Show this help
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --race)
      RUN_RACE=1
      shift
      ;;
    --no-shellcheck)
      RUN_SHELLCHECK=0
      shift
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

cd "${ROOT_DIR}"

echo "==> gofmt check"
gofmt_files=$(gofmt -l $(find . -path ./vendor -prune -o -name '*.go' -print))
if [ -n "${gofmt_files}" ]; then
  echo "${gofmt_files}" >&2
  echo "Run gofmt on the files above." >&2
  exit 1
fi

echo "==> go test ./..."
go test ./...

echo "==> go vet ./..."
go vet ./...

if [ "${RUN_RACE}" = "1" ]; then
  echo "==> go test -race ./..."
  CGO_ENABLED=1 go test -race ./...
fi

echo "==> git diff --check"
git diff --check

echo "==> bash syntax"
bash -n scripts/*.sh

if [ "${RUN_SHELLCHECK}" = "1" ]; then
  if command -v shellcheck >/dev/null 2>&1; then
    echo "==> shellcheck"
    shellcheck scripts/*.sh
  else
    echo "shellcheck not found; skipped"
  fi
fi

echo "All checks passed."
