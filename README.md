# docker-manager

`docker-manager` 是一个面向 Docker 日常运维、镜像迁移和容器诊断的命令行工具。二进制默认命名为 `dm`。

它覆盖几类常见工作:

- 镜像拉取、导入、导出和重新推送。
- 运行中容器反向生成 `docker run` 或 compose。
- 容器离线备份、迁移包生成和恢复。
- 本机 Docker 资源清理预览、网络/健康/日志/volume/镜像层诊断。
- registry 登录配置和连通性检查。

> 说明: 工具里包含会修改 Docker 状态的命令，例如 `restore`、`prune-report --apply`、`rerun --confirm`、`pull --to`。在生产环境执行前建议先使用 `--dry-run` 或在测试机验证。

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
internal/resourcefilter/        # 容器、镜像、volume 本地资源筛选器
internal/version/               # version 命令和构建版本信息
scripts/                        # 端到端测试等辅助脚本
```

## 全局参数

```bash
--config string   配置文件路径，默认 .dm.yaml
--verbose         输出详细日志
--quiet           静默 info 日志
--log-json        以 JSON 格式输出日志和错误
--json            兼容短写，等同 --log-json
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

资源参数会尽量从本机 Docker 补齐，例如容器、镜像和 volume。已支持的典型位置包括 `backup container`、`reverse`、`inspect-diff`、`logs-scan`、`health`、`network`、`image tree`、`volume ls-unused`，以及 `save --filter` 等筛选参数。

PowerShell 临时加载示例:

```powershell
dm completion powershell | Out-String | Invoke-Expression
```

## 本地资源筛选语法

处理本地 Docker 资源的命令支持统一筛选规则。裸值会匹配资源的常用候选字段，也可以使用 `key:value` 或 `key=value` 指定字段；多个筛选条件之间是或关系，匹配任意一个即选中。筛选值支持 `*` 和 `?` 通配符，也支持大小写不敏感的精确匹配和前缀匹配。建议在 shell 中给带通配符的条件加引号，避免被 shell 提前展开。

容器筛选适用于 `reverse`、`rerun`、`backup container`、`health`、`network`、`logs-scan` 等容器目标或报告命令:

| 字段 | 匹配内容 |
| --- | --- |
| `name` | 容器名，自动忽略 Docker inspect 中的前导 `/` |
| `id` | 容器 ID、短 ID，以及镜像 ID 候选 |
| `image` | 镜像完整引用、仓库名、仓库 basename 和 tag |
| `state` | Docker 容器状态，例如 `running`、`exited` |
| `status` | Docker 状态描述，例如 `Up 2 minutes`、`Exited (0)` |
| `label` | label key、value 或 `key=value` |

```bash
dm reverse --filter 'name:api-*'
dm reverse --running --filter 'image:nginx*'
dm health --filter 'state=running' --filter 'label:env=prod'
dm network --filter 'status:Up*'
dm logs-scan --filter 'image:team/api' --keyword timeout
dm backup container 'name:api-*' --dry-run
dm rerun --filter 'label:dm.managed=true' --dry-run
```

镜像筛选适用于 `save --filter`:

| 字段 | 匹配内容 |
| --- | --- |
| `id` | 镜像 ID 或短 ID |
| `image` | 镜像完整引用、仓库名、仓库 basename、tag 或 digest |
| `repo` | 镜像仓库名，不包含 tag |
| `tag` | 镜像 tag |
| `digest` | repo digest |
| `label` | label key、value 或 `key=value` |

```bash
dm save images --filter 'repo:team/api'
dm save images --filter 'tag:v1'
dm save images --filter 'image:busybox'
dm save images --filter 'digest:*deadbeef'
dm save images --filter 'label:org.opencontainers.image.source=*github*' --dry-run
```

Volume 筛选适用于 `volume ls-unused`:

| 字段 | 匹配内容 |
| --- | --- |
| `name` | volume 名称 |
| `driver` | volume driver，例如 `local`、`nfs` |
| `mountpoint` | volume 在宿主机上的挂载路径 |
| `scope` | volume scope |
| `label` | label key、value 或 `key=value` |
| `option` | driver option key、value 或 `key=value` |

```bash
dm volume ls-unused --filter 'name:app_*'
dm volume ls-unused --filter 'driver:local'
dm volume ls-unused --filter 'mountpoint:*/app_data/*'
dm volume ls-unused --filter 'label:env=dev'
dm volume ls-unused --filter 'option:type=nfs'
```

`prune-report --filter` 使用 Docker prune 语义，和上面的本地资源筛选器不同。它只支持 `label=...`、`label!=...`、`until=...`，并会影响实际 `--apply` 清理范围。

