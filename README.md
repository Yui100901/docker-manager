# docker-manager

`docker-manager` 是一个面向 Docker 日常运维、镜像迁移和容器诊断的命令行工具。二进制默认命名为 `dm`。

它覆盖几类常见工作:

- 镜像拉取、导入、导出和重新推送。
- 运行中容器反向生成 `docker run` 或 compose。
- 容器离线备份、迁移包生成和恢复。
- 本机 Docker 资源清理预览、网络/健康/日志/volume/镜像层诊断。
- registry 登录配置和连通性检查。

> 说明: 工具里包含会修改 Docker 状态的命令，例如 `restore`、`prune-report --apply`、`reverse --rerun --confirm`、`pull --to`。在生产环境执行前建议先使用 `--dry-run` 或在测试机验证。

## 构建

```bash
go build -o dm .
```

注入版本、commit 和构建时间:

```bash
go build -ldflags "-X docker-manager/internal/version.version=v0.1.0 -X docker-manager/internal/version.commit=$(git rev-parse --short HEAD) -X docker-manager/internal/version.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o dm .
```

Windows:

```powershell
go build -o dm.exe .
```

查看帮助:

```bash
dm --help
dm <command> --help
dm version
```

## 项目结构

```text
main.go                         # 程序入口，只负责调用 internal/cli
internal/cli/                   # 根命令、配置读取、日志和全局错误输出
internal/commands/images/       # load/save 镜像导入导出命令
internal/commands/pull/         # pull 镜像拉取、导入和重新推送命令
internal/commands/reverse/      # reverse 容器 inspect 到 docker run/compose 的逆向解析命令
internal/commands/backup/       # backup/restore 容器备份、迁移包和恢复命令
internal/commands/diagnostics/  # health、network、logs-scan、prune、volume、image tree 等诊断命令
internal/completion/            # shell 补全命令和本地 Docker 资源补全
internal/docker/                # Docker API client 和镜像/容器管理封装
internal/report/                # text/json 报告输出格式
internal/version/               # version 命令和构建版本信息
scripts/                        # 端到端测试等辅助脚本
```

## 全局参数

```bash
--config string   配置文件路径，默认 .dm.yaml
--verbose         输出详细日志
--quiet           静默 info 日志
--json            以 JSON 格式输出日志和错误
```

示例 `.dm.yaml`:

```yaml
proxy: http://127.0.0.1:7890
os: linux
arch: amd64
output_dir: images
verbose: false
quiet: false
json: false
```

## Shell 自动补全

生成补全脚本:

```bash
dm completion bash
dm completion zsh
dm completion fish
dm completion powershell
```

资源参数会尽量从本机 Docker 补齐，例如容器、镜像和 volume。容器筛选支持 `name:`、`id:`、`image:`、`state:`、`status:`、`label:` 前缀和 `* ?` 通配符。已支持的典型位置包括 `backup container`、`reverse`、`inspect-diff`、`logs-scan`、`health`、`network`、`image tree`、`volume ls-unused`，以及 `save --filter` 等筛选参数。

PowerShell 临时加载示例:

```powershell
dm completion powershell | Out-String | Invoke-Expression
```

## 端到端集成测试

仓库提供 `scripts/e2e.sh`，用于在有 Docker 的测试机上启动临时 registry，并覆盖 `registry-login-check --plain-http`、`pull --plain-http --output`、`pull --plain-http --load`、`pull --to`、`backup container --bundle` 和 `restore <archive>`。

```bash
bash scripts/e2e.sh
```

可通过环境变量调整测试参数:

```bash
DM_E2E_IMAGE=busybox:latest bash scripts/e2e.sh
DM_E2E_GOFLAGS=-mod=vendor bash scripts/e2e.sh
DM_E2E_DM_BIN=/root/dm bash scripts/e2e.sh
DM_E2E_OFFLINE=1 bash scripts/e2e.sh
DM_E2E_KEEP_WORKDIR=1 bash scripts/e2e.sh
```

脚本会使用 Docker 随机绑定本地 registry 端口，避免端口冲突。默认测试镜像为 `busybox:latest`、registry 镜像为 `registry:2`；如果本地没有这些镜像，脚本会尝试 `docker pull`。离线测试机可先预拉镜像并设置 `DM_E2E_OFFLINE=1`，有 `vendor/` 目录时脚本会默认使用 `-mod=vendor` 构建，也可通过 `DM_E2E_GOFLAGS` 显式指定。若测试机没有 Go，可先上传已编译的 `dm` 并设置 `DM_E2E_DM_BIN=/path/to/dm` 跳过构建。脚本会创建并清理临时 registry 容器、测试容器、恢复容器和临时工作目录。执行前请确认当前 Docker 环境适合运行集成测试。

## 命令速查

