#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
MODE=${DM_E2E_MODE:-smoke}
MODE_EXPLICIT=0
if [ -n "${DM_E2E_MODE+x}" ]; then
  MODE_EXPLICIT=1
fi
CONFIRM_DESTRUCTIVE=${DM_E2E_CONFIRM_DESTRUCTIVE:-0}
WORK_DIR=${DM_E2E_WORK_DIR:-"${TMPDIR:-/tmp}/dm-e2e-$$_${RANDOM}"}
WORK_DIR_OWNED=0
WORK_DIR_TOKEN="dm-e2e:$$_${RANDOM}_$(date +%s)"
WORK_DIR_SENTINEL=""
SOURCE_IMAGE=${DM_E2E_IMAGE:-busybox:latest}
REGISTRY_IMAGE=${DM_E2E_REGISTRY_IMAGE:-registry:2}
SUFFIX=${DM_E2E_SUFFIX:-$(date +%s)_$$_${RANDOM}}
LABEL_KEY=${DM_E2E_LABEL_KEY:-dm.e2e}
LABEL_VALUE=${DM_E2E_LABEL_VALUE:-${SUFFIX}}
LABEL="${LABEL_KEY}=${LABEL_VALUE}"
REGISTRY_NAME="dm_e2e_registry_${SUFFIX}"
REGISTRY_VOLUME_NAME="dm_e2e_registry_data_${SUFFIX}"
CONTAINER_NAME="dm_e2e_container_${SUFFIX}"
SECOND_CONTAINER_NAME="dm_e2e_container_b_${SUFFIX}"
RERUN_CONTAINER_NAME="dm_e2e_rerun_${SUFFIX}"
STOPPED_CONTAINER_NAME="dm_e2e_stopped_${SUFFIX}"
RESTORED_NAME="dm_e2e_restored_${SUFFIX}"
RESTORED_REPLACE_NAME="dm_e2e_replace_${SUFFIX}"
RUNTIME_PROBE_NAME="dm_e2e_probe_${SUFFIX}"
VOLUME_NAME="dm_e2e_volume_${SUFFIX}"
SOURCE_LOCAL_TAG="dm-e2e-source-${SUFFIX}/busybox:latest"
TARGET_NAMESPACE="dm-e2e-target-${SUFFIX}"
BACKUP_DIR="${WORK_DIR}/backup"
BACKUP_ARCHIVE="${WORK_DIR}/container-backup.tar.gz"
BATCH_BACKUP_DIR="${WORK_DIR}/backup-batch"
MERGED_BACKUP_DIR="${WORK_DIR}/backup-merged"
MERGED_BACKUP_ARCHIVE="${WORK_DIR}/backup-merged.tar.gz"
DM_BIN=${DM_E2E_DM_BIN:-"${WORK_DIR}/dm"}
GOFLAGS_VALUE=${DM_E2E_GOFLAGS:-${GOFLAGS:-}}
RESULTS_TSV="${WORK_DIR}/results.tsv"
REPORT_MD="${WORK_DIR}/e2e-report.md"
LOG_DIR="${WORK_DIR}/logs"
STALL_PID=""
STALL_PORT=""
STALL_PORT_FILE="${WORK_DIR}/stall-server.port"
STALL_SEEN_FILE="${WORK_DIR}/stall-server.seen"
STALL_LOG="${LOG_DIR}/stall-server.log"
DOCKER_SCOPE_CLAIMED=0

usage() {
  cat <<'EOF'
Usage: scripts/e2e.sh [--mode smoke|full|destructive|install|cancel]

Modes:
  smoke        Build or use dm, then run help/version/config/doctor checks without Docker.
  full         Run the complete Docker e2e matrix; requires --confirm-destructive.
  destructive  Alias of full; requires --confirm-destructive.
  install      Build or use dm, install into a temporary directory, verify wrapper/config, then uninstall.
  cancel       Verify SIGINT/context cancellation for long-running command paths.

Environment:
  DM_E2E_MODE       Default mode when --mode is not set.
  DM_E2E_WORK_DIR   New, non-existing directory for logs and temporary files.
  DM_E2E_DM_BIN     Existing dm binary; skips building when set.
  DM_E2E_CONFIRM_DESTRUCTIVE=1 confirms full/destructive mode.
  DM_E2E_KEEP_WORKDIR=1 keeps the work directory after the run.
  DM_E2E_CANCEL_EXIT_TIMEOUT=10 timeout in seconds after SIGINT before the child is terminated.
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --mode)
      MODE=${2:?missing value for --mode}
      MODE_EXPLICIT=1
      shift 2
      ;;
    --confirm-destructive)
      CONFIRM_DESTRUCTIVE=1
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

case "${MODE}" in
  smoke|full|destructive|install|cancel)
    ;;
  *)
    echo "Unsupported e2e mode: ${MODE}" >&2
    usage >&2
    exit 2
    ;;
esac

case "${SUFFIX}" in
  ""|*[!a-z0-9_.-]*)
    echo "DM_E2E_SUFFIX contains unsafe characters: ${SUFFIX}" >&2
    exit 2
    ;;
esac
case "${LABEL_KEY}" in
  ""|*[!A-Za-z0-9_.-]*)
    echo "DM_E2E_LABEL_KEY contains unsafe characters: ${LABEL_KEY}" >&2
    exit 2
    ;;
esac
case "${LABEL_VALUE}" in
  ""|*[!A-Za-z0-9_.-]*)
    echo "DM_E2E_LABEL_VALUE contains unsafe characters: ${LABEL_VALUE}" >&2
    exit 2
    ;;
esac

if [ "${MODE}" = "full" ] || [ "${MODE}" = "destructive" ]; then
  if [ "${MODE_EXPLICIT}" -ne 1 ]; then
    echo "full/destructive mode must be selected explicitly with --mode or DM_E2E_MODE." >&2
    exit 2
  fi
  if [ "${CONFIRM_DESTRUCTIVE}" != "1" ]; then
    echo "full/destructive mode requires --confirm-destructive or DM_E2E_CONFIRM_DESTRUCTIVE=1." >&2
    exit 2
  fi
fi

protected_work_dir() {
  local candidate=$1
  local home_dir=""
  if [ -n "${HOME:-}" ] && [ -d "${HOME}" ]; then
    home_dir=$(CDPATH='' cd -- "${HOME}" && pwd -P)
  fi
  [ "${candidate}" = "/" ] || [ "${candidate}" = "${ROOT_DIR}" ] || { [ -n "${home_dir}" ] && [ "${candidate}" = "${home_dir}" ]; }
}

prepare_work_dir() {
  local requested=$1 parent leaf parent_real candidate
  if [ -z "${requested}" ] || [ -e "${requested}" ] || [ -L "${requested}" ]; then
    echo "DM_E2E_WORK_DIR must name a new, non-existing directory: ${requested}" >&2
    return 1
  fi
  parent=$(dirname -- "${requested}")
  leaf=$(basename -- "${requested}")
  if [ -z "${leaf}" ] || [ "${leaf}" = "." ] || [ "${leaf}" = ".." ] || [ ! -d "${parent}" ]; then
    echo "Invalid DM_E2E_WORK_DIR: ${requested}" >&2
    return 1
  fi
  parent_real=$(CDPATH='' cd -- "${parent}" && pwd -P) || return 1
  candidate="${parent_real}/${leaf}"
  if protected_work_dir "${candidate}"; then
    echo "Refusing protected E2E work directory: ${candidate}" >&2
    return 1
  fi
  umask 077
  if ! mkdir -- "${candidate}"; then
    echo "Could not exclusively create E2E work directory: ${candidate}" >&2
    return 1
  fi
  WORK_DIR=${candidate}
  WORK_DIR_SENTINEL="${WORK_DIR}/.dm-e2e-owned"
  if ! printf '%s\n' "${WORK_DIR_TOKEN}" >"${WORK_DIR_SENTINEL}" || ! chmod 600 "${WORK_DIR_SENTINEL}"; then
    rm -f -- "${WORK_DIR_SENTINEL}" >/dev/null 2>&1 || true
    rmdir -- "${WORK_DIR}" >/dev/null 2>&1 || true
    return 1
  fi
  WORK_DIR_OWNED=1
}

