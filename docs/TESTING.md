# 测试和验收

本文档集中记录 `docker-manager` 的本地检查、远程 Docker 验收、企业 registry 验收和已完成测试结论。README 不再展开测试细节，发布操作清单见 [RELEASE_CHECKLIST.md](RELEASE_CHECKLIST.md)。

## 本地检查

基础检查:

```bash
go test ./...
go vet ./...
git diff HEAD --check
```

推荐使用脚本:

```bash
bash scripts/check.sh
bash scripts/check.sh --race
```

Windows:

```powershell
.\scripts\check.ps1
.\scripts\check.ps1 -Race
```

ShellCheck 是默认门禁；环境未安装 ShellCheck 时脚本会失败。仅在明确由其他 CI job 执行 ShellCheck 时，才使用 `--no-shellcheck` 或 `-NoShellCheck` 显式跳过。

`--no-go-checks` / `-NoGoChecks` 仅供已经单独执行 `go test`、`go vet` 和所需 race job 的 CI 使用，可避免同一 job 重复运行 Go 检查；它会同时跳过 test、vet 和 race，且不能与 `--race` / `-Race` 同时使用。本地和发布前检查不得使用该选项代替默认全量门禁。

`check.sh` 和 `check.ps1` 还会调用同一个 `scripts/text-check.go`，要求仓库文本源码使用 UTF-8 无 BOM、LF 换行、文件末尾换行，且不包含 Unicode replacement character `U+FFFD`。该检查同时覆盖已跟踪文件和未被 ignore 的新增源码。

Linux CI 生成一个 package-local Go coverage profile，并执行全局和关键包双层门禁。本地可用同一命令复现：

```bash
coverage_profile="${TMPDIR:-/tmp}/dm-coverage-$$.out"
go test -count=1 -covermode=atomic -coverprofile="$coverage_profile" ./...
go run ./scripts/coverage-check \
  -profile "$coverage_profile" \
  -total 70 \
  -package internal/appconfig=85 \
  -package internal/dockerconfig=90 \
  -package internal/registryauth=85 \
  -package internal/runconfig=90 \
  -package internal/targets=95 \
  -package internal/docker=65 \
  -package internal/completion=85
rm -f -- "$coverage_profile"
```

门禁按 profile 中的 statement block 计数，不使用 `-coverpkg=./...` 将其他包的执行错误归属到当前测试包。Docker service 单测使用真实 Moby SDK 对 fake HTTP daemon 验证请求契约；Docker 24/27/29 的真实 daemon 行为仍由独立 E2E matrix 验证。

本地 smoke:

```powershell
$localTestOutput = Join-Path $env:TEMP ("dm-local-test-" + [guid]::NewGuid().ToString("N"))
.\scripts\local-test.ps1 -OutputDir $localTestOutput
.\scripts\local-test.ps1 -OutputDir ($localTestOutput + "-noenv") -NoEnvironment
```

覆盖范围包括帮助输出、版本输出、completion 生成、`DM_CONFIG`、错误输出格式、PowerShell 安装/卸载和 Docker 不可用时的错误路径。默认运行还会验证 `DM_CONFIG`、`DM_HOME`、`DM_OUTPUT_DIR` 的 User/Process 快照恢复、多安装所有权链、失败回滚和真实 junction/reparse 拒绝；`-NoEnvironment` 用于不允许写用户环境变量的受限环境，并会跳过多安装所有权用例。两种模式都应使用唯一 `-OutputDir`，避免混用旧报告。

## Completion 深度测试

Linux 上的无 Docker shell 门禁需要安装 `bash-completion`、zsh 和 fish：

```bash
sudo apt-get install --yes --no-install-recommends bash-completion zsh fish
completion_bin="${TMPDIR:-/tmp}/dm-completion-bin-$$"
go build -o "$completion_bin" .
bash scripts/completion-test.sh --dm-bin "$completion_bin" --no-docker --require-shells
rm -f -- "$completion_bin"
```

