#!/usr/bin/env bash
set -euo pipefail

if [ "$(uname -s)" != "Linux" ]; then
  echo "install.sh: the shell installer currently supports Linux only; install Darwin release binaries manually as documented" >&2
  exit 1
fi

ROOT_DIR=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
IS_ROOT=0
if [ "$(id -u)" -eq 0 ]; then
  IS_ROOT=1
fi

if [ "${IS_ROOT}" = "1" ]; then
  PREFIX="/usr/local"
  CONFIG_DIR="/etc/docker-manager"
  DATA_DIR="/var/lib/docker-manager"
  PROFILE_FILE="/etc/profile.d/docker-manager.sh"
else
  PREFIX="${HOME}/.local"
  CONFIG_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/docker-manager"
  DATA_DIR="${XDG_DATA_HOME:-${HOME}/.local/share}/docker-manager"
  PROFILE_FILE="${HOME}/.profile"
fi

BIN_DIR=""
LIBEXEC_DIR=""
BINARY=""
BUILD_FROM_SOURCE=0
NO_PROFILE=0
DRY_RUN=0
OVERWRITE_CONFIG=0
COMPLETION_SHELLS=()
COMPLETION_DIR=""
NO_COMPLETION=0
INSTALL_UID=$(id -u)
INSTALL_GID=$(id -g)
PREFIX_EXPLICIT=0
BIN_DIR_EXPLICIT=0
LIBEXEC_DIR_EXPLICIT=0
DATA_DIR_EXPLICIT=0
COMPLETION_DIR_EXPLICIT=0
PREVIOUS_COMPLETION_FILES=()

die() {
  echo "install.sh: $*" >&2
  exit 1
}

reject_unsafe_text() {
  local value="${1-}"
  local description="${2:-value}"
  case "${value}" in
    *[[:cntrl:]]*)
      die "${description} contains a control character"
      ;;
  esac
}

normalize_path() {
  local input="${1-}"
  local description="${2:-path}"
  [ -n "${input}" ] || die "${description} must not be empty"
  reject_unsafe_text "${input}" "${description}"
  case "/${input}/" in
    */../*) die "${description} must not contain '..' path components: ${input}" ;;
  esac
  case "${input}" in
    /*) ;;
    *) input="$(CDPATH='' pwd -P)/${input}" || die "cannot resolve ${description}: ${input}" ;;
  esac

  local rest="${input#/}"
  local segment
  local result=""
  while [ -n "${rest}" ]; do
    if [[ "${rest}" == */* ]]; then
      segment="${rest%%/*}"
      rest="${rest#*/}"
    else
      segment="${rest}"
      rest=""
    fi
    case "${segment}" in
      ""|.) ;;
      ..) die "${description} must not contain '..' path components: ${input}" ;;
      *) result="${result}/${segment}" ;;
    esac
  done
  if [ -z "${result}" ]; then
    printf '/\n'
  else
    printf '%s\n' "${result}"
  fi
}

path_exists() {
  [ -e "$1" ] || [ -L "$1" ]
}

stat_uid() {
  local path="$1"
  local value
  if value=$(stat -c '%u' "${path}" 2>/dev/null); then
    printf '%s\n' "${value}"
    return 0
  fi
  if value=$(stat -f '%u' "${path}" 2>/dev/null); then
    printf '%s\n' "${value}"
    return 0
  fi
  return 1
}

stat_mode() {
  local path="$1"
  local value
  if value=$(stat -c '%a' "${path}" 2>/dev/null); then
    printf '%s\n' "${value}"
    return 0
  fi
  if value=$(stat -f '%Lp' "${path}" 2>/dev/null); then
    printf '%s\n' "${value}"
    return 0
  fi
  return 1
}

stat_identity() {
  local path="$1"
  local value
  if value=$(stat -c '%d:%i' "${path}" 2>/dev/null); then
    printf '%s\n' "${value}"
    return 0
  fi
  if value=$(stat -f '%d:%i' "${path}" 2>/dev/null); then
    printf '%s\n' "${value}"
    return 0
  fi
  return 1
}

assert_not_root_path() {
  local path="$1"
  local description="${2:-path}"
  [ "${path}" != "/" ] || die "refusing filesystem root as ${description}"
}

assert_dedicated_state_path() {
  local path="$1"
  local description="${2:-state directory}"
  local user_home=""
  if [ -n "${HOME:-}" ]; then
    user_home=$(normalize_path "${HOME}" "home directory")
  fi
  case "${path}" in
    /bin|/boot|/dev|/etc|/home|/lib|/lib32|/lib64|/media|/mnt|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/usr/local|/usr/local/etc|/usr/local/share|/usr/local/var|/var|/var/cache|/var/lib|/var/log|/var/tmp)
      die "refusing broad system path as ${description}: ${path}"
      ;;
  esac
  if [ -n "${user_home}" ]; then
    case "${path}" in
      "${user_home}"|"${user_home}/.config"|"${user_home}/.local"|"${user_home}/.local/share")
        die "refusing broad user path as ${description}: ${path}"
        ;;
    esac
  fi
  if [ -n "${XDG_CONFIG_HOME:-}" ]; then
    [ "${path}" != "$(normalize_path "${XDG_CONFIG_HOME}" "XDG config directory")" ] || die "refusing XDG config root as ${description}: ${path}"
  fi
  if [ -n "${XDG_DATA_HOME:-}" ]; then
    [ "${path}" != "$(normalize_path "${XDG_DATA_HOME}" "XDG data directory")" ] || die "refusing XDG data root as ${description}: ${path}"
  fi
}

