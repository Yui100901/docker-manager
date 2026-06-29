#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
DIST_DIR="${DIST_DIR:-${ROOT_DIR}/dist}"
VERSION="${VERSION:-dev}"
COMMIT="${COMMIT:-$(git -C "${ROOT_DIR}" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
BUILD_DATE="${BUILD_DATE:-$(date -u +"%Y-%m-%dT%H:%M:%SZ")}"
PLATFORMS=()
RUN_TESTS=1

usage() {
  cat <<'EOF'
Usage: scripts/package-release.sh [options]

Build release archives for multiple platforms.

Options:
  --dist-dir DIR      Output directory. Default: DIST_DIR or ./dist
  --version VALUE     Version injected into dm version. Default: VERSION or dev
  --commit VALUE      Commit injected into dm version. Default: git short HEAD
  --build-date VALUE  Build date injected into dm version. Default: current UTC time
  --platform OS/ARCH  Platform to build. Repeatable. Default: linux/amd64,linux/arm64,windows/amd64,darwin/amd64,darwin/arm64
  --no-test           Skip go test ./...
  -h, --help          Show this help

Environment:
  GOFLAGS             Extra Go flags used for test/build, for example -mod=vendor
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --dist-dir)
      DIST_DIR=${2:?missing value for --dist-dir}
      shift 2
      ;;
    --version)
      VERSION=${2:?missing value for --version}
      shift 2
      ;;
    --commit)
      COMMIT=${2:?missing value for --commit}
      shift 2
      ;;
    --build-date)
      BUILD_DATE=${2:?missing value for --build-date}
      shift 2
      ;;
    --platform)
      PLATFORMS+=("${2:?missing value for --platform}")
      shift 2
      ;;
    --no-test)
      RUN_TESTS=0
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

if [ "${#PLATFORMS[@]}" -eq 0 ]; then
  PLATFORMS=(linux/amd64 linux/arm64 windows/amd64 darwin/amd64 darwin/arm64)
fi

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "缺少命令: $1" >&2
    exit 127
  fi
}

sha256_file() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${file}" | awk '{print $1}'
  else
    shasum -a 256 "${file}" | awk '{print $1}'
  fi
}