Windows 应分别在 PowerShell 7 和 Windows PowerShell 5.1 执行：

```powershell
$completionBin = Join-Path $env:TEMP ("dm-completion-bin-" + [guid]::NewGuid().ToString("N") + ".exe")
go build -o $completionBin .
try {
    .\scripts\completion-test.ps1 -DmBin $completionBin -NoDocker
} finally {
    Remove-Item -LiteralPath $completionBin -Force -ErrorAction SilentlyContinue
}
```

Linux 脚本会在 `compinit` 后加载 zsh completion，并实际加载 bash/fish completion；PowerShell 脚本会 dot-source 生成文件，再通过 `TabExpansion2` 验证 `dm re` 返回 `report`。所有 native case 必须同时满足退出码为 0 和候选内容匹配。测试日志、TSV 和 Markdown 报告统一写为 UTF-8 无 BOM、LF。

Docker 候选测试不拉取外部镜像，需要 daemon 中预先存在一个带 tag、可执行 `sh -c 'sleep 3600'` 的本地镜像：

```bash
completion_bin="${TMPDIR:-/tmp}/dm-completion-docker-bin-$$"
go build -o "$completion_bin" .
bash scripts/completion-test.sh --dm-bin "$completion_bin" --require-shells --require-docker
rm -f -- "$completion_bin"
```

```powershell
$completionBin = Join-Path $env:TEMP ("dm-completion-docker-bin-" + [guid]::NewGuid().ToString("N") + ".exe")
go build -o $completionBin .
try {
    .\scripts\completion-test.ps1 -DmBin $completionBin -RequireDocker
} finally {
    Remove-Item -LiteralPath $completionBin -Force -ErrorAction SilentlyContinue
}
```

`--require-shells` 会把缺失的 bash-completion、zsh 或 fish 记为 FAIL；`--require-docker` / `-RequireDocker` 会把 daemon、镜像或临时资源不可用记为 FAIL。显式 `--no-docker` / `-NoDocker` 只用于无 Docker 门禁，不能和 require Docker 选项同时使用。需要保留报告时传入一个尚不存在的工作目录；PowerShell 还需同时传 `-KeepWorkDir`。

## P1 定向回归

P1 安全边界变更后至少运行：

```bash
go test -count=1 ./internal/appconfig ./internal/cli ./internal/registryauth
go test -count=1 ./internal/commands/pull ./internal/commands/backup ./internal/commands/reverse ./internal/commands/diagnostics
go test -race -count=1 ./internal/appconfig ./internal/cli ./internal/registryauth ./internal/commands/pull ./internal/commands/backup ./internal/commands/reverse ./internal/commands/diagnostics
```

定向用例应确认：

- 配置文件超过 `1 MiB`、根节点不是 mapping、未知字段、错误类型、多文档和显式缺失路径均失败；隐式缺失 `.dm.yaml` 才回退默认值。
- 默认 `redact_profile=none` 保留管理员输出；`basic`、`strict` 覆盖 text/JSON/Markdown/HTML、错误、verbose HTTP 和 reverse 文件。YAML 的 `redact_profile: none` 加 `redact_secrets: true` 必须失败，命令行同时指定时显式 `--redact-profile` 优先。
- pull 覆盖 token/manifest/config body 上限、单层及累计压缩/展开预算、临时空间峰值、层数、慢 body、取消与 `1h` 默认/`24h` 硬上限；`--skip-existing`、重试、load/push 都必须处于单镜像 `--total-timeout` 内。
- pull 输出覆盖已有普通文件的原子替换；backup 输出覆盖已有 artifact 的拒绝。两者还应覆盖 symlink/junction、并发 writer、取消、close/sync/rename 失败，确认失败时旧目标不变且只清理本事务 staging。
- 分卷备份覆盖 `.parts.pending.json` 文件锁、崩溃后重试、已提交 manifest、外来同名 part/marker 替换和 staging 所有权冲突；恢复只能删除 `os.SameFile` 证明属于旧事务的文件。
- restore/rerun 创建结果不确定与回滚测试必须注入同名外来容器；只有稳定 ID 和本次 128-bit owner label 同时匹配的候选可被删除。
- volume docker-run probe 使用 digest-pinned 默认 helper、无网络、只读 rootfs/volume、drop capabilities、`no-new-privileges`，并验证失败和取消后 helper 容器清理。
- image/volume prune 缺少 `--allow-non-atomic-delete` 时在首个删除请求前失败；container-only 候选不要求该选项。