assert_no_symlink_path() {
  local path
  path=$(normalize_path "$1" "${2:-path}")
  local description="${2:-path}"
  local rest="${path#/}"
  local current="/"
  local segment
  while [ -n "${rest}" ]; do
    if [[ "${rest}" == */* ]]; then
      segment="${rest%%/*}"
      rest="${rest#*/}"
    else
      segment="${rest}"
      rest=""
    fi
    current="${current%/}/${segment}"
    if [ -L "${current}" ]; then
      die "refusing ${description} through symlink: ${current}"
    fi
    if [ -n "${rest}" ] && [ -e "${current}" ] && [ ! -d "${current}" ]; then
      die "refusing ${description} through non-directory path component: ${current}"
    fi
  done
}

assert_owned_path() {
  local path="$1"
  local description="${2:-path}"
  if path_exists "${path}"; then
    local owner
    owner=$(stat_uid "${path}") || die "cannot inspect ${description}: ${path}"
    [ "${owner}" = "${INSTALL_UID}" ] || die "${description} is not owned by uid ${INSTALL_UID}: ${path}"
  fi
}

assert_safe_directory() {
  local path
  path=$(normalize_path "$1" "${2:-directory}")
  local description="${2:-directory}"
  assert_not_root_path "${path}" "${description}"
  assert_no_symlink_path "${path}" "${description}"
  if path_exists "${path}"; then
    [ -d "${path}" ] || die "${description} is not a directory: ${path}"
    assert_owned_path "${path}" "${description}"
  fi
  printf '%s\n' "${path}"
}

assert_safe_file_target() {
  local path
  path=$(normalize_path "$1" "${2:-file}")
  local description="${2:-file}"
  assert_no_symlink_path "${path}" "${description}"
  if path_exists "${path}"; then
    [ -f "${path}" ] || die "${description} is not a regular file: ${path}"
    assert_owned_path "${path}" "${description}"
  fi
  printf '%s\n' "${path}"
}

path_within() {
  local path="$1"
  local root="$2"
  if [ "${root}" = "/" ]; then
    [[ "${path}" = /* ]]
    return
  fi
  [ "${path}" = "${root}" ] || case "${path}" in "${root}"/*) return 0 ;; *) return 1 ;; esac
}

paths_overlap() {
  path_within "$1" "$2" || path_within "$2" "$1"
}

assert_purge_boundaries() {
  local config_dir="$1"
  local data_dir="$2"
  local prefix="$3"
  local bin_dir="$4"
  local libexec_dir="$5"
  local completion_dir="$6"
  local profile_file="$7"
  if path_within "${prefix}" "${config_dir}" || path_within "${bin_dir}" "${config_dir}" ||
    path_within "${libexec_dir}" "${config_dir}" || path_within "${completion_dir}" "${config_dir}" ||
    path_within "${profile_file}" "${config_dir}" || path_within "${prefix}" "${data_dir}" ||
    path_within "${bin_dir}" "${data_dir}" || path_within "${libexec_dir}" "${data_dir}" ||
    path_within "${completion_dir}" "${data_dir}" || path_within "${profile_file}" "${data_dir}"; then
    die "config/data purge path would include installation artifacts"
  fi
  if paths_overlap "${config_dir}" "${data_dir}"; then
    die "config and data purge paths overlap"
  fi
}

assert_safe_tree() {
  local path
  path=$(normalize_path "$1" "${2:-directory}")
  local description="${2:-directory}"
  assert_not_root_path "${path}" "${description}"
  assert_no_symlink_path "${path}" "${description}"
  if ! path_exists "${path}"; then
    return 0
  fi
  [ -d "${path}" ] || die "${description} is not a directory: ${path}"
  assert_owned_path "${path}" "${description}"
  local link foreign special
  link=$(find -P "${path}" -type l -print -quit 2>/dev/null) || die "cannot inspect ${description} for symlinks: ${path}"
  [ -z "${link}" ] || die "refusing symlink in ${description}: ${link}"
  foreign=$(find -P "${path}" ! -uid "${INSTALL_UID}" -print -quit 2>/dev/null) || die "cannot inspect ownership in ${description}: ${path}"
  [ -z "${foreign}" ] || die "${description} contains a path not owned by uid ${INSTALL_UID}: ${foreign}"
  special=$(find -P "${path}" \( ! -type f ! -type d \) -print -quit 2>/dev/null) || die "cannot inspect file types in ${description}: ${path}"
  [ -z "${special}" ] || die "refusing special file in ${description}: ${special}"
}

ensure_directory() {
  local path
  path=$(normalize_path "$1" "${2:-directory}")
  local description="${2:-directory}"
  assert_not_root_path "${path}" "${description}"
  assert_no_symlink_path "${path}" "${description}"
  if [ "${DRY_RUN}" = "1" ]; then
    echo "DRY-RUN: mkdir -p ${path}"
    return 0
  fi

  # Create one path component at a time so the transaction can remove every
  # directory it created, including parents implicitly created by mkdir -p.
  local rest="${path#/}"
  local segment current="/"
  while [ -n "${rest}" ]; do
    if [[ "${rest}" == */* ]]; then
      segment="${rest%%/*}"
      rest="${rest#*/}"
    else
      segment="${rest}"
      rest=""
    fi
    current="${current%/}/${segment}"
    if path_exists "${current}"; then
      [ -d "${current}" ] || die "${description} contains a non-directory path component: ${current}"
      [ ! -L "${current}" ] || die "refusing ${description} through symlink: ${current}"
      # System-owned ancestors (for example /home or /usr/local) are valid;
      # only the directory that will receive files must belong to the caller.
      if [ -z "${rest}" ]; then
        assert_owned_path "${current}" "${description}"
      fi
      continue
    fi
    if ! mkdir "${current}" 2>/dev/null; then
      # A concurrent creator is acceptable only when it produced a safe,
      # owned directory. Anything else remains a hard failure.
      path_exists "${current}" || die "cannot create ${description}: ${current}"
      if [ ! -d "${current}" ] || [ -L "${current}" ]; then
        die "${description} was not created as a directory: ${current}"
      fi
      assert_owned_path "${current}" "${description}"
      continue
    fi
    if ! transaction_track_created_directory "${current}"; then
      rmdir -- "${current}" 2>/dev/null || true
      die "cannot record install directory: ${current}"
    fi
  done
  assert_no_symlink_path "${path}" "${description}"
  [ -d "${path}" ] || die "${description} was not created as a directory: ${path}"
  assert_owned_path "${path}" "${description}"
}

shell_quote() {
  local value="${1-}"
  reject_unsafe_text "${value}" "shell value"
  value=${value//\'/\'\\\'\'}
  printf "'%s'" "${value}"
}

generate_install_token() {
  local token
  token=$(od -An -N16 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n') || die "cannot generate install ownership token"
  [[ "${token}" =~ ^[0-9a-f]{32}$ ]] || die "cannot generate a valid install ownership token"
  printf '%s\n' "${token}"
}

render_state_marker() {
  local role="$1"
  local path="$2"
  local token="$3"
  printf '%s\n' \
    '# Managed by docker-manager install.sh' \
    'version=1' \
    "role=${role}" \
    "uid=${INSTALL_UID}" \
    "path=${path}" \
    "token=${token}"
}

assert_existing_state_marker() {
  local marker="$1"
  local role="$2"
  local path="$3"
  if ! path_exists "${marker}"; then
    return 0
  fi
  assert_safe_file_target "${marker}" "${role} state marker" >/dev/null
  local mode mode_value size token
  mode=$(stat_mode "${marker}") || die "cannot inspect ${role} state marker mode: ${marker}"
  case "${mode}" in
    ""|*[!0-7]*) die "${role} state marker has an invalid mode: ${marker}" ;;
  esac
  mode_value=$((8#${mode}))
  (( (mode_value & 077) == 0 )) || die "${role} state marker must be private: ${marker}"
  size=$(wc -c <"${marker}")
  [ "${size}" -le 4096 ] || die "${role} state marker is too large: ${marker}"
  token=$(sed -n '6s/^token=//p' "${marker}")
  [[ "${token}" =~ ^[0-9a-f]{32}$ ]] || die "${role} state marker has an invalid token: ${marker}"
  render_state_marker "${role}" "${path}" "${token}" | cmp -s "${marker}" - || die "refusing modified ${role} state marker: ${marker}"
  EXISTING_STATE_MARKER_TOKEN=${token}
}

state_directory_has_entries() {
  local path="$1"
  local description="${2:-state directory}"
  local entry
  if ! path_exists "${path}"; then
    return 1
  fi
  entry=$(find -P "${path}" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null) || die "cannot inspect ${description}: ${path}"
  [ -n "${entry}" ]
}

decode_single_quoted() {
  local raw="$1"
  local length=${#raw}
  local index=1
  local character
  local value=""
  [ "${length}" -ge 2 ] || return 1
  [ "${raw:0:1}" = "'" ] || return 1
  while [ "${index}" -lt "${length}" ]; do
    character=${raw:${index}:1}
    if [ "${character}" = "'" ]; then
      if [ "${index}" -eq $((length - 1)) ]; then
        printf '%s\n' "${value}"
        return 0
      fi
      [ "${raw:${index}:4}" = "'\\''" ] || return 1
      value="${value}'"
      index=$((index + 4))
    else
      value="${value}${character}"
      index=$((index + 1))
    fi
  done
  return 1
}

decode_double_quoted() {
  local raw="$1"
  local length=${#raw}
  local index=1
  local end=$((length - 1))
  local character next
  local value=""
  [ "${length}" -ge 2 ] || return 1
  [ "${raw:0:1}" = '"' ] || return 1
  [ "${raw:length-1:1}" = '"' ] || return 1
  while [ "${index}" -lt "${end}" ]; do
    character=${raw:${index}:1}
    if [ "${character}" = "\\" ]; then
      [ "${index}" -lt $((end - 1)) ] || return 1
      next=${raw:$((index + 1)):1}
      case "${next}" in
        '"'|\\) value="${value}${next}" ;;
        *) return 1 ;;
      esac
      index=$((index + 2))
    else
      [ "${character}" != '"' ] || return 1
      value="${value}${character}"
      index=$((index + 1))
    fi
  done
  printf '%s\n' "${value}"
}

decode_manifest_value() {
  local raw="$1"
  [ "${#raw}" -le 16384 ] || return 1
  case "${raw}" in
    \'*) decode_single_quoted "${raw}" ;;
    \"*) decode_double_quoted "${raw}" ;;
    *) return 1 ;;
  esac
}

manifest_value_for_key() {
  local wanted="$1"
  local line key raw value
  local found=0
  local size
  [ -f "${MANIFEST}" ] || return 1
  size=$(wc -c <"${MANIFEST}") || return 1
  [ "${size}" -le 65536 ] || return 1
  while IFS= read -r line || [ -n "${line}" ]; do
    case "${line}" in
      ""|\#*) continue ;;
    esac
    [[ "${line}" == *=* ]] || return 1
    key=${line%%=*}
    [ "${key}" = "${wanted}" ] || continue
    [ "${found}" = "0" ] || return 1
    raw=${line#*=}
    value=$(decode_manifest_value "${raw}") || return 1
    reject_unsafe_text "${value}" "manifest ${wanted}"
    found=1
  done <"${MANIFEST}"
  [ "${found}" = "1" ] || return 1
  printf '%s\n' "${value}"
}

manifest_declares_version() {
  local expected="$1"
  local value
  value=$(manifest_value_for_key DM_MANIFEST_VERSION) || return 1
  [ "${value}" = "${expected}" ]
}

completion_path_matches_base() {
  local path="$1"
  local base="$2"
  case "${path}" in
    "${base}/bash-completion/completions/dm"|\
    "${base}/zsh/site-functions/_dm"|\
    "${base}/fish/vendor_completions.d/dm.fish"|\
    "${base}/powershell/Completions/dm.ps1")
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

completion_base_from_path() {
  local path="$1"
  case "${path}" in
    */bash-completion/completions/dm) printf '%s\n' "${path%/bash-completion/completions/dm}" ;;
    */zsh/site-functions/_dm) printf '%s\n' "${path%/zsh/site-functions/_dm}" ;;
    */fish/vendor_completions.d/dm.fish) printf '%s\n' "${path%/fish/vendor_completions.d/dm.fish}" ;;
    */powershell/Completions/dm.ps1) printf '%s\n' "${path%/powershell/Completions/dm.ps1}" ;;
    *) return 1 ;;
  esac
}

