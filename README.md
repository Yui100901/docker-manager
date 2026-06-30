# docker-manager

`docker-manager` 是一个面向 Docker 日常运维、镜像迁移和容器诊断的命令行工具。二进制默认命名为 `dm`。

它覆盖几类常见工作:

- 镜像拉取、导入、导出和重新推送。
- 运行中容器反向生成 `docker run` 或 compose。
- 容器离线备份、迁移包生成和恢复。
- 本机 Docker 资源清理预览、网络/健康/日志/volume/镜像层诊断。
- registry 登录配置和连通性检查。

> 说明: 工具里包含会修改 Docker 状态的命令，例如 `restore`、`report prune --apply`、`rerun --confirm`、`image pull --to`。在生产环境执行前建议先使用 `--dry-run` 或在测试机验证。

## 构建和安装

开发环境快速构建当前平台二进制:

```bash
bash scripts/dev-build.sh
bash scripts/dev-build.sh --vet --race
```

Windows PowerShell:

```powershell
.\scripts\dev-build.ps1
.\scripts\dev-build.ps1 -Vet
```

本地静态检查:

```bash
bash scripts/check.sh
bash scripts/check.sh --race
```

Windows PowerShell:

```powershell
.\scripts\check.ps1
.\scripts\check.ps1 -Race
```

生成发布归档、checksum 和版本清单:

```bash
VERSION=v0.1.0 bash scripts/package-release.sh
bash scripts/package-release.sh --version v0.1.0 --platform linux/amd64 --platform windows/amd64
```

Windows PowerShell:

```powershell
.\scripts\package-release.ps1 -Version v0.1.0
.\scripts\package-release.ps1 -Version v0.1.0 -Platform linux/amd64,windows/amd64
```

产物默认写入 `dist/`，包括按平台命名的 `tar.gz`/`zip`、`checksums.txt`、`release-manifest.json` 和 `release-summary.md`。每个归档内包含 `INSTALL.md`，记录对应平台的安装和验证命令。发布前可先查看 [CHANGELOG.md](CHANGELOG.md) 了解变更摘要，再按 [docs/RELEASE_CHECKLIST.md](docs/RELEASE_CHECKLIST.md) 逐项检查。

### 发布验收摘要

当前发布候选版本已完成以下核心验收:

- 本地静态和单元检查: `go test ./...`、`go vet ./...`、`go test -race ./...`、`scripts/check.ps1`、`git diff --check`。
- 干净 Ubuntu 24.04 Docker VM 全量验收: `scripts/e2e.sh --mode destructive`，48 PASS / 12 XFAIL / 0 FAIL。
- 干净 Ubuntu 24.04 安装验收: `scripts/e2e.sh --mode install`，14 PASS / 5 XFAIL / 0 FAIL。
- 企业 registry 模拟和 Harbor 验收核心结论已合并到 [CHANGELOG.md](CHANGELOG.md)。
- 发布包打包验证: 5 个平台归档、checksum、manifest 和 summary 均可生成。

历史详细测试报告已经清理，避免仓库长期积累一次性报告和内网环境信息。后续发布验证请把稳定结论写入 `CHANGELOG.md`，需要复测步骤时参考 [docs/REMOTE_TESTING.md](docs/REMOTE_TESTING.md)。

Linux 安装。安装脚本会安装真实二进制为 `dm-bin`，并生成 `dm` 包装入口；包装入口默认读取 `DM_CONFIG`，未设置时使用安装脚本生成的配置文件:

```bash
sudo bash scripts/install.sh --binary ./bin/dev/dm
sudo bash scripts/install.sh --install-dir /opt/docker-manager --binary ./bin/dev/dm
sudo bash scripts/install.sh --build
sudo bash scripts/install.sh --binary ./bin/dev/dm --completion bash --completion zsh --completion fish
sudo bash scripts/install.sh --binary ./bin/dev/dm --no-completion
```

默认安装位置:

| 场景 | 二进制入口 | 配置 | 数据目录 |
| --- | --- | --- | --- |
| root | `/usr/local/bin/dm` | `/etc/docker-manager/dm.yaml` | `/var/lib/docker-manager` |
| 普通用户 | `~/.local/bin/dm` | `~/.config/docker-manager/dm.yaml` | `~/.local/share/docker-manager` |

