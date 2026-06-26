#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
WORK_DIR=${DM_E2E_WORK_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/dm-e2e-XXXXXX")}
SOURCE_IMAGE=${DM_E2E_IMAGE:-busybox:latest}
REGISTRY_IMAGE=${DM_E2E_REGISTRY_IMAGE:-registry:2}
SUFFIX=${DM_E2E_SUFFIX:-$(date +%s)}
REGISTRY_NAME="dm_e2e_registry_${SUFFIX}"
CONTAINER_NAME="dm_e2e_container_${SUFFIX}"
RESTORED_NAME="dm_e2e_restored_${SUFFIX}"
SOURCE_LOCAL_TAG="dm-e2e-source-${SUFFIX}/busybox:latest"
TARGET_NAMESPACE="dm-e2e-target-${SUFFIX}"
BACKUP_DIR="${WORK_DIR}/backup"
BACKUP_ARCHIVE="${WORK_DIR}/container-backup.tar.gz"
DM_BIN=${DM_E2E_DM_BIN:-"${WORK_DIR}/dm"}
GOFLAGS_VALUE=${DM_E2E_GOFLAGS:-${GOFLAGS:-}}

cleanup() {
  docker rm -f "${CONTAINER_NAME}" "${RESTORED_NAME}" "${REGISTRY_NAME}" >/dev/null 2>&1 || true
  if [ -n "${REGISTRY:-}" ]; then
    docker image rm "${REGISTRY}/${SOURCE_LOCAL_TAG}" >/dev/null 2>&1 || true
    docker image ls --format '{{.Repository}}:{{.Tag}}' |
      grep -E "^${REGISTRY}/${TARGET_NAMESPACE}/" |
      xargs -r docker image rm >/dev/null 2>&1 || true
  fi
  docker image rm "${SOURCE_LOCAL_TAG}" >/dev/null 2>&1 || true
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

ensure_image() {
  local image="$1"
  if docker image inspect "${image}" >/dev/null 2>&1; then
    return 0
  fi
  if [ "${DM_E2E_OFFLINE:-0}" = "1" ]; then
    echo "本地不存在镜像 ${image}，且 DM_E2E_OFFLINE=1，无法继续。" >&2
    echo "请预先执行: docker pull ${image}" >&2
    exit 1
  fi
  log "本地不存在镜像 ${image}，尝试 docker pull"
  docker pull "${image}" >/dev/null
}

registry_port() {
  docker port "${REGISTRY_NAME}" 5000/tcp | sed 's/.*://'
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

build_dm() {
  mkdir -p "${WORK_DIR}"
  (
    cd "${ROOT_DIR}"
    if [ -z "${GOFLAGS_VALUE}" ] && [ -d vendor ]; then
      GOFLAGS_VALUE="-mod=vendor"
    fi
    if [ -n "${GOFLAGS_VALUE}" ]; then
      echo "使用 GOFLAGS=${GOFLAGS_VALUE}"
      GOFLAGS="${GOFLAGS_VALUE}" go build -o "${DM_BIN}" .
    else
      go build -o "${DM_BIN}" .
    fi
  )
}

need_cmd docker

log "构建 dm 测试二进制"
if [ -n "${DM_E2E_DM_BIN:-}" ]; then
  if [ ! -x "${DM_BIN}" ]; then
    echo "DM_E2E_DM_BIN 指向的文件不可执行: ${DM_BIN}" >&2
    exit 1
  fi
  echo "使用已有 dm 二进制: ${DM_BIN}"
else
  need_cmd go
  build_dm
fi
"${DM_BIN}" version

log "准备测试镜像"
ensure_image "${REGISTRY_IMAGE}"
ensure_image "${SOURCE_IMAGE}"

log "启动临时 registry ${REGISTRY_NAME}"
docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
docker run -d --name "${REGISTRY_NAME}" -p "127.0.0.1::5000" "${REGISTRY_IMAGE}" >/dev/null
REGISTRY="127.0.0.1:$(registry_port)"
TARGET_PREFIX="${REGISTRY}/${TARGET_NAMESPACE}"
SOURCE_REGISTRY_IMAGE="${REGISTRY}/${SOURCE_LOCAL_TAG}"
wait_for_registry

log "seed 本地临时 registry"
docker tag "${SOURCE_IMAGE}" "${SOURCE_REGISTRY_IMAGE}"
docker push "${SOURCE_REGISTRY_IMAGE}" >/dev/null

log "测试 registry-login-check --plain-http"
"${DM_BIN}" registry-login-check "${REGISTRY}" --plain-http

log "测试 pull --plain-http --output"
"${DM_BIN}" pull "${SOURCE_REGISTRY_IMAGE}" --plain-http --output "${WORK_DIR}/pull-local.tar"
test -f "${WORK_DIR}/pull-local.tar"

log "测试 pull --plain-http --load"
"${DM_BIN}" pull "${SOURCE_REGISTRY_IMAGE}" --plain-http --load --output "${WORK_DIR}/pull-load.tar"
test -f "${WORK_DIR}/pull-load.tar"

log "测试 pull --to 推送到临时 registry"
"${DM_BIN}" pull "${SOURCE_REGISTRY_IMAGE}" --to "${TARGET_PREFIX}" --plain-http --output-dir "${WORK_DIR}/pulled"
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
"${DM_BIN}" backup container "${CONTAINER_NAME}" --bundle --output-dir "${BACKUP_DIR}" --bundle-output "${BACKUP_ARCHIVE}"
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