append_previous_completion_file() {
  local path="$1"
  local base="$2"
  local existing
  path=$(normalize_path "${path}" "manifest completion file")
  completion_path_matches_base "${path}" "${base}" || die "existing install manifest completion file is outside its base: ${path}"
  for existing in "${PREVIOUS_COMPLETION_FILES[@]+"${PREVIOUS_COMPLETION_FILES[@]}"}"; do
    [ "${existing}" != "${path}" ] || die "existing install manifest contains duplicate completion file: ${path}"
  done
  PREVIOUS_COMPLETION_FILES+=("${path}")
}

load_previous_completion_files() {
  PREVIOUS_COMPLETION_FILES=()
  PREVIOUS_COMPLETION_BASE_DIR=""
  local version base count legacy value index inferred joined old_ifs legacy_present=0
  local indexed_files=() legacy_files=()
  version=$(manifest_value_for_key DM_MANIFEST_VERSION) || return 0
  if legacy=$(manifest_value_for_key DM_COMPLETION_FILES); then
    legacy_present=1
  fi
  case "${version}" in
    2|3)
      if base=$(manifest_value_for_key DM_COMPLETION_BASE_DIR); then
        [ -n "${base}" ] || die "existing install manifest has an empty completion base directory"
        base=$(normalize_path "${base}" "manifest completion base directory")
      else
        base=""
      fi
      if count=$(manifest_value_for_key DM_COMPLETION_COUNT); then
        [[ "${count}" =~ ^[0-9]+$ ]] || die "existing install manifest has an invalid completion count"
        [ "${count}" -le 4 ] || die "existing install manifest has too many completion files"
        for ((index = 0; index < count; index++)); do
          value=$(manifest_value_for_key "DM_COMPLETION_FILE_${index}") || die "existing install manifest is missing completion file ${index}"
          value=$(normalize_path "${value}" "manifest completion file")
          indexed_files+=("${value}")
        done
        if [ -z "${base}" ] && [ "${count}" -gt 0 ]; then
          base=$(completion_base_from_path "${indexed_files[0]}" || true)
          [ -n "${base}" ] || die "existing install manifest completion file has an unsupported path: ${indexed_files[0]}"
          base=$(normalize_path "${base}" "manifest completion base directory")
        fi
        for value in "${indexed_files[@]+${indexed_files[@]}}"; do
          append_previous_completion_file "${value}" "${base}"
        done
        if [ "${legacy_present}" = "1" ]; then
          joined=$(IFS=:; printf '%s' "${indexed_files[*]}")
          [ "${joined}" = "${legacy}" ] || die "existing install manifest legacy completion list disagrees with indexed entries"
        fi
      elif [ "${legacy_present}" = "1" ] && [ -n "${legacy}" ]; then
        case "${legacy}" in
          :*|*:|*::*) die "existing install manifest contains an empty completion file" ;;
        esac
        old_ifs="${IFS}"
        IFS=: read -r -a legacy_files <<<"${legacy}"
        IFS="${old_ifs}"
        for value in "${legacy_files[@]+"${legacy_files[@]}"}"; do
          [ -n "${value}" ] || die "existing install manifest contains an empty completion file"
          value=$(normalize_path "${value}" "manifest completion file")
          if [ -z "${base}" ]; then
            inferred=$(completion_base_from_path "${value}" || true)
            [ -n "${inferred}" ] || die "existing install manifest completion file has an unsupported path: ${value}"
            base=$(normalize_path "${inferred}" "manifest completion base directory")
          else
            inferred=$(completion_base_from_path "${value}" || true)
            [ "${inferred}" = "${base}" ] || die "existing install manifest completion file is outside its base: ${value}"
          fi
          append_previous_completion_file "${value}" "${base}"
        done
      fi
      if [ -n "${base}" ]; then
        PREVIOUS_COMPLETION_BASE_DIR=${base}
        if [ "${COMPLETION_DIR_EXPLICIT}" = "1" ]; then
          [ "${COMPLETION_BASE_DIR}" = "${base}" ] || die "existing install manifest completion base directory does not match requested path"
        else
          COMPLETION_BASE_DIR=${base}
        fi
      fi
      ;;
    *)
      ;;
  esac
}

validate_existing_install_for_claim() {
  local expected_version="$1"
  local expected_token="${2:-}"
  local uninstall_script="${ROOT_DIR}/scripts/uninstall.sh"
  local args manifest_token
  [ -f "${uninstall_script}" ] || die "cannot validate existing install without uninstall.sh: ${uninstall_script}"
  manifest_declares_version "${expected_version}" || die "existing install manifest is not a valid version ${expected_version} manifest: ${MANIFEST}"
  args=(bash "${uninstall_script}" \
    --verify-install-state \
    --prefix "${PREFIX}" \
    --bin-dir "${BIN_DIR}" \
    --libexec-dir "${LIBEXEC_DIR}" \
    --config-dir "${CONFIG_DIR}" \
    --data-dir "${DATA_DIR}" \
    --completion-dir "${COMPLETION_BASE_DIR}")
  "${args[@]}" >/dev/null || die "existing version ${expected_version} install failed ownership/path preflight: ${MANIFEST}"
  if [ "${expected_version}" = "3" ]; then
    manifest_token=$(manifest_value_for_key DM_INSTALL_TOKEN) || die "existing version 3 install manifest is missing an ownership token: ${MANIFEST}"
    [[ "${manifest_token}" =~ ^[0-9a-f]{32}$ ]] || die "existing version 3 install manifest has an invalid ownership token: ${MANIFEST}"
    [ "${manifest_token}" = "${expected_token}" ] || die "existing version 3 install manifest and ownership markers use different tokens"
  fi
}

