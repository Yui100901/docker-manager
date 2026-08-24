#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
RUN_RACE=0
RUN_SHELLCHECK=1
RUN_GO_CHECKS=1

usage() {
  cat <<'EOF'
Usage: scripts/check.sh [options]

Run local static checks without creating build artifacts.

Options:
  --race           Run go test -race ./... with CGO_ENABLED=1
  --no-go-checks   Skip go test, go vet and race after CI ran them separately
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
    --no-go-checks)
      RUN_GO_CHECKS=0
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

if [ "${RUN_GO_CHECKS}" -eq 0 ] && [ "${RUN_RACE}" -eq 1 ]; then
  echo "--race and --no-go-checks cannot be used together." >&2
  exit 2
fi

cd "${ROOT_DIR}"

echo "==> gofmt check"
gofmt_files=$(find . -path ./vendor -prune -o -name '*.go' -exec gofmt -l {} +)
if [ -n "${gofmt_files}" ]; then
  echo "${gofmt_files}" >&2
  echo "Run gofmt on the files above." >&2
  exit 1
fi

echo "==> repository text encoding"
go run ./scripts/text-check.go

if [ "${RUN_GO_CHECKS}" -eq 1 ]; then
  echo "==> go test ./..."
  go test ./...

  echo "==> go vet ./..."
  go vet ./...

  if [ "${RUN_RACE}" -eq 1 ]; then
    echo "==> go test -race ./..."
    CGO_ENABLED=1 go test -race ./...
  fi
else
  echo "==> go test, go vet and race explicitly skipped (--no-go-checks)"
fi

echo "==> git diff HEAD --check"
git diff HEAD --check

echo "==> bash syntax"
bash -n scripts/*.sh

if [ "${RUN_SHELLCHECK}" = "1" ]; then
  if command -v shellcheck >/dev/null 2>&1; then
    echo "==> shellcheck"
    shellcheck scripts/*.sh
  else
    echo "shellcheck not found. Install ShellCheck or explicitly pass --no-shellcheck." >&2
    exit 127
  fi
else
  echo "==> shellcheck explicitly skipped (--no-shellcheck)"
fi

echo "All requested checks passed."
