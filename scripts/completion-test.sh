#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
DM_BIN="${DM_COMPLETION_TEST_BIN:-${ROOT_DIR}/dm}"
WORK_DIR="${DM_COMPLETION_TEST_WORK_DIR:-${TMPDIR:-/tmp}/dm-completion-$$_${RANDOM}}"
KEEP_WORKDIR=0
NO_DOCKER=0
REQUIRE_SHELLS=0
REQUIRE_DOCKER=0
PASS=0
FAIL=0
SKIP=0
WORK_DIR_OWNED=0
WORK_DIR_TOKEN="dm-completion:$$_${RANDOM}_$(date +%s)"
WORK_DIR_SENTINEL=""

usage() {
  cat <<'EOF'
Usage: scripts/completion-test.sh [options]

Deep-test dm shell completion without pulling external images.

Options:
  --dm-bin PATH       dm binary to test. Default: ./dm or DM_COMPLETION_TEST_BIN
  --work-dir DIR      New, non-existing directory for logs and reports
  --keep-workdir      Keep temporary work directory
  --no-docker         Skip Docker-backed resource candidate checks
  --require-shells    Fail when bash-completion, zsh or fish is unavailable
  --require-docker    Fail when Docker resource candidate checks cannot run
  -h, --help          Show this help
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --dm-bin)
      DM_BIN=${2:?missing value for --dm-bin}
      shift 2
      ;;
    --work-dir)
      WORK_DIR=${2:?missing value for --work-dir}
      KEEP_WORKDIR=1
      shift 2
      ;;
    --keep-workdir)
      KEEP_WORKDIR=1
      shift
      ;;
    --no-docker)
      NO_DOCKER=1
      shift
      ;;
    --require-shells)
      REQUIRE_SHELLS=1
      shift
      ;;
    --require-docker)
      REQUIRE_DOCKER=1
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

if [ "${NO_DOCKER}" -eq 1 ] && [ "${REQUIRE_DOCKER}" -eq 1 ]; then
  echo "--no-docker and --require-docker cannot be used together." >&2
  exit 2
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
    echo "Completion work directory must be new and non-existing: ${requested}" >&2
    return 1
  fi
  parent=$(dirname -- "${requested}")
  leaf=$(basename -- "${requested}")
  if [ -z "${leaf}" ] || [ "${leaf}" = "." ] || [ "${leaf}" = ".." ] || [ ! -d "${parent}" ]; then
    echo "Invalid completion work directory: ${requested}" >&2
    return 1
  fi
  parent_real=$(CDPATH='' cd -- "${parent}" && pwd -P) || return 1
  candidate="${parent_real}/${leaf}"
  if protected_work_dir "${candidate}"; then
    echo "Refusing protected completion work directory: ${candidate}" >&2
    return 1
  fi
  umask 077
  mkdir -- "${candidate}" || return 1
  WORK_DIR=${candidate}
  WORK_DIR_SENTINEL="${WORK_DIR}/.dm-completion-owned"
  if ! printf '%s\n' "${WORK_DIR_TOKEN}" >"${WORK_DIR_SENTINEL}" || ! chmod 600 "${WORK_DIR_SENTINEL}"; then
    rm -f -- "${WORK_DIR_SENTINEL}" >/dev/null 2>&1 || true
    rmdir -- "${WORK_DIR}" >/dev/null 2>&1 || true
    return 1
  fi
  WORK_DIR_OWNED=1
}

remove_owned_work_dir() {
  if [ "${WORK_DIR_OWNED}" -ne 1 ] || [ -z "${WORK_DIR}" ] || [ -L "${WORK_DIR}" ] || [ ! -d "${WORK_DIR}" ]; then
    echo "Refusing to remove unowned completion work directory: ${WORK_DIR}" >&2
    return 1
  fi
  if protected_work_dir "${WORK_DIR}" || [ -L "${WORK_DIR_SENTINEL}" ] || [ ! -f "${WORK_DIR_SENTINEL}" ] || [ "$(cat "${WORK_DIR_SENTINEL}")" != "${WORK_DIR_TOKEN}" ]; then
    echo "Refusing to remove unsafe completion work directory: ${WORK_DIR}" >&2
    return 1
  fi
  rm -rf -- "${WORK_DIR}"
  WORK_DIR_OWNED=0
}