prepare_existing_install_paths() {
  path_exists "${MANIFEST}" || return 0
  assert_safe_file_target "${MANIFEST}" "install manifest" >/dev/null
  # Load completion ownership before the uninstall preflight so a legacy
  # manifest can supply its inferred custom completion base.
  load_previous_completion_files

  local uninstall_script="${ROOT_DIR}/scripts/uninstall.sh"
  local args value
  [ -f "${uninstall_script}" ] || die "cannot validate existing install without uninstall.sh: ${uninstall_script}"
  args=(bash "${uninstall_script}" --verify-install-state --config-dir "${CONFIG_DIR}")
  if [ "${PREFIX_EXPLICIT}" = "1" ]; then
    args+=(--prefix "${PREFIX}")
  fi
  if [ "${BIN_DIR_EXPLICIT}" = "1" ]; then
    args+=(--bin-dir "${BIN_DIR}")
  fi
  if [ "${LIBEXEC_DIR_EXPLICIT}" = "1" ]; then
    args+=(--libexec-dir "${LIBEXEC_DIR}")
  fi
  if [ "${DATA_DIR_EXPLICIT}" = "1" ]; then
    args+=(--data-dir "${DATA_DIR}")
  fi
  if [ "${COMPLETION_DIR_EXPLICIT}" = "1" ]; then
    args+=(--completion-dir "${COMPLETION_BASE_DIR}")
  fi
  "${args[@]}" >/dev/null || die "existing install failed manifest/path/ownership preflight: ${MANIFEST}"

  if [ "${PREFIX_EXPLICIT}" != "1" ]; then
    value=$(manifest_value_for_key DM_INSTALL_PREFIX) || die "existing install manifest is missing install_prefix: ${MANIFEST}"
    PREFIX=$(normalize_path "${value}" "manifest install prefix")
  fi
  if [ "${BIN_DIR_EXPLICIT}" != "1" ]; then
    value=$(manifest_value_for_key DM_BIN_DIR) || die "existing install manifest is missing bin_dir: ${MANIFEST}"
    BIN_DIR=$(normalize_path "${value}" "manifest bin directory")
  fi
  if [ "${LIBEXEC_DIR_EXPLICIT}" != "1" ]; then
    value=$(manifest_value_for_key DM_LIBEXEC_DIR) || die "existing install manifest is missing libexec_dir: ${MANIFEST}"
    LIBEXEC_DIR=$(normalize_path "${value}" "manifest libexec directory")
  fi
  if [ "${DATA_DIR_EXPLICIT}" != "1" ]; then
    value=$(manifest_value_for_key DM_DATA_DIR) || die "existing install manifest is missing data_dir: ${MANIFEST}"
    DATA_DIR=$(normalize_path "${value}" "manifest data directory")
  fi
  if [ "${COMPLETION_DIR_EXPLICIT}" != "1" ] && [ -z "${PREVIOUS_COMPLETION_BASE_DIR}" ]; then
    if value=$(manifest_value_for_key DM_COMPLETION_BASE_DIR); then
      [ -n "${value}" ] || die "existing install manifest has an empty completion_base_dir: ${MANIFEST}"
      COMPLETION_BASE_DIR=$(normalize_path "${value}" "manifest completion directory")
    fi
  fi

  CONFIG_FILE="${CONFIG_DIR}/dm.yaml"
  OUTPUT_DIR="${DATA_DIR}/images"
  INSTALLED_BIN="${LIBEXEC_DIR}/dm-bin"
  WRAPPER="${BIN_DIR}/dm"
  CONFIG_STATE_MARKER="${CONFIG_DIR}/.docker-manager-managed"
  DATA_STATE_MARKER="${DATA_DIR}/.docker-manager-managed"
}

render_root_profile() {
  local data_q config_q output_q bin_q
  data_q=$(shell_quote "${DATA_DIR}")
  config_q=$(shell_quote "${CONFIG_FILE}")
  output_q=$(shell_quote "${OUTPUT_DIR}")
  bin_q=$(shell_quote "${BIN_DIR}")
  printf '%s\n' \
    '# Managed by docker-manager install.sh' \
    "export DM_HOME=${data_q}" \
    "export DM_CONFIG=${config_q}" \
    "export DM_OUTPUT_DIR=${output_q}" \
    "case \":\$PATH:\" in" \
    "  *:${bin_q}:*) ;;" \
    "  *) export PATH=${bin_q}:\$PATH ;;" \
    'esac'
}

render_legacy_root_profile() {
  printf '%s\n' \
    '# Managed by docker-manager install.sh' \
    "export DM_HOME=\"${DATA_DIR}\"" \
    "export DM_CONFIG=\"${CONFIG_FILE}\"" \
    "export DM_OUTPUT_DIR=\"${OUTPUT_DIR}\"" \
    "case \":\$PATH:\" in" \
    "  *:\"${BIN_DIR}\":*) ;;" \
    "  *) export PATH=\"${BIN_DIR}:\$PATH\" ;;" \
    'esac'
}

root_profile_matches_install() {
  local path="$1"
  render_root_profile | cmp -s "${path}" - || render_legacy_root_profile | cmp -s "${path}" -
}

assert_profile_update_safe() {
  local path="$1"
  if ! path_exists "${path}"; then
    return 0
  fi
  assert_safe_file_target "${path}" "profile file" >/dev/null
  if [ "${IS_ROOT}" = "1" ]; then
    root_profile_matches_install "${path}" || die "refusing to overwrite a modified or unmanaged profile file: ${path}"
    return 0
  fi
  local start='# >>> docker-manager >>>'
  local end='# <<< docker-manager <<<'
  local start_count end_count start_line end_line
  start_count=$(grep -Fxc -- "${start}" "${path}" || true)
  end_count=$(grep -Fxc -- "${end}" "${path}" || true)
  if [ "${start_count}" = "0" ] && [ "${end_count}" = "0" ]; then
    return 0
  fi
  if [ "${start_count}" != "1" ] || [ "${end_count}" != "1" ]; then
    die "profile contains a malformed docker-manager block: ${path}"
  fi
  start_line=$(grep -Fnx -- "${start}" "${path}" | cut -d: -f1)
  end_line=$(grep -Fnx -- "${end}" "${path}" | cut -d: -f1)
  [ "${start_line}" -lt "${end_line}" ] || die "profile docker-manager block has invalid order: ${path}"
}

update_user_profile() {
  local path="$1"
  local start='# >>> docker-manager >>>'
  local end='# <<< docker-manager <<<'
  local parent content_tmp replacement_tmp original_mode original_identity current_identity replacement_mode replacement_mode_value
  local existed=0
  transaction_backup_file "${path}"
  if path_exists "${path}"; then
    existed=1
  else
    touch "${path}"
  fi
  assert_safe_file_target "${path}" "profile file" >/dev/null
  original_mode=$(stat_mode "${path}") || die "cannot inspect profile mode: ${path}"
  original_identity=$(stat_identity "${path}") || die "cannot inspect profile identity: ${path}"
  parent=$(dirname -- "${path}")
  assert_no_symlink_path "${parent}" "profile parent"
  content_tmp=$(mktemp "${parent}/.dm-profile-content.XXXXXX")
  transaction_track_temp "${content_tmp}"
  if ! awk -v start="${start}" -v end="${end}" '($0 == start) { skip=1; next } ($0 == end) { skip=0; next } !skip { print }' "${path}" >"${content_tmp}"; then
    rm -f -- "${content_tmp}"
    die "cannot prepare profile update: ${path}"
  fi
  cat >>"${content_tmp}" <<EOF
${start}
export DM_HOME=${DATA_DIR_Q}
export DM_CONFIG=${CONFIG_FILE_Q}
export DM_OUTPUT_DIR=${OUTPUT_DIR_Q}
case ":\$PATH:" in
  *:${BIN_DIR_Q}:*) ;;
  *) export PATH=${BIN_DIR_Q}:\$PATH ;;