`reverse`、`health`、`network`、`logs-scan` 在不传容器目标和 `--filter` 时默认处理全部本地容器。文本输出会显示本次目标数量提示；JSON/Markdown/HTML 报告会在 `target` 字段或对应小节中保留结构化目标信息。`reverse` 的提示以 `# 目标: ...` 注释形式输出，避免破坏 shell/YAML 内容。

## 端到端集成测试

仓库提供 `scripts/e2e.sh`，用于在有 Docker 的测试机上启动临时 registry，并覆盖 `registry-login-check --plain-http`、`pull --plain-http --output`、`pull --plain-http --load`、`pull --to`、`backup container --bundle` 和 `restore <archive>`。

生产前或远程服务器验收可参考 [docs/REMOTE_TESTING.md](docs/REMOTE_TESTING.md)，其中整理了临时目录、日志留档、代理、测试资源命名、手工验证命令、通过标准和清理步骤。

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
| `dm pull mirror` | 从参数或列表文件批量同步镜像到目标 registry |
| `dm load` | 从目录或单个 tar 文件导入 Docker 镜像 |
| `dm save` | 导出本地 Docker 镜像，支持筛选、合并和 dry-run |
| `dm reverse` | 从运行容器反向生成 `docker run` 或 compose |
| `dm rerun` | 基于 Docker inspect 停止、删除并重建容器 |
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
| `dm doctor` | 检查 Docker、registry、代理、磁盘和测试前置条件 |
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

## 输出格式

全局 `--log-json` 只影响日志和错误输出，适合脚本统一解析命令执行状态；兼容短写 `--json` 等同于 `--log-json`。报告类命令的 `--format json` 输出的是业务报告内容，两者互不替代。

```bash
dm --log-json doctor
dm doctor --format json
```

## 环境诊断

`doctor` 是只读检查命令，用于在生产机或测试机上快速确认运行环境是否适合执行镜像同步、备份恢复和端到端测试。

```bash
dm doctor
dm doctor --format json
dm doctor --registry registry.local:5000 --plain-http
dm doctor --registry registry.local:5000 --docker-config /root/.docker/config.json
dm doctor --output-dir /data/dm-work --min-disk-free-mb 10240
```

检查范围包括 Docker daemon 权限和版本、dm 配置文件、代理环境变量和 `.dm.yaml` 代理、输出目录剩余空间、Docker config、credential helper、registry `/v2/` 连通性和 Docker RegistryLogin 结果，以及 Go/vendor/e2e 测试前置条件。未指定 `--registry` 时会跳过 registry 远端检查。

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

批量镜像同步:

```bash
dm pull mirror busybox:latest nginx:1.25 --to registry.local:5000
dm pull mirror --file images.txt --to registry.local/mirror --concurrency 2 --retries 2
dm pull mirror --file images.txt --to registry.local/mirror --skip-existing --resume
dm pull mirror --file images.txt --to registry.local/mirror --state-file ./mirror-state.json --report ./mirror-report.json
dm pull mirror --file images.txt --to registry.local:5000 --plain-http --docker-config /root/.docker/config.json
```

镜像列表文件一行一个镜像，空行和以 `#` 开头的行会被忽略。`pull mirror` 会复用 `pull --to` 的拉取、digest 校验、导入、tag、push、代理、认证和 registry 预检流程；`--skip-existing` 会在同步前检查目标 manifest，已存在则跳过；`--resume` 会读取状态文件并跳过上次已成功的镜像。`--to` 使用完整镜像名时只适合单个镜像；批量同步请使用 registry 或 namespace 前缀。

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
dm save images --filter 'repo:team/api'
dm save images --filter 'tag:v1' --filter 'label:env=prod'
dm save images --merge
dm save images --all
```

`save --filter` 支持裸镜像名、tag、ID、短 ID，也支持 `id:`、`image:`、`repo:`、`tag:`、`digest:`、`label:` 字段筛选和 `*`、`?` 通配符。

## 容器反向解析

生成 `docker run`:

```bash
dm reverse my-container
dm reverse my-container --pretty
dm reverse my-container --redact-secrets
dm reverse my-container --no-default-envs --no-merge-ports
dm reverse --filter 'name:api-*'
dm reverse --filter 'image:team/api'
```

生成 compose:

```bash
dm reverse my-container --reverse-type compose
dm reverse --running
dm reverse --running --filter 'label:env=prod'
```

同时输出 `docker run` 和 compose:

```bash
dm reverse my-container --reverse-type all
```

保存输出:

```bash
dm reverse my-container --save
```

`reverse` 是只读命令，只输出 `docker run` 或 compose 配置，不会修改 Docker 状态。

## 容器重建

`rerun` 会基于 Docker inspect 的 `Config`、`HostConfig` 和网络配置，通过 Docker API 停止、删除并重建容器。该命令不会执行 `reverse` 生成的 shell 命令。

```bash
dm rerun my-container --dry-run
dm rerun my-container --confirm
dm rerun --filter 'name:app-*' --dry-run
dm rerun --filter 'name:app-*' --confirm
```

`rerun --confirm` 会停止、删除并重建目标容器，执行前会保存 inspect JSON 备份。为避免误操作，`rerun` 必须显式提供容器名称、`--filter` 或 `--running`。

## 离线备份和恢复

备份容器。`backup container` 现在按批量模型工作；单个容器是批量的一种特例。多个目标默认拆成多个独立备份包:

```bash
dm backup container app
dm backup container app ./docker-backups/app
dm backup container app --no-image
dm backup container "api-*" worker --output-dir ./docker-backups/prod
dm backup container "image:team/api" "label:env=prod" --output-dir ./docker-backups/prod
dm backup container app --dry-run
```

`backup container --dry-run` 不写入文件、不导出镜像，但会读取容器 inspect，确认 compose 可生成，检查 network/volume 元数据可读取，并输出将生成的 manifest、inspect、compose、镜像归档、离线包和 checksum 计划。

生成离线迁移包。多个目标默认仍然拆成多个独立包；需要整体恢复时使用 `--merge` 合并为一个批量包:

```bash
dm backup container app --bundle
dm backup container app --bundle --output-dir ./backups/app --bundle-output ./app-offline.tar.gz
dm backup container "api-*" worker --bundle --output-dir ./backups/prod
dm backup container "api-*" worker --merge --bundle --output-dir ./backups/prod-merged --bundle-output ./prod-offline.tar.gz
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

