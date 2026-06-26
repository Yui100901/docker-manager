#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
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

BIN_DIR=${BIN_DIR:-"${PREFIX}/bin"}
LIBEXEC_DIR=${LIBEXEC_DIR:-"${PREFIX}/lib/docker-manager"}
CONFIG_FILE="${CONFIG_DIR}/dm.yaml"
OUTPUT_DIR="${DATA_DIR}/images"
INSTALLED_BIN="${LIBEXEC_DIR}/dm-bin"
WRAPPER="${BIN_DIR}/dm"
MANIFEST="${CONFIG_DIR}/install.env"

run() {
  if [ "${DRY_RUN}" = "1" ]; then
    printf 'DRY-RUN:'
    printf ' %q' "$@"
    printf '\n'
  else
    "$@"
  fi
}

yaml_single_quote() {
  printf "'%s'" "$(printf '%s' "$1" | sed "s/'/''/g")"
}

write_file() {
  local path="$1"
  local mode="$2"
  local tmp
  tmp=$(mktemp)
  cat >"${tmp}"
  if [ "${DRY_RUN}" = "1" ]; then
    echo "DRY-RUN: write ${path}"
    rm -f "${tmp}"
    return 0
  fi
  install -m "${mode}" "${tmp}" "${path}"
  rm -f "${tmp}"
}

resolve_binary() {
  if [ -n "${BINARY}" ]; then
    printf '%s\n' "${BINARY}"
    return 0
  fi
  if [ -x "${ROOT_DIR}/dm" ]; then
    printf '%s\n' "${ROOT_DIR}/dm"
    return 0
  fi
  if [ -x "${ROOT_DIR}/bin/linux/dm" ]; then
    printf '%s\n' "${ROOT_DIR}/bin/linux/dm"
    return 0
  fi
  if [ "${BUILD_FROM_SOURCE}" = "1" ]; then
    if ! command -v go >/dev/null 2>&1; then
      echo "--build requires go in PATH" >&2
      exit 1
    fi
    local built="${ROOT_DIR}/bin/install/dm"
    run mkdir -p "$(dirname "${built}")"
    if [ "${DRY_RUN}" != "1" ]; then
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

SOURCE_BIN=$(resolve_binary)
if [ ! -f "${SOURCE_BIN}" ] && [ "${DRY_RUN}" != "1" ]; then
  echo "Binary not found: ${SOURCE_BIN}" >&2
  exit 1
fi

echo "Installing docker-manager"
echo "  wrapper: ${WRAPPER}"
echo "  binary:  ${INSTALLED_BIN}"
echo "  config:  ${CONFIG_FILE}"
echo "  data:    ${DATA_DIR}"

run mkdir -p "${BIN_DIR}" "${LIBEXEC_DIR}" "${CONFIG_DIR}" "${OUTPUT_DIR}"
run install -m 0755 "${SOURCE_BIN}" "${INSTALLED_BIN}"

write_file "${WRAPPER}" 0755 <<EOF
#!/usr/bin/env sh
# Managed by docker-manager install.sh
set -eu
CONFIG_ARG=""
if [ -z "\${DM_CONFIG:-}" ]; then
  DM_CONFIG="${CONFIG_FILE}"
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
  exec "${INSTALLED_BIN}" "\$@"
fi
exec "${INSTALLED_BIN}" --config "\${DM_CONFIG}" "\$@"
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

write_file "${MANIFEST}" 0644 <<EOF
DM_INSTALL_PREFIX="${PREFIX}"
DM_BIN_DIR="${BIN_DIR}"
DM_LIBEXEC_DIR="${LIBEXEC_DIR}"
DM_CONFIG_DIR="${CONFIG_DIR}"
DM_CONFIG_FILE="${CONFIG_FILE}"
DM_DATA_DIR="${DATA_DIR}"
DM_OUTPUT_DIR="${OUTPUT_DIR}"
DM_PROFILE_FILE="${PROFILE_FILE}"
EOF

if [ "${NO_PROFILE}" != "1" ]; then
  if [ "${IS_ROOT}" = "1" ]; then
    write_file "${PROFILE_FILE}" 0644 <<EOF
# Managed by docker-manager install.sh
export DM_HOME="${DATA_DIR}"
export DM_CONFIG="${CONFIG_FILE}"
export DM_OUTPUT_DIR="${OUTPUT_DIR}"
case ":\$PATH:" in
  *:"${BIN_DIR}":*) ;;
  *) export PATH="${BIN_DIR}:\$PATH" ;;
esac
EOF
  else
    if [ "${DRY_RUN}" = "1" ]; then
      echo "DRY-RUN: update ${PROFILE_FILE}"
    else
      touch "${PROFILE_FILE}"
      start="# >>> docker-manager >>>"
      end="# <<< docker-manager <<<"
      tmp=$(mktemp)
      sed "/${start}/,/${end}/d" "${PROFILE_FILE}" >"${tmp}"
      cat >>"${tmp}" <<EOF
${start}
export DM_HOME="${DATA_DIR}"
export DM_CONFIG="${CONFIG_FILE}"
export DM_OUTPUT_DIR="${OUTPUT_DIR}"
case ":\$PATH:" in
  *:"${BIN_DIR}":*) ;;
  *) export PATH="${BIN_DIR}:\$PATH" ;;
esac
${end}
EOF
      cat "${tmp}" >"${PROFILE_FILE}"
      rm -f "${tmp}"
    fi
  fi
else
  echo "Skipped profile update."
fi

echo "Installation complete."
echo "Run: dm version"
echo "Current shell may need: export PATH=\"${BIN_DIR}:\$PATH\""