prepare_work_dir "${WORK_DIR}"
trap 'remove_owned_work_dir >/dev/null 2>&1 || true' EXIT
RESULTS="${WORK_DIR}/results.tsv"
REPORT="${WORK_DIR}/completion-test-report.md"
printf 'case\tstatus\tnote\tlog\n' >"${RESULTS}"

record() {
  local name=$1 status=$2 note=$3 log_file=${4:-}
  printf '%s\t%s\t%s\t%s\n' "${name}" "${status}" "${note}" "${log_file}" >>"${RESULTS}"
  case "${status}" in
    PASS) PASS=$((PASS + 1)) ;;
    FAIL) FAIL=$((FAIL + 1)) ;;
    SKIP) SKIP=$((SKIP + 1)) ;;
  esac
  printf '%s %s %s\n' "${name}" "${status}" "${note}"
}

run_case() {
  local name=$1 want=$2
  shift 2
  local log_file="${WORK_DIR}/${name}.log"
  set +e
  "$@" >"${log_file}" 2>&1
  local rc=$?
  set -e
  if [ "${rc}" -eq 0 ] && grep -Fq -- "${want}" "${log_file}"; then
    record "${name}" PASS "found ${want}" "${log_file}"
  else
    record "${name}" FAIL "rc=${rc}; want ${want}" "${log_file}"
  fi
}

record_unavailable() {
  local name=$1 note=$2 required=$3
  if [ "${required}" -eq 1 ]; then
    record "${name}" FAIL "${note}" ""
  else
    record "${name}" SKIP "${note}" ""
  fi
}