esac
${end}
EOF
  chmod 0600 "${content_tmp}"
  replacement_tmp=$(mktemp "${parent}/.dm-profile-replacement.XXXXXX")
  transaction_track_temp "${replacement_tmp}"
  if [ "${existed}" = "1" ]; then
    if ! cp --preserve=all "${path}" "${replacement_tmp}" 2>/dev/null && ! cp -p "${path}" "${replacement_tmp}"; then
      rm -f -- "${content_tmp}" "${replacement_tmp}"
      die "cannot preserve profile metadata: ${path}"
    fi
  fi
  replacement_mode=$(stat_mode "${replacement_tmp}") || {
    rm -f -- "${content_tmp}" "${replacement_tmp}"
    die "cannot inspect replacement profile mode: ${path}"
  }
  case "${replacement_mode}" in
    ""|*[!0-7]*)
      rm -f -- "${content_tmp}" "${replacement_tmp}"
      die "replacement profile has an invalid mode: ${path}"
      ;;
  esac
  replacement_mode_value=$((8#${replacement_mode}))
  if (( (replacement_mode_value & 0200) == 0 )); then
    if ! chmod u+w "${replacement_tmp}"; then
      rm -f -- "${content_tmp}" "${replacement_tmp}"
      die "cannot make replacement profile writable: ${path}"
    fi
  fi
  if ! cat "${content_tmp}" >"${replacement_tmp}"; then
    rm -f -- "${content_tmp}" "${replacement_tmp}"
    die "cannot write profile replacement: ${path}"
  fi
  rm -f -- "${content_tmp}"
  if ! chmod "${original_mode}" "${replacement_tmp}"; then
    rm -f -- "${replacement_tmp}"
    die "cannot restore replacement profile mode: ${path}"
  fi
  assert_no_symlink_path "${path}" "profile file"
  current_identity=$(stat_identity "${path}") || {
    rm -f -- "${replacement_tmp}"
    die "cannot recheck profile identity: ${path}"
  }
  [ "${current_identity}" = "${original_identity}" ] || {
    rm -f -- "${replacement_tmp}"
    die "profile changed during update: ${path}"
  }
  mv -f -- "${replacement_tmp}" "${path}"
  chmod "${original_mode}" "${path}"
  assert_safe_file_target "${path}" "profile file" >/dev/null
}

usage() {
  cat <<'EOF'
Usage: scripts/install.sh [options]

Install docker-manager as dm.

Options:
  --prefix DIR          Install prefix. Default: /usr/local for root, ~/.local otherwise
  --install-dir DIR     Alias of --prefix
  --bin-dir DIR         Directory for the dm wrapper. Default: <prefix>/bin
  --libexec-dir DIR     Directory for dm-bin. Default: <prefix>/lib/docker-manager
  --config-dir DIR      Config directory. Default: /etc/docker-manager or ~/.config/docker-manager
  --data-dir DIR        Data directory. Default: /var/lib/docker-manager or ~/.local/share/docker-manager
  --binary PATH         Existing dm binary to install
  --build               Build dm from the current source tree when --binary is not set
  --overwrite-config    Replace existing config file
  --completion SHELL    Install shell completion for bash, zsh, fish, powershell or all. Repeatable.
                        Default: bash
  --completion-dir DIR  Base directory for completion files. Default: <prefix>/share
  --no-completion       Do not install shell completion
  --no-profile          Do not write shell environment profile
  --dry-run             Print actions without changing files
  -h, --help            Show this help

Installed environment variables:
  DM_CONFIG       Default config file used by the dm wrapper
  DM_HOME         docker-manager data directory
  DM_OUTPUT_DIR   Default image output directory used in generated config
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --prefix|--install-dir)
      PREFIX=${2:?missing value for $1}
      PREFIX_EXPLICIT=1
      shift 2
      ;;
    --bin-dir)
      BIN_DIR=${2:?missing value for --bin-dir}
      BIN_DIR_EXPLICIT=1
      shift 2
      ;;
    --libexec-dir)
      LIBEXEC_DIR=${2:?missing value for --libexec-dir}
      LIBEXEC_DIR_EXPLICIT=1
      shift 2
      ;;
    --config-dir)
      CONFIG_DIR=${2:?missing value for --config-dir}
      shift 2
      ;;
    --data-dir)
      DATA_DIR=${2:?missing value for --data-dir}
      DATA_DIR_EXPLICIT=1
      shift 2
      ;;
    --binary)
      BINARY=${2:?missing value for --binary}
      shift 2
      ;;
    --build)
      BUILD_FROM_SOURCE=1
      shift
      ;;
    --overwrite-config)
      OVERWRITE_CONFIG=1
      shift
      ;;
    --completion)
      IFS=',' read -r -a requested_completions <<<"${2:?missing value for --completion}"
      COMPLETION_SHELLS+=("${requested_completions[@]+"${requested_completions[@]}"}")
      shift 2
      ;;
    --completion-dir)
      COMPLETION_DIR=${2:?missing value for --completion-dir}
      COMPLETION_DIR_EXPLICIT=1
      shift 2
      ;;
    --no-completion)
      NO_COMPLETION=1
      COMPLETION_SHELLS=()
      shift
      ;;
    --no-profile)
      NO_PROFILE=1
      shift
      ;;
    --dry-run)
      DRY_RUN=1
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

PREFIX=$(normalize_path "${PREFIX}" "prefix")
BIN_DIR=${BIN_DIR:-"${PREFIX}/bin"}
LIBEXEC_DIR=${LIBEXEC_DIR:-"${PREFIX}/lib/docker-manager"}
CONFIG_DIR=$(normalize_path "${CONFIG_DIR}" "config directory")
DATA_DIR=$(normalize_path "${DATA_DIR}" "data directory")
BIN_DIR=$(normalize_path "${BIN_DIR}" "binary directory")
LIBEXEC_DIR=$(normalize_path "${LIBEXEC_DIR}" "libexec directory")
CONFIG_FILE="${CONFIG_DIR}/dm.yaml"
OUTPUT_DIR="${DATA_DIR}/images"
INSTALLED_BIN="${LIBEXEC_DIR}/dm-bin"
WRAPPER="${BIN_DIR}/dm"
MANIFEST="${CONFIG_DIR}/install.env"
CONFIG_STATE_MARKER="${CONFIG_DIR}/.docker-manager-managed"
DATA_STATE_MARKER="${DATA_DIR}/.docker-manager-managed"
INSTALL_TOKEN=$(generate_install_token)
COMPLETION_BASE_DIR=${COMPLETION_DIR:-"${PREFIX}/share"}
COMPLETION_BASE_DIR=$(normalize_path "${COMPLETION_BASE_DIR}" "completion directory")
PROFILE_FILE=$(normalize_path "${PROFILE_FILE}" "profile file")
COMPLETION_FILES=()
if [ "${NO_COMPLETION}" != "1" ] && [ "${COMPLETION_SHELLS[0]+set}" != "set" ]; then
  COMPLETION_SHELLS=(bash)
fi

# A verified existing manifest may carry custom paths. Adopt those values
# before any ownership or destination preflight when the caller did not
# explicitly override the corresponding option.
prepare_existing_install_paths

assert_not_root_path "${PREFIX}" "prefix"
assert_not_root_path "${BIN_DIR}" "binary directory"
assert_not_root_path "${LIBEXEC_DIR}" "libexec directory"
assert_not_root_path "${CONFIG_DIR}" "config directory"
assert_not_root_path "${DATA_DIR}" "data directory"
assert_not_root_path "${COMPLETION_BASE_DIR}" "completion directory"
for install_path in "${PREFIX}" "${BIN_DIR}" "${LIBEXEC_DIR}" "${CONFIG_DIR}" "${DATA_DIR}" "${COMPLETION_BASE_DIR}" "${PROFILE_FILE}"; do
  assert_no_symlink_path "${install_path}" "install path"
done
for install_dir in "${PREFIX}" "${BIN_DIR}" "${LIBEXEC_DIR}" "${CONFIG_DIR}" "${DATA_DIR}" "${COMPLETION_BASE_DIR}"; do
  if path_exists "${install_dir}" && [ ! -d "${install_dir}" ]; then
    die "install directory is not a directory: ${install_dir}"
  fi
done
assert_purge_boundaries "${CONFIG_DIR}" "${DATA_DIR}" "${PREFIX}" "${BIN_DIR}" "${LIBEXEC_DIR}" "${COMPLETION_BASE_DIR}" "${PROFILE_FILE}"
assert_dedicated_state_path "${CONFIG_DIR}" "config directory"
assert_dedicated_state_path "${DATA_DIR}" "data directory"

CONFIG_MARKER_PRESENT=0
DATA_MARKER_PRESENT=0
path_exists "${CONFIG_STATE_MARKER}" && CONFIG_MARKER_PRESENT=1
path_exists "${DATA_STATE_MARKER}" && DATA_MARKER_PRESENT=1
if [ "${CONFIG_MARKER_PRESENT}" != "${DATA_MARKER_PRESENT}" ]; then
  die "config and data ownership markers must be present together; refusing partial install state"
fi

EXISTING_STATE_MARKER_TOKEN=""
if [ "${CONFIG_MARKER_PRESENT}" = "1" ]; then
  assert_existing_state_marker "${CONFIG_STATE_MARKER}" "config" "${CONFIG_DIR}"
  CONFIG_MARKER_TOKEN=${EXISTING_STATE_MARKER_TOKEN}
  EXISTING_STATE_MARKER_TOKEN=""
  assert_existing_state_marker "${DATA_STATE_MARKER}" "data" "${DATA_DIR}"
  DATA_MARKER_TOKEN=${EXISTING_STATE_MARKER_TOKEN}
  [ "${CONFIG_MARKER_TOKEN}" = "${DATA_MARKER_TOKEN}" ] || die "config and data ownership markers use different tokens"
  validate_existing_install_for_claim "3" "${CONFIG_MARKER_TOKEN}"
  INSTALL_TOKEN=${CONFIG_MARKER_TOKEN}
else
  config_has_entries=0
  data_has_entries=0
  if state_directory_has_entries "${CONFIG_DIR}" "config directory"; then
    config_has_entries=1
  fi
  if state_directory_has_entries "${DATA_DIR}" "data directory"; then
    data_has_entries=1
  fi
  if [ "${config_has_entries}" = "1" ] || [ "${data_has_entries}" = "1" ]; then
    assert_safe_file_target "${MANIFEST}" "install manifest" >/dev/null
    manifest_declares_version "2" || die "refusing to claim non-empty unowned config/data directories; only a validated version 2 install may be migrated"
    validate_existing_install_for_claim "2"
  fi
fi

run() {
  if [ "${DRY_RUN}" = "1" ]; then
    printf 'DRY-RUN:'
    printf ' %q' "$@"
    printf '\n'
  else
    "$@"
  fi
}

