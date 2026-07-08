# docker-manager

`docker-manager` 是一个面向 Docker 日常运维、镜像迁移、容器备份恢复和诊断报告的命令行工具，二进制默认名为 `dm`。

它补充 Docker 原生命令在批量镜像迁移、离线迁移包、容器配置逆向、资源关联报告和企业 registry 检查上的使用体验。工具包含会修改 Docker 状态的命令，例如 `restore`、`rerun --confirm`、`prune --apply --confirm`、`pull --to`；生产环境执行前建议先使用 `--dry-run` 或在非生产环境确认目标范围。

## 主要功能

- 镜像拉取、归档、导入和重新推送: `dm pull`、`dm save`、`dm load`、`dm tree`。
- 容器逆向和重建: `dm reverse` 只读输出 `docker run` 或 compose，`dm rerun` 显式确认后重建容器。
- 容器离线迁移: `dm backup` 和 `dm restore` 支持批量包、合并包、checksum、恢复前计划预览、加密包、分卷包、README 和 restore 脚本。
- 诊断报告: `dm health`、`dm network`、`dm logs`、`dm diff`、`dm prune`、`dm volumes`、`dm registry`、`dm doctor`。
- 远程 Docker 管理: 支持 Docker 标准环境变量、`.dm.yaml` 和全局参数指定 Docker endpoint。
- Shell completion: 支持 bash、zsh、fish 和 PowerShell，容器/镜像/volume 候选会按当前 Docker endpoint 查询。

## 构建

开发构建当前平台二进制:

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

产物默认写入 `dist/`，包括按平台命名的 `tar.gz`/`zip`、`checksums.txt`、`release-manifest.json` 和 `release-summary.md`。归档内包含目标平台对应的安装和卸载脚本，以及 `INSTALL.md`。

## 安装

Linux/macOS:

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

卸载:

```bash
sudo bash scripts/uninstall.sh
sudo bash scripts/uninstall.sh --purge
```

Windows:

```powershell
.\scripts\install.ps1 -Binary .\bin\dev\dm.exe
.\scripts\install.ps1 -InstallDir C:\Tools\docker-manager -Binary .\bin\dev\dm.exe
.\scripts\install.ps1 -Build
.\scripts\install.ps1 -Binary .\bin\dev\dm.exe -Completion PowerShell
.\scripts\install.ps1 -Binary .\bin\dev\dm.exe -NoCompletion
```

Windows 安装脚本会把真实二进制安装为 `<InstallDir>\bin\dm.exe`，设置用户级 `DM_CONFIG`、`DM_HOME`、`DM_OUTPUT_DIR`，并将 bin 目录加入用户 `PATH`。

卸载:

```powershell
.\scripts\uninstall.ps1
.\scripts\uninstall.ps1 -Purge
```

## 全局参数和配置

```bash
--config string               配置文件路径，默认 .dm.yaml；未显式传入时优先读取 DM_CONFIG
--docker-host string          Docker daemon 地址，默认读取 DOCKER_HOST 或本地 Docker
--docker-tls-verify           启用 Docker TCP TLS 证书校验，默认读取 DOCKER_TLS_VERIFY
--docker-cert-path string     Docker TLS 证书目录，默认读取 DOCKER_CERT_PATH
--docker-api-version string   Docker API 版本，默认读取 DOCKER_API_VERSION 或自动协商
--verbose                     输出详细日志
--quiet                       隐藏信息日志
--log-json                    以 JSON 输出日志和错误，不影响业务报告格式
```

示例 `.dm.yaml`:

```yaml
proxy: http://127.0.0.1:7890
docker_host: tcp://docker.example.com:2376
docker_tls_verify: true
docker_cert_path: /etc/docker-manager/docker-certs
docker_api_version: "1.46"
os: linux
arch: amd64
output_dir: images
verbose: false
quiet: false
log_json: false
```

Docker API endpoint 优先级为: 全局命令行参数 > `.dm.yaml` > Docker 环境变量 > 本地 Docker 默认 endpoint。生产环境不建议裸露未启用 TLS 的 `tcp://host:2375`；`dm doctor` 会对明文 TCP endpoint 给出 warning。

## Shell 自动补全

生成补全脚本:

```bash
dm completion bash
dm completion zsh
dm completion fish
dm completion powershell
```

安装脚本默认安装对应 shell completion。Linux/macOS 默认生成 bash 补全，可通过 `--completion` 指定多个 shell；Windows 默认生成 PowerShell completion 并写入可卸载的 profile 片段。

补全会读取当前 Docker endpoint 配置，容器、镜像和 volume 候选可以来自远程 Docker。

## 命令速查