安装脚本会设置 `DM_CONFIG`、`DM_HOME`、`DM_OUTPUT_DIR`，并将 bin 目录加入 shell profile。卸载:

```bash
sudo bash scripts/uninstall.sh
sudo bash scripts/uninstall.sh --purge
```

Windows 安装:

```powershell
.\scripts\install.ps1 -Binary .\bin\dev\dm.exe
.\scripts\install.ps1 -InstallDir C:\Tools\docker-manager -Binary .\bin\dev\dm.exe
.\scripts\install.ps1 -Build
.\scripts\install.ps1 -Binary .\bin\dev\dm.exe -Completion PowerShell
.\scripts\install.ps1 -Binary .\bin\dev\dm.exe -NoCompletion
```

Windows 安装脚本会生成 `dm.cmd` 包装入口，设置用户级 `DM_CONFIG`、`DM_HOME`、`DM_OUTPUT_DIR`，并把安装 bin 目录加入用户 `PATH`。卸载:

```powershell
.\scripts\uninstall.ps1
.\scripts\uninstall.ps1 -Purge
```

查看帮助:

```bash
dm --help
dm <command> --help
dm version
```

## 项目结构

```text
CHANGELOG.md                    # 发布变更摘要和验收结论
docs/RELEASE_CHECKLIST.md        # 发布前检查清单
docs/REMOTE_TESTING.md           # 远程 Docker 主机验收手册
main.go                         # 程序入口，只负责调用 internal/cli
internal/cli/                   # 根命令、配置读取、日志和全局错误输出
internal/commands/images/       # load/save 镜像导入导出命令
internal/commands/pull/         # pull 镜像拉取、导入和重新推送命令
internal/commands/reverse/      # reverse 容器 inspect 到 docker run/compose 的逆向解析命令
internal/commands/backup/       # backup/restore 容器备份、迁移包和恢复命令
internal/commands/diagnostics/  # report、registry、volume、image tree 等诊断命令
internal/completion/            # shell 补全命令和本地 Docker 资源补全
internal/docker/                # Docker API client 和镜像/容器管理封装
internal/report/                # text/json/markdown/html 报告输出格式
internal/resourcefilter/        # 容器、镜像、volume 本地资源筛选器
internal/registryauth/          # Docker config、auths 和 credential helper 解析
internal/textfmt/               # 字节大小、速率等文本格式化
internal/version/               # version 命令和构建版本信息
scripts/                        # 构建、发布、安装、卸载、检查和端到端测试脚本
```

## 文档索引

| 文档 | 用途 |
| --- | --- |
| [CHANGELOG.md](CHANGELOG.md) | 查看版本变更、修复、验收结果和已知非阻断项 |
| [docs/RELEASE_CHECKLIST.md](docs/RELEASE_CHECKLIST.md) | 发布前逐项核对构建、安装、测试、安全边界和回滚 |
| [docs/REMOTE_TESTING.md](docs/REMOTE_TESTING.md) | 在干净 Docker 主机或远程服务器上执行 full/destructive 验收 |

不再保留按日期堆叠的旧测试报告。需要留档时，优先把最终结论补充到 changelog；只在排查问题期间临时保存详细日志。

## 架构边界

`docker-manager` 的代码按“命令入口、业务编排、Docker/API 适配、输出渲染、共享工具”分层:

- `internal/cli` 负责根命令、全局配置、日志和统一错误输出，不直接承载具体 Docker 业务。
- `internal/commands/*` 按命令语义组织业务编排。命令包可以解析参数、组织 dry-run/confirm 流程、调用 Docker 适配层和报告渲染层，但应避免把通用筛选、格式化或 Docker client 初始化散落在命令实现里。
- `internal/docker` 是 Docker API 访问封装。涉及 daemon、image、container、push/load/save 等可复用操作时，优先放在这里或通过命令包内的窄接口隔离，方便测试替换。
- `internal/report` 只负责 `text/json/markdown/html` 输出格式，不负责采集 Docker 数据。
- `internal/resourcefilter` 统一处理容器、镜像和 volume 的本地资源筛选。新增本地资源命令时优先复用这里的候选字段、通配符和 keyed filter 行为。
- `internal/completion` 负责 shell completion 和本地 Docker 资源候选，不应依赖具体命令的执行副作用。
- `internal/textfmt` 放通用命令行文本格式化，例如字节大小和下载速率。
- `scripts` 放开发构建、发布打包、安装卸载、静态检查和端到端测试脚本。发布脚本不应依赖开发机上的临时目录或未提交文件。