如果备份目录或离线包内包含 `checksums.txt`，`restore` 默认会在接触 Docker 前先校验文件完整性。只有在确认需要绕过校验时才使用 `--skip-checksum`。`restore --dry-run` 会执行 manifest/checksum/inspect/镜像归档/network/volume 元数据预检，并输出将导入的镜像、创建或复用的 network/volume、容器覆盖风险、端口绑定和启动策略；不会 load 镜像、创建资源、删除或启动容器。

## 资源清理

仅生成报告:

```bash
dm prune-report
```

执行清理:

```bash
dm prune-report --apply --confirm
dm prune-report --only container --filter label=dmtest=true --apply --confirm
dm prune-report --only volume --protect-label keep=true --apply --confirm
dm prune-report --filter until=168h --apply --confirm
```

`--apply` 会调用 Docker prune API 删除可清理资源，必须同时指定 `--confirm`。可用 `--only container|image|volume|build-cache` 限制资源类型，用 `--filter label=...`、`--filter label!=...`、`--filter until=...` 或 `--until` 收窄范围，用 `--protect-label` 保护带指定 label 的资源。使用 label 或 protect-label 范围时不会清理 build cache，因为 Docker 的 build cache 缺少可与报告一致核对的 label 元数据。执行前请确认报告内容。

## 诊断命令

下面这些诊断/报告命令默认输出文本，也支持 `json`、`markdown` 和 `html`。JSON 适合脚本和 CI 消费，Markdown/HTML 适合巡检留档、工单附件或分享给团队:

```bash
dm health --format json
dm network --format json
dm prune-report --format json
dm logs-scan app --format json
dm inspect-diff app-old app-new --format json
dm image tree nginx:latest --format json
dm volume ls-unused --format json
dm registry-login-check registry.local:5000 --format json
dm doctor --format json
```

生成可归档报告:

```bash
dm health --format markdown > health-report.md
dm network --format html > network-report.html
dm prune-report --format markdown > prune-report.md
dm volume ls-unused --format html > volume-report.html
dm image tree nginx:latest --format markdown > image-tree.md
```

网络关系和端口风险:

```bash
dm network
dm network --running
dm network --filter 'image:nginx*'
dm network --filter 'label:env=prod'
dm network --filter 'status:Up*'
```

健康检查:

```bash
dm health
dm health --running
dm health --filter 'name:api-*'
dm health --filter 'state=running'
dm health --no-logs
dm health --keyword error --keyword timeout --log-tail 200
dm health --redact-secrets
```

日志扫描:

```bash
dm logs-scan app
dm logs-scan --running
dm logs-scan --filter 'image:team/api'
dm logs-scan --filter 'label:env=prod'
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
dm volume ls-unused --filter 'name:app_*'
dm volume ls-unused --filter 'driver:local'
dm volume ls-unused --filter 'label:env=dev'
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
dm backup container app --bundle --bundle-output app-offline.tar.gz
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
- `rerun --confirm` 会删除并重建容器，建议先运行 `--dry-run`。
- `prune-report --apply` 会删除 Docker 资源，适合在确认报告后执行。
- 日志和 inspect 可能包含敏感信息，分享输出前请检查 env、label、命令行参数和日志内容。
