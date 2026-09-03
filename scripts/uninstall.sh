#!/usr/bin/env bash
set -euo pipefail

if [ "$(uname -s)" != "Linux" ]; then
  echo "uninstall.sh: the shell installer currently supports Linux only; remove Darwin release binaries manually as documented" >&2
  exit 1
fi

IS_ROOT=0
if [ "$(id -u)" -eq 0 ]; then
  IS_ROOT=1
fi
CURRENT_UID=$(id -u)

if [ "${IS_ROOT}" = "1" ]; then
  PREFIX="/usr/local"
  CONFIG_DIR="/etc/docker-manager"
  DATA_DIR="/var/lib/docker-manager"
  PROFILE_FILE="/etc/profile.d/docker-manager.sh"
else
  USER_HOME=${HOME:?HOME is required for a non-root uninstall}
  PREFIX="${USER_HOME}/.local"
  CONFIG_DIR="${XDG_CONFIG_HOME:-${USER_HOME}/.config}/docker-manager"
  DATA_DIR="${XDG_DATA_HOME:-${USER_HOME}/.local/share}/docker-manager"
  PROFILE_FILE="${USER_HOME}/.profile"
fi

BIN_DIR=""
LIBEXEC_DIR=""
COMPLETION_DIR=""
PURGE=0
DRY_RUN=0
VERIFY_INSTALL_STATE=0
PREFIX_EXPLICIT=0
BIN_DIR_EXPLICIT=0
LIBEXEC_DIR_EXPLICIT=0
DATA_DIR_EXPLICIT=0
COMPLETION_DIR_EXPLICIT=0

usage() {
  cat <<'EOF'
Usage: scripts/uninstall.sh [options]

Uninstall docker-manager installed by scripts/install.sh.

Options:
  --prefix DIR          Install prefix used during install
  --install-dir DIR     Alias of --prefix
  --bin-dir DIR         Directory containing dm wrapper
  --libexec-dir DIR     Directory containing dm-bin
  --config-dir DIR      Config directory
  --data-dir DIR        Data directory
  --completion-dir DIR  Base directory containing shell completion files
  --purge               Also remove config and data directories
  --dry-run             Print actions without changing files
  --verify-install-state
                        Validate an existing install manifest and ownership markers, then exit
  -h, --help            Show this help
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
    --completion-dir)
      COMPLETION_DIR=${2:?missing value for --completion-dir}
      COMPLETION_DIR_EXPLICIT=1
      shift 2
      ;;
    --purge)
      PURGE=1
      shift
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --verify-install-state)
      VERIFY_INSTALL_STATE=1
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

die() {
  echo "uninstall.sh: $*" >&2
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
    [ "${owner}" = "${CURRENT_UID}" ] || die "${description} is not owned by uid ${CURRENT_UID}: ${path}"
  fi
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
}