| 命令 | 功能 |
| --- | --- |
| `dm pull` | 无需 Docker CLI 直接拉取镜像并打包为 tar，可选导入或推送到目标 registry |
| `dm load` | 从目录或单个 tar 文件导入 Docker 镜像 |
| `dm save` | 导出本地 Docker 镜像，支持筛选、合并和 dry-run |
| `dm reverse` | 从运行容器反向生成 `docker run` 或 compose |
| `dm backup container` | 备份容器 inspect、镜像、compose、network 和 volume 元数据 |
| `dm restore` | 从备份目录或离线 tar.gz 包恢复容器 |
| `dm prune-report` | 生成可清理资源报告，可选执行清理 |
| `dm network` | 查看容器网络关系、端口映射和网络风险 |
| `dm health` | 输出 Docker 体检报告 |
| `dm logs-scan` | 扫描容器日志关键词 |
| `dm inspect-diff` | 对比两个容器关键配置差异 |
| `dm image tree` | 分析镜像层、大小和构建历史 |
| `dm volume ls-unused` | 查找未使用或疑似未使用 volume |
| `dm registry-login-check` | 检查 registry 登录配置、凭据和连通性 |
| `dm version` | 输出版本、commit、构建时间和运行平台 |

## 版本信息

```bash
dm version
dm version --format json
```

`build.sh` 和 `build.bat` 会默认注入当前 git commit 和 UTC 构建时间。可通过环境变量覆盖版本号:

```bash
VERSION=v0.1.0 ./build.sh
```

## 镜像拉取和迁移

拉取 Docker Hub 镜像:

```bash
dm pull nginx:latest
dm pull busybox:latest --output-dir pulled
dm pull nginx:1.25 --output ./nginx-1.25.tar
```

指定平台:

```bash
dm pull nginx:latest --os linux --arch arm64
```

代理:

```bash
dm pull nginx:latest --proxy http://127.0.0.1:7890
```

不指定 `--proxy` 时，默认读取 `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY` 等环境变量；未设置则直连。

私有 registry / 内网 registry:

```bash
dm pull harbor.example.com/project/app:v1
dm pull registry.local:5000/team/app:v1 --plain-http
dm pull ghcr.io/org/app:v1 --docker-config /root/.docker/config.json
```

`pull` 支持匿名 registry、Basic challenge、Bearer token challenge，以及 Docker config 中的 `auths`、`credHelpers`、`credsStore`。

拉取后导入 Docker:

```bash
dm pull busybox:latest --load
```

拉取后重新 tag 并推送到目标 registry:

```bash
dm pull busybox:latest --to registry.local:5000
dm pull nginx:1.25 --to registry.local/mirror
dm pull nginx:1.25 --to registry.local/mirror/nginx:stable
```

目标规则:

- `--to registry.local:5000`: 保留源仓库路径，例如 `library/busybox:latest` -> `registry.local:5000/library/busybox:latest`
- `--to registry.local/mirror`: 使用目标 namespace，例如 `library/busybox:latest` -> `registry.local/mirror/busybox:latest`
- `--to registry.local/mirror/app:v2`: 使用完整目标镜像名

执行 `--to` 时，工具会在导入、tag 和 push 前先检查目标 registry 的 `/v2/` 连通性和认证状态；如果 registry 需要登录但本地 Docker config 中没有可用凭据，会提前失败并提示先 `docker login` 或使用 `--docker-config`。内网明文 registry 可配合 `--plain-http`。

## 镜像导入和导出

导入镜像:

```bash
dm load
dm load images
dm load ./busybox.tar
```

`load` 会递归查找 `.tar`、`.tar.gz`、`.tgz` 镜像归档。

导出镜像:

```bash
dm save images
dm save images --filter 'nginx*'
dm save images --filter busybox:latest --dry-run
dm save images --merge
dm save images --all
```

筛选支持镜像名、tag、ID、短 ID，以及 `*`、`?` 通配符。

## 容器反向解析

生成 `docker run`:

```bash
dm reverse my-container
dm reverse my-container --pretty
dm reverse my-container --redact-secrets
```

生成 compose:

```bash
dm reverse my-container --reverse-type compose
dm reverse --running
```

同时输出 `docker run` 和 compose:

```bash
dm reverse my-container --reverse-type all
```

保存输出:

```bash
dm reverse my-container --save
```

谨慎重建容器:

```bash
dm reverse my-container --rerun --dry-run
dm reverse my-container --rerun --confirm
```

`--rerun --confirm` 会停止、删除并重建目标容器，执行前会保存 inspect JSON 备份。

## 离线备份和恢复

备份容器。`backup container` 现在按批量模型工作；单个容器是批量的一种特例。多个目标默认拆成多个独立备份包:

```bash
dm backup container app
dm backup container app ./docker-backups/app
dm backup container "api-*" worker --output-dir ./docker-backups/prod
dm backup container app --dry-run
```

生成离线迁移包。多个目标默认仍然拆成多个独立包；需要整体恢复时使用 `--merge` 合并为一个批量包:

```bash
dm backup container app --bundle
dm backup container app ./backups/app --bundle --output ./app-offline.tar.gz
dm backup container "api-*" worker --bundle --output-dir ./backups/prod
dm backup container "api-*" worker --merge --bundle --output-dir ./backups/prod-merged --output ./prod-offline.tar.gz
```