## 维护约定

- 文件拆分: 单个命令文件只保留一个明确职责。CLI 构建、runner/业务编排、Docker service、归档/checksum、报告渲染、类型定义和测试辅助应尽量分开。
- 注释: 导出类型、复杂安全边界、认证流程、批量/恢复/清理这类容易误改的流程需要说明意图；简单 getter、字段赋值和样板代码不需要灌水式注释。
- 错误信息: 面向用户的命令错误和日志使用中文；底层协议、Docker API 或外部命令名称保持原文。错误要包含目标资源和下一步提示，脚本场景依靠非零退出码判断失败。
- 输出: 报告类命令走 `internal/report`；进度输出只写到命令指定的 progress writer；大小、速率等格式化走 `internal/textfmt`。
- 筛选: 处理本地容器、镜像或 volume 的命令默认处理全部资源，并提供 `--filter` 精确收窄；容器类命令保留 `--running` 作为常用筛选。
- dry-run 和 confirm: 只读报告默认不修改 Docker；会删除、替换、push 或恢复资源的行为必须有清晰的 dry-run 或 confirm 边界。已有确认边界不要在新增选项时绕过。
- 测试: 行为改动至少跑 `go test ./...` 和 `go vet ./...`；结构性重构、并发下载、批量执行或破坏性命令改动建议跑 `scripts/check.* -Race` 和相关 e2e。

## 全局参数

```bash
--config string   配置文件路径，默认 .dm.yaml
--verbose         输出详细日志
--quiet           静默 info 日志
--log-json        以 JSON 格式输出日志和错误
```

未显式传 `--config` 时，二进制会优先读取 `DM_CONFIG` 环境变量；未设置时再使用当前目录下的 `.dm.yaml`。

示例 `.dm.yaml`:

```yaml
proxy: http://127.0.0.1:7890
# ca_file: /etc/ssl/certs/company-ca.pem
# registry_ca_file: /etc/docker/certs.d/registry.local:5000/ca.crt
os: linux
arch: amd64
output_dir: images
verbose: false
quiet: false
log_json: false
```

## Shell 自动补全

生成补全脚本:

```bash
dm completion bash
dm completion zsh
dm completion fish
dm completion powershell
```

资源参数会尽量从本机 Docker 补齐，例如容器、镜像和 volume。已支持的典型位置包括 `backup`、`reverse`、`report diff`、`report logs`、`report health`、`report network`、`image tree`、`report volumes`，以及 `image save --filter` 等筛选参数。

安装脚本默认安装补全脚本。Linux/macOS 的 `scripts/install.sh` 默认生成 bash 补全；如果传入 `--completion`，则按传入列表生成，例如 `--completion bash --completion zsh --completion fish`；使用 `--no-completion` 可关闭。Windows 的 `scripts/install.ps1` 默认生成 PowerShell 补全并写入可卸载的 profile 片段，使用 `-NoCompletion` 可关闭。

PowerShell 临时加载示例:

```powershell
dm completion powershell | Out-String | Invoke-Expression
```

## 本地资源筛选语法

处理本地 Docker 资源的命令支持统一筛选规则。裸值会匹配资源的常用候选字段，也可以使用 `key:value` 或 `key=value` 指定字段；多个筛选条件之间是或关系，匹配任意一个即选中。筛选值支持 `*` 和 `?` 通配符，也支持大小写不敏感的精确匹配和前缀匹配。建议在 shell 中给带通配符的条件加引号，避免被 shell 提前展开。