remove_owned_work_dir() {
  if [ "${WORK_DIR_OWNED}" -ne 1 ] || [ -z "${WORK_DIR}" ] || [ -L "${WORK_DIR}" ] || [ ! -d "${WORK_DIR}" ]; then
    echo "Refusing to remove unowned E2E work directory: ${WORK_DIR}" >&2
    return 1
  fi
  if protected_work_dir "${WORK_DIR}" || [ -L "${WORK_DIR_SENTINEL}" ] || [ ! -f "${WORK_DIR_SENTINEL}" ]; then
    echo "Refusing to remove unsafe E2E work directory: ${WORK_DIR}" >&2
    return 1
  fi
  if [ "$(cat "${WORK_DIR_SENTINEL}")" != "${WORK_DIR_TOKEN}" ]; then
    echo "Refusing to remove E2E work directory with invalid ownership sentinel: ${WORK_DIR}" >&2
    return 1
  fi
  rm -rf -- "${WORK_DIR}"
  WORK_DIR_OWNED=0
}

prepare_work_dir "${WORK_DIR}"
trap 'remove_owned_work_dir >/dev/null 2>&1 || true' EXIT
BACKUP_DIR="${WORK_DIR}/backup"
BACKUP_ARCHIVE="${WORK_DIR}/container-backup.tar.gz"
BATCH_BACKUP_DIR="${WORK_DIR}/backup-batch"
MERGED_BACKUP_DIR="${WORK_DIR}/backup-merged"
MERGED_BACKUP_ARCHIVE="${WORK_DIR}/backup-merged.tar.gz"
if [ -z "${DM_E2E_DM_BIN:-}" ]; then
  DM_BIN="${WORK_DIR}/dm"
fi
RESULTS_TSV="${WORK_DIR}/results.tsv"
REPORT_MD="${WORK_DIR}/e2e-report.md"
LOG_DIR="${WORK_DIR}/logs"
STALL_PORT_FILE="${WORK_DIR}/stall-server.port"
STALL_SEEN_FILE="${WORK_DIR}/stall-server.seen"
STALL_LOG="${LOG_DIR}/stall-server.log"
mkdir -p "${LOG_DIR}"
printf 'case\tstatus\texit_code\tseconds\tlog\n' >"${RESULTS_TSV}"