## 远程 Docker 验收

建议在干净 Docker 主机上使用临时目录执行:

```bash
export DM_TEST_ROOT="/root/dm-test-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$DM_TEST_ROOT"
cd "$DM_TEST_ROOT"
export DM_TEST_LOG="$DM_TEST_ROOT/test-output.log"
exec > >(tee -a "$DM_TEST_LOG") 2>&1
```

自动化验收:

```bash
bash scripts/e2e.sh --mode smoke
bash scripts/e2e.sh --mode install
bash scripts/e2e.sh --mode cancel
bash scripts/e2e.sh --mode full --confirm-destructive
bash scripts/e2e.sh --mode destructive --confirm-destructive
```

常用环境变量:

```bash
DM_E2E_IMAGE=busybox:latest bash scripts/e2e.sh --mode full --confirm-destructive
DM_E2E_PROXY=http://proxy.example:7890 bash scripts/e2e.sh --mode full --confirm-destructive
DM_E2E_OFFLINE=1 bash scripts/e2e.sh --mode full --confirm-destructive
DM_E2E_DM_BIN=/root/dm bash scripts/e2e.sh
DM_E2E_KEEP_WORKDIR=1 DM_E2E_WORK_DIR="$DM_TEST_ROOT/e2e-work" bash scripts/e2e.sh
```

不指定模式时默认执行 `smoke`，且不依赖 Docker；`install` 验证临时安装/卸载；`cancel` 使用本地阻塞探针验证 SIGINT/context 取消；`full` 和 `destructive` 会启动临时 registry，并覆盖镜像拉取、导入、推送、备份恢复、报告和破坏性命令安全边界，因此必须显式选择模式并确认。`DM_E2E_WORK_DIR` 必须指向尚不存在的新目录；脚本只会删除自己创建且 ownership sentinel 匹配的目录。

网络受限时可使用代理:

```bash
export HTTP_PROXY="http://proxy.example:7890"
export HTTPS_PROXY="http://proxy.example:7890"
export NO_PROXY="127.0.0.1,localhost,registry.local"
```

如果源 registry 需要代理、目标 registry 是本地或内网地址，优先使用环境变量加 `NO_PROXY`，避免目标 registry 被代理转发。单条命令强制代理可使用 `dm pull --proxy`。

## Docker daemon TLS 契约

发布前至少运行以下定向测试：

```bash
go test -count=1 ./internal/docker ./internal/commands/diagnostics
go test -race -count=1 ./internal/docker ./internal/commands/diagnostics
```

测试矩阵必须覆盖 `tcp://` 与 `docker_tls_verify=true/false`、证书目录为空/有效/不存在的六种组合，并通过真实 TLS test server 验证以下契约：

- 有效证书目录始终启用 HTTPS/mTLS；`false` 只关闭 daemon 证书校验，不切回 HTTP。
- `true` 且证书目录为空、缺失或损坏时，client 初始化失败且不会发出明文请求。
- `https://`、`http://`、`ssh://`、未知 scheme，以及 unix/npipe 配 TLS 参数会被早期拒绝。
- `dm doctor` 根据实际 transport 报告 HTTP、未校验 HTTPS、已校验 HTTPS，并显示协商后的 client API version。
- `DOCKER_*` 环境变化不会复用旧 client，client 初始化不会改写这些进程环境变量。