容器筛选适用于 `reverse`、`rerun`、`backup`、`report health`、`report network`、`report logs` 等容器目标或报告命令:

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
dm report health --filter 'state=running' --filter 'label:env=prod'
dm report network --filter 'status:Up*'
dm report logs --filter 'image:team/api' --keyword timeout
dm backup 'name:api-*' --dry-run
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
dm image save images --filter 'repo:team/api'
dm image save images --filter 'tag:v1'
dm image save images --filter 'image:busybox'
dm image save images --filter 'digest:*deadbeef'
dm image save images --filter 'label:org.opencontainers.image.source=*github*' --dry-run
```

Volume 筛选适用于 `report volumes`:

| 字段 | 匹配内容 |
| --- | --- |
| `name` | volume 名称 |
| `driver` | volume driver，例如 `local`、`nfs` |
| `mountpoint` | volume 在宿主机上的挂载路径 |
| `scope` | volume scope |
| `label` | label key、value 或 `key=value` |
| `option` | driver option key、value 或 `key=value` |

```bash
dm report volumes --filter 'name:app_*'
dm report volumes --filter 'driver:local'
dm report volumes --filter 'mountpoint:*/app_data/*'
dm report volumes --filter 'label:env=dev'
dm report volumes --filter 'option:type=nfs'
```

`report prune --filter` 使用 Docker prune 语义，和上面的本地资源筛选器不同。它只支持 `label=...`、`label!=...`、`until=...`，并会影响实际 `--apply` 清理范围。

`reverse`、`report health`、`report network`、`report logs` 在不传容器目标和 `--filter` 时默认处理全部本地容器。文本输出会显示本次目标数量提示；JSON/Markdown/HTML 报告会在 `target` 字段或对应小节中保留结构化目标信息。`reverse` 的提示以 `# 目标: ...` 注释形式输出，避免破坏 shell/YAML 内容。

## 端到端集成测试

仓库提供 `scripts/e2e.sh`，用于在有 Docker 的测试机上启动临时 registry，并覆盖 `report registry --plain-http`、`image pull --plain-http --output`、`image pull --plain-http --load`、`image pull --to`、`backup --bundle` 和 `restore <archive>`。

生产前或远程服务器验收可参考 [docs/REMOTE_TESTING.md](docs/REMOTE_TESTING.md)，其中整理了临时目录、日志留档、代理、测试资源命名、手工验证命令、通过标准和清理步骤。

当前机器没有 bash 或 Docker 时，可先运行 Windows 本地 smoke:

```powershell
.\scripts\local-test.ps1
.\scripts\local-test.ps1 -OutputDir .\dist\local-test
```

该脚本会运行 Go 静态测试、race 测试、帮助输出、completion 生成、`DM_CONFIG`、旧入口拒绝、PowerShell 安装/卸载和 Docker 不可用时的错误路径，并生成 `local-test-report.md` 和 `results.tsv`。

```bash
bash scripts/e2e.sh
bash scripts/e2e.sh --mode smoke
bash scripts/e2e.sh --mode install
bash scripts/e2e.sh --mode full
bash scripts/e2e.sh --mode destructive
```

可通过环境变量调整测试参数:

```bash
DM_E2E_IMAGE=busybox:latest bash scripts/e2e.sh
DM_E2E_MODE=smoke bash scripts/e2e.sh
DM_E2E_GOFLAGS=-mod=vendor bash scripts/e2e.sh
DM_E2E_DM_BIN=/root/dm bash scripts/e2e.sh
DM_E2E_OFFLINE=1 bash scripts/e2e.sh
DM_E2E_KEEP_WORKDIR=1 bash scripts/e2e.sh
```

`smoke` 模式不依赖 Docker，只验证构建、帮助、版本、配置加载和基础 `doctor`；`install` 模式会安装到临时目录并卸载；`full`/`destructive` 会使用 Docker 随机绑定本地 registry 端口，覆盖完整功能和破坏性命令安全边界。默认测试镜像为 `busybox:latest`、registry 镜像为 `registry:2`；如果本地没有这些镜像，脚本会尝试 `docker pull`。离线测试机可先预拉镜像并设置 `DM_E2E_OFFLINE=1`，有 `vendor/` 目录时脚本会默认使用 `-mod=vendor` 构建，也可通过 `DM_E2E_GOFLAGS` 显式指定。若测试机没有 Go，可先上传已编译的 `dm` 并设置 `DM_E2E_DM_BIN=/path/to/dm` 跳过构建。脚本会创建并清理临时 registry 容器、测试容器、恢复容器和临时工作目录。执行前请确认当前 Docker 环境适合运行集成测试。