| 命令 | 功能 |
| --- | --- |
| `dm pull` / `dm image pull` | 从 registry 拉取镜像，支持未压缩、gzip、zstd 镜像层归档、导入 Docker、批量同步和重新推送 |
| `dm save` / `dm image save` | 导出本地镜像，支持筛选、通配符、dry-run 和批量导出 |
| `dm load` / `dm image load` | 导入镜像 tar/tar.gz/tgz，默认递归扫描目录 |
| `dm tree` / `dm image tree` | 分析镜像层、历史、大小占比和本地容器引用 |
| `dm reverse` | 从容器 inspect 生成 `docker run` 或 compose，只读输出 |
| `dm rerun` | 基于 inspect 执行容器重建，实际执行必须传 `--confirm` |
| `dm backup` | 备份容器 inspect、镜像、compose、volume/network 元数据和迁移包 |
| `dm restore` | 从备份目录或 tar.gz 离线包恢复镜像、网络、volume 和容器，支持恢复前计划导出 |
| `dm health` | 输出容器健康、重启、日志、端口和挂载风险报告 |
| `dm network` | 输出网络、端口映射、endpoint、IPAM 和暴露端口风险报告 |
| `dm logs` | 扫描容器日志关键字，支持上下文和 `none/basic/strict` 脱敏策略 |
| `dm diff` | 对比两个容器 inspect 的关键配置差异 |
| `dm prune` | 生成可清理资源报告，可通过 `--apply --confirm` 执行 |
| `dm volumes` | 分析 volume 使用关系、大小和疑似未使用资源 |
| `dm registry` | 检查 registry 凭据、连通性和 Docker RegistryLogin |
| `dm doctor` | 检查 Docker、registry、代理、磁盘、配置和工具链 |
| `dm version` | 输出版本、commit、构建时间和平台 |

## 常用示例

镜像拉取并归档:

```bash
dm pull busybox:latest --output-dir images
dm pull busybox:latest --load
dm pull --file images.txt --to http://registry.local:5000/team --plain-http --concurrency 2
```

镜像导入导出:

```bash
dm save ./images --filter 'repo:nginx*' --dry-run
dm save ./images --filter 'repo:nginx*'
dm load ./images
```

容器逆向和重建:

```bash
dm reverse web --pretty
dm reverse --filter 'label:app=demo' --reverse-type compose
dm rerun web --dry-run
dm rerun web --confirm
```

离线备份和恢复:

```bash
dm backup web --dry-run
dm backup web --bundle --bundle-output web-backup.tar.gz
dm backup web --bundle --encrypt --passphrase-file ./backup.pass --bundle-output web-backup.tar.gz
dm backup web --bundle --split-size 2G --bundle-output web-backup.tar.gz
dm restore web-backup.tar.gz --dry-run
dm restore web-backup.tar.gz --dry-run --format html
dm restore web-backup.tar.gz --dry-run --format json
dm restore web-backup.tar.gz.enc --passphrase-file ./backup.pass --dry-run --format html
dm restore web-backup.tar.gz.part-001 --dry-run --format json
dm restore web-backup.tar.gz --name web-restored
```

诊断报告:

```bash
dm health --format markdown
dm network --format html
dm logs --keyword error --tail 500
dm logs --keyword error --redact-profile strict
dm diff old-web new-web --redact-secrets
dm reverse web --redact-profile basic
dm volumes --size-mode auto --format json
dm prune --filter label=env=test --format markdown
dm registry registry.local:5000 --plain-http
dm doctor --registry registry.local:5000 --plain-http
```

## 项目结构

```text
main.go                         # 程序入口，只负责调用 internal/cli
internal/cli/                   # 根命令、全局配置、日志和统一错误输出
internal/appconfig/             # .dm.yaml、DM_CONFIG 和 Docker endpoint 默认配置解析
internal/commandflags/          # 命令层共享 flag 与补全注册
internal/commands/images/       # load/save 镜像导入导出命令
internal/commands/pull/         # pull 镜像拉取、导入和重新推送命令
internal/commands/reverse/      # reverse/rerun 命令入口和输出包装
internal/commands/backup/       # backup/restore 容器备份、迁移包和恢复命令
internal/commands/diagnostics/  # report、registry、volume、image tree 等诊断命令
internal/completion/            # shell 补全命令和 Docker 资源补全
internal/docker/                # Docker API client 和镜像/容器管理封装
internal/report/                # text/json/markdown/html 报告输出格式
internal/resourcefilter/        # 容器、镜像、volume 本地资源筛选器
internal/registryauth/          # Docker config、auths 和 credential helper 解析
internal/runconfig/             # 容器 inspect 到 docker run/compose 的共享解析模型
internal/textfmt/               # 字节大小、速率等文本格式化
internal/version/               # version 命令和构建版本信息
scripts/                        # 构建、发布、安装、卸载、检查和端到端脚本
docs/                           # 测试、发布检查和维护文档
```

## 文档

| 文档 | 用途 |
| --- | --- |
| [CHANGELOG.md](CHANGELOG.md) | 版本变化、已完成优化和已知非阻断项 |
| [docs/TESTING.md](docs/TESTING.md) | 本地、远程、企业 registry 和发布前验收说明 |
| [docs/RELEASE_CHECKLIST.md](docs/RELEASE_CHECKLIST.md) | 发布操作核对清单 |
| [docs/DOCKER_API_MIGRATION.md](docs/DOCKER_API_MIGRATION.md) | Docker Go SDK 到 Moby client/API 的迁移清单 |