远程明文 daemon 只能用于契约确认，不代表 TLS daemon 矩阵通过。至少检查 `dm doctor --format json` 中 `docker-endpoint` 为 `warning`、`transport=http`、`tls=false`，同时 `docker-daemon` 与 `docker-version` 为 `ok`。真实生产 TLS 验收还需使用独立 CA、服务端证书和客户端证书运行 Docker 2376，并分别验证正确证书、错误 CA、缺失客户端证书和 hostname 不匹配。

## 手动验收示例

基础信息:

```bash
dm version
dm doctor --format markdown > "$DM_TEST_ROOT/doctor.md"
docker version
docker info
```

镜像链路:

```bash
dm image pull busybox:latest --output-dir "$DM_TEST_ROOT/pulled"
dm image pull busybox:latest --load --output-dir "$DM_TEST_ROOT/pulled-load"
dm image pull busybox:latest --output-dir "$DM_TEST_ROOT/pulled-budget" --max-layers 256 --max-layer-bytes 2147483648 --max-expanded-layer-bytes 4294967296 --max-total-layer-bytes 4294967296 --max-total-expanded-bytes 8589934592 --max-temporary-bytes 17179869184 --total-timeout 20m
dm image save "$DM_TEST_ROOT/saved" --filter 'repo:busybox' --dry-run
dm image save "$DM_TEST_ROOT/saved" --filter 'repo:busybox'
dm image load "$DM_TEST_ROOT/saved"
```

临时 registry:

```bash
docker run -d --name "dm_registry_test" -p 0:5000 registry:2
export DM_REGISTRY_PORT="$(docker port dm_registry_test 5000/tcp | sed 's/.*://')"
export DM_REGISTRY="127.0.0.1:${DM_REGISTRY_PORT}"

dm registry "$DM_REGISTRY" --plain-http
dm pull busybox:latest --to "$DM_REGISTRY/dm-mirror" --plain-http
printf '%s\n' busybox:latest > "$DM_TEST_ROOT/images.txt"
dm pull --file "$DM_TEST_ROOT/images.txt" --to "$DM_REGISTRY/dm-batch" --plain-http --concurrency 1 --retries 1 --resume --report "$DM_TEST_ROOT/pull-report.json"
```

容器和备份:

```bash
docker run -d --name dm_test_container --label dmtest=true busybox:latest sh -c 'while true; do echo dm-test; sleep 5; done'

dm reverse dm_test_container --pretty
dm reverse --filter "label:dmtest=true" --reverse-type compose
dm rerun dm_test_container --dry-run
dm backup dm_test_container --dry-run
dm backup dm_test_container --bundle --bundle-output "$DM_TEST_ROOT/container-backup.tar.gz"
dm backup dm_test_container --bundle --split-size 1M --bundle-output "$DM_TEST_ROOT/container-split.tar.gz"
dm restore "$DM_TEST_ROOT/container-backup.tar.gz" --dry-run
dm restore "$DM_TEST_ROOT/container-backup.tar.gz" --dry-run --max-archive-size 20G --max-expanded-size 40G --max-json-size 64M --max-parts 32
# 使用唯一目标名执行实际恢复，实际写操作必须显式确认
dm restore "$DM_TEST_ROOT/container-backup.tar.gz" --name dm_test_restored --confirm
```

`--signing-key`、`--passphrase-file` 和最终 bundle 路径必须放在备份输出目录外。测试 checksum 缺失兼容路径时必须显式使用 `--skip-checksum`，其他实际恢复测试不得跳过完整性校验。成功分卷后应存在连续 `.part-NNN` 和 `container-split.tar.gz.parts.json`，manifest 的 `commit` 为 `complete`，逐卷和整体 digest 可复核，且不再存在 `.parts.pending.json` 或 `.dm-backup-staging-*`。

实际 restore/rerun 后检查内部事务标签为 32 个十六进制字符；相同名称本身不能作为清理所有权依据：