path_within() {
  local path="$1"
  local root="$2"
  if [ "${root}" = "/" ]; then
    [[ "${path}" = /* ]]
    return
  fi
  [ "${path}" = "${root}" ] || case "${path}" in
    "${root}"/*) return 0 ;;
    *) return 1 ;;
  esac
}

paths_overlap() {
  path_within "$1" "$2" || path_within "$2" "$1"
}

decode_mount_path() {
  local value="$1"
  value=${value//\\040/ }
  value=${value//\\011/$'\t'}
  value=${value//\\012/$'\n'}
  value=${value//\\134/\\}
  printf '%s\n' "${value}"
}

assert_no_mounts_in_tree() {
  local path="$1"
  local description="${2:-directory}"
  [ -r /proc/self/mountinfo ] || die "cannot inspect mount boundaries for ${description}: /proc/self/mountinfo is unavailable"
  # Linux does not expose an explicit "bind" flag in mountinfo. A bind of a
  # filesystem root therefore has root=/, just like an ordinary independent
  # mount. Such a bind still leaves a duplicate mount identity. Mark every
  # member of a duplicate group (rather than only later entries), because
  # mountinfo order is not a source/target ordering contract. The canonical
  # mount point / is excluded from the duplicate list: otherwise an unrelated
  # propagated view of the root filesystem would make every path below / look
  # like a bind ancestor. This keeps normal one-off /home, /var, and tmpfs
  # mounts usable while covering non-root root binds whose source is visible.
  local duplicate_root_mount_ids
  duplicate_root_mount_ids=$(awk '
    {
      for (i=6; i<=NF; i++) {
        if ($i == "-") {
          if ($4 == "/" && $5 != "/") {
            key=$3 SUBSEP $4 SUBSEP $(i+1) SUBSEP $(i+2)
            if (!(key in key_order)) {
              key_order[key]=++key_count
              ordered_keys[key_count]=key
            }
            key_seen[key]++
            key_mount_ids[key]=key_mount_ids[key] " " $1
          }
          break
        }
      }
    }
    END {
      for (i=1; i<=key_count; i++) {
        key=ordered_keys[i]
        if (key_seen[key] > 1) {
          print key_mount_ids[key]
        }
      }
    }
  ' /proc/self/mountinfo) || die "cannot inspect duplicate mount identities for ${description}"

  local mount_id mount_root mount_point
  while IFS=' ' read -r mount_id _ _ mount_root mount_point _; do
    [[ "${mount_id}" =~ ^[0-9]+$ ]] || die "cannot parse mount boundary id for ${description}"
    [ -n "${mount_root}" ] || die "cannot parse mount boundary root for ${description}"
    [ -n "${mount_point}" ] || die "cannot parse mount boundary point for ${description}"
    mount_root=$(decode_mount_path "${mount_root}")
    mount_point=$(decode_mount_path "${mount_point}")
    if path_within "${mount_point}" "${path}"; then
      die "refusing mount point in ${description}: ${mount_point}"
    fi
    # A mount whose filesystem root is below / is a bind/subtree view (or
    # another non-root filesystem view). If it is an ancestor of the purge
    # target, removing the lexical target can delete data from an unrelated
    # source tree. Ordinary independent mounts (for example /home or /var)
    # report root=/ and remain compatible.
    if path_within "${path}" "${mount_point}"; then
      if [ "${mount_root}" != "/" ]; then
        die "refusing bind mount ancestor of ${description}: ${mount_point} (root ${mount_root})"
      fi
      case " ${duplicate_root_mount_ids} " in
        *" ${mount_id} "*)
          die "refusing duplicate-root mount ancestor of ${description}: ${mount_point}"
          ;;
      esac
    fi
  done </proc/self/mountinfo
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
  assert_no_mounts_in_tree "${path}" "${description}"
  local link foreign special
  link=$(find -P "${path}" -type l -print -quit 2>/dev/null) || die "cannot inspect ${description} for symlinks: ${path}"
  [ -z "${link}" ] || die "refusing symlink in ${description}: ${link}"
  foreign=$(find -P "${path}" ! -uid "${CURRENT_UID}" -print -quit 2>/dev/null) || die "cannot inspect ownership in ${description}: ${path}"
  [ -z "${foreign}" ] || die "${description} contains a path not owned by uid ${CURRENT_UID}: ${foreign}"
  special=$(find -P "${path}" \( ! -type f ! -type d \) -print -quit 2>/dev/null) || die "cannot inspect file types in ${description}: ${path}"
  [ -z "${special}" ] || die "refusing special file in ${description}: ${special}"
}

assert_paths_equal() {
  local actual="$1"
  local expected="$2"
  local description="${3:-path}"
  [ "${actual}" = "${expected}" ] || die "manifest ${description} does not match requested path: ${actual} (expected ${expected})"
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

M_VERSION=""
M_INSTALL_UID=""
M_INSTALL_GID=""
M_INSTALL_TOKEN=""
M_PREFIX=""
M_BIN_DIR=""
M_LIBEXEC_DIR=""
M_CONFIG_DIR=""
M_CONFIG_FILE=""
M_DATA_DIR=""
M_OUTPUT_DIR=""
M_PROFILE_FILE=""
  M_COMPLETION_BASE_DIR=""
  M_COMPLETION_FILES_LEGACY=""
  M_COMPLETION_FILES_LEGACY_PRESENT=0
  M_COMPLETION_COUNT=""
M_COMPLETION_FILE=()
COMPLETION_FILES=()
PROFILE_REMOVE_EXPECTED=0
PROFILE_PRECHECK_IDENTITY=""
PROFILE_SNAPSHOT_TMP=""
PROFILE_CONTENT_TMP=""
PROFILE_REPLACEMENT_TMP=""

cleanup_profile_temps() {
  if [ -n "${PROFILE_SNAPSHOT_TMP}" ]; then
    rm -f -- "${PROFILE_SNAPSHOT_TMP}" 2>/dev/null || true
  fi
  if [ -n "${PROFILE_CONTENT_TMP}" ]; then
    rm -f -- "${PROFILE_CONTENT_TMP}" 2>/dev/null || true
  fi
  if [ -n "${PROFILE_REPLACEMENT_TMP}" ]; then
    rm -f -- "${PROFILE_REPLACEMENT_TMP}" 2>/dev/null || true
  fi
  PROFILE_SNAPSHOT_TMP=""
  PROFILE_CONTENT_TMP=""
  PROFILE_REPLACEMENT_TMP=""
}

trap cleanup_profile_temps EXIT

read_manifest() {
  local path="$1"
  local line key raw value completion_index_text
  local seen="|"
  manifest_uid=$(stat_uid "${path}") || die "cannot inspect install manifest: ${path}"
  [ "${manifest_uid}" = "${CURRENT_UID}" ] || die "install manifest is not owned by uid ${CURRENT_UID}: ${path}"
  manifest_mode=$(stat_mode "${path}") || die "cannot inspect install manifest mode: ${path}"
  case "${manifest_mode}" in
    ""|*[!0-7]*) die "install manifest has an invalid mode: ${path}" ;;
  esac
  manifest_size=$(wc -c <"${path}")
  [ "${manifest_size}" -le 65536 ] || die "install manifest exceeds the 64 KiB safety limit: ${path}"

  while IFS= read -r line || [ -n "${line}" ]; do
    case "${line}" in
      ""|\#*) continue ;;
    esac
    [[ "${line}" == *=* ]] || die "malformed install manifest line"
    key=${line%%=*}
    raw=${line#*=}
    case "${key}" in
    DM_MANIFEST_VERSION|DM_INSTALL_UID|DM_INSTALL_GID|DM_INSTALL_TOKEN|DM_INSTALL_PREFIX|DM_BIN_DIR|DM_LIBEXEC_DIR|\
      DM_CONFIG_DIR|DM_CONFIG_FILE|DM_DATA_DIR|DM_OUTPUT_DIR|DM_PROFILE_FILE|DM_COMPLETION_BASE_DIR|\
      DM_COMPLETION_FILES|DM_COMPLETION_COUNT)
        ;;
      DM_COMPLETION_FILE_*)
        [[ "${key}" =~ ^DM_COMPLETION_FILE_([0-9]+)$ ]] || die "unknown install manifest key: ${key}"
        completion_index_text=${BASH_REMATCH[1]}
        ;;
      *) die "unknown install manifest key: ${key}" ;;
    esac
    [[ "${key}" =~ ^[A-Z0-9_]+$ ]] || die "invalid install manifest key: ${key}"
    case "${seen}" in
      *"|${key}|"*) die "duplicate install manifest key: ${key}" ;;
    esac
    value=$(decode_manifest_value "${raw}") || die "invalid quoted value for install manifest key: ${key}"
    reject_unsafe_text "${value}" "manifest ${key}"
    seen="${seen}${key}|"
    case "${key}" in
      DM_MANIFEST_VERSION) M_VERSION=${value} ;;
      DM_INSTALL_UID) M_INSTALL_UID=${value} ;;
      DM_INSTALL_GID) M_INSTALL_GID=${value} ;;
      DM_INSTALL_TOKEN) M_INSTALL_TOKEN=${value} ;;
      DM_INSTALL_PREFIX) M_PREFIX=${value} ;;
      DM_BIN_DIR) M_BIN_DIR=${value} ;;
      DM_LIBEXEC_DIR) M_LIBEXEC_DIR=${value} ;;
      DM_CONFIG_DIR) M_CONFIG_DIR=${value} ;;
      DM_CONFIG_FILE) M_CONFIG_FILE=${value} ;;
      DM_DATA_DIR) M_DATA_DIR=${value} ;;
      DM_OUTPUT_DIR) M_OUTPUT_DIR=${value} ;;
      DM_PROFILE_FILE) M_PROFILE_FILE=${value} ;;
      DM_COMPLETION_BASE_DIR) M_COMPLETION_BASE_DIR=${value} ;;
      DM_COMPLETION_FILES)
        M_COMPLETION_FILES_LEGACY=${value}
        M_COMPLETION_FILES_LEGACY_PRESENT=1
        ;;
      DM_COMPLETION_COUNT) M_COMPLETION_COUNT=${value} ;;
      DM_COMPLETION_FILE_*)
        local index_text=${completion_index_text}
        [[ "${index_text}" =~ ^(0|[1-9][0-9]*)$ ]] || die "invalid completion manifest index: ${key}"
        local index=$((10#${index_text}))
        [ "${index}" -lt 64 ] || die "too many completion manifest entries"
        M_COMPLETION_FILE[${index}]=${value}
        ;;
    esac
  done <"${path}"
}

normalize_manifest_path() {
  local value="$1"
  local description="$2"
  case "${value}" in
    /*) ;;
    *) die "install manifest contains a non-absolute ${description}: ${value}" ;;
  esac
  normalize_path "${value}" "manifest ${description}"
}

safe_remove_file() {
  local path="$1"
  local description="${2:-file}"
  assert_safe_file_target "${path}" "${description}"
  if ! path_exists "${path}"; then
    return 0
  fi
  if [ "${DRY_RUN}" = "1" ]; then
    printf 'DRY-RUN: rm -f %s\n' "${path}"
    return 0
  fi
  rm -f -- "${path}"
  if path_exists "${path}"; then
    die "${description} remained after deletion: ${path}"
  fi
}

safe_remove_tree() {
  local path="$1"
  local description="${2:-directory}"
  assert_safe_tree "${path}" "${description}"
  if ! path_exists "${path}"; then
    return 0
  fi
  if [ "${DRY_RUN}" = "1" ]; then
    printf 'DRY-RUN: rm -rf %s\n' "${path}"
    return 0
  fi
  rm -rf -- "${path}"
  if path_exists "${path}"; then
    die "${description} remained after deletion: ${path}"
  fi
}

shell_quote() {
  local value="${1-}"
  reject_unsafe_text "${value}" "shell value"
  value=${value//\'/\'\\\'\'}
  printf "'%s'" "${value}"
}

render_root_profile() {
  local data_q config_q output_q bin_q
  data_q=$(shell_quote "${DATA_DIR}")
  config_q=$(shell_quote "${CONFIG_FILE}")
  output_q=$(shell_quote "${DATA_DIR}/images")
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

render_state_marker() {
  local role="$1"
  local path="$2"
  local token="$3"
  printf '%s\n' \
    '# Managed by docker-manager install.sh' \
    'version=1' \
    "role=${role}" \
    "uid=${CURRENT_UID}" \
    "path=${path}" \
    "token=${token}"
}

assert_managed_state_marker() {
  local marker="$1"
  local role="$2"
  local path="$3"
  local token="$4"
  path_exists "${marker}" || die "${role} purge directory is missing its ownership marker: ${marker}"
  assert_safe_file_target "${marker}" "${role} state marker"
  local mode mode_value size
  mode=$(stat_mode "${marker}") || die "cannot inspect ${role} state marker mode: ${marker}"
  case "${mode}" in
    ""|*[!0-7]*) die "${role} state marker has an invalid mode: ${marker}" ;;
  esac
  mode_value=$((8#${mode}))
  (( (mode_value & 077) == 0 )) || die "${role} state marker must be private: ${marker}"
  size=$(wc -c <"${marker}")
  [ "${size}" -le 4096 ] || die "${role} state marker is too large: ${marker}"
  render_state_marker "${role}" "${path}" "${token}" | cmp -s "${marker}" - || die "${role} purge directory has a modified ownership marker: ${marker}"
}

render_legacy_root_profile() {
  printf '%s\n' \
    '# Managed by docker-manager install.sh' \
    "export DM_HOME=\"${DATA_DIR}\"" \
    "export DM_CONFIG=\"${CONFIG_FILE}\"" \
    "export DM_OUTPUT_DIR=\"${DATA_DIR}/images\"" \
    "case \":\$PATH:\" in" \
    "  *:\"${BIN_DIR}\":*) ;;" \
    "  *) export PATH=\"${BIN_DIR}:\$PATH\" ;;" \
    'esac'
}

root_profile_matches_install() {
  local path="$1"
  render_root_profile | cmp -s "${path}" - || render_legacy_root_profile | cmp -s "${path}" -
}

count_exact_lines() {
  local needle="$1"
  local path="$2"
  local count status
  if count=$(grep -Fxc -- "${needle}" "${path}" 2>/dev/null); then
    printf '%s\n' "${count}"
    return 0
  else
    status=$?
  fi
  [ "${status}" -eq 1 ] || return "${status}"
  printf '%s\n' "${count:-0}"
}

profile_block_is_well_formed() {
  local path="$1"
  local start="$2"
  local end="$3"
  awk -v start="${start}" -v end="${end}" '
    $0 == start {
      start_count++
      if (inside) invalid = 1
      inside = 1
      next
    }
    $0 == end {
      end_count++
      if (!inside) invalid = 1
      inside = 0
      next
    }
    END {
      if (invalid || inside || start_count != 1 || end_count != 1) exit 1
    }
  ' "${path}" >/dev/null
}

remove_empty_directory() {
  local path="$1"
  local description="${2:-directory}"
  assert_safe_directory "${path}" "${description}"
  if ! path_exists "${path}"; then
    return 0
  fi
  if [ "${DRY_RUN}" = "1" ]; then
    printf 'DRY-RUN: rmdir %s\n' "${path}"
    return 0
  fi
  rmdir -- "${path}" 2>/dev/null || true
}

remove_user_profile_block() {
  local path="$1"
  local start='# >>> docker-manager >>>'
  local end='# <<< docker-manager <<<'
  if [ "${PROFILE_REMOVE_EXPECTED}" != "1" ]; then
    return 0
  fi
  path_exists "${path}" || die "profile disappeared before update: ${path}"
  assert_safe_file_target "${path}" "profile file"
  local start_count end_count
  local parent original_mode original_identity current_identity current_mode replacement_mode replacement_mode_value
  original_identity=$(stat_identity "${path}") || die "cannot inspect profile identity: ${path}"
  [ "${original_identity}" = "${PROFILE_PRECHECK_IDENTITY}" ] || die "profile changed before update: ${path}"
  start_count=$(count_exact_lines "${start}" "${path}") || die "cannot inspect profile start marker: ${path}"
  end_count=$(count_exact_lines "${end}" "${path}") || die "cannot inspect profile end marker: ${path}"
  if [ "${start_count}" != "1" ] || [ "${end_count}" != "1" ]; then
    die "profile contains a malformed docker-manager block: ${path}"
  fi
  profile_block_is_well_formed "${path}" "${start}" "${end}" || die "profile docker-manager block has invalid order: ${path}"
  parent=$(dirname -- "${path}")
  assert_no_symlink_path "${parent}" "profile parent"
  if [ "${DRY_RUN}" = "1" ]; then
    printf 'DRY-RUN: remove docker-manager block from %s\n' "${path}"
    return 0
  fi

  original_mode=$(stat_mode "${path}") || die "cannot inspect profile mode: ${path}"
  case "${original_mode}" in
    ""|*[!0-7]*) die "profile has an invalid mode: ${path}" ;;
  esac

  PROFILE_SNAPSHOT_TMP=$(mktemp "${parent}/.dm-profile-snapshot.XXXXXX")
  chmod 0600 "${PROFILE_SNAPSHOT_TMP}"
  if ! cat "${path}" >"${PROFILE_SNAPSHOT_TMP}"; then
    die "cannot snapshot profile before update: ${path}"
  fi
  current_identity=$(stat_identity "${path}") || die "cannot recheck profile identity: ${path}"
  [ "${current_identity}" = "${original_identity}" ] || die "profile changed while being read: ${path}"
  cmp -s "${path}" "${PROFILE_SNAPSHOT_TMP}" || die "profile changed while being read: ${path}"
  start_count=$(count_exact_lines "${start}" "${PROFILE_SNAPSHOT_TMP}") || die "cannot inspect profile snapshot start marker: ${path}"
  end_count=$(count_exact_lines "${end}" "${PROFILE_SNAPSHOT_TMP}") || die "cannot inspect profile snapshot end marker: ${path}"
  if [ "${start_count}" != "1" ] || [ "${end_count}" != "1" ]; then
    die "profile snapshot contains a malformed docker-manager block: ${path}"
  fi
  profile_block_is_well_formed "${PROFILE_SNAPSHOT_TMP}" "${start}" "${end}" || die "profile snapshot has an invalid docker-manager block: ${path}"

  PROFILE_CONTENT_TMP=$(mktemp "${parent}/.dm-profile-content.XXXXXX")
  if ! awk -v start="${start}" -v end="${end}" '($0 == start) { skip=1; next } ($0 == end) { skip=0; next } !skip { print }' "${PROFILE_SNAPSHOT_TMP}" >"${PROFILE_CONTENT_TMP}"; then
    die "cannot prepare profile update: ${path}"
  fi
  chmod 0600 "${PROFILE_CONTENT_TMP}"

  PROFILE_REPLACEMENT_TMP=$(mktemp "${parent}/.dm-profile-replacement.XXXXXX")
  if ! cp --preserve=all "${path}" "${PROFILE_REPLACEMENT_TMP}" 2>/dev/null && ! cp -p "${path}" "${PROFILE_REPLACEMENT_TMP}"; then
    die "cannot preserve profile metadata: ${path}"
  fi
  replacement_mode=$(stat_mode "${PROFILE_REPLACEMENT_TMP}") || die "cannot inspect replacement profile mode: ${path}"
  case "${replacement_mode}" in
    ""|*[!0-7]*) die "replacement profile has an invalid mode: ${path}" ;;
  esac
  replacement_mode_value=$((8#${replacement_mode}))
  if (( (replacement_mode_value & 0200) == 0 )); then
    chmod u+w "${PROFILE_REPLACEMENT_TMP}" || die "cannot make replacement profile writable: ${path}"
  fi
  if ! cat "${PROFILE_CONTENT_TMP}" >"${PROFILE_REPLACEMENT_TMP}"; then
    die "cannot write profile replacement: ${path}"
  fi
  chmod "${original_mode}" "${PROFILE_REPLACEMENT_TMP}" || die "cannot restore replacement profile mode: ${path}"

  assert_no_symlink_path "${path}" "profile file"
  current_identity=$(stat_identity "${path}") || die "cannot recheck profile identity: ${path}"
  [ "${current_identity}" = "${original_identity}" ] || die "profile changed during update: ${path}"
  current_mode=$(stat_mode "${path}") || die "cannot recheck profile mode: ${path}"
  [ "${current_mode}" = "${original_mode}" ] || die "profile mode changed during update: ${path}"
  cmp -s "${path}" "${PROFILE_SNAPSHOT_TMP}" || die "profile content changed during update: ${path}"
  if ! mv -f -- "${PROFILE_REPLACEMENT_TMP}" "${path}"; then
    die "cannot replace profile file: ${path}"
  fi
  PROFILE_REPLACEMENT_TMP=""
  cleanup_profile_temps
}

remove_root_profile_file() {
  local path="$1"
  if [ "${PROFILE_REMOVE_EXPECTED}" != "1" ]; then
    return 0
  fi
  path_exists "${path}" || die "profile disappeared before removal: ${path}"
  assert_safe_file_target "${path}" "profile file"
  local marker_count original_identity current_identity
  marker_count=$(count_exact_lines '# Managed by docker-manager install.sh' "${path}") || die "cannot inspect profile marker: ${path}"
  [ "${marker_count}" = "1" ] || die "profile contains duplicate docker-manager markers: ${path}"
  original_identity=$(stat_identity "${path}") || die "cannot inspect profile identity: ${path}"
  [ "${original_identity}" = "${PROFILE_PRECHECK_IDENTITY}" ] || die "profile changed before removal: ${path}"
  root_profile_matches_install "${path}" || die "refusing to remove a modified profile file: ${path}"
  if [ "${DRY_RUN}" = "1" ]; then
    printf 'DRY-RUN: rm -f %s\n' "${path}"
    return 0
  fi
  assert_no_symlink_path "${path}" "profile file"
  root_profile_matches_install "${path}" || die "profile changed before removal: ${path}"
  current_identity=$(stat_identity "${path}") || die "cannot recheck profile identity: ${path}"
  [ "${current_identity}" = "${original_identity}" ] || die "profile changed before removal: ${path}"
  rm -f -- "${path}"
  if path_exists "${path}"; then
    die "profile file remained after deletion: ${path}"
  fi
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

append_completion_file() {
  local path="$1"
  local base="$2"
  path=$(normalize_manifest_path "${path}" "completion file")
  [ -n "${base}" ] || die "completion base directory is missing"
  completion_path_matches_base "${path}" "${base}" || die "completion file is outside its declared base: ${path}"
  local existing
  for existing in "${COMPLETION_FILES[@]+"${COMPLETION_FILES[@]}"}"; do
    [ "${existing}" != "${path}" ] || die "duplicate completion file: ${path}"
  done
  COMPLETION_FILES+=("${path}")
}

validate_completion_manifest() {
  local index path expected_base legacy_path inferred_base
  COMPLETION_FILES=()
  if [ "${M_VERSION}" = "3" ]; then
    [ -n "${M_COMPLETION_BASE_DIR}" ] || die "versioned install manifest is missing completion base directory"
    validate_decimal "${M_COMPLETION_COUNT}" "completion count"
    local completion_count=$((10#${M_COMPLETION_COUNT}))
    [ "${completion_count}" -le 4 ] || die "too many completion manifest entries"
    expected_base=$(normalize_manifest_path "${M_COMPLETION_BASE_DIR}" "completion base directory")
    assert_not_root_path "${expected_base}" "completion base directory"
    for ((index = 0; index < 64; index++)); do
      if [ "${M_COMPLETION_FILE[${index}]+set}" = "set" ]; then
        [ "${index}" -lt "${completion_count}" ] || die "completion manifest index is outside declared count"
      fi
    done
    for ((index = 0; index < completion_count; index++)); do
      [ "${M_COMPLETION_FILE[${index}]+set}" = "set" ] || die "completion manifest index ${index} is missing"
      append_completion_file "${M_COMPLETION_FILE[${index}]}" "${expected_base}"
    done
    if [ "${M_COMPLETION_FILES_LEGACY_PRESENT}" = "1" ]; then
      local joined
      joined=$(IFS=:; printf '%s' "${COMPLETION_FILES[*]}")
      [ "${joined}" = "${M_COMPLETION_FILES_LEGACY}" ] || die "legacy completion list disagrees with indexed manifest entries"
    fi
    COMPLETION_DIR=${expected_base}
    return 0
  fi

  if [ "${M_VERSION}" = "2" ]; then
    local completion_count="" legacy_files=() old_ifs joined
    expected_base=""
    if [ -n "${M_COMPLETION_BASE_DIR}" ]; then
      expected_base=$(normalize_manifest_path "${M_COMPLETION_BASE_DIR}" "completion base directory")
      assert_not_root_path "${expected_base}" "completion base directory"
    fi
    if [ -n "${M_COMPLETION_COUNT}" ]; then
      validate_decimal "${M_COMPLETION_COUNT}" "completion count"
      completion_count=$((10#${M_COMPLETION_COUNT}))
      [ "${completion_count}" -le 4 ] || die "too many completion manifest entries"
      for ((index = 0; index < 64; index++)); do
        if [ "${M_COMPLETION_FILE[${index}]+set}" = "set" ]; then
          [ "${index}" -lt "${completion_count}" ] || die "completion manifest index is outside declared count"
        fi
      done
      for ((index = 0; index < completion_count; index++)); do
        [ "${M_COMPLETION_FILE[${index}]+set}" = "set" ] || die "completion manifest index ${index} is missing"
        path=$(normalize_manifest_path "${M_COMPLETION_FILE[${index}]}" "completion file")
        if [ -z "${expected_base}" ]; then
          expected_base=$(completion_base_from_path "${path}" || true)
          [ -n "${expected_base}" ] || die "legacy completion file has an unsupported path: ${path}"
          expected_base=$(normalize_manifest_path "${expected_base}" "completion base directory")
          assert_not_root_path "${expected_base}" "completion base directory"
        fi
        append_completion_file "${path}" "${expected_base}"
      done
      if [ "${M_COMPLETION_FILES_LEGACY_PRESENT}" = "1" ]; then
        joined=$(IFS=:; printf '%s' "${COMPLETION_FILES[*]}")
        [ "${joined}" = "${M_COMPLETION_FILES_LEGACY}" ] || die "legacy completion list disagrees with indexed manifest entries"
      fi
    elif [ "${M_COMPLETION_FILES_LEGACY_PRESENT}" = "1" ] && [ -n "${M_COMPLETION_FILES_LEGACY}" ]; then
      case "${M_COMPLETION_FILES_LEGACY}" in
        :*|*:|*::*) die "legacy completion list contains an empty path" ;;
      esac
      old_ifs="${IFS}"
      IFS=: read -r -a legacy_files <<<"${M_COMPLETION_FILES_LEGACY}"
      IFS="${old_ifs}"
      for legacy_path in "${legacy_files[@]+${legacy_files[@]}}"; do
        [ -n "${legacy_path}" ] || die "legacy completion list contains an empty path"
        path=$(normalize_manifest_path "${legacy_path}" "completion file")
        inferred_base=$(completion_base_from_path "${path}" || true)
        [ -n "${inferred_base}" ] || die "legacy completion file has an unsupported path: ${path}"
        if [ -z "${expected_base}" ]; then
          expected_base=$(normalize_manifest_path "${inferred_base}" "completion base directory")
          assert_not_root_path "${expected_base}" "completion base directory"
        else
          [ "${inferred_base}" = "${expected_base}" ] || die "legacy completion files use different base directories"
        fi
        append_completion_file "${path}" "${expected_base}"
      done
    else
      for ((index = 0; index < 64; index++)); do
        [ "${M_COMPLETION_FILE[${index}]+set}" != "set" ] || die "version 2 manifest has indexed completion entries but no completion count"
      done
    fi
    if [ -n "${expected_base}" ]; then
      if [ "${COMPLETION_DIR_EXPLICIT}" = "1" ]; then
        assert_paths_equal "${expected_base}" "${COMPLETION_DIR}" "completion base directory"
      else
        COMPLETION_DIR=${expected_base}
      fi
    fi
    return 0
  fi

  if [ "${M_COMPLETION_FILES_LEGACY_PRESENT}" = "1" ] && [ -n "${M_COMPLETION_FILES_LEGACY}" ]; then
    case "${M_COMPLETION_FILES_LEGACY}" in
      :*|*:|*::*) die "legacy completion list contains an empty path" ;;
    esac
    local legacy_files=()
    local old_ifs="${IFS}"
    IFS=: read -r -a legacy_files <<<"${M_COMPLETION_FILES_LEGACY}"
    IFS="${old_ifs}"
    expected_base=""
    for legacy_path in "${legacy_files[@]+"${legacy_files[@]}"}"; do
      [ -n "${legacy_path}" ] || die "legacy completion list contains an empty path"
      legacy_path=$(normalize_manifest_path "${legacy_path}" "completion file")
      inferred_base=$(completion_base_from_path "${legacy_path}") || die "legacy completion file has an unsupported path: ${legacy_path}"
      assert_not_root_path "${inferred_base}" "completion base directory"
      if [ -z "${expected_base}" ]; then
        expected_base=${inferred_base}
      else
        [ "${expected_base}" = "${inferred_base}" ] || die "legacy completion files use different base directories"
      fi
      append_completion_file "${legacy_path}" "${expected_base}"
    done
    if [ -n "${expected_base}" ]; then
      if [ "${COMPLETION_DIR_EXPLICIT}" = "1" ]; then
        assert_paths_equal "${expected_base}" "${COMPLETION_DIR}" "completion base directory"
      else
        COMPLETION_DIR=${expected_base}
      fi
    fi
  fi
}

validate_decimal() {
  local value="$1"
  local description="$2"
  [[ "${value}" =~ ^[0-9]+$ ]] || die "invalid ${description}: ${value}"
}

inspect_profile_for_removal() {
  local path="$1"
  if ! path_exists "${path}"; then
    return 0
  fi
  assert_safe_file_target "${path}" "profile file"
  if [ "${IS_ROOT}" = "1" ]; then
    local marker_count
    marker_count=$(count_exact_lines '# Managed by docker-manager install.sh' "${path}") || die "cannot inspect profile marker: ${path}"
    if [ "${marker_count}" = "0" ]; then
      return 0
    fi
    [ "${marker_count}" = "1" ] || die "profile contains duplicate docker-manager markers: ${path}"
    root_profile_matches_install "${path}" || die "refusing to remove a modified profile file: ${path}"
    PROFILE_PRECHECK_IDENTITY=$(stat_identity "${path}") || die "cannot inspect profile identity: ${path}"
    PROFILE_REMOVE_EXPECTED=1
    return 0
  fi
  local start='# >>> docker-manager >>>'
  local end='# <<< docker-manager <<<'
  local start_count end_count
  start_count=$(count_exact_lines "${start}" "${path}") || die "cannot inspect profile start marker: ${path}"
  end_count=$(count_exact_lines "${end}" "${path}") || die "cannot inspect profile end marker: ${path}"
  if [ "${start_count}" = "0" ] && [ "${end_count}" = "0" ]; then
    return 0
  fi
  if [ "${start_count}" != "1" ] || [ "${end_count}" != "1" ]; then
    die "profile contains a malformed docker-manager block: ${path}"
  fi
  profile_block_is_well_formed "${path}" "${start}" "${end}" || die "profile docker-manager block has invalid order: ${path}"
  PROFILE_PRECHECK_IDENTITY=$(stat_identity "${path}") || die "cannot inspect profile identity: ${path}"
  PROFILE_REMOVE_EXPECTED=1
}

PREFIX=$(normalize_path "${PREFIX}" "prefix")
CONFIG_DIR=$(normalize_path "${CONFIG_DIR}" "config directory")
DATA_DIR=$(normalize_path "${DATA_DIR}" "data directory")
if [ -n "${BIN_DIR}" ]; then
  BIN_DIR=$(normalize_path "${BIN_DIR}" "binary directory")
fi
if [ -n "${LIBEXEC_DIR}" ]; then
  LIBEXEC_DIR=$(normalize_path "${LIBEXEC_DIR}" "libexec directory")
fi
if [ -n "${COMPLETION_DIR}" ]; then
  COMPLETION_DIR=$(normalize_path "${COMPLETION_DIR}" "completion directory")
fi
DEFAULT_PROFILE_FILE=$(normalize_path "${PROFILE_FILE}" "profile file")

PREFIX_REQUESTED=${PREFIX}
CONFIG_DIR_REQUESTED=${CONFIG_DIR}
DATA_DIR_REQUESTED=${DATA_DIR}
BIN_DIR_REQUESTED=${BIN_DIR}
LIBEXEC_DIR_REQUESTED=${LIBEXEC_DIR}
COMPLETION_DIR_REQUESTED=${COMPLETION_DIR}
MANIFEST="${CONFIG_DIR}/install.env"
assert_no_symlink_path "${CONFIG_DIR}" "config directory"
assert_no_symlink_path "${MANIFEST}" "install manifest"

manifest_uid=""
manifest_mode=""
manifest_size=0
HAS_MANIFEST=0
if path_exists "${MANIFEST}"; then
  HAS_MANIFEST=1
  [ -f "${MANIFEST}" ] || die "install manifest is not a regular file: ${MANIFEST}"
  read_manifest "${MANIFEST}"
  if [ -n "${M_VERSION}" ] && [ "${M_VERSION}" != "2" ] && [ "${M_VERSION}" != "3" ]; then
    die "unsupported install manifest version: ${M_VERSION}"
  fi
  if [ -n "${M_INSTALL_UID}" ]; then
    validate_decimal "${M_INSTALL_UID}" "install manifest uid"
    [ "${M_INSTALL_UID}" = "${manifest_uid}" ] || die "install manifest uid does not match file owner"
  fi
  if [ -n "${M_INSTALL_GID}" ]; then
    validate_decimal "${M_INSTALL_GID}" "install manifest gid"
  fi
  manifest_mode_value=$((8#${manifest_mode}))
  if (( manifest_mode_value & 022 )); then
    die "install manifest must not be group/world writable: ${MANIFEST}"
  fi
  if [ "${M_VERSION}" = "2" ] || [ "${M_VERSION}" = "3" ]; then
    if [ -z "${M_INSTALL_UID}" ] || [ -z "${M_INSTALL_GID}" ]; then
      die "versioned install manifest is missing owner metadata"
    fi
    [ "${M_INSTALL_UID}" = "${CURRENT_UID}" ] || die "install manifest owner metadata does not match current uid"
    (( (manifest_mode_value & 077) == 0 )) || die "versioned install manifest must be private: ${MANIFEST}"
  fi
  if [ "${M_VERSION}" = "3" ]; then
    [[ "${M_INSTALL_TOKEN}" =~ ^[0-9a-f]{32}$ ]] || die "versioned install manifest has an invalid ownership token"
  fi
  for required_value in "${M_PREFIX}" "${M_BIN_DIR}" "${M_LIBEXEC_DIR}" "${M_CONFIG_DIR}" "${M_CONFIG_FILE}" "${M_DATA_DIR}" "${M_OUTPUT_DIR}" "${M_PROFILE_FILE}"; do
    [ -n "${required_value}" ] || die "install manifest is missing a required path"
  done
  M_PREFIX=$(normalize_manifest_path "${M_PREFIX}" "install_prefix")
  M_BIN_DIR=$(normalize_manifest_path "${M_BIN_DIR}" "bin_dir")
  M_LIBEXEC_DIR=$(normalize_manifest_path "${M_LIBEXEC_DIR}" "libexec_dir")
  M_CONFIG_DIR=$(normalize_manifest_path "${M_CONFIG_DIR}" "config_dir")
  M_CONFIG_FILE=$(normalize_manifest_path "${M_CONFIG_FILE}" "config_file")
  M_DATA_DIR=$(normalize_manifest_path "${M_DATA_DIR}" "data_dir")
  M_OUTPUT_DIR=$(normalize_manifest_path "${M_OUTPUT_DIR}" "output_dir")
  M_PROFILE_FILE=$(normalize_manifest_path "${M_PROFILE_FILE}" "profile_file")
  if [ -n "${M_COMPLETION_BASE_DIR}" ]; then
    M_COMPLETION_BASE_DIR=$(normalize_manifest_path "${M_COMPLETION_BASE_DIR}" "completion_base_dir")
  fi
  assert_paths_equal "${M_CONFIG_DIR}" "${CONFIG_DIR_REQUESTED}" "config_dir"
  if [ "${PREFIX_EXPLICIT}" = "1" ]; then
    assert_paths_equal "${M_PREFIX}" "${PREFIX_REQUESTED}" "install_prefix"
  else
    PREFIX=${M_PREFIX}
  fi
  if [ "${BIN_DIR_EXPLICIT}" = "1" ]; then
    assert_paths_equal "${M_BIN_DIR}" "${BIN_DIR_REQUESTED}" "bin_dir"
  else
    BIN_DIR=${M_BIN_DIR}
  fi
  if [ "${LIBEXEC_DIR_EXPLICIT}" = "1" ]; then
    assert_paths_equal "${M_LIBEXEC_DIR}" "${LIBEXEC_DIR_REQUESTED}" "libexec_dir"
  else
    LIBEXEC_DIR=${M_LIBEXEC_DIR}
  fi
  if [ "${DATA_DIR_EXPLICIT}" = "1" ]; then
    assert_paths_equal "${M_DATA_DIR}" "${DATA_DIR_REQUESTED}" "data_dir"
  else
    DATA_DIR=${M_DATA_DIR}
  fi
  assert_paths_equal "${M_CONFIG_FILE}" "${CONFIG_DIR}/dm.yaml" "config_file"
  assert_paths_equal "${M_OUTPUT_DIR}" "${DATA_DIR}/images" "output_dir"
  assert_paths_equal "${M_PROFILE_FILE}" "${DEFAULT_PROFILE_FILE}" "profile_file"
  if [ -n "${M_COMPLETION_BASE_DIR}" ]; then
    if [ "${COMPLETION_DIR_EXPLICIT}" = "1" ]; then
      assert_paths_equal "${M_COMPLETION_BASE_DIR}" "${COMPLETION_DIR_REQUESTED}" "completion_base_dir"
    else
      COMPLETION_DIR=${M_COMPLETION_BASE_DIR}
    fi
  fi
  validate_completion_manifest
fi

BIN_DIR=${BIN_DIR:-"${PREFIX}/bin"}
LIBEXEC_DIR=${LIBEXEC_DIR:-"${PREFIX}/lib/docker-manager"}
COMPLETION_DIR=${COMPLETION_DIR:-"${PREFIX}/share"}
PREFIX=$(normalize_path "${PREFIX}" "prefix")
BIN_DIR=$(normalize_path "${BIN_DIR}" "binary directory")
LIBEXEC_DIR=$(normalize_path "${LIBEXEC_DIR}" "libexec directory")
CONFIG_DIR=$(normalize_path "${CONFIG_DIR}" "config directory")
DATA_DIR=$(normalize_path "${DATA_DIR}" "data directory")
COMPLETION_DIR=$(normalize_path "${COMPLETION_DIR}" "completion directory")
PROFILE_FILE=${DEFAULT_PROFILE_FILE}
assert_not_root_path "${PREFIX}" "prefix"
assert_not_root_path "${BIN_DIR}" "binary directory"
assert_not_root_path "${LIBEXEC_DIR}" "libexec directory"
assert_not_root_path "${CONFIG_DIR}" "config directory"
assert_not_root_path "${DATA_DIR}" "data directory"
assert_not_root_path "${COMPLETION_DIR}" "completion directory"
assert_not_root_path "${PROFILE_FILE}" "profile file"
assert_purge_boundaries "${CONFIG_DIR}" "${DATA_DIR}" "${PREFIX}" "${BIN_DIR}" "${LIBEXEC_DIR}" "${COMPLETION_DIR}" "${PROFILE_FILE}"

CONFIG_FILE="${CONFIG_DIR}/dm.yaml"
WRAPPER="${BIN_DIR}/dm"
INSTALLED_BIN="${LIBEXEC_DIR}/dm-bin"
CONFIG_STATE_MARKER="${CONFIG_DIR}/.docker-manager-managed"
DATA_STATE_MARKER="${DATA_DIR}/.docker-manager-managed"
assert_safe_directory "${PREFIX}" "prefix"
assert_safe_directory "${BIN_DIR}" "binary directory"
assert_safe_directory "${LIBEXEC_DIR}" "libexec directory"
assert_safe_directory "${CONFIG_DIR}" "config directory"
assert_safe_directory "${DATA_DIR}" "data directory"
assert_safe_directory "${COMPLETION_DIR}" "completion directory"

if [ "${VERIFY_INSTALL_STATE}" = "1" ]; then
  [ "${PURGE}" = "0" ] || die "--verify-install-state cannot be combined with --purge"
  [ "${HAS_MANIFEST}" = "1" ] || die "install state verification requires an install manifest"
  case "${M_VERSION}" in
    2)
      ;;
    3)
      assert_managed_state_marker "${CONFIG_STATE_MARKER}" "config" "${CONFIG_DIR}" "${M_INSTALL_TOKEN}"
      assert_managed_state_marker "${DATA_STATE_MARKER}" "data" "${DATA_DIR}" "${M_INSTALL_TOKEN}"
      ;;
    *)
      die "install state verification requires manifest version 2 or 3"
      ;;
  esac
  echo "Install state verified."
  exit 0
fi

assert_safe_file_target "${WRAPPER}" "wrapper"
assert_safe_file_target "${INSTALLED_BIN}" "installed binary"
assert_safe_file_target "${CONFIG_FILE}" "config file"
if [ "${HAS_MANIFEST}" = "1" ]; then
  assert_safe_file_target "${MANIFEST}" "install manifest"
fi
for completion_path in "${COMPLETION_FILES[@]+"${COMPLETION_FILES[@]}"}"; do
  assert_safe_file_target "${completion_path}" "completion file"
done
inspect_profile_for_removal "${PROFILE_FILE}"

# A purge must inspect both trees before removing any executable or profile.
# This prevents a foreign file or symlink discovered late from leaving a
# partially destructive uninstall.
if [ "${PURGE}" = "1" ]; then
  if [ "${HAS_MANIFEST}" != "1" ] || [ "${M_VERSION}" != "3" ]; then
    die "--purge requires a version 3 install manifest; re-run install.sh to migrate this installation"
  fi
  assert_managed_state_marker "${CONFIG_STATE_MARKER}" "config" "${CONFIG_DIR}" "${M_INSTALL_TOKEN}"
  assert_managed_state_marker "${DATA_STATE_MARKER}" "data" "${DATA_DIR}" "${M_INSTALL_TOKEN}"
  assert_dedicated_state_path "${CONFIG_DIR}" "config purge directory"
  assert_dedicated_state_path "${DATA_DIR}" "data purge directory"
  assert_safe_tree "${CONFIG_DIR}" "config purge tree"
  assert_safe_tree "${DATA_DIR}" "data purge tree"
fi

echo "Uninstalling docker-manager"
safe_remove_file "${WRAPPER}" "wrapper"
safe_remove_file "${INSTALLED_BIN}" "installed binary"
for completion_path in "${COMPLETION_FILES[@]+"${COMPLETION_FILES[@]}"}"; do
  safe_remove_file "${completion_path}" "completion file"
done
remove_empty_directory "${LIBEXEC_DIR}" "libexec directory"
if [ "${IS_ROOT}" = "1" ]; then
  remove_root_profile_file "${PROFILE_FILE}"
else
  remove_user_profile_block "${PROFILE_FILE}"
fi

if [ "${PURGE}" = "1" ]; then
  assert_managed_state_marker "${CONFIG_STATE_MARKER}" "config" "${CONFIG_DIR}" "${M_INSTALL_TOKEN}"
  assert_managed_state_marker "${DATA_STATE_MARKER}" "data" "${DATA_DIR}" "${M_INSTALL_TOKEN}"
  safe_remove_tree "${CONFIG_DIR}" "config directory"
  assert_managed_state_marker "${DATA_STATE_MARKER}" "data" "${DATA_DIR}" "${M_INSTALL_TOKEN}"
  safe_remove_tree "${DATA_DIR}" "data directory"
else
  echo "Keeping config and data. Use --purge to remove:"
  echo "  ${CONFIG_DIR}"
  echo "  ${DATA_DIR}"
fi

echo "Uninstall complete."