备份内容包括。单容器包和合并批量包都使用同一种 `manifest.json` 结构，通过 `containers` 数组描述一个或多个容器:

- `manifest.json`
- `container.inspect.json`
- `docker-compose.yml`
- 镜像 tar
- network 元数据
- volume 元数据
- `README.md`
- `restore.sh`
- `checksums.txt`

离线包内的 `README.md` 会记录创建该包的 dm 版本、commit、构建时间、源平台、容器清单、恢复前置条件和 checksum 行为说明。`restore.sh` 会先检查 `dm` 是否可用，打印 `dm version`，提示 Docker daemon 权限和 checksum 校验策略，然后调用 `dm restore`。

恢复:

```bash
dm restore ./docker-backups/app
dm restore ./app-offline.tar.gz
dm restore ./docker-backups/prod/api ./docker-backups/prod/worker
dm restore ./prod-offline.tar.gz
dm restore ./app-offline.tar.gz --replace
dm restore ./app-offline.tar.gz --name app-copy --no-start
dm restore ./app-offline.tar.gz --dry-run
dm restore ./app-offline.tar.gz --skip-checksum
```

如果备份目录或离线包内包含 `checksums.txt`，`restore` 默认会在接触 Docker 前先校验文件完整性。只有在确认需要绕过校验时才使用 `--skip-checksum`。

## 资源清理

仅生成报告:

```bash
dm prune-report
```

执行清理:

```bash
dm prune-report --apply
```

`--apply` 会调用 Docker prune API 删除可清理资源，执行前请确认报告内容。

## 诊断命令

下面这些诊断/报告命令默认输出文本，也支持机器可读 JSON:

```bash
dm health --format json
dm network --format json
dm prune-report --format json
dm logs-scan app --format json
dm inspect-diff app-old app-new --format json
dm image tree nginx:latest --format json
dm volume ls-unused --format json
dm registry-login-check registry.local:5000 --format json
```

网络关系和端口风险:

```bash
dm network
dm network --running
dm network --filter 'image:nginx*'
```

健康检查:

```bash
dm health
dm health --running
dm health --no-logs
dm health --keyword error --keyword timeout --log-tail 200
dm health --redact-secrets
```

日志扫描:

```bash
dm logs-scan app
dm logs-scan --running
dm logs-scan --keyword panic --keyword oom --tail 1000 --context 2
dm logs-scan app --since 30m
dm logs-scan app --redact-secrets
```

容器配置对比:

```bash
dm inspect-diff app-old app-new
dm inspect-diff app-old app-new --redact-secrets
```

该工具默认按管理员视角输出完整信息。需要分享报告或命令片段时，可对 `reverse`、`health`、`logs-scan`、`inspect-diff` 使用 `--redact-secrets`，隐藏 env、label 和日志行中疑似敏感字段。

镜像层分析:

```bash
dm image tree nginx:latest
dm image tree nginx:latest --top 10
dm image tree nginx:latest --no-trunc
```

Volume 分析:

```bash
dm volume ls-unused
dm volume ls-unused --all
dm volume ls-unused --no-trunc
```

Registry 登录检查:

```bash
dm registry-login-check registry.local:5000
dm registry-login-check registry.local:5000 --plain-http
dm registry-login-check registry.local:5000 --docker-config /root/.docker/config.json
```

该命令会检查 Docker config、`auths`、credential helper、registry `/v2/` 连通性和 Docker RegistryLogin 认证结果。

## 常见场景

拉取镜像并迁移到内网 registry:

```bash
dm registry-login-check registry.local:5000 --plain-http
dm pull nginx:latest --to registry.local:5000 --plain-http
```

把容器打包后迁移到另一台机器:

```bash
dm backup container app --bundle --output app-offline.tar.gz
dm restore app-offline.tar.gz --replace
```

审计当前 Docker 状态:

```bash
dm health
dm network
dm prune-report
dm volume ls-unused
```

排查容器差异:

```bash
dm reverse app --reverse-type compose
dm inspect-diff app app-new
dm logs-scan app --since 2h --context 3
```

## 注意事项

- `pull` 直接实现 registry HTTP 拉取流程，不完全等价于 Docker daemon 的所有行为；私有 registry 已支持常见 Basic/Bearer challenge，但复杂企业 SSO 或自定义认证仍建议先测试。
- `pull --to` 会使用本地 Docker daemon 执行 load、tag、push，目标 registry 的 push 权限依赖本机 Docker 登录状态。
- `restore` 会重建 network、volume 和容器。遇到已有容器时默认拒绝覆盖，需要显式使用 `--replace`。
- `reverse --rerun --confirm` 会删除并重建容器，建议先运行 `--dry-run`。
- `prune-report --apply` 会删除 Docker 资源，适合在确认报告后执行。
- 日志和 inspect 可能包含敏感信息，分享输出前请检查 env、label、命令行参数和日志内容。