cleanup() {
  if [ -n "${STALL_PID}" ]; then
    kill "${STALL_PID}" >/dev/null 2>&1 || true
    wait "${STALL_PID}" >/dev/null 2>&1 || true
  fi
  if [ "${DOCKER_SCOPE_CLAIMED}" -eq 1 ] && command -v docker >/dev/null 2>&1; then
    for owned_container in \
      "${CONTAINER_NAME}" \
      "${SECOND_CONTAINER_NAME}" \
      "${RERUN_CONTAINER_NAME}" \
      "${STOPPED_CONTAINER_NAME}" \
      "${RESTORED_NAME}" \
      "${RESTORED_REPLACE_NAME}" \
      "${RUNTIME_PROBE_NAME}" \
      "${REGISTRY_NAME}"; do
      if [ "$(docker inspect --format "{{ index .Config.Labels \"${LABEL_KEY}\" }}" "${owned_container}" 2>/dev/null || true)" = "${LABEL_VALUE}" ]; then
        docker rm -fv -- "${owned_container}" >/dev/null 2>&1 || true
      fi
    done
    for owned_volume in "${VOLUME_NAME}" "${REGISTRY_VOLUME_NAME}"; do
      if [ "$(docker volume inspect --format "{{ index .Labels \"${LABEL_KEY}\" }}" "${owned_volume}" 2>/dev/null || true)" = "${LABEL_VALUE}" ]; then
        docker volume rm -- "${owned_volume}" >/dev/null 2>&1 || true
      fi
    done
    if [ -n "${REGISTRY:-}" ]; then
      docker image rm "${REGISTRY}/${SOURCE_LOCAL_TAG}" >/dev/null 2>&1 || true
      docker image ls --format '{{.Repository}}:{{.Tag}}' |
        awk -v first="${REGISTRY}/${TARGET_NAMESPACE}/" -v second="${REGISTRY}/dm-e2e-" 'index($0, first) == 1 || index($0, second) == 1' |
        xargs -r docker image rm >/dev/null 2>&1 || true
    fi
    docker image rm "${SOURCE_LOCAL_TAG}" >/dev/null 2>&1 || true
  fi
  if [ "${DM_E2E_KEEP_WORKDIR:-0}" != "1" ]; then
    remove_owned_work_dir || true
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

safe_name() {
  printf '%s' "$1" | tr -c 'A-Za-z0-9_.-' '_'
}

record_result() {
  local name="$1"
  local status="$2"
  local code="$3"
  local seconds="$4"
  local log_file="$5"
  printf '%s\t%s\t%s\t%s\t%s\n' "${name}" "${status}" "${code}" "${seconds}" "${log_file}" >>"${RESULTS_TSV}"
}

run_case() {
  local name="$1"
  shift
  local log_file
  log_file="${LOG_DIR}/$(safe_name "${name}").log"
  local start end code status
  log "测试 ${name}"
  start=$(date +%s)
  set +e
  "$@" >"${log_file}" 2>&1
  code=$?
  set -e
  end=$(date +%s)
  if [ "${code}" -eq 0 ]; then
    status="PASS"
    printf 'PASS %s\n' "${name}"
  else
    status="FAIL"
    printf 'FAIL %s, exit=%s, log=%s\n' "${name}" "${code}" "${log_file}" >&2
    tail -n 80 "${log_file}" >&2 || true
    record_result "${name}" "${status}" "${code}" "$((end - start))" "${log_file}"
    exit "${code}"
  fi
  record_result "${name}" "${status}" "${code}" "$((end - start))" "${log_file}"
}

run_expect_fail() {
  local name="$1"
  shift
  local log_file
  log_file="${LOG_DIR}/$(safe_name "${name}").log"
  local start end code status
  log "测试 ${name} (期望失败)"
  start=$(date +%s)
  set +e
  "$@" >"${log_file}" 2>&1
  code=$?
  set -e
  end=$(date +%s)
  if [ "${code}" -ne 0 ]; then
    status="XFAIL"
    printf 'XFAIL %s, exit=%s\n' "${name}" "${code}"
    record_result "${name}" "${status}" "${code}" "$((end - start))" "${log_file}"
    return 0
  fi
  status="FAIL"
  printf 'FAIL %s, expected non-zero exit, log=%s\n' "${name}" "${log_file}" >&2
  record_result "${name}" "${status}" "${code}" "$((end - start))" "${log_file}"
  exit 1
}

wait_for_file() {
  local path="$1"
  local attempts="${2:-100}"
  local delay="${3:-0.1}"
  for _ in $(seq 1 "${attempts}"); do
    if [ -s "${path}" ] || [ -f "${path}" ]; then
      return 0
    fi
    sleep "${delay}"
  done
  return 1
}

start_stall_server() {
  local python_bin=""
  if command -v python3 >/dev/null 2>&1 && python3 -c 'import http.server' >/dev/null 2>&1; then
    python_bin="python3"
  elif command -v python >/dev/null 2>&1 && python -c 'import http.server' >/dev/null 2>&1; then
    python_bin="python"
  else
    echo "missing usable python3/python for cancel mode" >&2
    exit 127
  fi
  rm -f "${STALL_PORT_FILE}" "${STALL_SEEN_FILE}"
  cat >"${WORK_DIR}/stall_server.py" <<'PY'
import http.server
import os
import pathlib
import time

port_file = pathlib.Path(os.environ["DM_E2E_STALL_PORT_FILE"])
seen_file = pathlib.Path(os.environ["DM_E2E_STALL_SEEN_FILE"])

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self._stall()
    def do_HEAD(self):
        self._stall()
    def do_POST(self):
        self._stall()
    def do_PUT(self):
        self._stall()
    def do_DELETE(self):
        self._stall()
    def log_message(self, fmt, *args):
        return
    def _stall(self):
        seen_file.write_text(self.command + " " + self.path + "\n", encoding="utf-8")
        time.sleep(3600)

server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
port_file.write_text(str(server.server_port), encoding="utf-8")
server.serve_forever()
PY
  DM_E2E_STALL_PORT_FILE="${STALL_PORT_FILE}" \
    DM_E2E_STALL_SEEN_FILE="${STALL_SEEN_FILE}" \
    "${python_bin}" "${WORK_DIR}/stall_server.py" >"${STALL_LOG}" 2>&1 &
  STALL_PID=$!
  if ! wait_for_file "${STALL_PORT_FILE}" 100 0.1; then
    echo "stall server failed to start; log=${STALL_LOG}" >&2
    exit 1
  fi
  STALL_PORT=$(cat "${STALL_PORT_FILE}")
}

run_cancel_case() {
  local name="$1"
  shift
  local log_file
  log_file="${LOG_DIR}/$(safe_name "${name}").log"
  local start end code status watchdog
  log "测试 ${name} (cancel)"
  rm -f "${STALL_SEEN_FILE}"
  start=$(date +%s)
  set +e
  "$@" >"${log_file}" 2>&1 &
  local child=$!
  set -e

  if ! wait_for_cancel_request "${child}" "${name}" "${log_file}"; then
    return 1
  fi

  kill -INT "${child}" >/dev/null 2>&1 || true
  (
    sleep "${DM_E2E_CANCEL_EXIT_TIMEOUT:-10}"
    kill -TERM "${child}" >/dev/null 2>&1 || true
  ) &
  watchdog=$!
  set +e
  wait "${child}"
  code=$?
  set -e
  kill "${watchdog}" >/dev/null 2>&1 || true
  wait "${watchdog}" >/dev/null 2>&1 || true
  end=$(date +%s)

  if [ "${code}" -ne 130 ]; then
    status="FAIL"
    printf 'FAIL %s, expected exit 130 after SIGINT, got %s, log=%s\n' "${name}" "${code}" "${log_file}" >&2
    tail -n 80 "${log_file}" >&2 || true
    record_result "${name}" "${status}" "${code}" "$((end - start))" "${log_file}"
    exit 1
  fi
  if ! grep -q "操作已取消" "${log_file}"; then
    status="FAIL"
    printf 'FAIL %s, cancel output missing friendly message, log=%s\n' "${name}" "${log_file}" >&2
    tail -n 80 "${log_file}" >&2 || true
    record_result "${name}" "${status}" "${code}" "$((end - start))" "${log_file}"
    exit 1
  fi
  status="PASS"
  printf 'PASS %s\n' "${name}"
  record_result "${name}" "${status}" "${code}" "$((end - start))" "${log_file}"
}

wait_for_cancel_request() {
  local child="$1"
  local name="$2"
  local log_file="$3"
  for _ in $(seq 1 "${DM_E2E_CANCEL_REQUEST_ATTEMPTS:-100}"); do
    if [ -f "${STALL_SEEN_FILE}" ]; then
      return 0
    fi
    if ! kill -0 "${child}" >/dev/null 2>&1; then
      local code
      set +e
      wait "${child}"
      code=$?
      set -e
      printf 'FAIL %s, command exited before reaching cancel probe, exit=%s, log=%s\n' "${name}" "${code}" "${log_file}" >&2
      tail -n 80 "${log_file}" >&2 || true
      record_result "${name}" "FAIL" "${code}" "0" "${log_file}"
      exit 1
    fi
    sleep "${DM_E2E_CANCEL_REQUEST_DELAY:-0.1}"
  done
  kill -TERM "${child}" >/dev/null 2>&1 || true
  wait "${child}" >/dev/null 2>&1 || true
  printf 'FAIL %s, command did not reach cancel probe, log=%s\n' "${name}" "${log_file}" >&2
  tail -n 80 "${log_file}" >&2 || true
  record_result "${name}" "FAIL" "timeout" "0" "${log_file}"
  exit 1
}

ensure_image() {
  local image="$1"
  if docker image inspect "${image}" >/dev/null 2>&1; then
    return 0
  fi
  if [ -n "${DM_E2E_PROXY:-}" ]; then
    log "本地不存在镜像 ${image}，尝试通过 dm image pull --proxy 预拉并导入"
    "${DM_BIN}" image pull "${image}" --proxy "${DM_E2E_PROXY}" --load --output-dir "${WORK_DIR}/preload"
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
  for _ in $(seq 1 "${attempts}"); do
    if "${DM_BIN}" report registry "${REGISTRY}" --plain-http --format json >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "registry 未在 ${attempts}s 内就绪: ${REGISTRY}" >&2
  return 1
}

verify_docker_runtime() {
  if docker inspect "${RUNTIME_PROBE_NAME}" >/dev/null 2>&1; then
    echo "Docker runtime probe name is already in use: ${RUNTIME_PROBE_NAME}" >&2
    exit 1
  fi
  if ! timeout 20 docker create --name "${RUNTIME_PROBE_NAME}" --label "${LABEL}" "${SOURCE_IMAGE}" sh -c "echo dm-e2e-probe" >/dev/null; then
    echo "Docker 无法在 20s 内创建测试容器，full/destructive 测试无法继续。" >&2
    echo "请先确认 Docker/containerd 运行状态，或换用干净测试机。" >&2
    if [ "$(docker inspect --format "{{ index .Config.Labels \"${LABEL_KEY}\" }}" "${RUNTIME_PROBE_NAME}" 2>/dev/null || true)" = "${LABEL_VALUE}" ]; then
      docker rm -fv -- "${RUNTIME_PROBE_NAME}" >/dev/null 2>&1 || true
    fi
    exit 1
  fi
  if ! timeout 20 docker start -a "${RUNTIME_PROBE_NAME}" >/dev/null; then
    echo "Docker 无法在 20s 内启动测试容器，full/destructive 测试无法继续。" >&2
    echo "请先确认 Docker/containerd 运行状态，或换用干净测试机。" >&2
    if [ "$(docker inspect --format "{{ index .Config.Labels \"${LABEL_KEY}\" }}" "${RUNTIME_PROBE_NAME}" 2>/dev/null || true)" = "${LABEL_VALUE}" ]; then
      docker rm -fv -- "${RUNTIME_PROBE_NAME}" >/dev/null 2>&1 || true
    fi
    exit 1
  fi
  if [ "$(docker inspect --format "{{ index .Config.Labels \"${LABEL_KEY}\" }}" "${RUNTIME_PROBE_NAME}" 2>/dev/null || true)" = "${LABEL_VALUE}" ]; then
    docker rm -fv -- "${RUNTIME_PROBE_NAME}" >/dev/null 2>&1 || true
  fi
}

claim_docker_scope() {
  local candidate
  for candidate in \
    "${CONTAINER_NAME}" \
    "${SECOND_CONTAINER_NAME}" \
    "${RERUN_CONTAINER_NAME}" \
    "${STOPPED_CONTAINER_NAME}" \
    "${RESTORED_NAME}" \
    "${RESTORED_REPLACE_NAME}" \
    "${RUNTIME_PROBE_NAME}" \
    "${REGISTRY_NAME}"; do
    if docker inspect "${candidate}" >/dev/null 2>&1; then
      echo "Refusing to reuse existing Docker test container: ${candidate}" >&2
      exit 1
    fi
  done
  for candidate in "${VOLUME_NAME}" "${REGISTRY_VOLUME_NAME}"; do
    if docker volume inspect "${candidate}" >/dev/null 2>&1; then
      echo "Refusing to reuse existing Docker test volume: ${candidate}" >&2
      exit 1
    fi
  done
  if docker image inspect "${SOURCE_LOCAL_TAG}" >/dev/null 2>&1; then
    echo "Refusing to reuse existing Docker test image tag: ${SOURCE_LOCAL_TAG}" >&2
    exit 1
  fi
  DOCKER_SCOPE_CLAIMED=1
}

build_dm() {
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

write_report() {
  {
    echo "# docker-manager e2e 测试报告"
    echo
    echo "- 执行模式: \`${MODE}\`"
    echo "- 工作目录: \`${WORK_DIR}\`"
    echo "- 测试标签: \`${LABEL}\`"
    echo "- 测试镜像: \`${SOURCE_IMAGE}\`"
    echo "- 临时 registry: \`${REGISTRY:-not-started}\`"
    echo
    echo "| 用例 | 状态 | 退出码 | 耗时(s) | 日志 |"
    echo "| --- | --- | --- | --- | --- |"
    tail -n +2 "${RESULTS_TSV}" | while IFS=$'\t' read -r name status code seconds log_file; do
      echo "| ${name} | ${status} | ${code} | ${seconds} | ${log_file} |"
    done
  } >"${REPORT_MD}"
}

verify_install_manifest_contract() {
  local manifest="$1"
  local completion_base="$2"
  local data_dir="$3"
  local config_dir token
  config_dir=$(dirname -- "${manifest}")
  local mode
  if mode=$(stat -c '%a' "${manifest}" 2>/dev/null); then
    :
  elif mode=$(stat -f '%Lp' "${manifest}" 2>/dev/null); then
    :
  else
    echo "cannot inspect install manifest mode: ${manifest}" >&2
    return 1
  fi
  [ "${mode}" = "600" ] || {
    echo "install manifest mode is ${mode}, want 600" >&2
    return 1
  }
  grep -Fqx "DM_MANIFEST_VERSION='3'" "${manifest}"
  token=$(sed -n "s/^DM_INSTALL_TOKEN='\([0-9a-f][0-9a-f]*\)'$/\1/p" "${manifest}")
  [[ "${token}" =~ ^[0-9a-f]{32}$ ]]
  grep -Fqx "DM_COMPLETION_COUNT='4'" "${manifest}"
  grep -Eq '^DM_COMPLETION_FILE_0=' "${manifest}"
  grep -Eq '^DM_COMPLETION_FILE_1=' "${manifest}"
  grep -Eq '^DM_COMPLETION_FILE_2=' "${manifest}"
  grep -Eq '^DM_COMPLETION_FILE_3=' "${manifest}"
  [ -f "${completion_base}/bash-completion/completions/dm" ]
  [ -f "${completion_base}/zsh/site-functions/_dm" ]
  [ -f "${completion_base}/fish/vendor_completions.d/dm.fish" ]
  [ -f "${completion_base}/powershell/Completions/dm.ps1" ]
  [ "$(stat -c '%a' "${config_dir}/.docker-manager-managed")" = "600" ]
  [ "$(stat -c '%a' "${data_dir}/.docker-manager-managed")" = "600" ]
  grep -Fqx "role=config" "${config_dir}/.docker-manager-managed"
  grep -Fqx "path=${config_dir}" "${config_dir}/.docker-manager-managed"
  grep -Fqx "token=${token}" "${config_dir}/.docker-manager-managed"
  grep -Fqx "role=data" "${data_dir}/.docker-manager-managed"
  grep -Fqx "path=${data_dir}" "${data_dir}/.docker-manager-managed"
  grep -Fqx "token=${token}" "${data_dir}/.docker-manager-managed"
}

manifest_quote_for_test() {
  local value="$1"
  value=${value//\'/\'\\\'\'}
  printf "'%s'" "${value}"
}

verify_completion_file_state() {
  local expected="$1"
  local path="$2"
  case "${expected}" in
    1)
      [ -f "${path}" ]
      [ ! -L "${path}" ]
      ;;
    0)
      [ ! -e "${path}" ]
      [ ! -L "${path}" ]
      ;;
    *)
      echo "invalid expected completion state: ${expected}" >&2
      return 1
      ;;
  esac
}

verify_completion_state() {
  local manifest="$1"
  local completion_base="$2"
  local expected_count="$3"
  local expect_bash="$4"
  local expect_zsh="$5"
  local expect_fish="$6"
  local expect_powershell="$7"
  local actual_count indexed_count index path raw presence_count
  case "${expected_count}" in
    0|1|4) ;;
    *)
      echo "invalid expected completion count: ${expected_count}" >&2
      return 1
      ;;
  esac
  for raw in "${expect_bash}" "${expect_zsh}" "${expect_fish}" "${expect_powershell}"; do
    case "${raw}" in
      0|1) ;;
      *)
        echo "invalid expected completion presence flag: ${raw}" >&2
        return 1
      ;;
    esac
  done
  presence_count=$((expect_bash + expect_zsh + expect_fish + expect_powershell))
  [ "${presence_count}" -eq "${expected_count}" ] || {
    echo "completion presence flags total ${presence_count}, want ${expected_count}" >&2
    return 1
  }

  actual_count=$(sed -n "s/^DM_COMPLETION_COUNT='\([0-9][0-9]*\)'$/\1/p" "${manifest}")
  [ "${actual_count}" = "${expected_count}" ] || {
    echo "completion count is ${actual_count}, want ${expected_count}" >&2
    return 1
  }
  indexed_count=$(grep -Ec '^DM_COMPLETION_FILE_[0-9]+=' "${manifest}" || true)
  [ "${indexed_count}" -eq "${expected_count}" ] || {
    echo "indexed completion entry count is ${indexed_count}, want ${expected_count}" >&2
    return 1
  }

  for index in 0 1 2 3; do
    if [ "${index}" -lt "${expected_count}" ]; then
      case "${index}" in
        0) path="${completion_base}/bash-completion/completions/dm" ;;
        1) path="${completion_base}/zsh/site-functions/_dm" ;;
        2) path="${completion_base}/fish/vendor_completions.d/dm.fish" ;;
        3) path="${completion_base}/powershell/Completions/dm.ps1" ;;
      esac
      raw=$(manifest_quote_for_test "${path}")
      grep -Fqx "DM_COMPLETION_FILE_${index}=${raw}" "${manifest}" || {
        echo "manifest completion entry ${index} does not match ${path}" >&2
        return 1
      }
    else
      if grep -Eq "^DM_COMPLETION_FILE_${index}=" "${manifest}"; then
        echo "manifest contains completion entry outside count: ${index}" >&2
        return 1
      fi
    fi
  done

  verify_completion_file_state "${expect_bash}" "${completion_base}/bash-completion/completions/dm"
  verify_completion_file_state "${expect_zsh}" "${completion_base}/zsh/site-functions/_dm"
  verify_completion_file_state "${expect_fish}" "${completion_base}/fish/vendor_completions.d/dm.fish"
  verify_completion_file_state "${expect_powershell}" "${completion_base}/powershell/Completions/dm.ps1"
}

verify_install_rejection_preserved_state() {
  local wrapper="$1"
  local installed_binary="$2"
  local config_dir="$3"
  local data_dir="$4"
  local forbidden_marker="$5"
  [ -x "${wrapper}" ]
  [ -x "${installed_binary}" ]
  [ -d "${config_dir}" ]
  [ -d "${data_dir}" ]
  [ ! -e "${forbidden_marker}" ]
}

verify_failed_fresh_install_cleanup() {
  local prefix="$1"
  local config_dir="$2"
  local data_dir="$3"
  [ ! -e "${prefix}" ]
  [ ! -e "${config_dir}" ]
  [ ! -e "${data_dir}" ]
  [ ! -e "${prefix}/bin/dm" ]
  [ ! -e "${prefix}/lib/docker-manager/dm-bin" ]
  [ ! -e "${config_dir}/install.env" ]
  [ ! -e "${config_dir}/.docker-manager-managed" ]
  [ ! -e "${data_dir}/.docker-manager-managed" ]
}

verify_failed_reinstall_rollback() {
  local manifest="$1"
  local saved_manifest="$2"
  local config_marker="$3"
  local saved_config_marker="$4"
  local data_marker="$5"
  local saved_data_marker="$6"
  local wrapper="$7"
  local saved_wrapper="$8"
  local installed_binary="$9"
  shift 9
  local saved_installed_binary="$1"
  cmp -s "${saved_manifest}" "${manifest}"
  cmp -s "${saved_config_marker}" "${config_marker}"
  cmp -s "${saved_data_marker}" "${data_marker}"
  cmp -s "${saved_wrapper}" "${wrapper}"
  cmp -s "${saved_installed_binary}" "${installed_binary}"
}

run_bind_mount_ancestor_case() {
  local name="uninstall rejects bind-mounted ancestor"
  local log_file
  log_file="${LOG_DIR}/$(safe_name "${name}").log"
  local reason=""
  if [ "$(uname -s)" != "Linux" ]; then
    reason="Linux mount namespaces are required"
  elif [ "$(id -u)" -ne 0 ]; then
    reason="root/CAP_SYS_ADMIN is required to create a mount namespace"
  elif ! command -v unshare >/dev/null 2>&1 || ! command -v mount >/dev/null 2>&1 || ! command -v umount >/dev/null 2>&1; then
    reason="unshare/mount/umount are unavailable"
  elif ! unshare --mount --fork /bin/true >/dev/null 2>&1; then
    reason="mount namespace creation is not permitted"
  fi

  if [ -n "${reason}" ]; then
    printf 'SKIP %s: %s\n' "${name}" "${reason}"
    printf '%s\n' "SKIP: ${reason}" >"${log_file}"
    record_result "${name}" "SKIP" "0" "0" "${log_file}"
    return 0
  fi

  local fixture="${WORK_DIR}/bind-mount-ancestor"
  local helper="${fixture}/case.sh"
  mkdir -p "${fixture}"
  cat >"${helper}" <<'SH'
#!/usr/bin/env bash
set -euo pipefail

install_script="$1"
uninstall_script="$2"
dm_binary="$3"
fixture="$4"
mount_parent="${fixture}/mounted-parent"
source_tree="${fixture}/external-source"
prefix="${mount_parent}/prefix"
config_dir="${mount_parent}/config"
data_dir="${mount_parent}/data"
independent_parent="${fixture}/independent-parent"
independent_prefix="${independent_parent}/prefix"
independent_config_dir="${independent_parent}/config"
independent_data_dir="${independent_parent}/data"
root_bind_parent="${fixture}/root-bind-parent"
root_bind_source="${fixture}/root-bind-source"
root_bind_prefix="${root_bind_parent}/prefix"
root_bind_config_dir="${root_bind_parent}/config"
root_bind_data_dir="${root_bind_parent}/data"
mounted=0
independent_mounted=0
root_source_mounted=0
root_bind_mounted=0

cleanup() {
  if [ "${root_bind_mounted}" -eq 1 ]; then
    umount "${root_bind_parent}" >/dev/null 2>&1 || return 0
    root_bind_mounted=0
  fi
  if [ "${root_source_mounted}" -eq 1 ]; then
    umount "${root_bind_source}" >/dev/null 2>&1 || return 0
    root_source_mounted=0
  fi
  if [ "${independent_mounted}" -eq 1 ]; then
    # Do not remove the fixture if a mount remains attached.
    umount "${independent_parent}" >/dev/null 2>&1 || return 0
    independent_mounted=0
  fi
  if [ "${mounted}" -eq 1 ]; then
    # Never recursively remove the fixture while a mount is still attached;
    # that could traverse into the mounted source tree on cleanup.
    umount "${mount_parent}" >/dev/null 2>&1 || return 0
    mounted=0
  fi
  rm -rf -- "${fixture}"
}
trap cleanup EXIT

mount --make-rprivate /
mkdir -p "${mount_parent}" "${source_tree}"
bash "${install_script}" \
  --binary "${dm_binary}" \
  --prefix "${prefix}" \
  --config-dir "${config_dir}" \
  --data-dir "${data_dir}" \
  --no-completion \
  --no-profile

# The source tree deliberately carries a valid manifest and markers whose
# recorded paths still name the lexical target. A bind over the parent then
# makes purge operate on this unrelated copy unless it rejects the ancestor.
cp -a "${mount_parent}/." "${source_tree}/"
printf 'external sentinel\n' >"${source_tree}/external-sentinel"
mount --bind "${source_tree}" "${mount_parent}"
mounted=1

set +e
bash "${uninstall_script}" \
  --prefix "${prefix}" \
  --config-dir "${config_dir}" \
  --data-dir "${data_dir}" \
  --purge
status=$?
set -e
[ "${status}" -ne 0 ] || {
  echo "purge accepted a bind-mounted ancestor" >&2
  exit 1
}
[ -f "${source_tree}/external-sentinel" ] || {
  echo "purge removed the external sentinel" >&2
  exit 1
}
[ -x "${prefix}/bin/dm" ] || {
  echo "purge mutated the mounted installation before rejecting the ancestor" >&2
  exit 1
}

umount "${mount_parent}"
mounted=0
[ -x "${prefix}/bin/dm" ] || {
  echo "purge mutated the original installation" >&2
  exit 1
}
[ -f "${source_tree}/external-sentinel" ] || {
  echo "purge removed the external sentinel after unmount" >&2
  exit 1
}

# A bind of another filesystem's root reports root=/ too. The duplicate
# mount-identity guard should reject that view while leaving the source and
# original installation untouched.
mkdir -p "${root_bind_parent}" "${root_bind_source}"
bash "${install_script}" \
  --binary "${dm_binary}" \
  --prefix "${root_bind_prefix}" \
  --config-dir "${root_bind_config_dir}" \
  --data-dir "${root_bind_data_dir}" \
  --no-completion \
  --no-profile
mount -t tmpfs -o size=64m dm-e2e-root-bind "${root_bind_source}"
root_source_mounted=1
cp -a "${root_bind_parent}/." "${root_bind_source}/"
printf 'root bind external sentinel\n' >"${root_bind_source}/external-sentinel"
mount --bind "${root_bind_source}" "${root_bind_parent}"
root_bind_mounted=1

set +e
bash "${uninstall_script}" \
  --prefix "${root_bind_prefix}" \
  --config-dir "${root_bind_config_dir}" \
  --data-dir "${root_bind_data_dir}" \
  --purge
status=$?
set -e
[ "${status}" -ne 0 ] || {
  echo "purge accepted a root=/ bind-mounted ancestor" >&2
  exit 1
}
[ -f "${root_bind_source}/external-sentinel" ] || {
  echo "purge removed the root-bind external sentinel" >&2
  exit 1
}
[ -x "${root_bind_prefix}/bin/dm" ] || {
  echo "purge mutated the root-bind source before rejecting the ancestor" >&2
  exit 1
}
umount "${root_bind_parent}"
root_bind_mounted=0
[ -x "${root_bind_prefix}/bin/dm" ] || {
  echo "purge mutated the original root-bind installation" >&2
  exit 1
}
umount "${root_bind_source}"
root_source_mounted=0

# An independent filesystem mount has root=/ in mountinfo and is a supported
# location for an explicitly configured installation. Verify that purge can
# remove the managed trees inside such a mount.
mkdir -p "${independent_parent}"
mount -t tmpfs -o size=64m dm-e2e-independent "${independent_parent}"
independent_mounted=1
bash "${install_script}" \
  --binary "${dm_binary}" \
  --prefix "${independent_prefix}" \
  --config-dir "${independent_config_dir}" \
  --data-dir "${independent_data_dir}" \
  --no-completion \
  --no-profile
bash "${uninstall_script}" \
  --prefix "${independent_prefix}" \
  --config-dir "${independent_config_dir}" \
  --data-dir "${independent_data_dir}" \
  --purge
[ ! -e "${independent_config_dir}" ] || {
  echo "purge left config data on an independent filesystem mount" >&2
  exit 1
}
[ ! -e "${independent_data_dir}" ] || {
  echo "purge left data on an independent filesystem mount" >&2
  exit 1
}
umount "${independent_parent}"
independent_mounted=0
SH
  chmod 0700 "${helper}"
  run_case "${name}" unshare --mount --fork -- "${helper}" \
    "${ROOT_DIR}/scripts/install.sh" \
    "${ROOT_DIR}/scripts/uninstall.sh" \
    "${DM_BIN}" \
    "${fixture}"
}

run_install_mode() {
  local prefix="${WORK_DIR}/install-root"
  local config_dir="${WORK_DIR}/install-config"
  local data_dir="${WORK_DIR}/install-data"
  local bin_dir="${prefix}/bin"
  local libexec_dir="${prefix}/lib/docker-manager"
  local completion_dir="${prefix}/share"
  local manifest="${config_dir}/install.env"
  local config_marker="${config_dir}/.docker-manager-managed"
  local saved_manifest="${WORK_DIR}/install.env.valid"
  local saved_config_marker="${WORK_DIR}/config-marker.valid"
  local data_marker="${data_dir}/.docker-manager-managed"
  local saved_data_marker="${WORK_DIR}/data-marker.valid"
  local saved_wrapper="${WORK_DIR}/dm-wrapper.valid"
  local saved_installed_binary="${WORK_DIR}/dm-bin.valid"
  local failing_binary="${WORK_DIR}/dm-completion-fail"
  local failed_prefix="${WORK_DIR}/failed-install-root"
  local failed_config_dir="${WORK_DIR}/failed-install-config"
  local failed_data_dir="${WORK_DIR}/failed-install-data"
  local foreign_prefix="${WORK_DIR}/foreign-install-root"
  local foreign_config_dir="${WORK_DIR}/foreign-install-config"
  local foreign_data_dir="${WORK_DIR}/foreign-install-data"
  local foreign_config_sentinel="${foreign_config_dir}/keep.txt"
  local foreign_data_sentinel="${foreign_data_dir}/keep.txt"
  local injection_marker="${WORK_DIR}/manifest-injection-executed"
  local external_sentinel="${WORK_DIR}/purge-external-sentinel"
  local purge_symlink="${data_dir}/unsafe-link"
  cat >"${failing_binary}" <<'SH'
#!/usr/bin/env sh
if [ "${1:-}" = "completion" ]; then
  echo "injected completion failure" >&2
  exit 73
fi
exit 74
SH
  chmod 0755 "${failing_binary}"

  run_case "install script dry-run" bash "${ROOT_DIR}/scripts/install.sh" --binary "${DM_BIN}" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --completion all --no-profile --dry-run
  run_expect_fail "install rejects broad config directory" bash "${ROOT_DIR}/scripts/install.sh" --binary "${DM_BIN}" --prefix "${prefix}" --config-dir "${HOME}" --data-dir "${data_dir}" --no-completion --no-profile --dry-run
  mkdir -p "${foreign_prefix}" "${foreign_config_dir}" "${foreign_data_dir}"
  printf 'foreign config\n' >"${foreign_config_sentinel}"
  printf 'foreign data\n' >"${foreign_data_sentinel}"
  run_expect_fail "fresh install rejects non-empty unowned state" bash "${ROOT_DIR}/scripts/install.sh" --binary "${DM_BIN}" --prefix "${foreign_prefix}" --config-dir "${foreign_config_dir}" --data-dir "${foreign_data_dir}" --no-completion --no-profile
  run_case "foreign state remains unchanged" cmp -s "${foreign_config_sentinel}" <(printf 'foreign config\n')
  run_case "foreign data remains unchanged" cmp -s "${foreign_data_sentinel}" <(printf 'foreign data\n')
  rm -rf -- "${foreign_prefix}" "${foreign_config_dir}" "${foreign_data_dir}"
  run_expect_fail "fresh install rolls back completion failure" bash "${ROOT_DIR}/scripts/install.sh" --binary "${failing_binary}" --prefix "${failed_prefix}" --config-dir "${failed_config_dir}" --data-dir "${failed_data_dir}" --completion bash --no-profile
  run_case "failed fresh install leaves no managed state" verify_failed_fresh_install_cleanup "${failed_prefix}" "${failed_config_dir}" "${failed_data_dir}"
  run_case "install script" bash "${ROOT_DIR}/scripts/install.sh" --binary "${DM_BIN}" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --completion all --no-profile
  run_case "installed wrapper version" "${bin_dir}/dm" version
  run_case "installed wrapper doctor dm-config" "${bin_dir}/dm" doctor --format json --check-e2e=false
  test -f "${config_dir}/dm.yaml"
  test -f "${manifest}"
  run_case "install manifest contract" verify_install_manifest_contract "${manifest}" "${completion_dir}" "${data_dir}"

  run_case "completion all state" verify_completion_state "${manifest}" "${completion_dir}" 4 1 1 1 1
  run_case "reduce completion to bash" bash "${ROOT_DIR}/scripts/install.sh" --binary "${DM_BIN}" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --completion bash --no-profile
  run_case "completion bash state" verify_completion_state "${manifest}" "${completion_dir}" 1 1 0 0 0
  run_case "disable completion" bash "${ROOT_DIR}/scripts/install.sh" --binary "${DM_BIN}" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --no-completion --no-profile
  run_case "no completion state" verify_completion_state "${manifest}" "${completion_dir}" 0 0 0 0 0
  run_case "restore completion all" bash "${ROOT_DIR}/scripts/install.sh" --binary "${DM_BIN}" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --completion all --no-profile
  run_case "restored completion all state" verify_completion_state "${manifest}" "${completion_dir}" 4 1 1 1 1

  run_bind_mount_ancestor_case

  cp -p -- "${manifest}" "${saved_manifest}"
  cp -p -- "${config_marker}" "${saved_config_marker}"
  cp -p -- "${data_marker}" "${saved_data_marker}"
  cp -p -- "${bin_dir}/dm" "${saved_wrapper}"
  cp -p -- "${libexec_dir}/dm-bin" "${saved_installed_binary}"

  token=$(sed -n "s/^DM_INSTALL_TOKEN='\([0-9a-f][0-9a-f]*\)'$/\1/p" "${manifest}")
  [[ "${token}" =~ ^[0-9a-f]{32}$ ]]
  sed -i "s/^DM_INSTALL_TOKEN='${token}'$/DM_INSTALL_TOKEN=\"${token}\"/" "${manifest}"
  run_case "reinstall accepts double-quoted ownership token" bash "${ROOT_DIR}/scripts/install.sh" --binary "${DM_BIN}" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --completion all --no-profile
  run_case "double-quoted token preserves manifest contract" verify_install_manifest_contract "${manifest}" "${completion_dir}" "${data_dir}"

  run_expect_fail "reinstall rolls back completion failure" bash "${ROOT_DIR}/scripts/install.sh" --binary "${failing_binary}" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --completion all --no-profile
  run_case "failed reinstall preserves previous files" verify_failed_reinstall_rollback \
    "${manifest}" "${saved_manifest}" \
    "${config_marker}" "${saved_config_marker}" \
    "${data_marker}" "${saved_data_marker}" \
    "${bin_dir}/dm" "${saved_wrapper}" \
    "${libexec_dir}/dm-bin" "${saved_installed_binary}"
  run_case "failed reinstall preserves wrapper entry" "${bin_dir}/dm" version
  run_case "failed reinstall preserves manifest contract" verify_install_manifest_contract "${manifest}" "${completion_dir}" "${data_dir}"

  {
    printf 'DM_INSTALL_PREFIX=%q\n' "${prefix}"
    printf 'touch %q\n' "${injection_marker}"
  } >"${manifest}"
  chmod 0600 "${manifest}"
  run_expect_fail "uninstall rejects executable manifest" bash "${ROOT_DIR}/scripts/uninstall.sh" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --purge
  run_case "manifest rejection is non-mutating" verify_install_rejection_preserved_state "${bin_dir}/dm" "${libexec_dir}/dm-bin" "${config_dir}" "${data_dir}" "${injection_marker}"
  cp -p -- "${saved_manifest}" "${manifest}"

  chmod 0644 "${manifest}"
  run_expect_fail "uninstall rejects public v2 manifest" bash "${ROOT_DIR}/scripts/uninstall.sh" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --purge
  run_case "manifest mode rejection is non-mutating" verify_install_rejection_preserved_state "${bin_dir}/dm" "${libexec_dir}/dm-bin" "${config_dir}" "${data_dir}" "${injection_marker}"
  chmod 0600 "${manifest}"

  printf 'modified marker\n' >>"${data_marker}"
  run_expect_fail "uninstall rejects modified data marker" bash "${ROOT_DIR}/scripts/uninstall.sh" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --purge
  run_case "marker rejection is non-mutating" verify_install_rejection_preserved_state "${bin_dir}/dm" "${libexec_dir}/dm-bin" "${config_dir}" "${data_dir}" "${injection_marker}"
  cp -p -- "${saved_data_marker}" "${data_marker}"

  printf 'external sentinel\n' >"${external_sentinel}"
  ln -s -- "${external_sentinel}" "${purge_symlink}"
  run_expect_fail "uninstall rejects purge symlink" bash "${ROOT_DIR}/scripts/uninstall.sh" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --purge
  run_case "purge symlink rejection is non-mutating" verify_install_rejection_preserved_state "${bin_dir}/dm" "${libexec_dir}/dm-bin" "${config_dir}" "${data_dir}" "${injection_marker}"
  test -f "${external_sentinel}"
  rm -f -- "${purge_symlink}"

  run_case "uninstall script" bash "${ROOT_DIR}/scripts/uninstall.sh" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --purge
  if [ -e "${bin_dir}/dm" ] || [ -e "${config_dir}" ] || [ -e "${data_dir}" ]; then
    echo "install 模式清理失败" >&2
    exit 1
  fi
}

write_cancel_restore_fixture() {
  local dir="$1"
  mkdir -p "${dir}"
  cat >"${dir}/manifest.json" <<'JSON'
{
  "version": 1,
  "created_at": "1970-01-01T00:00:00Z",
  "containers": [
    {
      "container_name": "dm_cancel_restore",
      "source_name": "dm_cancel_restore",
      "inspect_file": "container.inspect.json",
      "compose_file": "docker-compose.yml"
    }
  ]
}
JSON
  cat >"${dir}/container.inspect.json" <<'JSON'
{
  "Id": "dm_cancel_restore",
  "Name": "/dm_cancel_restore",
  "Config": {
    "Image": "busybox:latest"
  },
  "HostConfig": {},
  "NetworkSettings": {}
}
JSON
  cat >"${dir}/docker-compose.yml" <<'YAML'
services:
  dm_cancel_restore:
    image: busybox:latest
YAML
}

run_cancel_mode() {
  start_stall_server
  local fake_docker="tcp://127.0.0.1:${STALL_PORT}"
  local fake_registry="127.0.0.1:${STALL_PORT}"
  local restore_fixture="${WORK_DIR}/cancel-restore"
  write_cancel_restore_fixture "${restore_fixture}"

  run_cancel_case "cancel pull" "${DM_BIN}" image pull "${fake_registry}/busybox:latest" --plain-http --timeout 5m --output-dir "${WORK_DIR}/cancel-pull"
  run_cancel_case "cancel backup bundle" "${DM_BIN}" --docker-host "${fake_docker}" backup "dm_cancel" --bundle --output-dir "${WORK_DIR}/cancel-backup" --bundle-output "${WORK_DIR}/cancel-backup.tar.gz"
  run_cancel_case "cancel restore no-start" "${DM_BIN}" --docker-host "${fake_docker}" restore "${restore_fixture}" --skip-checksum --no-start --confirm
  run_cancel_case "cancel logs report" "${DM_BIN}" --docker-host "${fake_docker}" report logs --filter "name:dm_cancel" --keyword dm-test --tail 100
  run_cancel_case "cancel prune dry-run" "${DM_BIN}" --docker-host "${fake_docker}" report prune --only container --format json
  run_cancel_case "cancel reverse" "${DM_BIN}" --docker-host "${fake_docker}" reverse --filter "name:dm_cancel"
}

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

run_case "version text" "${DM_BIN}" version
run_case "version json" "${DM_BIN}" version --format json
run_case "root help" "${DM_BIN}" --help
run_case "image help" "${DM_BIN}" image --help
run_case "report help" "${DM_BIN}" report --help
run_case "shortcut pull help" "${DM_BIN}" pull --help
run_case "shortcut health help" "${DM_BIN}" health --help
run_case "shortcut registry help" "${DM_BIN}" registry --help
run_expect_fail "old logs-scan rejected" "${DM_BIN}" logs-scan --help
run_expect_fail "old inspect-diff rejected" "${DM_BIN}" inspect-diff --help
run_expect_fail "old prune-report rejected" "${DM_BIN}" prune-report --help
run_expect_fail "old registry-login-check rejected" "${DM_BIN}" registry-login-check --help
run_expect_fail "old global json rejected" "${DM_BIN}" --json version
run_case "doctor smoke" "${DM_BIN}" doctor --format json --check-e2e=false --output-dir "${WORK_DIR}"

if [ "${MODE}" = "smoke" ]; then
  write_report
  log "smoke 测试通过"
  echo "测试报告: ${REPORT_MD}"
  echo "测试明细: ${RESULTS_TSV}"
  exit 0
fi

if [ "${MODE}" = "install" ]; then
  run_install_mode
  write_report
  log "install 测试通过"
  echo "测试报告: ${REPORT_MD}"
  echo "测试明细: ${RESULTS_TSV}"
  exit 0
fi

if [ "${MODE}" = "cancel" ]; then
  run_cancel_mode
  write_report
  log "cancel 测试通过"
  echo "测试报告: ${REPORT_MD}"
  echo "测试明细: ${RESULTS_TSV}"
  exit 0
fi

need_cmd docker
claim_docker_scope

log "准备测试镜像"
ensure_image "${REGISTRY_IMAGE}"
ensure_image "${SOURCE_IMAGE}"
verify_docker_runtime

log "启动临时 registry ${REGISTRY_NAME}"
docker volume create --label "${LABEL}" "${REGISTRY_VOLUME_NAME}" >/dev/null
if [ "$(docker volume inspect --format "{{ index .Labels \"${LABEL_KEY}\" }}" "${REGISTRY_VOLUME_NAME}" 2>/dev/null || true)" != "${LABEL_VALUE}" ]; then
  echo "Failed to claim registry volume ownership: ${REGISTRY_VOLUME_NAME}" >&2
  exit 1
fi
docker run -d --name "${REGISTRY_NAME}" --label "${LABEL}" -p "127.0.0.1::5000" -v "${REGISTRY_VOLUME_NAME}:/var/lib/registry" "${REGISTRY_IMAGE}" >/dev/null
REGISTRY="127.0.0.1:$(registry_port)"
TARGET_PREFIX="${REGISTRY}/${TARGET_NAMESPACE}"
SOURCE_REGISTRY_IMAGE="${REGISTRY}/${SOURCE_LOCAL_TAG}"
wait_for_registry

run_case "doctor basic" "${DM_BIN}" doctor --format json --check-e2e=false --output-dir "${WORK_DIR}"
run_case "doctor registry plain-http" "${DM_BIN}" doctor --registry "${REGISTRY}" --plain-http --format markdown --check-e2e=false --output-dir "${WORK_DIR}"

log "seed 本地临时 registry"
docker tag "${SOURCE_IMAGE}" "${SOURCE_REGISTRY_IMAGE}"
docker push "${SOURCE_REGISTRY_IMAGE}" >/dev/null

run_case "report registry text" "${DM_BIN}" report registry "${REGISTRY}" --plain-http
run_case "report registry json" "${DM_BIN}" report registry "${REGISTRY}" --plain-http --format json
run_case "report registry markdown" "${DM_BIN}" report registry "${REGISTRY}" --plain-http --format markdown
run_case "report registry html" "${DM_BIN}" report registry "${REGISTRY}" --plain-http --format html

run_case "image pull output" "${DM_BIN}" image pull "${SOURCE_REGISTRY_IMAGE}" --plain-http --output "${WORK_DIR}/pull-local.tar"
test -f "${WORK_DIR}/pull-local.tar"
run_case "image pull load" "${DM_BIN}" image pull "${SOURCE_REGISTRY_IMAGE}" --plain-http --load --output "${WORK_DIR}/pull-load.tar"
test -f "${WORK_DIR}/pull-load.tar"
run_case "image pull to registry" "${DM_BIN}" image pull "${SOURCE_REGISTRY_IMAGE}" --to "${TARGET_PREFIX}" --plain-http --output-dir "${WORK_DIR}/pulled"
TARGET_IMAGE=$(docker images --format '{{.Repository}}:{{.Tag}}' | awk -v prefix="${TARGET_PREFIX}/" 'index($0, prefix) == 1 && $0 !~ /:<none>$/ { print; exit }')
if [ -z "${TARGET_IMAGE}" ]; then
  echo "未找到 image pull --to 生成的目标镜像，前缀: ${TARGET_PREFIX}" >&2
  exit 1
fi
docker pull "${TARGET_IMAGE}" >/dev/null

printf '%s\n' "${SOURCE_REGISTRY_IMAGE}" >"${WORK_DIR}/images.txt"
run_case "image pull batch to registry" "${DM_BIN}" image pull --file "${WORK_DIR}/images.txt" --to "${REGISTRY}/dm-e2e-mirror-${SUFFIX}" --plain-http --concurrency 1 --retries 1 --resume --output-dir "${WORK_DIR}/pulled-batch" --state-file "${WORK_DIR}/pull-state.json" --report "${WORK_DIR}/pull-report.json" --format markdown
test -f "${WORK_DIR}/pull-report.json"
run_case "image pull batch skip-existing" "${DM_BIN}" image pull --file "${WORK_DIR}/images.txt" --to "${REGISTRY}/dm-e2e-mirror-${SUFFIX}" --plain-http --concurrency 1 --skip-existing --output-dir "${WORK_DIR}/pulled-skip-existing" --state-file "${WORK_DIR}/pull-skip-state.json" --format json

run_case "image save dry-run" "${DM_BIN}" image save "${WORK_DIR}/saved" --filter "repo:busybox" --dry-run
run_case "image save filter" "${DM_BIN}" image save "${WORK_DIR}/saved" --filter "repo:busybox"
run_case "image save merge" "${DM_BIN}" image save "${WORK_DIR}/saved-merged" --filter "repo:busybox" --merge
run_case "image load saved dir" "${DM_BIN}" image load "${WORK_DIR}/saved"
run_case "image tree" "${DM_BIN}" image tree "${SOURCE_IMAGE}" --format markdown --top 5

log "创建测试容器"
docker volume create --label "${LABEL}" "${VOLUME_NAME}" >/dev/null
if [ "$(docker volume inspect --format "{{ index .Labels \"${LABEL_KEY}\" }}" "${VOLUME_NAME}" 2>/dev/null || true)" != "${LABEL_VALUE}" ]; then
  echo "Failed to claim test volume ownership: ${VOLUME_NAME}" >&2
  exit 1
fi
docker run -d --name "${CONTAINER_NAME}" --label "${LABEL}" -v "${VOLUME_NAME}:/data" "${TARGET_IMAGE}" sh -c "while true; do echo dm-test-primary; sleep 5; done" >/dev/null
docker run -d --name "${SECOND_CONTAINER_NAME}" --label "${LABEL}" "${TARGET_IMAGE}" sh -c "while true; do echo dm-test-secondary; sleep 5; done" >/dev/null
docker run -d --name "${RERUN_CONTAINER_NAME}" --label "${LABEL}" "${TARGET_IMAGE}" sh -c "while true; do echo dm-test-rerun; sleep 5; done" >/dev/null
docker run --name "${STOPPED_CONTAINER_NAME}" --label "${LABEL}" "${TARGET_IMAGE}" sh -c "echo dm-test-stopped" >/dev/null

run_case "reverse cmd" "${DM_BIN}" reverse "${CONTAINER_NAME}" --pretty
run_case "reverse compose" "${DM_BIN}" reverse "${CONTAINER_NAME}" --reverse-type compose
run_case "reverse all filter" "${DM_BIN}" reverse --filter "label:${LABEL}" --reverse-type all --redact-secrets
run_case "rerun dry-run" "${DM_BIN}" rerun "${RERUN_CONTAINER_NAME}" --dry-run
run_expect_fail "rerun without confirm rejected" "${DM_BIN}" rerun "${RERUN_CONTAINER_NAME}"
run_case "rerun confirm scoped test container" "${DM_BIN}" rerun "${RERUN_CONTAINER_NAME}" --confirm
docker inspect "${RERUN_CONTAINER_NAME}" >/dev/null

run_case "backup dry-run" "${DM_BIN}" backup "${CONTAINER_NAME}" --dry-run
run_case "backup single bundle" "${DM_BIN}" backup "${CONTAINER_NAME}" --bundle --output-dir "${BACKUP_DIR}" --bundle-output "${BACKUP_ARCHIVE}"
test -f "${BACKUP_ARCHIVE}"
test -f "${BACKUP_DIR}/manifest.json"
test -f "${BACKUP_DIR}/checksums.txt"
test -f "${BACKUP_DIR}/README.md"
test -f "${BACKUP_DIR}/restore.sh"
run_case "backup batch separate" "${DM_BIN}" backup "label:${LABEL}" --output-dir "${BATCH_BACKUP_DIR}" --no-image
run_case "backup batch merge bundle" "${DM_BIN}" backup "${CONTAINER_NAME}" "${SECOND_CONTAINER_NAME}" --merge --bundle --output-dir "${MERGED_BACKUP_DIR}" --bundle-output "${MERGED_BACKUP_ARCHIVE}" --no-image
test -f "${MERGED_BACKUP_ARCHIVE}"

run_case "restore dry-run archive" "${DM_BIN}" restore "${BACKUP_ARCHIVE}" --name "${RESTORED_NAME}" --no-start --dry-run
run_case "restore no-start archive" "${DM_BIN}" restore "${BACKUP_ARCHIVE}" --name "${RESTORED_NAME}" --no-start --confirm
docker inspect "${RESTORED_NAME}" >/dev/null
run_expect_fail "restore existing without replace rejected" "${DM_BIN}" restore "${BACKUP_ARCHIVE}" --name "${RESTORED_NAME}" --no-start --confirm
run_case "restore replace archive" "${DM_BIN}" restore "${BACKUP_ARCHIVE}" --name "${RESTORED_NAME}" --replace --no-start --confirm
run_case "restore merged dry-run" "${DM_BIN}" restore "${MERGED_BACKUP_ARCHIVE}" --dry-run --no-start

run_case "report health text" "${DM_BIN}" report health --filter "label:${LABEL}"
run_case "report health markdown redact" "${DM_BIN}" report health --filter "label:${LABEL}" --redact-secrets --format markdown
run_case "report network json" "${DM_BIN}" report network --filter "label:${LABEL}" --format json
run_case "report network html" "${DM_BIN}" report network --filter "label:${LABEL}" --format html
run_case "report logs markdown" "${DM_BIN}" report logs --filter "label:${LABEL}" --keyword dm-test --tail 100 --context 1 --format markdown
run_case "report logs redact json" "${DM_BIN}" report logs --filter "label:${LABEL}" --keyword dm-test --redact-secrets --format json
run_case "report diff markdown" "${DM_BIN}" report diff "${CONTAINER_NAME}" "${SECOND_CONTAINER_NAME}" --redact-secrets --format markdown
run_case "report volumes json" "${DM_BIN}" report volumes "${VOLUME_NAME}" --all --format json
run_case "report prune markdown" "${DM_BIN}" report prune --only container --filter "label=${LABEL}" --format markdown
run_expect_fail "report prune apply without confirm rejected" "${DM_BIN}" report prune --only container --filter "label=${LABEL}" --apply
run_case "report prune apply stopped container scoped" "${DM_BIN}" report prune --only container --filter "label=${LABEL}" --apply --confirm --format json
if docker inspect "${CONTAINER_NAME}" >/dev/null 2>&1; then
  :
else
  echo "运行中的测试容器被 prune 删除，安全边界失败: ${CONTAINER_NAME}" >&2
  exit 1
fi
if docker inspect "${STOPPED_CONTAINER_NAME}" >/dev/null 2>&1; then
  echo "停止的测试容器未被 prune 删除: ${STOPPED_CONTAINER_NAME}" >&2
  exit 1
fi

run_expect_fail "backup old output flag rejected" "${DM_BIN}" backup "${CONTAINER_NAME}" --output "${WORK_DIR}/old.tar.gz"
run_expect_fail "backup old include-image flag rejected" "${DM_BIN}" backup "${CONTAINER_NAME}" --include-image=false
run_expect_fail "reverse old filter-default-envs flag rejected" "${DM_BIN}" reverse "${CONTAINER_NAME}" --filter-default-envs=false
run_expect_fail "reverse old merge-ports flag rejected" "${DM_BIN}" reverse "${CONTAINER_NAME}" --merge-ports=false

write_report
log "端到端集成测试通过"
echo "测试报告: ${REPORT_MD}"
echo "测试明细: ${RESULTS_TSV}"
