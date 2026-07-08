#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
MODE=${DM_E2E_MODE:-full}
WORK_DIR=${DM_E2E_WORK_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/dm-e2e-XXXXXX")}
SOURCE_IMAGE=${DM_E2E_IMAGE:-busybox:latest}
REGISTRY_IMAGE=${DM_E2E_REGISTRY_IMAGE:-registry:2}
SUFFIX=${DM_E2E_SUFFIX:-$(date +%s)}
LABEL_KEY=${DM_E2E_LABEL_KEY:-dm.e2e}
LABEL_VALUE=${DM_E2E_LABEL_VALUE:-${SUFFIX}}
LABEL="${LABEL_KEY}=${LABEL_VALUE}"
REGISTRY_NAME="dm_e2e_registry_${SUFFIX}"
CONTAINER_NAME="dm_e2e_container_${SUFFIX}"
SECOND_CONTAINER_NAME="dm_e2e_container_b_${SUFFIX}"
RERUN_CONTAINER_NAME="dm_e2e_rerun_${SUFFIX}"
STOPPED_CONTAINER_NAME="dm_e2e_stopped_${SUFFIX}"
RESTORED_NAME="dm_e2e_restored_${SUFFIX}"
RESTORED_REPLACE_NAME="dm_e2e_replace_${SUFFIX}"
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

usage() {
  cat <<'EOF'
Usage: scripts/e2e.sh [--mode smoke|full|destructive|install|cancel]

Modes:
  smoke        Build or use dm, then run help/version/config/doctor checks without Docker.
  full         Run the complete Docker e2e matrix. This is the default.
  destructive  Alias of full, kept as an explicit opt-in label for safety-matrix runs.
  install      Build or use dm, install into a temporary directory, verify wrapper/config, then uninstall.
  cancel       Verify SIGINT/context cancellation for long-running command paths.

Environment:
  DM_E2E_MODE       Default mode when --mode is not set.
  DM_E2E_WORK_DIR   Directory for logs and temporary files.
  DM_E2E_DM_BIN     Existing dm binary; skips building when set.
  DM_E2E_KEEP_WORKDIR=1 keeps the work directory after the run.
  DM_E2E_CANCEL_EXIT_TIMEOUT=10 timeout in seconds after SIGINT before the child is terminated.
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --mode)
      MODE=${2:?missing value for --mode}
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

case "${MODE}" in
  smoke|full|destructive|install|cancel)
    ;;
  *)
    echo "Unsupported e2e mode: ${MODE}" >&2
    usage >&2
    exit 2
    ;;
esac

mkdir -p "${WORK_DIR}" "${LOG_DIR}"
printf 'case\tstatus\texit_code\tseconds\tlog\n' >"${RESULTS_TSV}"