## 命令速查

| 命令 | 功能 |
| --- | --- |
| `dm pull` / `dm image pull` | 无需 Docker CLI 直接拉取镜像并打包为 tar，可选导入或推送到目标 registry |
| `dm pull --file` / `dm image pull --file` | 从参数或列表文件批量拉取镜像，可选同步到目标 registry |
| `dm load` / `dm image load` | 从目录或单个 tar 文件导入 Docker 镜像 |
| `dm save` / `dm image save` | 导出本地 Docker 镜像，支持筛选、合并和 dry-run |
| `dm reverse` | 从运行容器反向生成 `docker run` 或 compose |
| `dm rerun` | 基于 Docker inspect 停止、删除并重建容器 |
| `dm backup` | 备份容器 inspect、镜像、compose、network 和 volume 元数据 |
| `dm restore` | 从备份目录或离线 tar.gz 包恢复容器 |
| `dm prune` / `dm report prune` | 生成可清理资源报告，可选执行清理 |
| `dm network` / `dm report network` | 查看容器网络关系、端口映射和网络风险 |
| `dm health` / `dm report health` | 输出 Docker 体检报告 |
| `dm logs` / `dm report logs` | 扫描容器日志关键词 |
| `dm diff` / `dm report diff` | 对比两个容器关键配置差异 |
| `dm tree` / `dm image tree` | 分析镜像层、大小和构建历史 |
| `dm volumes` / `dm report volumes` | 查找未使用或疑似未使用 volume |
| `dm registry` / `dm report registry` | 检查 registry 登录配置、凭据和连通性 |
| `dm doctor` | 检查 Docker、registry、代理、磁盘和测试前置条件 |
| `dm version` | 输出版本、commit、构建时间和运行平台 |

`image` 和 `report` 命名空间仍作为语义分组保留；其叶子命令同时提供二级入口，适合日常交互和脚本简写。

## 版本信息

```bash
dm version
dm version --format json
```

`scripts/dev-build.*` 和 `scripts/package-release.*` 会默认注入当前 git commit 和 UTC 构建时间。可通过环境变量或参数覆盖版本号:

```bash
VERSION=v0.1.0 bash scripts/dev-build.sh
bash scripts/package-release.sh --version v0.1.0
```

## 输出格式

全局 `--log-json` 只影响日志和错误输出，适合脚本统一解析命令执行状态。报告类命令的 `--format json` 输出的是业务报告内容，两者互不替代。

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

检查范围包括 Docker daemon 权限和版本、dm 配置文件、代理环境变量和 `.dm.yaml` 代理格式、私有 CA 路径、Docker daemon registry 配置、输出目录剩余空间/剩余 inode/写入探测、Docker config、credential helper、registry `/v2/` 连通性和 Docker RegistryLogin 结果，以及 Go/vendor/e2e 测试前置条件。未指定 `--registry` 时会跳过 registry 远端检查。

## 镜像拉取和迁移

拉取 Docker Hub 镜像:

```bash
dm image pull nginx:latest
dm image pull busybox:latest --output-dir pulled
dm image pull nginx:1.25 --output ./nginx-1.25.tar
```

指定平台:

```bash
dm image pull nginx:latest --os linux --arch arm64
```

代理:

```bash
dm image pull nginx:latest --proxy http://127.0.0.1:7890
```

不指定 `--proxy` 时，默认读取 `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY` 等环境变量；未设置则直连。`--timeout` 控制连接、TLS 握手和响应头超时，默认 30 秒；镜像层 body 下载不设置总时限，慢网络下可用 Ctrl+C 取消。

私有 registry / 内网 registry:

```bash
dm image pull harbor.example.com/project/app:v1
dm image pull registry.local:5000/team/app:v1 --plain-http
dm image pull ghcr.io/org/app:v1 --docker-config /root/.docker/config.json
```

`image pull` 支持匿名 registry、Basic challenge、Bearer token challenge，以及 Docker config 中的 `auths`、`credHelpers`、`credsStore`。

拉取后导入 Docker:

```bash
dm image pull busybox:latest --load
```

拉取后重新 tag 并推送到目标 registry:

```bash
dm image pull busybox:latest --to registry.local:5000
dm image pull nginx:1.25 --to registry.local/mirror
dm image pull nginx:1.25 --to registry.local/mirror/nginx:stable
```

