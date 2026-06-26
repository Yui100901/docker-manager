# 远程测试与生产前验收手册

本文档用于在一台带 Docker 的远程服务器上验证 `dm` 的核心能力。目标是用小镜像、小容器和临时目录完成低影响测试，并把过程输出留档。

## 适用场景

- 发布前确认 `dm` 二进制在目标 Linux 服务器可运行。
- 验证镜像拉取、导入、推送、容器逆向、备份恢复和诊断报告。
- 在网络受限环境下使用临时 HTTP/HTTPS 代理完成 registry 相关测试。
- 保留一次完整测试过程，便于回溯失败命令和环境限制。

## 前置条件

远程服务器需要满足:

- Docker daemon 已启动，当前用户可执行 Docker 命令。
- 已上传或可在服务器上构建 `dm`。
- 测试目录有足够空间，建议至少 2GB。
- 如需访问外网 registry，确认直连或代理可用。
- 如需 push 私有 registry，先执行 `docker login <registry>`，或准备可用的 Docker config。

建议先运行:

```bash
dm doctor --format text
dm doctor --registry registry.local:5000 --plain-http
```

如果是内网 HTTP registry，使用 `--plain-http`。如果 Docker config 不在默认位置，追加 `--docker-config /path/to/config.json`。

## 推荐临时目录

不要在项目目录或业务目录中直接测试。建议使用带时间戳的临时目录:

```bash
export DM_TEST_ROOT="/root/dm-test-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$DM_TEST_ROOT"
cd "$DM_TEST_ROOT"
```

建议统一保存命令输出:

```bash
export DM_TEST_LOG="$DM_TEST_ROOT/test-output.log"
exec > >(tee -a "$DM_TEST_LOG") 2>&1
```

如果需要保留 `scripts/e2e.sh` 工作目录:

```bash
export DM_E2E_KEEP_WORKDIR=1
export DM_E2E_WORK_DIR="$DM_TEST_ROOT/e2e-work"
```

## 测试资源命名

统一使用带时间戳或随机后缀的名称，避免和生产资源冲突:

```bash
export DM_TEST_SUFFIX="$(date +%s)"
export DM_TEST_IMAGE="busybox:latest"
export DM_TEST_CONTAINER="dm_test_${DM_TEST_SUFFIX}"
export DM_TEST_RESTORED="dm_test_restored_${DM_TEST_SUFFIX}"
export DM_TEST_LABEL="dmtest=${DM_TEST_SUFFIX}"
```

测试容器尽量使用无状态、小资源容器:

```bash
docker run -d --name "$DM_TEST_CONTAINER" --label "$DM_TEST_LABEL" busybox:latest sh -c 'while true; do echo dm-test; sleep 5; done'
```

## 代理设置

默认情况下 `dm pull` 和 `dm pull mirror` 会读取 `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY`。网络受限时可以临时设置:

```bash
export HTTP_PROXY="http://192.168.31.70:7890"
export HTTPS_PROXY="http://192.168.31.70:7890"
export NO_PROXY="127.0.0.1,localhost,registry.local"
```

也可以只对单条命令指定代理:

```bash
dm pull busybox:latest --proxy http://192.168.31.70:7890
```

测试结束后恢复环境:

```bash
unset HTTP_PROXY HTTPS_PROXY NO_PROXY
```

## 快速自动化验收

仓库自带端到端脚本，推荐先跑一遍:

```bash
bash scripts/e2e.sh
```

常用参数:

```bash
DM_E2E_IMAGE=busybox:latest bash scripts/e2e.sh
DM_E2E_OFFLINE=1 bash scripts/e2e.sh
DM_E2E_GOFLAGS=-mod=vendor bash scripts/e2e.sh
DM_E2E_DM_BIN=/root/dm bash scripts/e2e.sh
DM_E2E_KEEP_WORKDIR=1 DM_E2E_WORK_DIR="$DM_TEST_ROOT/e2e-work" bash scripts/e2e.sh
```

脚本会启动临时 registry，并覆盖:

- `registry-login-check --plain-http`
- `pull --plain-http --output`
- `pull --plain-http --load`
- `pull --to`
- `backup container --bundle`
- `restore <archive>`

如果服务器无法访问 Docker Hub，可先预拉 `busybox:latest` 和 `registry:2`，再使用 `DM_E2E_OFFLINE=1`。

## 手动验收命令

下面命令适合在自动脚本之外做小范围验证。建议每组命令都保留输出。

### 基础信息

```bash
dm version
dm version --format json
dm doctor --format markdown > "$DM_TEST_ROOT/doctor.md"
docker version
docker info
```

### 镜像拉取、导入和保存

```bash
mkdir -p "$DM_TEST_ROOT/pulled" "$DM_TEST_ROOT/saved"

dm pull "$DM_TEST_IMAGE" --output-dir "$DM_TEST_ROOT/pulled"
dm pull "$DM_TEST_IMAGE" --load --output-dir "$DM_TEST_ROOT/pulled-load"
dm save "$DM_TEST_ROOT/saved" --filter 'repo:busybox' --dry-run
dm save "$DM_TEST_ROOT/saved" --filter 'repo:busybox'
dm load "$DM_TEST_ROOT/saved"
```