```bash
docker inspect --format '{{ index .Config.Labels "com.docker-manager.restore-owner" }}' dm_test_restored
```

报告:

```bash
dm health --filter "label:dmtest=true" --format markdown
dm network --filter "label:dmtest=true" --format html
dm logs --filter "label:dmtest=true" --keyword dm-test --tail 50
dm volumes --format json
docker pull 'busybox:1.36.1@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662'
dm volumes --size-mode docker-run --format json
docker ps -a --filter label=com.docker-manager.volume-probe
dm tree busybox:latest --format markdown
dm prune --filter "label=dmtest=true" --format markdown
docker volume create --label dmtest=true dm_test_prune_volume
# 预期失败，且 dm_test_prune_volume 仍存在
dm prune --only volume --filter "label=dmtest=true" --apply --confirm
docker volume inspect dm_test_prune_volume
# 明确接受 Docker 非原子删除窗口后才允许删除
dm prune --only volume --filter "label=dmtest=true" --apply --confirm --allow-non-atomic-delete
```

恢复资源预算测试应覆盖超限归档、展开炸弹、超长加密 chunk、过多/缺失/乱序分卷、JSON 深度和累计字节，并确认失败时没有已发布的 join 文件、解密明文树或 Docker mutation。volume probe 结束后，按 `com.docker-manager.volume-probe` 筛选不应有残留容器。prune 应先验证缺少 `--allow-non-atomic-delete` 时 image/volume 候选不会触发任何删除，再在隔离测试资源上显式确认。

清理:

```bash
docker rm -f dm_test_container dm_test_restored dm_registry_test >/dev/null 2>&1 || true
docker volume rm dm_test_prune_volume >/dev/null 2>&1 || true
rm -rf "$DM_TEST_ROOT"
unset HTTP_PROXY HTTPS_PROXY NO_PROXY
```

## 企业 registry 验收

建议覆盖以下维度:

- HTTP/insecure registry 和 HTTPS registry。
- Docker config `auths`、credential helper、错误凭据和无凭据私有项目拒绝。
- `dm registry` text/json 输出和退出码策略。
- `dm pull`、`dm pull --load`、`dm pull --to`、批量 `--file`、`--skip-existing`。
- 企业代理、`NO_PROXY`、私有 CA、缺失 CA 失败和超时重试。
- Harbor robot token、quota 拒绝、项目权限和审计链路。
- Nexus Docker hosted registry。
- Artifactory/JCR 8081 Docker 原生入口和 8082 Router 诊断提示。

Artifactory/JCR 单节点测试环境需要显式允许 Derby、预置 `master.key` / `join.key`、预接受 EULA 并初始化 Docker 仓库。生产形态仍应使用 PostgreSQL、HTTPS、可信证书和完整反向代理/external URL 配置。

OIDC/Keycloak 如受网络影响，可先做降级验收: 大镜像能进入 manifest/auth/blob/layer 拉取流程，小镜像完成归档、`--load`、`--to` 和回拉。真实 Harbor OIDC 登录、权限映射和审计链路仍需要 Keycloak 完整部署或真实企业 OIDC 环境。

## 已完成验收记录