INSTALL_TRANSACTION_ACTIVE=0
INSTALL_TRANSACTION_COMMITTED=0
INSTALL_TRANSACTION_DIR=""

transaction_file_recorded() {
  local path="$1"
  local entry recorded
  for entry in "${INSTALL_TRANSACTION_DIR}/backups"/entry.*; do
    [ -d "${entry}" ] || continue
    recorded=""
    if IFS= read -r recorded <"${entry}/path" && [ "${recorded}" = "${path}" ]; then
      return 0
    fi
  done
  return 1
}

transaction_backup_file() {
  local path="$1"
  [ "${INSTALL_TRANSACTION_ACTIVE}" = "1" ] || return 0
  transaction_file_recorded "${path}" && return 0

  local entry backup="" existed=0 parent
  entry=$(mktemp -d "${INSTALL_TRANSACTION_DIR}/backups/entry.XXXXXX") || die "cannot create install transaction record"
  if path_exists "${path}"; then
    existed=1
    parent=$(dirname -- "${path}")
    backup=$(mktemp "${parent}/.dm-install-backup.XXXXXX") || {
      rmdir -- "${entry}" 2>/dev/null || true
      die "cannot create backup for install target: ${path}"
    }
    if ! cp --preserve=all "${path}" "${backup}" 2>/dev/null && ! cp -p "${path}" "${backup}"; then
      rm -f -- "${backup}"
      rmdir -- "${entry}" 2>/dev/null || true
      die "cannot back up install target: ${path}"
    fi
  fi
  if ! printf '%s\n' "${path}" >"${entry}/path" ||
    ! printf '%s\n' "${existed}" >"${entry}/existed" ||
    ! printf '%s\n' "${backup}" >"${entry}/backup"; then
    [ -z "${backup}" ] || rm -f -- "${backup}"
    rm -f -- "${entry}/path" "${entry}/existed" "${entry}/backup"
    rmdir -- "${entry}" 2>/dev/null || true
    die "cannot record install transaction target: ${path}"
  fi
}

transaction_track_temp() {
  local path="$1"
  [ "${INSTALL_TRANSACTION_ACTIVE}" = "1" ] || return 0
  local record
  record=$(mktemp "${INSTALL_TRANSACTION_DIR}/temps/path.XXXXXX") || {
    rm -f -- "${path}"
    die "cannot record install temporary file: ${path}"
  }
  if ! printf '%s\n' "${path}" >"${record}"; then
    rm -f -- "${record}" "${path}"
    die "cannot record install temporary file: ${path}"
  fi
}

transaction_track_created_directory() {
  local path="$1"
  [ "${INSTALL_TRANSACTION_ACTIVE}" = "1" ] || return 0
  local record
  record=$(mktemp "${INSTALL_TRANSACTION_DIR}/directories/path.XXXXXX") || return 1
  if ! printf '%s\n' "${path}" >"${record}"; then
    rm -f -- "${record}"
    return 1
  fi
}

cleanup_install_transaction_journal() {
  local entry record failed=0
  [ -n "${INSTALL_TRANSACTION_DIR}" ] || return 0
  for entry in "${INSTALL_TRANSACTION_DIR}/backups"/entry.*; do
    [ -d "${entry}" ] || continue
    rm -f -- "${entry}/path" "${entry}/existed" "${entry}/backup" || failed=1
    rmdir -- "${entry}" 2>/dev/null || failed=1
  done
  for record in "${INSTALL_TRANSACTION_DIR}/temps"/path.* "${INSTALL_TRANSACTION_DIR}/directories"/path.*; do
    [ -f "${record}" ] || continue
    rm -f -- "${record}" || failed=1
  done
  rmdir -- "${INSTALL_TRANSACTION_DIR}/backups" 2>/dev/null || failed=1
  rmdir -- "${INSTALL_TRANSACTION_DIR}/temps" 2>/dev/null || failed=1
  rmdir -- "${INSTALL_TRANSACTION_DIR}/directories" 2>/dev/null || failed=1
  rmdir -- "${INSTALL_TRANSACTION_DIR}" 2>/dev/null || failed=1
  return "${failed}"
}

rollback_install_transaction() {
  local record entry path existed backup pass progress remaining failed=0
  set +e
  for record in "${INSTALL_TRANSACTION_DIR}/temps"/path.*; do
    [ -f "${record}" ] || continue
    path=""
    if ! IFS= read -r path <"${record}"; then
      failed=1
      continue
    fi
    rm -f -- "${path}" || failed=1
  done

  for entry in "${INSTALL_TRANSACTION_DIR}/backups"/entry.*; do
    [ -d "${entry}" ] || continue
    path=""
    existed=""
    backup=""
    if ! IFS= read -r path <"${entry}/path" ||
      ! IFS= read -r existed <"${entry}/existed" ||
      ! IFS= read -r backup <"${entry}/backup"; then
      failed=1
      continue
    fi
    if [ "${existed}" = "1" ]; then
      if [ ! -f "${backup}" ] || { [ -d "${path}" ] && [ ! -L "${path}" ]; }; then
        failed=1
        continue
      fi
      rm -f -- "${path}" || {
        failed=1
        continue
      }
      mv -f -- "${backup}" "${path}" || failed=1
    else
      rm -f -- "${path}" || failed=1
    fi
  done

  # Directory records are created from the outside in. Remove them until no
  # progress is possible, then keep the journal if anything remains (for
  # example because a concurrent writer made a tracked directory non-empty).
  pass=0
  while [ "${pass}" -lt 4096 ]; do
    progress=0
    remaining=0
    for record in "${INSTALL_TRANSACTION_DIR}/directories"/path.*; do
      [ -f "${record}" ] || continue
      path=""
      if ! IFS= read -r path <"${record}"; then
        failed=1
        remaining=1
        continue
      fi
      if [ -L "${path}" ]; then
        failed=1
        remaining=1
      elif [ -d "${path}" ]; then
        if rmdir -- "${path}" 2>/dev/null; then
          progress=1
        else
          remaining=1
        fi
      elif path_exists "${path}"; then
        failed=1
        remaining=1
      fi
    done
    [ "${remaining}" = "0" ] && break
    if [ "${progress}" = "0" ]; then
      failed=1
      break
    fi
    pass=$((pass + 1))
  done

  for record in "${INSTALL_TRANSACTION_DIR}/directories"/path.*; do
    [ -f "${record}" ] || continue
    path=""
    if ! IFS= read -r path <"${record}" || path_exists "${path}"; then
      failed=1
    fi
  done

  if [ "${failed}" = "0" ]; then
    cleanup_install_transaction_journal || failed=1
  fi
  return "${failed}"
}

finish_install_transaction() {
  local status=$?
  trap - EXIT
  if [ "${INSTALL_TRANSACTION_ACTIVE}" = "1" ] && [ "${INSTALL_TRANSACTION_COMMITTED}" != "1" ]; then
    [ "${status}" -ne 0 ] || status=1
    echo "install.sh: installation failed; restoring previous installation state" >&2
    if ! rollback_install_transaction; then
      echo "install.sh: rollback was incomplete; recovery records remain in ${INSTALL_TRANSACTION_DIR}" >&2
      status=1
    fi
  fi
  exit "${status}"
}

begin_install_transaction() {
  [ "${DRY_RUN}" != "1" ] || return 0
  local transaction_parent="${TMPDIR:-/tmp}"
  [ -d "${transaction_parent}" ] || die "install transaction parent is not a directory: ${transaction_parent}"
  INSTALL_TRANSACTION_DIR=$(mktemp -d "${transaction_parent%/}/dm-install-transaction.XXXXXX") || die "cannot create install transaction directory"
  if ! chmod 0700 "${INSTALL_TRANSACTION_DIR}" ||
    ! mkdir "${INSTALL_TRANSACTION_DIR}/backups" "${INSTALL_TRANSACTION_DIR}/temps" "${INSTALL_TRANSACTION_DIR}/directories"; then
    rm -rf -- "${INSTALL_TRANSACTION_DIR}"
    INSTALL_TRANSACTION_DIR=""
    die "cannot initialize install transaction"
  fi
  INSTALL_TRANSACTION_ACTIVE=1
  INSTALL_TRANSACTION_COMMITTED=0
  trap finish_install_transaction EXIT
}