如果直连失败，追加 `--proxy http://192.168.31.70:7890` 或设置代理环境变量。

### 临时 registry 和批量镜像同步

如果不使用 `scripts/e2e.sh`，可以手动启动临时 registry:

```bash
docker run -d --name "dm_registry_${DM_TEST_SUFFIX}" -p 0:5000 registry:2
export DM_REGISTRY_PORT="$(docker port "dm_registry_${DM_TEST_SUFFIX}" 5000/tcp | sed 's/.*://')"
export DM_REGISTRY="127.0.0.1:${DM_REGISTRY_PORT}"

dm registry-login-check "$DM_REGISTRY" --plain-http
dm pull "$DM_TEST_IMAGE" --to "$DM_REGISTRY/dm-mirror" --plain-http

printf '%s\n' "$DM_TEST_IMAGE" > "$DM_TEST_ROOT/images.txt"
dm pull mirror --file "$DM_TEST_ROOT/images.txt" --to "$DM_REGISTRY/dm-batch" --plain-http --concurrency 1 --retries 1 --resume --report "$DM_TEST_ROOT/pull-mirror-report.json"
```

### 容器逆向和重建预检

```bash
dm reverse "$DM_TEST_CONTAINER" --pretty
dm reverse "$DM_TEST_CONTAINER" --reverse-type compose > "$DM_TEST_ROOT/reverse-compose.yml"
dm reverse --filter "label:$DM_TEST_LABEL" --reverse-type all > "$DM_TEST_ROOT/reverse-all.txt"
dm rerun "$DM_TEST_CONTAINER" --dry-run
```

不要在生产容器上直接运行 `dm rerun --confirm`。如需验证实际重建，只对临时测试容器执行:

```bash
dm rerun "$DM_TEST_CONTAINER" --confirm
```

### 备份和恢复

```bash
mkdir -p "$DM_TEST_ROOT/backups"

dm backup container "$DM_TEST_CONTAINER" --dry-run
dm backup container "$DM_TEST_CONTAINER" --bundle --output-dir "$DM_TEST_ROOT/backups" --output "$DM_TEST_ROOT/container-backup.tar.gz"
dm restore "$DM_TEST_ROOT/container-backup.tar.gz" --dry-run
dm restore "$DM_TEST_ROOT/container-backup.tar.gz" --name "$DM_TEST_RESTORED" --replace
docker ps --filter "name=$DM_TEST_RESTORED"
```

### 诊断报告

```bash
dm health --filter "label:$DM_TEST_LABEL" --format markdown > "$DM_TEST_ROOT/health.md"
dm network --filter "label:$DM_TEST_LABEL" --format html > "$DM_TEST_ROOT/network.html"
dm logs-scan --filter "label:$DM_TEST_LABEL" --keyword dm-test --tail 50 --format markdown > "$DM_TEST_ROOT/logs-scan.md"
dm volume ls-unused --format json > "$DM_TEST_ROOT/volumes.json"
dm image tree "$DM_TEST_IMAGE" --format markdown > "$DM_TEST_ROOT/image-tree.md"
dm prune-report --filter "label=$DM_TEST_LABEL" --format markdown > "$DM_TEST_ROOT/prune-report.md"
```

清理类命令必须先看报告，再决定是否执行:

```bash
dm prune-report --filter "label=$DM_TEST_LABEL" --apply --confirm
```

## 结果判定

建议将以下结果视为通过:

- `dm doctor` 没有和测试目标相关的 `failed` 项。Docker 未运行、registry 无法访问、凭据不可用应视为阻塞。
- `scripts/e2e.sh` 正常退出，测试目录中无未清理的临时容器。
- `pull --to` 或 `pull mirror` 能将小镜像推送到临时 registry。
- `backup --bundle` 生成离线包，`restore --dry-run` 能完成预检。
- 诊断命令能输出 `text/json/markdown/html` 格式中的目标格式。
- 所有破坏性动作都只作用于 `dm_test_*` 或带测试 label 的资源。

## 清理步骤

测试结束后清理临时容器、registry、镜像和目录:

```bash
docker rm -f "$DM_TEST_CONTAINER" "$DM_TEST_RESTORED" "dm_registry_${DM_TEST_SUFFIX}" >/dev/null 2>&1 || true

docker image ls --format '{{.Repository}}:{{.Tag}}' |
  grep -E "(${DM_REGISTRY}/dm-|dm-e2e-|busybox)" |
  xargs -r docker image rm >/dev/null 2>&1 || true

rm -rf "$DM_TEST_ROOT"
```

如果设置过代理:

```bash
unset HTTP_PROXY HTTPS_PROXY NO_PROXY
```

## 已知限制

- Docker Hub 在部分网络环境下可能超时或限流，推荐使用代理或提前预拉小镜像。
- 内网 HTTP registry 必须显式使用 `--plain-http`。
- `pull --to` 和 `pull mirror` 的 push 权限依赖本机 Docker 登录状态。
- `restore --replace` 和 `rerun --confirm` 会修改 Docker 状态，只能在明确测试目标时执行。
- `prune-report --apply` 会删除 Docker 资源，必须先确认筛选条件和报告结果。
- 日志、inspect 和报告可能包含敏感信息，分享前可使用 `--redact-secrets` 或只分享脱敏后的报告。
