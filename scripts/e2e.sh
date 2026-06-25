#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
WORK_DIR=${DM_E2E_WORK_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/dm-e2e-XXXXXX")}
REGISTRY_PORT=${DM_E2E_REGISTRY_PORT:-5005}
SOURCE_IMAGE=${DM_E2E_IMAGE:-busybox:latest}
SUFFIX=${DM_E2E_SUFFIX:-$(date +%s)}
REGISTRY_NAME="dm_e2e_registry_${SUFFIX}"
CONTAINER_NAME="dm_e2e_container_${SUFFIX}"
RESTORED_NAME="dm_e2e_restored_${SUFFIX}"
REGISTRY="127.0.0.1:${REGISTRY_PORT}"
TARGET_PREFIX="${REGISTRY}/dm-e2e-${SUFFIX}"
BACKUP_DIR="${WORK_DIR}/backup"
BACKUP_ARCHIVE="${WORK_DIR}/container-backup.tar.gz"
DM_BIN="${WORK_DIR}/dm"

cleanup() {
  docker rm -f "${CONTAINER_NAME}" "${RESTORED_NAME}" "${REGISTRY_NAME}" >/dev/null 2>&1 || true
  if [ "${DM_E2E_KEEP_WORKDIR:-0}" != "1" ]; then
    rm -rf "${WORK_DIR}"
  else
    echo "保留测试目录: ${WORK_DIR}"
  fi
}
trap cleanup EXIT

log() {
  printf '\n==> %s\n' "$*"
}

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "缺少命令: $1" >&2
    exit 127
  fi
}

wait_for_registry() {
  local attempts=30
  local i
  for i in $(seq 1 "${attempts}"); do
    if "${DM_BIN}" registry-login-check "${REGISTRY}" --plain-http --format json >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "registry 未在 ${attempts}s 内就绪: ${REGISTRY}" >&2
  return 1
}

need_cmd go
need_cmd docker

log "构建 dm 测试二进制"
mkdir -p "${WORK_DIR}"
(
  cd "${ROOT_DIR}"
  go build -o "${DM_BIN}" .
)
"${DM_BIN}" version

log "启动临时 registry ${REGISTRY_NAME} (${REGISTRY})"
docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
docker run -d --name "${REGISTRY_NAME}" -p "127.0.0.1:${REGISTRY_PORT}:5000" registry:2 >/dev/null
wait_for_registry

log "测试 registry-login-check --plain-http"
"${DM_BIN}" registry-login-check "${REGISTRY}" --plain-http

log "测试 pull --to 推送到临时 registry"
"${DM_BIN}" pull "${SOURCE_IMAGE}" --to "${TARGET_PREFIX}" --plain-http --output-dir "${WORK_DIR}/pulled"
TARGET_IMAGE=$(docker images --format '{{.Repository}}:{{.Tag}}' | awk -v prefix="${TARGET_PREFIX}/" 'index($0, prefix) == 1 && $0 !~ /:<none>$/ { print; exit }')
if [ -z "${TARGET_IMAGE}" ]; then
  echo "未找到 pull --to 生成的目标镜像，前缀: ${TARGET_PREFIX}" >&2
  exit 1
fi
docker pull "${TARGET_IMAGE}" >/dev/null

log "创建测试容器 ${CONTAINER_NAME}"
docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
docker run -d --name "${CONTAINER_NAME}" "${TARGET_IMAGE}" sh -c "while true; do sleep 3600; done" >/dev/null

log "测试 backup container --bundle"
"${DM_BIN}" backup container "${CONTAINER_NAME}" --bundle --output-dir "${BACKUP_DIR}" --output "${BACKUP_ARCHIVE}"
test -f "${BACKUP_ARCHIVE}"
test -f "${BACKUP_DIR}/manifest.json"
test -f "${BACKUP_DIR}/checksums.txt"
test -f "${BACKUP_DIR}/README.md"
test -f "${BACKUP_DIR}/restore.sh"

log "删除原容器并测试 restore archive"
docker rm -f "${CONTAINER_NAME}" >/dev/null
"${DM_BIN}" restore "${BACKUP_ARCHIVE}" --name "${RESTORED_NAME}" --no-start
docker inspect "${RESTORED_NAME}" >/dev/null

log "端到端集成测试通过"