commit_install_transaction() {
  local entry record backup="" cleanup_failed=0
  [ "${INSTALL_TRANSACTION_ACTIVE}" = "1" ] || return 0
  INSTALL_TRANSACTION_COMMITTED=1
  INSTALL_TRANSACTION_ACTIVE=0
  trap - EXIT

  for entry in "${INSTALL_TRANSACTION_DIR}/backups"/entry.*; do
    [ -d "${entry}" ] || continue
    backup=""
    IFS= read -r backup <"${entry}/backup" || cleanup_failed=1
    if [ -n "${backup}" ]; then
      rm -f -- "${backup}" || cleanup_failed=1
    fi
  done
  for record in "${INSTALL_TRANSACTION_DIR}/temps"/path.*; do
    [ -f "${record}" ] || continue
    if IFS= read -r backup <"${record}" && [ -n "${backup}" ]; then
      rm -f -- "${backup}" || cleanup_failed=1
    else
      cleanup_failed=1
    fi
  done
  cleanup_install_transaction_journal || cleanup_failed=1
  if [ "${cleanup_failed}" != "0" ]; then
    echo "install.sh: warning: could not remove all transaction temporary files from ${INSTALL_TRANSACTION_DIR}" >&2
  fi
}

yaml_single_quote() {
  printf "'%s'" "$(printf '%s' "$1" | sed "s/'/''/g")"
}

write_file() {
  local path="$1"
  local mode="$2"
  local parent tmp
  parent=$(dirname -- "${path}")
  assert_no_symlink_path "${parent}" "file parent"
  if [ ! -d "${parent}" ] && [ "${DRY_RUN}" != "1" ]; then
    die "file parent does not exist: ${parent}"
  fi
  assert_safe_file_target "${path}" "file target" >/dev/null
  if [ "${DRY_RUN}" = "1" ]; then
    # Consume the heredoc without creating a file in the caller's directory.
    tmp=$(mktemp)
    cat >"${tmp}"
    echo "DRY-RUN: write ${path}"
    rm -f -- "${tmp}"
    return 0
  fi
  transaction_backup_file "${path}"
  tmp=$(mktemp "${parent}/.dm-install.tmp.XXXXXX")
  transaction_track_temp "${tmp}"
  cat >"${tmp}"
  chmod 0600 "${tmp}"
  assert_no_symlink_path "${tmp}" "temporary file"
  mv -f "${tmp}" "${path}"
  chmod "${mode}" "${path}"
  assert_safe_file_target "${path}" "file target" >/dev/null
}

copy_binary() {
  local source="$1"
  local destination="$2"
  local parent tmp
  if [ "${DRY_RUN}" = "1" ]; then
    echo "DRY-RUN: install ${source} ${destination}"
    return 0
  fi
  assert_no_symlink_path "${source}" "source binary"
  [ -f "${source}" ] || die "source binary is not a regular file: ${source}"
  parent=$(dirname -- "${destination}")
  assert_no_symlink_path "${parent}" "binary parent"
  assert_safe_file_target "${destination}" "installed binary" >/dev/null
  transaction_backup_file "${destination}"
  tmp=$(mktemp "${parent}/.dm-install.tmp.XXXXXX")
  transaction_track_temp "${tmp}"
  chmod 0600 "${tmp}"
  cp "${source}" "${tmp}"
  chmod 0755 "${tmp}"
  mv -f "${tmp}" "${destination}"
  chmod 0755 "${destination}"
  assert_safe_file_target "${destination}" "installed binary" >/dev/null
}

write_command_output() {
  local destination="$1"
  shift
  local parent tmp
  parent=$(dirname -- "${destination}")
  assert_no_symlink_path "${parent}" "completion parent"
  assert_safe_file_target "${destination}" "completion file" >/dev/null
  if [ "${DRY_RUN}" = "1" ]; then
    echo "DRY-RUN: $* > ${destination}"
    return 0
  fi
  transaction_backup_file "${destination}"
  tmp=$(mktemp "${parent}/.dm-install.tmp.XXXXXX")
  transaction_track_temp "${tmp}"
  chmod 0600 "${tmp}"
  "$@" >"${tmp}"
  chmod 0644 "${tmp}"
  mv -f "${tmp}" "${destination}"
  chmod 0644 "${destination}"
  assert_safe_file_target "${destination}" "completion file" >/dev/null
}

completion_file_for_shell() {
  local shell="$1"
  case "${shell}" in
    bash) printf '%s\n' "${COMPLETION_BASE_DIR}/bash-completion/completions/dm" ;;
    zsh) printf '%s\n' "${COMPLETION_BASE_DIR}/zsh/site-functions/_dm" ;;
    fish) printf '%s\n' "${COMPLETION_BASE_DIR}/fish/vendor_completions.d/dm.fish" ;;
    powershell) printf '%s\n' "${COMPLETION_BASE_DIR}/powershell/Completions/dm.ps1" ;;
    *) return 1 ;;
  esac
}

normalize_completion_shells() {
  local shell
  local normalized=()
  if [ "${COMPLETION_SHELLS[0]+set}" != "set" ]; then
    return 0
  fi
  for shell in "${COMPLETION_SHELLS[@]+"${COMPLETION_SHELLS[@]}"}"; do
    shell=$(printf '%s' "${shell}" | tr '[:upper:]' '[:lower:]')
    case "${shell}" in
      all)
        normalized+=(bash zsh fish powershell)
        ;;
      bash|zsh|fish|powershell)
        normalized+=("${shell}")
        ;;
      "")
        ;;
      *)
        echo "Unsupported completion shell: ${shell}" >&2
        exit 2
        ;;
    esac
  done
  COMPLETION_SHELLS=()
  local seen=" "
  for shell in "${normalized[@]+"${normalized[@]}"}"; do
    case "${seen}" in
      *" ${shell} "*) ;;
      *)
        COMPLETION_SHELLS+=("${shell}")
        seen="${seen}${shell} "
        ;;
    esac
  done
}

install_completions() {
  local shell path dir
  if [ "${NO_COMPLETION}" = "1" ]; then
    return 0
  fi
  normalize_completion_shells
  for shell in "${COMPLETION_SHELLS[@]+"${COMPLETION_SHELLS[@]}"}"; do
    path=$(completion_file_for_shell "${shell}")
    dir=$(dirname "${path}")
    ensure_directory "${dir}" "completion directory"
    write_command_output "${path}" "${INSTALLED_BIN}" completion "${shell}"
    COMPLETION_FILES+=("${path}")
  done
}

completion_is_current() {
  local candidate="$1"
  local current
  for current in "${COMPLETION_FILES[@]+"${COMPLETION_FILES[@]}"}"; do
    [ "${current}" = "${candidate}" ] && return 0
  done
  return 1
}

remove_stale_completions() {
  local path
  for path in "${PREVIOUS_COMPLETION_FILES[@]+"${PREVIOUS_COMPLETION_FILES[@]}"}"; do
    if completion_is_current "${path}"; then
      continue
    fi
    # The manifest authorizes only the exact completion paths it recorded;
    # recheck the target immediately before removing it so a replacement
    # symlink or foreign-owned file fails closed and rolls back the install.
      assert_safe_file_target "${path}" "stale completion file" >/dev/null
      if path_exists "${path}"; then
        transaction_backup_file "${path}"
        rm -f -- "${path}"
        if path_exists "${path}"; then
          die "stale completion file remained after removal: ${path}"
        fi
      fi
  done
}

resolve_binary() {
  if [ -n "${BINARY}" ]; then
    printf '%s\n' "${BINARY}"
    return 0
  fi
  if [ -f "${ROOT_DIR}/dm" ]; then
    printf '%s\n' "${ROOT_DIR}/dm"
    return 0
  fi
  if [ -f "${ROOT_DIR}/bin/dev/dm" ]; then
    printf '%s\n' "${ROOT_DIR}/bin/dev/dm"
    return 0
  fi
  if [ "${BUILD_FROM_SOURCE}" = "1" ]; then
    local built="${ROOT_DIR}/bin/install/dm"
    ensure_directory "$(dirname -- "${built}")" "build directory" >&2
    if [ "${DRY_RUN}" != "1" ]; then
      assert_safe_file_target "${built}" "build output" >/dev/null
      transaction_backup_file "${built}"
      if ! command -v go >/dev/null 2>&1; then
        echo "--build requires go in PATH" >&2
        exit 1
      fi
      (
        cd "${ROOT_DIR}"
        VERSION=${VERSION:-dev}
        COMMIT=${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}
        BUILD_DATE=${BUILD_DATE:-$(date -u +"%Y-%m-%dT%H:%M:%SZ")}
        LDFLAGS="-s -w -X docker-manager/internal/version.version=${VERSION} -X docker-manager/internal/version.commit=${COMMIT} -X docker-manager/internal/version.buildDate=${BUILD_DATE}"
        CGO_ENABLED=0 go build -trimpath -ldflags "${LDFLAGS}" -o "${built}" .
      )
    fi
    printf '%s\n' "${built}"
    return 0
  fi
  echo "No dm binary found. Pass --binary PATH or --build." >&2
  exit 1
}