- 本地静态检查: `go test ./...`、`go vet ./...`、`go test -race ./...`、`scripts/check.ps1 -Race`、`git diff --check` 已通过。
- Windows 本地 smoke: 覆盖帮助、版本、completion、配置加载、安装卸载和 Docker 不可用错误路径。
- 发布打包: Windows PowerShell 打包脚本可生成 linux/amd64、linux/arm64、windows/amd64、darwin/amd64、darwin/arm64 归档、checksum、manifest 和 summary。
- 2026-07-07 Docker API 迁移阶段 6: `scripts/check.ps1` 通过；VM smoke 9 PASS / 5 XFAIL / 0 FAIL；VM destructive/full 48 PASS / 12 XFAIL / 0 FAIL；Windows 侧通过 `--docker-host tcp://192.168.31.57:2375` 验证远程 doctor、reverse、health、logs、prune dry-run 和容器/镜像 completion。
- 干净 Ubuntu 24.04 / Docker 29.1.3 VM: install 14 PASS / 5 XFAIL / 0 FAIL；destructive/full 48 PASS / 12 XFAIL / 0 FAIL；测试后无 `dm_e2e_*` 残留资源。
- 2026-07-01 远程复测: smoke 9 PASS / 5 XFAIL / 0 FAIL；install 14 PASS / 5 XFAIL / 0 FAIL；destructive/full 48 PASS / 12 XFAIL / 0 FAIL；企业 registry 模拟 11 PASS / 3 XFAIL / 0 FAIL。
- Harbor v2.14.4 HTTP/insecure registry: 14 PASS / 1 XFAIL / 0 FAIL，覆盖部署、Docker login、项目创建、push、`dm registry`、`dm pull`、`--load`、`--to`、批量 report、`--skip-existing` 和私有项目无凭据拒绝。
- Nexus Repository Community 3.93.2-01 HTTP Docker hosted registry: 17 PASS / 1 XFAIL / 0 FAIL，覆盖 DockerToken realm、login、push/pull、`dm registry`、`dm pull`、`--load`、`--to`、批量 report 和无凭据拒绝。
- Artifactory/JCR: 20 PASS / 0 FAIL / 1 INFO，覆盖 8081 Docker 原生 login/push/pull、`dm registry`、`dm doctor`、`dm pull`、`--load`、`--to http://...` 和 8082 Router 诊断提示。
- 企业网络、代理、CA 和 doctor: 16 PASS / 0 FAIL，覆盖 HTTP/HTTPS 代理认证、HTTPS CONNECT、`NO_PROXY`、企业根 CA、缺失 CA 失败、insecure registry、credential helper、磁盘/inode 和输出目录写入探测。
- Harbor 扩展: 16 PASS / 0 FAIL / 1 INFO，覆盖 robot token、robot login/push/pull、`dm registry`、`dm pull`、项目 summary 和 quota push 拒绝；审计 API 可访问但测试环境返回空列表，记为 INFO。
- Artifactory HTTPS 反向代理: 8 PASS / 0 FAIL，覆盖临时企业 CA、HTTPS 反代、`dm registry`、`dm doctor`、`dm pull`、缺失 CA 失败和 Docker 原生 login/pull。
- 中等规模资源: 11 PASS / 0 FAIL / 1 INFO，覆盖 24 个 registry 镜像、80 个容器、100 个 volume、`health/logs/volumes/prune`、批量 mirror 和 skip-existing。
- 取消行为复测: `backup --bundle`、`restore --no-start`、`logs`、`prune` dry-run 收到 SIGINT/context cancel 后输出 `操作已取消` 并以 130 退出。
- Completion 历史验收已覆盖容器、镜像和 volume 候选；P2 后四种 shell 的加载证据以本节严格模式和 CI completion artifact 为准，关键依赖被 SKIP 不计作发布通过。
- Harbor LDAP 身份源: 临时 OpenLDAP、LDAP bind、Harbor `ldap_auth` 配置和 LDAP 用户 API 登录通过；审计 API 可访问但当前页未返回登录记录，记为 SKIP。
- OIDC/Keycloak 镜像拉取链路降级验收: Keycloak、MySQL、Harbor Core 进入并完成 manifest/layer 拉取流程；busybox/hello-world 完成归档、load 和本地 registry mirror 回拉。

## 已知非阻断项

- linux/arm64、darwin/amd64、darwin/arm64 已完成交叉编译产物生成，但尚未做真机运行验证。
- 真实 Harbor OIDC 登录、权限映射和审计链路仍需 Keycloak 完整部署或企业 OIDC 环境复测。
- 数百级大规模资源压测建议在专用环境单独运行。
