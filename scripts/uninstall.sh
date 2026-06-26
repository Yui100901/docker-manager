#!/usr/bin/env bash
set -euo pipefail

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
PURGE=0
DRY_RUN=0

usage() {
  cat <<'EOF'
Usage: scripts/uninstall.sh [options]

Uninstall docker-manager installed by scripts/install.sh.

Options:
  --prefix DIR       Install prefix used during install
  --install-dir DIR  Alias of --prefix
  --bin-dir DIR      Directory containing dm wrapper
  --libexec-dir DIR  Directory containing dm-bin
  --config-dir DIR   Config directory
  --data-dir DIR     Data directory
  --purge            Also remove config and data directories
  --dry-run          Print actions without changing files
  -h, --help         Show this help
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --prefix|--install-dir)
      PREFIX=${2:?missing value for $1}
      shift 2
      ;;
    --bin-dir)
      BIN_DIR=${2:?missing value for --bin-dir}
      shift 2
      ;;
    --libexec-dir)
      LIBEXEC_DIR=${2:?missing value for --libexec-dir}
      shift 2
      ;;
    --config-dir)
      CONFIG_DIR=${2:?missing value for --config-dir}
      shift 2
      ;;
    --data-dir)
      DATA_DIR=${2:?missing value for --data-dir}
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

MANIFEST="${CONFIG_DIR}/install.env"
if [ -f "${MANIFEST}" ]; then
  # shellcheck disable=SC1090
  . "${MANIFEST}"
  PREFIX="${DM_INSTALL_PREFIX:-${PREFIX}}"
  BIN_DIR="${BIN_DIR:-${DM_BIN_DIR:-}}"
  LIBEXEC_DIR="${LIBEXEC_DIR:-${DM_LIBEXEC_DIR:-}}"
  CONFIG_DIR="${DM_CONFIG_DIR:-${CONFIG_DIR}}"
  DATA_DIR="${DM_DATA_DIR:-${DATA_DIR}}"
  PROFILE_FILE="${DM_PROFILE_FILE:-${PROFILE_FILE}}"
  COMPLETION_FILES="${DM_COMPLETION_FILES:-}"
fi

BIN_DIR=${BIN_DIR:-"${PREFIX}/bin"}
LIBEXEC_DIR=${LIBEXEC_DIR:-"${PREFIX}/lib/docker-manager"}
WRAPPER="${BIN_DIR}/dm"
INSTALLED_BIN="${LIBEXEC_DIR}/dm-bin"

run() {
  if [ "${DRY_RUN}" = "1" ]; then
    printf 'DRY-RUN:'
    printf ' %q' "$@"
    printf '\n'
  else
    "$@"
  fi
}

echo "Uninstalling docker-manager"
run rm -f "${WRAPPER}" "${INSTALLED_BIN}"
if [ -n "${COMPLETION_FILES:-}" ]; then
  old_ifs="${IFS}"
  IFS=':'
  for file in ${COMPLETION_FILES}; do
    if [ -n "${file}" ]; then
      run rm -f "${file}"
    fi
  done
  IFS="${old_ifs}"
fi
run rmdir "${LIBEXEC_DIR}" 2>/dev/null || true

if [ "${IS_ROOT}" = "1" ]; then
  if [ -f "${PROFILE_FILE}" ]; then
    run rm -f "${PROFILE_FILE}"
  fi
else
  if [ -f "${PROFILE_FILE}" ]; then
    if [ "${DRY_RUN}" = "1" ]; then
      echo "DRY-RUN: remove docker-manager block from ${PROFILE_FILE}"
    else
      tmp=$(mktemp)
      sed "/# >>> docker-manager >>>/,/# <<< docker-manager <<</d" "${PROFILE_FILE}" >"${tmp}"
      cat "${tmp}" >"${PROFILE_FILE}"
      rm -f "${tmp}"
    fi
  fi
fi

if [ "${PURGE}" = "1" ]; then
  run rm -rf "${CONFIG_DIR}" "${DATA_DIR}"
else
  echo "Keeping config and data. Use --purge to remove:"
  echo "  ${CONFIG_DIR}"
  echo "  ${DATA_DIR}"
fi

echo "Uninstall complete."