目标规则:

- `--to registry.local:5000`: 保留源仓库路径，例如 `library/busybox:latest` -> `registry.local:5000/library/busybox:latest`
- `--to registry.local/mirror`: 使用目标 namespace，例如 `library/busybox:latest` -> `registry.local/mirror/busybox:latest`
- `--to registry.local/mirror/app:v2`: 使用完整目标镜像名

执行 `--to` 时，工具会在导入、tag 和 push 前先检查目标 registry 的 `/v2/` 连通性和认证状态；如果 registry 需要登录但本地 Docker config 中没有可用凭据，会提前失败并提示先 `docker login` 或使用 `--docker-config`。内网明文 registry 可配合 `--plain-http`。

批量镜像同步:

```bash
dm image pull busybox:latest nginx:1.25 --to registry.local:5000
dm image pull --file images.txt --to registry.local/mirror --concurrency 2 --retries 2
dm image pull --file images.txt --to registry.local/mirror --skip-existing --resume
dm image pull --file images.txt --to registry.local/mirror --state-file ./pull-state.json --report ./pull-report.json
dm image pull --file images.txt --to registry.local:5000 --plain-http --docker-config /root/.docker/config.json
```

镜像列表文件一行一个镜像，空行和以 `#` 开头的行会被忽略。`image pull` 在传入多个镜像或 `--file` 时进入批量模式，可直接批量拉取 tar、配合 `--load` 批量导入，或配合 `--to` 批量同步到目标 registry。`--skip-existing` 会在同步前检查目标 manifest，已存在则跳过；`--resume` 会读取状态文件并跳过上次已成功的镜像。`--to` 使用完整镜像名时只适合单个镜像；批量同步请使用 registry 或 namespace 前缀。

## 镜像导入和导出

导入镜像:

```bash
dm image load
dm image load images
dm image load ./busybox.tar
```

`image load` 会递归查找 `.tar`、`.tar.gz`、`.tgz` 镜像归档。

导出镜像:

```bash
dm image save images
dm image save images --filter 'nginx*'
dm image save images --filter busybox:latest --dry-run
dm image save images --filter 'repo:team/api'
dm image save images --filter 'tag:v1' --filter 'label:env=prod'
dm image save images --merge
dm image save images --all
```

`image save --filter` 支持裸镜像名、tag、ID、短 ID，也支持 `id:`、`image:`、`repo:`、`tag:`、`digest:`、`label:` 字段筛选和 `*`、`?` 通配符。

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

备份容器。`backup` 按批量模型工作；单个容器是批量的一种特例。多个目标默认拆成多个独立备份包:

```bash
dm backup app
dm backup app --output-dir ./docker-backups/app
dm backup app --no-image
dm backup "api-*" worker --output-dir ./docker-backups/prod
dm backup "image:team/api" "label:env=prod" --output-dir ./docker-backups/prod
dm backup app --dry-run
```

`backup --dry-run` 不写入文件、不导出镜像，但会读取容器 inspect，确认 compose 可生成，检查 network/volume 元数据可读取，并输出将生成的 manifest、inspect、compose、镜像归档、离线包和 checksum 计划。

生成离线迁移包。多个目标默认仍然拆成多个独立包；需要整体恢复时使用 `--merge` 合并为一个批量包:

```bash
dm backup app --bundle
dm backup app --bundle --output-dir ./backups/app --bundle-output ./app-offline.tar.gz
dm backup "api-*" worker --bundle --output-dir ./backups/prod
dm backup "api-*" worker --merge --bundle --output-dir ./backups/prod-merged --bundle-output ./prod-offline.tar.gz
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
dm report prune
```

执行清理:

```bash
dm report prune --apply --confirm
dm report prune --only container --filter label=dmtest=true --apply --confirm
dm report prune --only volume --protect-label keep=true --apply --confirm
dm report prune --filter until=168h --apply --confirm
```

`--apply` 会调用 Docker prune API 删除可清理资源，必须同时指定 `--confirm`。可用 `--only container|image|volume|build-cache` 限制资源类型，用 `--filter label=...`、`--filter label!=...`、`--filter until=...` 或 `--until` 收窄范围，用 `--protect-label` 保护带指定 label 的资源。使用 label 或 protect-label 范围时不会清理 build cache，因为 Docker 的 build cache 缺少可与报告一致核对的 label 元数据。执行前请确认报告内容。