cleanup() {
  if [ -n "${STALL_PID}" ]; then
    kill "${STALL_PID}" >/dev/null 2>&1 || true
    wait "${STALL_PID}" >/dev/null 2>&1 || true
  fi
  docker rm -f \
    "${CONTAINER_NAME}" \
    "${SECOND_CONTAINER_NAME}" \
    "${RERUN_CONTAINER_NAME}" \
    "${STOPPED_CONTAINER_NAME}" \
    "${RESTORED_NAME}" \
    "${RESTORED_REPLACE_NAME}" \
    "${REGISTRY_NAME}" >/dev/null 2>&1 || true
  docker volume rm "${VOLUME_NAME}" >/dev/null 2>&1 || true
  if [ -n "${REGISTRY:-}" ]; then
    docker image rm "${REGISTRY}/${SOURCE_LOCAL_TAG}" >/dev/null 2>&1 || true
    docker image ls --format '{{.Repository}}:{{.Tag}}' |
      grep -E "^${REGISTRY}/${TARGET_NAMESPACE}/|^${REGISTRY}/dm-e2e-" |
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
  log "娴嬭瘯 ${name} (cancel)"
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
  local probe="dm_e2e_probe_${SUFFIX}"
  docker rm -f "${probe}" >/dev/null 2>&1 || true
  if ! timeout 20 docker create --name "${probe}" "${SOURCE_IMAGE}" sh -c "echo dm-e2e-probe" >/dev/null; then
    echo "Docker 无法在 20s 内创建测试容器，full/destructive 测试无法继续。" >&2
    echo "请先确认 Docker/containerd 运行状态，或换用干净测试机。" >&2
    docker rm -f "${probe}" >/dev/null 2>&1 || true
    exit 1
  fi
  if ! timeout 20 docker start -a "${probe}" >/dev/null; then
    echo "Docker 无法在 20s 内启动测试容器，full/destructive 测试无法继续。" >&2
    echo "请先确认 Docker/containerd 运行状态，或换用干净测试机。" >&2
    docker rm -f "${probe}" >/dev/null 2>&1 || true
    exit 1
  fi
  docker rm -f "${probe}" >/dev/null 2>&1 || true
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

run_install_mode() {
  local prefix="${WORK_DIR}/install-root"
  local config_dir="${WORK_DIR}/install-config"
  local data_dir="${WORK_DIR}/install-data"
  local bin_dir="${prefix}/bin"
  run_case "install script dry-run" bash "${ROOT_DIR}/scripts/install.sh" --binary "${DM_BIN}" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --no-profile --dry-run
  run_case "install script" bash "${ROOT_DIR}/scripts/install.sh" --binary "${DM_BIN}" --prefix "${prefix}" --config-dir "${config_dir}" --data-dir "${data_dir}" --no-profile
  run_case "installed wrapper version" "${bin_dir}/dm" version
  run_case "installed wrapper doctor dm-config" "${bin_dir}/dm" doctor --format json --check-e2e=false
  test -f "${config_dir}/dm.yaml"
  test -f "${config_dir}/install.env"
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
  run_cancel_case "cancel restore no-start" "${DM_BIN}" --docker-host "${fake_docker}" restore "${restore_fixture}" --skip-checksum --no-start
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
  log "cancel 娴嬭瘯閫氳繃"
  echo "娴嬭瘯鎶ュ憡: ${REPORT_MD}"
  echo "娴嬭瘯鏄庣粏: ${RESULTS_TSV}"
  exit 0
fi

need_cmd docker

log "准备测试镜像"
ensure_image "${REGISTRY_IMAGE}"
ensure_image "${SOURCE_IMAGE}"
verify_docker_runtime

log "启动临时 registry ${REGISTRY_NAME}"
docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
docker run -d --name "${REGISTRY_NAME}" -p "127.0.0.1::5000" "${REGISTRY_IMAGE}" >/dev/null
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
run_case "image pull batch to registry" "${DM_BIN}" image pull --file "${WORK_DIR}/images.txt" --to "${REGISTRY}/dm-e2e-mirror-${SUFFIX}" --plain-http --concurrency 1 --retries 1 --resume --report "${WORK_DIR}/pull-report.json" --format markdown
test -f "${WORK_DIR}/pull-report.json"
run_case "image pull batch skip-existing" "${DM_BIN}" image pull --file "${WORK_DIR}/images.txt" --to "${REGISTRY}/dm-e2e-mirror-${SUFFIX}" --plain-http --concurrency 1 --skip-existing --format json

run_case "image save dry-run" "${DM_BIN}" image save "${WORK_DIR}/saved" --filter "repo:busybox" --dry-run
run_case "image save filter" "${DM_BIN}" image save "${WORK_DIR}/saved" --filter "repo:busybox"
run_case "image save merge" "${DM_BIN}" image save "${WORK_DIR}/saved-merged" --filter "repo:busybox" --merge
run_case "image load saved dir" "${DM_BIN}" image load "${WORK_DIR}/saved"
run_case "image tree" "${DM_BIN}" image tree "${SOURCE_IMAGE}" --format markdown --top 5

log "创建测试容器"
docker rm -f "${CONTAINER_NAME}" "${SECOND_CONTAINER_NAME}" "${RERUN_CONTAINER_NAME}" "${STOPPED_CONTAINER_NAME}" >/dev/null 2>&1 || true
docker volume rm "${VOLUME_NAME}" >/dev/null 2>&1 || true
docker volume create --label "${LABEL}" "${VOLUME_NAME}" >/dev/null
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
run_case "restore no-start archive" "${DM_BIN}" restore "${BACKUP_ARCHIVE}" --name "${RESTORED_NAME}" --no-start
docker inspect "${RESTORED_NAME}" >/dev/null
run_expect_fail "restore existing without replace rejected" "${DM_BIN}" restore "${BACKUP_ARCHIVE}" --name "${RESTORED_NAME}" --no-start
run_case "restore replace archive" "${DM_BIN}" restore "${BACKUP_ARCHIVE}" --name "${RESTORED_NAME}" --replace --no-start
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
