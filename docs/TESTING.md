# 测试和验收

本文档集中记录 `docker-manager` 的本地检查、远程 Docker 验收、企业 registry 验收和已完成测试结论。README 不再展开测试细节，发布操作清单见 [RELEASE_CHECKLIST.md](RELEASE_CHECKLIST.md)。

## 本地检查

基础检查:

```bash
go test ./...
go vet ./...
git diff --check
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

本地 smoke:

```powershell
.\scripts\local-test.ps1
```

覆盖范围包括帮助输出、版本输出、completion 生成、`DM_CONFIG`、错误输出格式、PowerShell 安装/卸载和 Docker 不可用时的错误路径。

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
bash scripts/e2e.sh --mode full
bash scripts/e2e.sh --mode destructive
```

常用环境变量:

```bash
DM_E2E_IMAGE=busybox:latest bash scripts/e2e.sh
DM_E2E_PROXY=http://proxy.example:7890 bash scripts/e2e.sh
DM_E2E_OFFLINE=1 bash scripts/e2e.sh
DM_E2E_DM_BIN=/root/dm bash scripts/e2e.sh
DM_E2E_KEEP_WORKDIR=1 DM_E2E_WORK_DIR="$DM_TEST_ROOT/e2e-work" bash scripts/e2e.sh
```

`smoke` 不依赖 Docker；`install` 验证临时安装/卸载；`full` 和 `destructive` 会启动临时 registry，并覆盖镜像拉取、导入、推送、备份恢复、报告和破坏性命令安全边界。

网络受限时可使用代理:

```bash
export HTTP_PROXY="http://proxy.example:7890"
export HTTPS_PROXY="http://proxy.example:7890"
export NO_PROXY="127.0.0.1,localhost,registry.local"
```

如果源 registry 需要代理、目标 registry 是本地或内网地址，优先使用环境变量加 `NO_PROXY`，避免目标 registry 被代理转发。单条命令强制代理可使用 `dm pull --proxy`。

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
dm restore "$DM_TEST_ROOT/container-backup.tar.gz" --dry-run
```

报告:

```bash
dm health --filter "label:dmtest=true" --format markdown
dm network --filter "label:dmtest=true" --format html
dm logs --filter "label:dmtest=true" --keyword dm-test --tail 50
dm volumes --format json
dm tree busybox:latest --format markdown
dm prune --filter "label=dmtest=true" --format markdown
```

清理:

```bash
docker rm -f dm_test_container dm_registry_test >/dev/null 2>&1 || true
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
- Completion 深度测试: bash/zsh/fish/PowerShell 均验证脚本加载；bash 真实交互依赖系统 `bash-completion`；容器、镜像、volume 候选已覆盖。
- Harbor LDAP 身份源: 临时 OpenLDAP、LDAP bind、Harbor `ldap_auth` 配置和 LDAP 用户 API 登录通过；审计 API 可访问但当前页未返回登录记录，记为 SKIP。
- OIDC/Keycloak 镜像拉取链路降级验收: Keycloak、MySQL、Harbor Core 进入并完成 manifest/layer 拉取流程；busybox/hello-world 完成归档、load 和本地 registry mirror 回拉。

## 已知非阻断项

- linux/arm64、darwin/amd64、darwin/arm64 已完成交叉编译产物生成，但尚未做真机运行验证。
- 真实 Harbor OIDC 登录、权限映射和审计链路仍需 Keycloak 完整部署或企业 OIDC 环境复测。
- 数百级大规模资源压测建议在专用环境单独运行。