archive_platform() {
  local platform="$1"
  local goos="${platform%/*}"
  local goarch="${platform#*/}"
  local name="dm_${VERSION}_${goos}_${goarch}"
  local package_dir="${WORK_DIR}/${name}"
  local binary="dm"
  local format="tar.gz"
  local archive
  local checksum

  if [ "${goos}" = "windows" ]; then
    binary="dm.exe"
    format="zip"
  fi

  mkdir -p "${package_dir}"
  echo "==> build ${platform}"
  (
    cd "${ROOT_DIR}"
    GOOS="${goos}" GOARCH="${goarch}" CGO_ENABLED=0 \
      go build -trimpath -ldflags "${LDFLAGS}" -o "${package_dir}/${binary}" .
  )

  cp "${ROOT_DIR}/README.md" "${package_dir}/"
  cp "${ROOT_DIR}/LICENSE" "${package_dir}/"
  cp "${ROOT_DIR}/.dm.yaml.example" "${package_dir}/dm.yaml.example"
  mkdir -p "${package_dir}/scripts"
  cp "${ROOT_DIR}/scripts/install.sh" "${ROOT_DIR}/scripts/uninstall.sh" "${package_dir}/scripts/"
  cp "${ROOT_DIR}/scripts/install.ps1" "${ROOT_DIR}/scripts/uninstall.ps1" "${package_dir}/scripts/"
  cat >"${package_dir}/INSTALL.md" <<EOF
# docker-manager ${VERSION} ${platform}

## Files

- \`${binary}\`: dm executable for ${platform}
- \`dm.yaml.example\`: sample configuration
- \`scripts/install.*\`: release install scripts
- \`scripts/uninstall.*\`: uninstall scripts

## Linux/macOS install

\`\`\`bash
bash scripts/install.sh --binary ./${binary}
bash scripts/install.sh --binary ./${binary} --completion bash --completion zsh --completion fish
bash scripts/install.sh --binary ./${binary} --no-completion
\`\`\`

## Windows install

\`\`\`powershell
.\scripts\install.ps1 -Binary .\${binary}
.\scripts\install.ps1 -Binary .\${binary} -NoCompletion
\`\`\`

Verify after installation:

\`\`\`bash
dm version
dm doctor --check-e2e=false
\`\`\`
EOF

  if [ "${goos}" = "windows" ]; then
    need_cmd zip
    archive="${DIST_DIR}/${name}.zip"
    (cd "${WORK_DIR}" && zip -qr "${archive}" "${name}")
  else
    archive="${DIST_DIR}/${name}.tar.gz"
    tar -C "${WORK_DIR}" -czf "${archive}" "${name}"
  fi
  checksum=$(sha256_file "${archive}")
  printf '%s  %s\n' "${checksum}" "$(basename "${archive}")" >>"${CHECKSUMS_FILE}"
  printf '    {"platform":"%s","os":"%s","arch":"%s","format":"%s","binary":"%s","archive":"%s","sha256":"%s"}' "${platform}" "${goos}" "${goarch}" "${format}" "${binary}" "$(basename "${archive}")" "${checksum}" >>"${MANIFEST_FILE}"
  printf "| \`%s\` | \`%s\` | \`%s\` | \`%s\` |\n" "${platform}" "${format}" "$(basename "${archive}")" "${checksum}" >>"${SUMMARY_FILE}"
}

need_cmd go
mkdir -p "${DIST_DIR}"
DIST_DIR=$(CDPATH='' cd -- "${DIST_DIR}" && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/dm-release-XXXXXX")
trap 'rm -rf "${WORK_DIR}"' EXIT

LDFLAGS="-s -w -X docker-manager/internal/version.version=${VERSION} -X docker-manager/internal/version.commit=${COMMIT} -X docker-manager/internal/version.buildDate=${BUILD_DATE}"
CHECKSUMS_FILE="${DIST_DIR}/checksums.txt"
MANIFEST_FILE="${DIST_DIR}/release-manifest.json"
SUMMARY_FILE="${DIST_DIR}/release-summary.md"
: >"${CHECKSUMS_FILE}"

if [ "${RUN_TESTS}" = "1" ]; then
  echo "==> go test ./..."
  (cd "${ROOT_DIR}" && go test ./...)
fi

cat >"${MANIFEST_FILE}" <<EOF
{
  "version": "${VERSION}",
  "commit": "${COMMIT}",
  "build_date": "${BUILD_DATE}",
  "artifacts": [
EOF

cat >"${SUMMARY_FILE}" <<EOF
# docker-manager ${VERSION} release artifacts

- Commit: \`${COMMIT}\`
- Build date: \`${BUILD_DATE}\`
- Checksums: \`checksums.txt\`
- Manifest: \`release-manifest.json\`

| Platform | Format | Archive | SHA256 |
| --- | --- | --- | --- |
EOF

first=1
for platform in "${PLATFORMS[@]}"; do
  if ! [[ "${platform}" =~ ^[A-Za-z0-9_]+/[A-Za-z0-9_]+$ ]]; then
    echo "Invalid platform: ${platform}" >&2
    exit 2
  fi
  if [ "${first}" = "1" ]; then
    first=0
  else
    printf ',\n' >>"${MANIFEST_FILE}"
  fi
  archive_platform "${platform}"
done

cat >>"${MANIFEST_FILE}" <<'EOF'

  ]
}
EOF

echo "Release artifacts written to: ${DIST_DIR}"
echo "Checksums: ${CHECKSUMS_FILE}"
echo "Manifest: ${MANIFEST_FILE}"
echo "Summary: ${SUMMARY_FILE}"