begin_install_transaction
SOURCE_BIN=$(resolve_binary)
if [ ! -f "${SOURCE_BIN}" ] && [ "${DRY_RUN}" != "1" ]; then
  echo "Binary not found: ${SOURCE_BIN}" >&2
  exit 1
fi
if [ "${DRY_RUN}" != "1" ]; then
  SOURCE_BIN=$(normalize_path "${SOURCE_BIN}" "source binary")
  assert_no_symlink_path "${SOURCE_BIN}" "source binary"
  [ -f "${SOURCE_BIN}" ] || die "source binary is not a regular file: ${SOURCE_BIN}"
fi

# Check every destination before the first mutation. This keeps a forged
# symlink or a foreign-owned target from causing a partial installation.
for destination in "${PREFIX}" "${BIN_DIR}" "${LIBEXEC_DIR}" "${CONFIG_DIR}" "${DATA_DIR}" "${COMPLETION_BASE_DIR}"; do
  assert_safe_directory "${destination}" "install destination" >/dev/null
done
assert_safe_file_target "${WRAPPER}" "wrapper" >/dev/null
assert_safe_file_target "${INSTALLED_BIN}" "installed binary" >/dev/null
assert_safe_file_target "${CONFIG_FILE}" "config file" >/dev/null
assert_safe_file_target "${MANIFEST}" "install manifest" >/dev/null
assert_safe_file_target "${CONFIG_STATE_MARKER}" "config state marker" >/dev/null
assert_safe_file_target "${DATA_STATE_MARKER}" "data state marker" >/dev/null
if [ "${NO_PROFILE}" != "1" ]; then
  assert_safe_file_target "${PROFILE_FILE}" "profile file" >/dev/null
  assert_profile_update_safe "${PROFILE_FILE}"
fi

echo "Installing docker-manager"
echo "  wrapper: ${WRAPPER}"
echo "  binary:  ${INSTALLED_BIN}"
echo "  config:  ${CONFIG_FILE}"
echo "  data:    ${DATA_DIR}"

ensure_directory "${BIN_DIR}" "binary directory"
ensure_directory "${LIBEXEC_DIR}" "libexec directory"
ensure_directory "${CONFIG_DIR}" "config directory"
ensure_directory "${DATA_DIR}" "data directory"
ensure_directory "${OUTPUT_DIR}" "output directory"
copy_binary "${SOURCE_BIN}" "${INSTALLED_BIN}"
install_completions
remove_stale_completions

PREFIX_Q=$(shell_quote "${PREFIX}")
BIN_DIR_Q=$(shell_quote "${BIN_DIR}")
LIBEXEC_DIR_Q=$(shell_quote "${LIBEXEC_DIR}")
INSTALLED_BIN_Q=$(shell_quote "${INSTALLED_BIN}")
CONFIG_DIR_Q=$(shell_quote "${CONFIG_DIR}")
CONFIG_FILE_Q=$(shell_quote "${CONFIG_FILE}")
DATA_DIR_Q=$(shell_quote "${DATA_DIR}")
OUTPUT_DIR_Q=$(shell_quote "${OUTPUT_DIR}")
PROFILE_FILE_Q=$(shell_quote "${PROFILE_FILE}")
COMPLETION_BASE_DIR_Q=$(shell_quote "${COMPLETION_BASE_DIR}")
COMPLETION_FILES_VALUE=""
COMPLETION_COUNT=0
if [ "${COMPLETION_FILES[0]+set}" = "set" ]; then
  COMPLETION_FILES_VALUE=$(IFS=:; printf '%s' "${COMPLETION_FILES[*]}")
  COMPLETION_COUNT=${#COMPLETION_FILES[@]}
fi
COMPLETION_FILES_Q=$(shell_quote "${COMPLETION_FILES_VALUE}")

write_file "${WRAPPER}" 0755 <<EOF
#!/usr/bin/env sh
# Managed by docker-manager install.sh
set -eu
CONFIG_ARG=""
if [ -z "\${DM_CONFIG:-}" ]; then
  DM_CONFIG=${CONFIG_FILE_Q}
  export DM_CONFIG
fi
for arg in "\$@"; do
  case "\$arg" in
    --config|--config=*)
      CONFIG_ARG="present"
      break
      ;;
  esac
done
if [ -n "\${CONFIG_ARG}" ]; then
  exec ${INSTALLED_BIN_Q} "\$@"
fi
exec ${INSTALLED_BIN_Q} --config "\${DM_CONFIG}" "\$@"
EOF

if [ "${OVERWRITE_CONFIG}" = "1" ] || [ ! -f "${CONFIG_FILE}" ]; then
  OUTPUT_DIR_YAML=$(yaml_single_quote "${OUTPUT_DIR}")
  write_file "${CONFIG_FILE}" 0644 <<EOF
# docker-manager config generated by install.sh
proxy:
os: linux
arch: amd64
output_dir: ${OUTPUT_DIR_YAML}
verbose: false
quiet: false
log_json: false
EOF
else
  echo "Keeping existing config: ${CONFIG_FILE}"
fi

if [ "${NO_PROFILE}" != "1" ]; then
  if [ "${IS_ROOT}" = "1" ]; then
    write_file "${PROFILE_FILE}" 0644 <<EOF
# Managed by docker-manager install.sh
export DM_HOME=${DATA_DIR_Q}
export DM_CONFIG=${CONFIG_FILE_Q}
export DM_OUTPUT_DIR=${OUTPUT_DIR_Q}
case ":\$PATH:" in
  *:${BIN_DIR_Q}:*) ;;
  *) export PATH=${BIN_DIR_Q}:\$PATH ;;
esac
EOF
  else
    if [ "${DRY_RUN}" = "1" ]; then
      echo "DRY-RUN: update ${PROFILE_FILE}"
    else
      update_user_profile "${PROFILE_FILE}"
    fi
  fi
else
  echo "Skipped profile update."
fi

# Commit ownership state only after every artifact that can fail has been
# installed. The transaction trap restores the previous files if any of these
# final writes fail partway through.
{
cat <<EOF
# docker-manager install manifest; values are data, never shell-evaluated.
DM_MANIFEST_VERSION='3'
DM_INSTALL_UID='${INSTALL_UID}'
DM_INSTALL_GID='${INSTALL_GID}'
DM_INSTALL_TOKEN='${INSTALL_TOKEN}'
DM_INSTALL_PREFIX=${PREFIX_Q}
DM_BIN_DIR=${BIN_DIR_Q}
DM_LIBEXEC_DIR=${LIBEXEC_DIR_Q}
DM_CONFIG_DIR=${CONFIG_DIR_Q}
DM_CONFIG_FILE=${CONFIG_FILE_Q}
DM_DATA_DIR=${DATA_DIR_Q}
DM_OUTPUT_DIR=${OUTPUT_DIR_Q}
DM_PROFILE_FILE=${PROFILE_FILE_Q}
DM_COMPLETION_BASE_DIR=${COMPLETION_BASE_DIR_Q}
DM_COMPLETION_FILES=${COMPLETION_FILES_Q}
DM_COMPLETION_COUNT='${COMPLETION_COUNT}'
EOF
# Keep one path per manifest key so a legal Unix ':' in a path cannot become
# an ambiguous list separator. DM_COMPLETION_FILES remains for old readers.
completion_index=0
for completion_path in "${COMPLETION_FILES[@]+"${COMPLETION_FILES[@]}"}"; do
  completion_path_q=$(shell_quote "${completion_path}")
  printf 'DM_COMPLETION_FILE_%s=%s\n' "${completion_index}" "${completion_path_q}"
  completion_index=$((completion_index + 1))
done
} | write_file "${MANIFEST}" 0600
render_state_marker "config" "${CONFIG_DIR}" "${INSTALL_TOKEN}" | write_file "${CONFIG_STATE_MARKER}" 0600
render_state_marker "data" "${DATA_DIR}" "${INSTALL_TOKEN}" | write_file "${DATA_STATE_MARKER}" 0600
commit_install_transaction

echo "Installation complete."
echo "Run: dm version"
echo "Current shell may need: export PATH=\"${BIN_DIR}:\$PATH\""