cleanup_containers=()
cleanup_volumes=()
cleanup() {
  set +e
  local index
  for ((index=${#cleanup_containers[@]} - 1; index >= 0; index--)); do
    docker rm -f -- "${cleanup_containers[index]}" >/dev/null 2>&1 || true
  done
  for ((index=${#cleanup_volumes[@]} - 1; index >= 0; index--)); do
    docker volume rm -- "${cleanup_volumes[index]}" >/dev/null 2>&1 || true
  done
  if [ "${KEEP_WORKDIR}" -eq 0 ]; then
    remove_owned_work_dir || true
  fi
}
trap cleanup EXIT

if [ ! -x "${DM_BIN}" ]; then
  echo "dm binary is not executable: ${DM_BIN}" >&2
  exit 2
fi

DM_COMMAND_SHIM="${WORK_DIR}/dm"
{
  printf '%s\n' '#!/usr/bin/env bash'
  # Expanded when the generated shim runs.
  # shellcheck disable=SC2016
  printf '%s\n' 'exec "$DM_COMPLETION_TEST_EXECUTABLE" "$@"'
} >"${DM_COMMAND_SHIM}"
chmod 700 "${DM_COMMAND_SHIM}"

run_case "generate-bash" "__start_dm" "${DM_BIN}" completion bash
run_case "generate-zsh" "_dm" "${DM_BIN}" completion zsh
run_case "generate-fish" "complete -c dm" "${DM_BIN}" completion fish
run_case "generate-powershell" "Register-ArgumentCompleter" "${DM_BIN}" completion powershell

export DM_COMPLETION_TEST_EXECUTABLE="${DM_BIN}"
export DM_COMPLETION_TEST_STATE_DIR="${WORK_DIR}"
export PATH="${WORK_DIR}:${PATH}"

if command -v bash >/dev/null 2>&1; then
  if [ -r /usr/share/bash-completion/bash_completion ]; then
    export DM_BASH_COMPLETION_FILE=/usr/share/bash-completion/bash_completion
    # These variables are expanded by the child Bash process.
    # shellcheck disable=SC2016
    run_case "bash-load" "__start_dm" bash --noprofile --norc -c 'source "$DM_BASH_COMPLETION_FILE"; source <("$DM_COMPLETION_TEST_EXECUTABLE" completion bash); declare -F __start_dm'
  else
    record_unavailable "bash-load" "bash-completion is not installed; install package bash-completion" "${REQUIRE_SHELLS}"
  fi
else
  record_unavailable "bash-load" "bash not found" "${REQUIRE_SHELLS}"
fi

if command -v zsh >/dev/null 2>&1; then
  # These variables are expanded by the child zsh process.
  # shellcheck disable=SC2016
  run_case "zsh-load" "_dm" zsh -f -c 'autoload -Uz compinit; compinit -d "$DM_COMPLETION_TEST_STATE_DIR/.zcompdump"; source <("$DM_COMPLETION_TEST_EXECUTABLE" completion zsh); whence -w _dm'
else
  record_unavailable "zsh-load" "zsh not found" "${REQUIRE_SHELLS}"
fi

if command -v fish >/dev/null 2>&1; then
  # This variable is expanded by the child fish process.
  # shellcheck disable=SC2016
  run_case "fish-command-complete" "report" fish -c 'source ("$DM_COMPLETION_TEST_EXECUTABLE" completion fish | psub); complete -C "dm re"'
else
  record_unavailable "fish-command-complete" "fish not found" "${REQUIRE_SHELLS}"
fi

run_case "cobra-command-complete" "report" "${DM_BIN}" __complete re

if [ "${NO_DOCKER}" -eq 1 ]; then
  record "docker-resource-complete" SKIP "Docker checks explicitly skipped" ""
elif ! command -v docker >/dev/null 2>&1; then
  record_unavailable "docker-resource-complete" "docker command not found" "${REQUIRE_DOCKER}"
elif ! docker info >/dev/null 2>&1; then
  record_unavailable "docker-resource-complete" "Docker daemon unavailable" "${REQUIRE_DOCKER}"
else
  suffix="$(date +%Y%m%d%H%M%S)_$$_${RANDOM}"
  container_name="dm_completion_api_${suffix}"
  volume_name="dm_completion_vol_${suffix}"
  image_ref=$(docker images --format '{{.Repository}}:{{.Tag}}' | grep -v '<none>' | head -1 || true)

  if docker volume create --label dm.completion="${suffix}" "${volume_name}" >/dev/null; then
    cleanup_volumes+=("${volume_name}")
    run_case "cobra-volume-filter-complete" "${volume_name}" "${DM_BIN}" __complete volumes --filter ""
  else
    record_unavailable "cobra-volume-filter-complete" "could not create test volume" "${REQUIRE_DOCKER}"
  fi

  if [ -n "${image_ref}" ]; then
    image_prefix=${image_ref:0:4}
    run_case "cobra-image-filter-complete" "${image_ref}" "${DM_BIN}" __complete save --filter "${image_prefix}"
    cleanup_containers+=("${container_name}")
    if docker run -d --name "${container_name}" --label dm.completion="${suffix}" "${image_ref}" sh -c 'sleep 3600' >/dev/null 2>&1; then
      run_case "cobra-container-complete" "${container_name}" "${DM_BIN}" __complete backup dm_completion
    else
      record_unavailable "cobra-container-complete" "could not start test container from ${image_ref}" "${REQUIRE_DOCKER}"
    fi
  else
    record_unavailable "docker-image-resource-complete" "no local tagged images; no external pull attempted" "${REQUIRE_DOCKER}"
  fi
fi

{
  echo "# dm completion test"
  echo
  echo "- Time: $(date -Is)"
  echo "- Binary: ${DM_BIN}"
  echo "- Work dir: ${WORK_DIR}"
  echo
  echo "## Summary"
  echo
  echo "- PASS: ${PASS}"
  echo "- FAIL: ${FAIL}"
  echo "- SKIP: ${SKIP}"
  echo
  echo "## Results"
  echo
  echo "| Case | Status | Note | Log |"
  echo "| --- | --- | --- | --- |"
  tail -n +2 "${RESULTS}" | while IFS=$'\t' read -r name status note log_file; do
    echo "| ${name} | ${status} | ${note} | $(basename "${log_file}") |"
  done
} >"${REPORT}"

cat "${REPORT}"

if [ "${FAIL}" -gt 0 ]; then
  exit 1
fi