## 诊断命令

下面这些诊断/报告命令默认输出文本，也支持 `json`、`markdown` 和 `html`。JSON 适合脚本和 CI 消费，Markdown/HTML 适合巡检留档、工单附件或分享给团队:

```bash
dm report health --format json
dm report network --format json
dm report prune --format json
dm report logs app --format json
dm report diff app-old app-new --format json
dm image tree nginx:latest --format json
dm report volumes --format json
dm report registry registry.local:5000 --format json
dm doctor --format json
```

生成可归档报告:

```bash
dm report health --format markdown > health-report.md
dm report network --format html > network-report.html
dm report prune --format markdown > prune-report.md
dm report volumes --format html > volume-report.html
dm image tree nginx:latest --format markdown > image-tree.md
```

网络关系和端口风险:

```bash
dm report network
dm report network --running
dm report network --filter 'image:nginx*'
dm report network --filter 'label:env=prod'
dm report network --filter 'status:Up*'
```

健康检查:

```bash
dm report health
dm report health --running
dm report health --filter 'name:api-*'
dm report health --filter 'state=running'
dm report health --no-logs
dm report health --keyword error --keyword timeout --log-tail 200
dm report health --redact-secrets
```

日志扫描:

```bash
dm report logs app
dm report logs --running
dm report logs --filter 'image:team/api'
dm report logs --filter 'label:env=prod'
dm report logs --keyword panic --keyword oom --tail 1000 --context 2
dm report logs app --since 30m
dm report logs app --redact-secrets
```

容器配置对比:

```bash
dm report diff app-old app-new
dm report diff app-old app-new --redact-secrets
```

该工具默认按管理员视角输出完整信息。需要分享报告或命令片段时，可对 `reverse`、`report health`、`report logs`、`report diff` 使用 `--redact-secrets`，隐藏 env、label 和日志行中疑似敏感字段。

镜像层分析:

```bash
dm image tree nginx:latest
dm image tree nginx:latest --top 10
dm image tree nginx:latest --no-trunc
```

Volume 分析:

```bash
dm report volumes
dm report volumes --all
dm report volumes --no-trunc
dm report volumes --filter 'name:app_*'
dm report volumes --filter 'driver:local'
dm report volumes --filter 'label:env=dev'
```

Registry 登录检查:

```bash
dm report registry registry.local:5000
dm report registry registry.local:5000 --plain-http
dm report registry registry.local:5000 --docker-config /root/.docker/config.json
```

该命令会检查 Docker config、`auths`、credential helper、registry `/v2/` 连通性和 Docker RegistryLogin 认证结果。

## 常见场景

拉取镜像并迁移到内网 registry:

```bash
dm report registry registry.local:5000 --plain-http
dm image pull nginx:latest --to registry.local:5000 --plain-http
```

把容器打包后迁移到另一台机器:

```bash
dm backup app --bundle --bundle-output app-offline.tar.gz
dm restore app-offline.tar.gz --replace
```

审计当前 Docker 状态:

```bash
dm report health
dm report network
dm report prune
dm report volumes
```

排查容器差异:

```bash
dm reverse app --reverse-type compose
dm report diff app app-new
dm report logs app --since 2h --context 3
```

## 注意事项

- `image pull` 直接实现 registry HTTP 拉取流程，不完全等价于 Docker daemon 的所有行为；私有 registry 已支持常见 Basic/Bearer challenge，但复杂企业 SSO 或自定义认证仍建议先测试。
- `image pull --to` 会使用本地 Docker daemon 执行 load、tag、push，目标 registry 的 push 权限依赖本机 Docker 登录状态。
- `restore` 会重建 network、volume 和容器。遇到已有容器时默认拒绝覆盖，需要显式使用 `--replace`。
- `rerun --confirm` 会删除并重建容器，建议先运行 `--dry-run`。
- `report prune --apply` 会删除 Docker 资源，适合在确认报告后执行。
- 日志和 inspect 可能包含敏感信息，分享输出前请检查 env、label、命令行参数和日志内容。
