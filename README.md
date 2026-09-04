# docker-manager

`docker-manager` 是一个面向 Docker 日常运维、镜像迁移、容器备份恢复和诊断报告的命令行工具，二进制默认名为 `dm`。

它补充 Docker 原生命令在批量镜像迁移、离线迁移包、容器配置逆向、资源关联报告和企业 registry 检查上的使用体验。工具包含会修改 Docker 状态的命令，例如 `restore`、`rerun --confirm`、`prune --apply --confirm`、`pull --to`；生产环境执行前建议先使用 `--dry-run` 或在非生产环境确认目标范围。prune 报告包含 image/volume 候选时还必须显式传入 `--allow-non-atomic-delete`。

## 主要功能

- 镜像拉取、归档、导入和重新推送: `dm pull`、`dm save`、`dm load`、`dm tree`。
- 容器逆向和重建: `dm reverse` 只读输出 `docker run` 或 compose，`dm rerun` 显式确认后重建容器。
- 容器离线迁移: `dm backup` 和 `dm restore` 支持批量包、合并包、checksum、恢复前计划预览、加密包、分卷包、README 和 restore 脚本。
- 诊断报告: `dm health`、`dm network`、`dm logs`、`dm diff`、`dm prune`、`dm volumes`、`dm registry`、`dm doctor`。
- 规模化运行和自动化: 为批量检查提供命令级并发、整条命令超时、启动速率和累计资源项预算；health/logs/network/volumes/prune/report all 支持策略门禁和 SARIF 2.1.0。
- 结构化审计: 可选 JSONL 审计记录操作人、脱敏 endpoint、候选集合、授权结果、耗时和错误，并为审计写入失败配置继续、拒绝 mutation 或整条命令失败策略。
- 远程 Docker 管理: 支持 Docker 标准环境变量、`.dm.yaml` 和全局参数指定 Docker endpoint，并可用命名 profile 切换多套环境。
- Registry 网络策略: 可按精确 registry 配置独立 CA、proxy/no-proxy、timeout、凭据操作范围、认证 realm 和 plain HTTP。
- Shell completion: 支持 bash、zsh、fish 和 PowerShell，容器/镜像/volume 候选会按当前 Docker endpoint 查询。

## 构建

构建要求 Go 1.27.0 或更高版本。Go 1.27.0 是当前项目的构建基线。
Go 1.27 生成的 Darwin 二进制最低支持 macOS 13。

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

产物默认写入独立的 `dist/<version>-<commit>/`，包括按平台命名的 `tar.gz`/`zip`、`checksums.txt`、`release-manifest.json` 和 `release-summary.md`；同版本和提交的目录已存在时会拒绝覆盖。Linux 包包含 shell 安装/卸载脚本，Windows 包包含 PowerShell 安装/卸载脚本；Darwin 包只提供二进制和手动安装说明，不附带仅支持 Linux 的 shell installer。所有归档都包含 `INSTALL.md`、`CHANGELOG.md` 以及 README 链接的维护文档，发布前会逐项校验 manifest 和归档 digest。

## 安装

Linux:

```bash
sudo bash scripts/install.sh --binary ./bin/dev/dm
sudo bash scripts/install.sh --install-dir /opt/docker-manager --binary ./bin/dev/dm
sudo bash scripts/install.sh --build
sudo bash scripts/install.sh --binary ./bin/dev/dm --completion bash --completion zsh --completion fish
sudo bash scripts/install.sh --binary ./bin/dev/dm --no-completion
```

`scripts/install.sh` 和 `scripts/uninstall.sh` 当前只支持 Linux，并会在其他系统上提前拒绝。`--build --dry-run` 只输出计划，不要求 `PATH` 中存在 Go，也不会创建源码树中的 `bin/install` 或任何安装目标。

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

Linux 安装状态使用严格按数据解析、不会被 shell 执行的私有 `install.env` manifest v3。manifest 中的 128-bit 安装 token 同时写入 config/data 两个目录各自的 `.docker-manager-managed` marker；`--purge` 只有在 manifest、两个 marker、路径和 token 一致后才会继续。v2 manifest 仍可执行不删除 config/data 的普通卸载，但如需 `--purge`，必须先重新运行 `install.sh` 迁移到 v3。

config/data 不能指向系统根、用户根等宽路径，不能相互重叠或包含 prefix、binary、completion、profile 等安装产物。purge 会在删除 wrapper 或 profile 前预检完整目标树，并拒绝目标目录本身或任一后代中的 mount、symlink、特殊文件和非当前用户所有的条目；对祖先挂载则拒绝 `root != /` 的 bind/subtree view，以及当前 mount namespace 中可见、与目标 `root=/` 挂载共享 `(major:minor, fstype, source)` 身份的重复 root mount。独立文件系统的单一 `root=/` 挂载仍可用。Linux mountinfo 无法可靠区分 source 已卸载或被其他 namespace 隐藏的 root bind，因此这类动态挂载变化仍是残余边界，生产 purge 应在挂载拓扑稳定且目标位于专用目录时执行。root profile 必须与受管内容精确匹配；普通用户 profile 的 marker 结构异常会失败关闭，卸载替换前还会复核 inode、内容和 mode。普通用户 profile 的原 mode 会恢复；其 ACL/xattr 在 GNU `cp --preserve=all` 可用时保留，回退到 `cp -p` 的环境只提供尽力保留，不构成跨平台保证。

Darwin/macOS 发布包不包含 `install.sh`/`uninstall.sh`，请直接安装二进制：

```bash
sudo mkdir -p /usr/local/bin
sudo install -m 0755 ./dm /usr/local/bin/dm
dm version
```

手动卸载：

```bash
sudo rm -f /usr/local/bin/dm
```

Windows:

```powershell
.\scripts\install.ps1 -Binary .\bin\dev\dm.exe
.\scripts\install.ps1 -InstallDir C:\Tools\docker-manager -Binary .\bin\dev\dm.exe
.\scripts\install.ps1 -Build
.\scripts\install.ps1 -Binary .\bin\dev\dm.exe -Completion PowerShell
.\scripts\install.ps1 -Binary .\bin\dev\dm.exe -NoCompletion
.\scripts\install.ps1 -Binary .\bin\dev\dm.exe -NoEnvironment -NoPathUpdate
```

Windows 安装脚本会把真实二进制安装为 `<InstallDir>\bin\dm.exe`，设置用户级 `DM_CONFIG`、`DM_HOME`、`DM_OUTPUT_DIR`，并将 bin 目录加入用户 `PATH`。`install.json` 会记录该安装写入的值、原值和安装所有权；卸载时只有当前值仍由该安装持有才会恢复上一安装或原始值，用户或其他安装随后改写的值不会被清空。`-NoEnvironment` 可完全跳过持久环境变量，`-NoPathUpdate` 只跳过 `PATH` 更新。

安装、升级、卸载和 `-Purge` 会检查目标路径的每个现有组件及待删除树；遇到符号链接、junction 或其他 reparse point 时在修改前拒绝，避免越过安装边界。需要迁移安装目录时，应先移除这些重解析路径并使用真实目录。

卸载:

```powershell
.\scripts\uninstall.ps1
.\scripts\uninstall.ps1 -Purge
```

## 全局参数和配置

```bash
--config string               配置文件路径，默认 .dm.yaml；未显式传入时优先读取 DM_CONFIG
--profile string              选择命名环境；优先于 DM_PROFILE 和 default_profile
--docker-host string          Docker daemon 地址，默认读取 DOCKER_HOST 或本地 Docker
--docker-tls-verify           校验 Docker TCP 服务端证书，要求有效证书目录；默认读取 DOCKER_TLS_VERIFY
--docker-cert-path string     Docker TLS/mTLS 证书目录，含 ca.pem/cert.pem/key.pem；默认读取 DOCKER_CERT_PATH
--docker-api-version string   Docker API 版本，默认读取 DOCKER_API_VERSION 或自动协商
--docker-timeout duration     Docker 连接、TLS 握手和响应头超时，默认 30s
--audit-file string           结构化审计 JSONL 文件；为空表示关闭
--audit-actor string          审计事件中的声明操作人标识，不替代系统操作人
--audit-detail string         审计详情级别: safe | full，默认 safe
--audit-on-error string       审计写入失败策略: warn | deny-mutation | fail
--audit-required              审计写入失败时拒绝命令，等价于 --audit-on-error=fail
--audit-key-file string       审计标识 HMAC key 文件；默认 <audit-file>.key
--audit-max-bytes int         单个审计文件轮转阈值，默认 64 MiB
--audit-max-files int         保留的编号轮转文件数，默认 5
--redact-secrets              启用 basic 脱敏
--redact-profile string       全局脱敏策略: none | basic | strict，默认 none
--verbose                     输出详细日志
--quiet                       隐藏信息日志
--log-json                    以 JSON 输出日志和错误，不影响业务报告格式
```

示例 `.dm.yaml`:

```yaml
default_profile: development
proxy: http://127.0.0.1:7890
docker_host: tcp://docker.example.com:2376
docker_tls_verify: true
docker_cert_path: /etc/docker-manager/docker-certs
docker_api_version: "1.46"
docker_timeout: 30s
# 以下 CA 路径只由 doctor 检查，不会注入运行时信任链
ca_file: /etc/ssl/certs/company-ca.pem
ca_path: /etc/ssl/certs
registry_ca_file: /etc/docker/certs.d/registry.example.com/ca.crt
registry_ca_path: /etc/docker/certs.d/registry.example.com
os: linux
arch: amd64
output_dir: images
ready_timeout: 30s
operation_concurrency: 8
operation_timeout: 30m
operation_rate_limit: 20
operation_max_items: 10000
max_log_bytes: 16M
max_total_log_bytes: 256M
fail_on: warning
thresholds: []
audit_file: /var/log/docker-manager/audit.jsonl
audit_actor: automation
audit_detail: safe
audit_on_error: deny-mutation
audit_max_bytes: 67108864
audit_max_files: 5
audit_key_file: /var/lib/docker-manager/audit.key
redact_profile: none
credential_helpers_disabled: false
credential_helper_timeout: 5s
registry_auth_realms:
  - https://auth.example.com
profiles:
  development:
    docker_host: tcp://docker-dev.example.com:2376
    docker_cert_path: /etc/docker-manager/docker-certs/dev
    output_dir: images/development
    operation_concurrency: 4
    operation_timeout: 15m
    operation_rate_limit: 10
    operation_max_items: 5000
    max_log_bytes: 8M
    max_total_log_bytes: 64M
    fail_on: none
    thresholds: []
  production:
    docker_host: tcp://docker-prod.example.com:2376
    docker_tls_verify: true
    docker_cert_path: /etc/docker-manager/docker-certs/prod
    output_dir: images/production
    operation_concurrency: 16
    operation_timeout: 30m
    operation_rate_limit: 50
    operation_max_items: 50000
    max_log_bytes: 16M
    max_total_log_bytes: 256M
    fail_on: warning
    thresholds:
      - health.unhealthy=0
      - network.public_bindings=0
    audit_file: /var/log/docker-manager/production.jsonl
    audit_actor: production-automation
    audit_on_error: fail
    registries:
      registry.prod.example.com:
        ca_file: /etc/docker-manager/ca/registry-prod.pem
        no_proxy: true
        timeout: 20s
        credential_scope: [pull, push, login]
        auth_realms: [https://auth.prod.example.com]
        plain_http: false
registries:
  registry.local:5000:
    no_proxy: true
    timeout: 10s
    credential_scope: [pull, push]
    plain_http: true
verbose: false
quiet: false
log_json: false
```

配置文件最大为 `1 MiB`，根节点必须是 YAML mapping。配置加载使用严格 schema，未知字段、错误类型、非 mapping 根节点、多文档和非法值都会失败。显式指定但不存在的 `--config`/`DM_CONFIG` 会失败；只有未显式指定的 `.dm.yaml` 缺失时才使用内置默认值。可用以下命令检查最终值和来源：

```bash
dm config validate
dm config profiles
dm config show --effective --show-source
dm config show --effective --show-source --format json
dm --profile production config show --effective --show-source
```

命名环境选择优先级为：显式 `--profile` > 非空 `DM_PROFILE` > `default_profile` > 不选择 profile。顶层配置是所有环境的 base；选中的 profile 只覆盖其中显式出现的字段，省略字段继续继承，显式空字符串、`false` 和 `[]` 会清除继承值。profile 名只允许字母、数字、点、下划线和连字符，未知 profile 会拒绝执行。旧的扁平 `.dm.yaml` 不需要修改。

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| `default_profile` / `profiles` | 空 | 默认命名环境及其字段覆盖；可由 `DM_PROFILE` 或 `--profile` 切换 |
| `proxy` | 空 | `dm pull` registry HTTP 代理；只有原始空字符串表示未配置，空白值或带首尾空白的值会被拒绝；非空值只允许带 scheme 和非空 hostname、无 fragment 的 `http`、`https`、`socks5` URL，可含 userinfo；不控制 Docker daemon client |
| `os` / `arch` | `linux` / `amd64` | pull 选择的目标镜像平台 |
| `output_dir` | 命令默认目录 | pull、save、doctor 等命令的默认输出/检查目录 |
| `docker_host` | Docker 环境变量或本地 endpoint | Docker daemon 地址 |
| `docker_tls_verify` | `DOCKER_TLS_VERIFY` | 是否校验 Docker TLS 服务端证书 |
| `docker_cert_path` | `DOCKER_CERT_PATH` | `ca.pem`、`cert.pem`、`key.pem` 所在目录 |
| `docker_api_version` | `DOCKER_API_VERSION` 或自动协商 | 固定 Docker API 版本 |
| `docker_timeout` | `30s` | daemon 连接、TLS 握手和响应头超时 |
| `ca_file` / `ca_path` | 空 | 仅供 `dm doctor` 检查的通用 CA 路径，不注入运行时信任 |
| `registry_ca_file` / `registry_ca_path` | 空 | 仅供 `dm doctor` 检查的 registry CA 路径 |
| `ready_timeout` | `30s` | restore replace 和 rerun 候选容器等待 running/healthy 的时间 |
| `operation_concurrency` | `8` | 一次命令共享的只读任务并发上限；`0` 不增加共享限制，命令行对应 `--concurrency` |
| `operation_timeout` | 无 | 整条命令的外层超时，最大 `24h`；命令行对应 `--operation-timeout` |
| `operation_rate_limit` | `0` | 每秒启动的只读资源任务数，`0` 不限速，最大 `1000`；命令行对应 `--rate-limit` |
| `operation_max_items` | `0` | 一次命令累计处理的资源项上限，`0` 不限制，最大 `100000`；命令行对应 `--max-items` |
| `max_log_bytes` | `16M` | health/logs/report all 每个容器最多读取 `16 MiB` 日志，可下调或上调至 `256 MiB` |
| `max_total_log_bytes` | `256M` | 同一命令所有容器累计最多读取 `256 MiB` 日志，可下调或上调至 `4 GiB` |
| `fail_on` | `none` | 报告门禁级别：`none`、`note`、`warning` 或 `error`；命令行对应 `--fail-on` |
| `thresholds` | 空 | 严格 `scope.metric=maximum` 列表；单报告自动选择自身 scope 并移除前缀，`report all` 保留完整名称；命令行 `--threshold` 仍使用当前命令的无前缀指标 |
| `audit_file` | 空（关闭） | JSONL 审计文件；配置后记录命令生命周期和 mutation 候选/授权事件 |
| `audit_actor` | 空 | 声明操作人；与系统用户名、UID/SID 和主机名同时记录，不替代系统身份 |
| `audit_detail` | `safe` | `safe` 只写 HMAC 标识和错误分类；`full` 额外写经过 strict 脱敏和长度限制的显示值/错误消息 |
| `audit_on_error` | `deny-mutation` | 审计失败策略：`warn`、`deny-mutation` 或 `fail` |
| `audit_max_bytes` / `audit_max_files` | `64 MiB` / `5` | 单文件轮转阈值和保留数；字节数必须使用 YAML 整数，取 `0` 或 `65536..1 TiB`，文件数取 `0..100`；非法 CLI 值在打开 sink 前退出 `1` |
| `audit_key_file` | `<audit_file>.key` | HMAC 标识 key 文件；应与 JSONL 一起保护和备份，轮换 key 会改变标识关联值 |
| `redact_profile` | `none` | `none`、`basic` 或 `strict`；管理员默认保留原始日志和输出 |
| `redact_secrets` | `false` | 兼容配置；`true` 等价于 `redact_profile: basic` |
| `credential_helpers_disabled` | `false` | 禁止执行 Docker credential helper，回退到 `auths` |
| `credential_helper_timeout` | `5s` | 单次 credential helper 独立超时 |
| `registry_auth_realms` | 空 | 允许接收 registry 凭据的额外 HTTPS auth origin 列表；每项必须包含非空 hostname |
| `registries` | 空 | 精确 `host[:port]` 对应的 registry 网络与凭据策略；profile 可按 registry 覆盖 |
| `verbose` / `quiet` | `false` / `false` | 详细日志或静默模式，两者不能同时为 `true` |
| `log_json` | `false` | 日志和错误使用 JSON；不改变业务报告的 `--format` |

### 规模化运行控制

`operation_*` 配置对一次命令共享同一个 controller，选中 profile 可以覆盖 base，显式命令 flag 再覆盖配置。diagnostics、backup 和 reverse/rerun 的叶子命令提供 `--concurrency`、`--operation-timeout`、`--rate-limit` 和 `--max-items`；`0` 表示不额外限制对应维度。`dm pull` 保留原有批量 `--concurrency` 和单镜像 `--total-timeout` 语义，同时遵守配置 controller 的并发、外层超时、启动速率和累计资源预算。

`--max-items` 是一次命令内跨资源类型累计的预算，而不是每个循环各自的限制。例如 network 报告会累计 container 和 network，volumes 会累计 volume 和 container；backup/restore 累计 container、按名称去重的 custom network 和 named volume。Backup 会在任何输出前复核已计费容器身份及资源集合，发生漂移时要求重试；restore 的多个输入会在产生 text/structured dry-run 输出或访问 Docker 前全部预检并预留预算。预算超限会在对应 Docker mutation 或文件写入开始前失败。并发、速率和超时共用命令 context，取消或超时时会停止尚未启动的任务。

health、logs 和 `dm report all` 还会流式计数 Docker 日志：`--max-log-bytes` 限制单个容器，`--max-total-log-bytes` 限制本次命令所有容器累计读取量。默认分别为 `16 MiB` 和 `256 MiB`，命令 flag 优先于 profile/base 配置；达到限制会停止继续读取并返回运行错误。

### 报告自动化

`dm health`、`dm logs`、`dm network`、`dm volumes`、`dm prune` 和 `dm report all` 支持以下自动化参数：

```bash
--format text|json|markdown|html|sarif
--fail-on none|note|warning|error
--threshold metric=maximum
```

`--threshold` 可重复指定，指标必须来自对应命令的 completion/帮助候选；`dm report all` 使用 `health.issues`、`logs.errors`、`network.risks` 等带子报告前缀的指标。配置文件中的 `thresholds` 始终使用完整 `scope.metric`，例如 `health.unhealthy=0`、`network.public_bindings=0`；单项报告会自动路由并剥离自身前缀。命令行显式策略优先于配置中的 `fail_on` 和 `thresholds`。默认未启用策略时，既有 text/JSON/Markdown/HTML 结构不增加门禁内容；JSON 仍是单个纯 JSON 文档。`--format sarif` 始终输出 SARIF 2.1.0，其中 findings 转为 results，策略与指标写入 invocation properties。

示例：

```bash
dm health --format sarif --fail-on warning > health.sarif
dm logs --format json --threshold errors=0 --threshold logs_unavailable=0
dm network --fail-on error --threshold public_bindings=0
dm report all --format sarif --threshold health.unhealthy=0 --threshold prune.candidates=20
```

### JSONL 审计

配置 `audit_file` 或传入 `--audit-file` 后，每次命令会写入带 `run_id` 和单调 `sequence` 的 JSONL 生命周期事件。显式 `--audit-file` 还覆盖参数缺失、未知 flag 和配置加载失败等 pre-run 之前/期间的失败；若配置文件本身无法解析，则其中尚未加载的 `audit_file` 无法生效，需要由命令行显式提供。事件包括命令开始/结束、分页后的 mutation 候选集合、拒绝或授权结果、操作人、profile、endpoint HMAC 标识、耗时和错误分类。`safe` 是默认详情级别，只保留不可逆 HMAC 标识；`full` 会额外保留经过 strict 脱敏和长度限制的候选显示值与错误消息，不会关闭凭据脱敏。

审计写入失败策略：

| 策略 | 行为 |
| --- | --- |
| `warn` | 输出一次警告后继续，包括 mutation；适合审计仅作辅助诊断的环境 |
| `deny-mutation` | 只读命令警告后继续；需要修改 Docker、文件系统或外部系统时，在授权事件无法落盘的情况下拒绝 mutation |
| `fail` | 审计文件打开、开始、结束或任一事件写入失败都使命令失败；`--audit-required` 是该策略的快捷方式 |

在 Unix 上，新建审计目录使用 `0700`，JSONL、HMAC key 和 lock 文件使用 `0600`；Windows 部署仍应通过目录 ACL 限制访问。写入持有跨进程文件锁，达到 `audit_max_bytes` 后按 `.1`、`.2` 等完整事件边界轮转。数据、key、lock 和轮转路径会拒绝 symlink、Windows junction/reparse 及非普通文件。审计 key 默认位于 `<audit_file>.key`；不要与公开报告一起分发。

显式 `--audit-max-bytes` 和 `--audit-max-files` 与 YAML 使用同一边界校验；非法值在创建审计目录、key、lock 或 JSONL 前返回运行错误，即使 `--audit-on-error=warn` 也不会降级继续。

进程退出码固定为：`0` 成功；`1` 配置、连接、执行、输出或审计等运行错误；`2` 只有报告已成功生成但 `--fail-on`/`--threshold` 门禁未通过；`130` 收到 SIGINT 或 context cancel。运行错误与门禁失败同时出现时返回 `1`，取消优先返回 `130`。

Docker API endpoint 优先级为: 全局命令行参数 > 选中 profile > 顶层 `.dm.yaml` > Docker 环境变量 > 本地 Docker 默认 endpoint。省略 Docker 配置项会继承对应环境变量；显式空字符串会清除环境变量回退。支持 `tcp://`、`unix://` 和 `npipe://`；其他 scheme 会在 client 初始化时拒绝。生产环境不建议裸露未启用 TLS 的 `tcp://host:2375`；`dm doctor` 会对明文 TCP endpoint 给出 warning。

`registries` 的 key 以及 `dm registry`/`dm doctor --registry` 的参数必须是精确规范化的 `host[:port]`，不接受 scheme、路径、userinfo、查询、fragment 或通配符。策略支持 `ca_file`/`ca_path`、`proxy`、`no_proxy`、正 duration `timeout`、`credential_scope`、`auth_realms` 和 `plain_http`；顶层、profile 和 per-registry proxy 共用校验，只有原始空字符串表示未配置，空白值或带首尾空白的值会被早期拒绝；非空值只允许带 scheme 和非空 hostname、无 fragment 的 `http`、`https`、`socks5` URL，合法 userinfo 会保留。显式或环境 registry proxy 在交给 HTTP transport 前也会拒绝空 hostname。`auth_realms` 和 `--auth-realm` 只接受带非空 hostname 的 HTTPS origin。`credential_scope` 可包含 `pull`、`push`、`login`；省略时允许这三种既有操作，显式 `[]` 禁止使用凭据。该设置只收窄凭据使用，Bearer token 请求仍固定为当前 repository 的 pull scope。`plain_http` 默认为 `false`，只能对精确匹配项显式开启，且不能与 CA 同时配置。命令行显式 `--proxy`、`--timeout`、`--auth-realm` 和 `--plain-http[=false]` 继续覆盖策略。

Per-registry CA、代理和 timeout 应用于 `dm` 自己的 registry `/v2/`、manifest、blob、token 请求，以及 push 前预检。`dm pull --to` 最终调用 Docker daemon 执行 push；daemon 的 registry CA、insecure registry 和代理仍需单独配置。`dm registry` 报告会区分 `dm` 直连检查和 daemon `RegistryLogin`，前者使用 policy，后者使用 daemon 网络配置。

Per-registry `ca_file` 只接受不超过 16 MiB 的非 symlink、非 reparse 普通文件；`ca_path` 必须是非 symlink、非 reparse 目录，最多包含 256 个非链接普通文件且累计不超过 32 MiB。每个文件必须完全由可解析的 `CERTIFICATE` PEM block 构成，空目录、混入 README/私钥/垃圾内容、链接或特殊文件都会失败关闭。系统证书目录通常含 symlink，不应直接作为该 `ca_path`；请使用专用 CA 目录或单独的 `ca_file`。

本工具默认面向管理员使用，因此日志和报告的默认策略为 `none`，不会隐藏敏感信息。输出需要共享时，可使用全局 `--redact-secrets`（等价于 `basic`）、`--redact-profile basic|strict`，或在配置中设置 `redact_profile`。YAML 仅在省略 `redact_profile` 时把 `redact_secrets: true` 解释为 `basic`；`redact_profile: none` 与 `redact_secrets: true` 会被视为冲突并拒绝加载。命令行同时显式指定两者时 `--redact-profile` 优先，命令级显式 `--redact-profile none` 可覆盖 YAML 或全局脱敏策略。该策略同时作用于 text、JSON、Markdown、HTML、错误、verbose HTTP 日志及 reverse 保存文件。Docker config 与 PATH 中的 credential helper 视为受信本地输入；可通过配置或 `--disable-credential-helpers`、`--credential-helper-timeout` 收紧执行边界。

Docker daemon TLS 规则如下：

| 配置 | 实际 transport | 行为 |
| --- | --- | --- |
| TCP，证书目录为空，未启用校验 | HTTP | 明文连接，`dm doctor` 报 warning |
| TCP，证书目录非空，未启用校验 | HTTPS + mTLS | 加载客户端证书，但不校验 daemon 证书，`dm doctor` 报 warning |
| TCP，证书目录非空，启用校验 | HTTPS + mTLS | 使用系统根证书加 `ca.pem` 校验 daemon，并加载 `cert.pem`/`key.pem` |
| 启用校验但证书目录为空或不存在 | 初始化失败 | 不发起明文请求，不回退到 HTTP |

`docker_cert_path`/`DOCKER_CERT_PATH` 是 Docker daemon TLS 的运行时证书来源，目录必须包含可读取的 `ca.pem`、`cert.pem` 和 `key.pem`。`.dm.yaml` 顶层的兼容项 `ca_file`、`ca_path`、`registry_ca_file`、`registry_ca_path` 仍只供 `dm doctor` 检查路径，不会注入运行时信任；只有 `registries.<host>.ca_file/ca_path` 会进入 `dm` 自己的 registry HTTP client。Docker daemon 的 registry 私有 CA 仍应安装到 Docker 信任目录。Docker daemon TCP transport 使用标准 `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY` 环境变量，`.dm.yaml` 的 `proxy` 不控制 daemon client。同一 `dm` 进程只在 client 初始化时加载证书；原路径内证书被替换后，应重新执行命令以加载新证书。

## Shell 自动补全

生成补全脚本:

```bash
dm completion bash
dm completion zsh
dm completion fish
dm completion powershell
```

Linux 安装脚本默认安装对应 shell completion；Linux 默认生成 bash 补全，可通过 `--completion` 指定多个 shell。Darwin 发布包不附安装脚本，需按包内说明手动安装二进制；Windows 默认生成 PowerShell completion 并写入可卸载的 profile 片段。

补全会读取当前 Docker endpoint 配置，容器、镜像和 volume 候选可以来自远程 Docker。

## 命令速查

| 命令 | 功能 |
| --- | --- |
| `dm pull` / `dm image pull` | 从 registry 拉取镜像，支持未压缩、gzip、zstd 镜像层归档、导入 Docker、批量同步和重新推送 |
| `dm save` / `dm image save` | 导出本地镜像，支持筛选、通配符、dry-run 和批量导出；最多一个输出路径 |
| `dm load` / `dm image load` | 导入镜像 tar/tar.gz/tgz，默认递归扫描目录；最多一个输入路径 |
| `dm tree` / `dm image tree` | 分析镜像层、历史、大小占比和本地容器引用 |
| `dm reverse` | 从容器 inspect 生成 `docker run` 或 compose，只读输出 |
| `dm rerun` | 基于 inspect 执行容器重建，实际执行必须传 `--confirm` |
| `dm backup` | 备份容器 inspect、镜像、compose、volume/network 元数据和迁移包 |
| `dm restore` | 从备份目录或 tar.gz 离线包生成恢复计划；实际恢复需显式 `--confirm` |
| `dm report all` | 聚合 health、network、logs、volumes 和 prune dry-run，并支持统一门禁与 SARIF |
| `dm health` | 输出容器健康、重启、日志、端口和挂载风险报告 |
| `dm network` | 输出网络、端口映射、endpoint、IPAM 和暴露端口风险报告 |
| `dm logs` | 扫描容器日志关键字，支持上下文和 `none/basic/strict` 脱敏策略 |
| `dm diff` | 对比两个容器 inspect 的关键配置差异 |
| `dm prune` | 生成可清理资源报告；执行 image/volume 清理需额外确认 Docker 非原子删除边界 |
| `dm volumes` | 分析 volume 使用关系、大小和疑似未使用资源 |
| `dm registry` | 检查 registry 凭据、连通性和 Docker RegistryLogin |
| `dm doctor` | 检查 Docker、registry、代理、磁盘、配置和工具链；不接受位置参数 |
| `dm version` | 输出版本、commit、构建时间和平台 |

## 常用示例

镜像拉取并归档:

```bash
dm pull busybox:latest --output-dir images
dm pull busybox:latest --load
dm pull --file images.txt --to http://registry.local:5000/team --plain-http --concurrency 2
dm pull busybox:latest --max-layers 256 --max-layer-bytes 10737418240 --max-expanded-layer-bytes 21474836480 --max-total-layer-bytes 21474836480 --max-total-expanded-bytes 42949672960 --max-temporary-bytes 85899345920 --total-timeout 20m
```

`dm pull` 的资源预算按单个镜像计算；所有数值参数必须大于 0，并且只能在内置硬上限内取值。`--max-*-bytes` flags 接受十进制字节数，不接受 `2G`、`4GiB` 一类后缀：

| 参数 | 默认值 | 硬上限 | 约束对象 |
| --- | ---: | ---: | --- |
| `--max-token-bytes` | 1 MiB | 16 MiB | 单次 Bearer token 响应 |
| `--max-manifest-bytes` | 16 MiB | 16 MiB | 单个 manifest 响应 |
| `--max-config-bytes` | 16 MiB | 16 MiB | 镜像 config descriptor 和响应 |
| `--max-layer-bytes` | 32 GiB | 512 GiB | 单个压缩层 descriptor 和下载 |
| `--max-expanded-layer-bytes` | 64 GiB | 512 GiB | 单层展开结果 |
| `--max-total-layer-bytes` | 64 GiB | 512 GiB | 镜像所有压缩层累计大小 |
| `--max-total-expanded-bytes` | 128 GiB | 1 TiB | 镜像所有层累计展开大小 |
| `--max-temporary-bytes` | 272 GiB | 3 TiB | 下载、展开和打包期间的临时文件峰值 |
| `--max-layers` | 1000 | 10000 | manifest 层数 |
| `--total-timeout` | 1h | 24h | 单个镜像从 manifest、`--skip-existing` 检查到归档或推送的总耗时 |

`--timeout` 默认 `30s`，约束连接、TLS 握手和响应头；`--total-timeout` 是覆盖完整单镜像流程的外层 deadline。拉取和解压会流式计数，任一累计预算超限或取消时停止 worker 并清理临时文件。tar 输出在目标目录内以 `0600`、128-bit 随机名 staging 写入，完成 `close` 和文件 `fsync` 后再 rename 到目标；rename 前失败会保留已有普通文件。Unix 在 rename 后还会同步父目录；父目录同步失败会返回错误，batch 不记录 success，但新归档可能已经通过 rename 发布。输出路径链中的符号链接、Windows junction/reparse，以及作为目标的目录或其他非普通文件都会被拒绝。

单次 pull 与 batch 共用归档输出目录的跨进程生命周期 scope，batch 内部并发任务复用已持有的 scope。锁定期间会持续复核目录和 lifecycle lock marker 身份；Unix 锁定已打开的父目录 inode，Windows marker handle 不允许 delete sharing，因此替换或 unlink marker 不能绕过活跃归档。批量 pull 还会在任何 registry/Docker 回调前预计算绝对归档路径，并拒绝 state、report、归档及辅助锁之间的精确、hardlink、仅大小写和祖先/后代碰撞。`.dm-pull-` 和 `.docker-manager-pull-` 是内部保留 basename namespace，原始名称以及 Windows canonical/实际 8.3 basename 均会检查；standalone pull 在首个 registry HTTP 请求前拒绝保留名，batch 在任何 pull/exists callback 前拒绝。Windows 还拒绝设备/扩展 namespace、设备名、尾点/尾空格、unsafe UNC server/share、尚不存在且形似 DOS 8.3 短名的组件，以及任一启用了 per-directory case sensitivity 的现存父目录；已存在祖先会解析成长路径，用于检测实际 short-name alias 冲突，不会无条件拒绝所有已存在的 8.3 alias。

审计授权后，batch 持有 state/report 生命周期锁；resume state 只从锚定父目录中的普通文件读取，最大 `64 MiB`。每个成功项写入版本化 fingerprint，包含归档绝对路径、大小、SHA-256、目标 OS/arch 和 effective Docker load（显式 `--load` 或存在 push target）；旧 state 缺 fingerprint、归档缺失/篡改，或路径、平台、load 语义变化时都会重新拉取并迁移 state。State/report 使用随机排他 `0600` staging，`basic`/`strict` 会在持久化副本中脱敏 message，`none` 保留管理员原文。Deadline 到期时 report 仍包含每个已计划项，包括未调度项；state/report 持久化错误和 context 错误通过 `errors.Join` 一并保留。Windows `0600` 不等价于私有 ACL，且 Go 不保证任意 Windows 文件系统具有与 Unix 相同的 rename 原子语义，部署时仍需限制目录 ACL 并使用本地文件系统。

State 提交协议当前为 v2。写 state 前先持久化固定 `.dm-pull-state-untrusted.marker`，其 payload 包含版本、state basename 原始字节的 hex 和 128-bit transaction；state rename 与目录同步成功后，只有 marker 文件身份、transaction 和 owner 均匹配才会清理。残留 marker 或 commit protocol 0/1 的 success 会保守重拉；属于其他 state、畸形、超限或被替换的 marker 会在 pull/exists callback 前失败关闭。Marker 创建持久化失败时不写 state，清理失败时保持 untrusted，删除后的目录同步失败最多导致下次保守重拉。

测试和发布门禁的执行方法、已验证范围与仍需外部环境确认的项目统一记录在 [docs/TESTING.md](docs/TESTING.md)；公开归档不包含内部服务器地址、运行路径或临时验收产物。

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
dm backup web --bundle --signing-key ./backup-signing-private.pem --bundle-output web-backup.tar.gz
dm restore web-backup.tar.gz # 默认只生成计划，不修改 Docker
dm restore web-backup.tar.gz --dry-run --format html
dm restore web-backup.tar.gz --dry-run --format json
dm restore web-backup.tar.gz.enc --passphrase-file ./backup.pass --dry-run --format html
dm restore web-backup.tar.gz.part-001 --dry-run --format json
dm restore web-backup.tar.gz --dry-run --max-archive-size 20G --max-expanded-size 40G --max-json-size 64M --max-parts 32
dm restore web-backup.tar.gz --name web-restored --confirm
dm restore web-backup.tar.gz --trusted-public-key ./backup-signing-public.pem --confirm
# 仅在确认可信备份确实需要高风险 HostConfig 时显式放行
dm restore web-backup.tar.gz --confirm --allow-dangerous-config
```

实际恢复默认要求完整有效的 `checksums.txt`；只有在已经独立验证来源时才应显式使用 `--skip-checksum`。Checksum 只能检测内容变化，不能认证来源；需要来源认证时使用 Ed25519 `--signing-key` 和位于备份根外的 `--trusted-public-key`。签名私钥、加密口令文件和 `--bundle-output` 都必须位于备份输出目录外。restore 对归档输入、展开总量、JSON 累计大小和分卷数设置硬上限，并允许用 `--max-archive-size`、`--max-expanded-size`、`--max-json-size`、`--max-parts` 进一步下调，不能上调硬上限。当前 volume 备份仅保存 Docker volume 元数据，不包含 volume 内的数据。

分卷备份先写入同目录私有 staging，并在 `<bundle>.parts.pending.json` 持有文件锁和事务所有权；所有 `.part-NNN` 发布后，最后发布带逐卷大小/digest、整体 SHA-256 和 `commit: complete` 的 `<bundle>.parts.json`。进程中断后，下次写入同一目标会恢复并只清理由该 pending marker 与 staging 共同证明属于旧事务的文件；已替换文件、外来同名 part 或活跃事务一律保留并报错。成功提交后 pending marker 会被移除。

`restore` 和 `rerun` 的候选容器分别带 `com.docker-manager.restore-owner`、`com.docker-manager.rerun-owner` 内部标签，值为每次事务生成的 128-bit 随机标识。创建结果不确定或需要回滚时，工具只有在稳定 container ID 与本次所有权标签同时匹配后才删除候选；同名但不属于本事务的容器会被保留并报告错误。

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
dm prune --filter label=env=test --apply --confirm --allow-non-atomic-delete
dm registry registry.local:5000 --plain-http
dm doctor --registry registry.local:5000 --plain-http
```

Docker API 没有 image/volume 的 compare-and-delete。`dm prune` 会在删除前复核快照并坚持非强制删除，但仍无法消除 inspect 与 delete 之间的竞态；因此只要报告含 image 或 volume 候选，缺少 `--allow-non-atomic-delete` 时不会执行任何删除。container-only 清理不需要该额外参数。

`dm volumes` 默认使用 Docker API 返回的大小；`--size-mode auto` 会先尝试本机只读遍历，再回退到 `docker-run` probe。默认 helper 固定为 `busybox:1.36.1@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662` 且必须已存在于目标 Docker；probe 使用 `NetworkMode=none`、只读 rootfs、只读 volume bind、drop `ALL` capabilities 和 `no-new-privileges`。通过 `--size-image` 改用自定义 helper 时，调用方负责固定并信任该镜像。

## 项目结构

```text
main.go                         # 程序入口，只负责调用 internal/cli
internal/cli/                   # 根命令、全局配置、日志和统一错误输出
internal/appconfig/             # .dm.yaml、DM_CONFIG 和 Docker endpoint 默认配置解析
internal/audit/                 # JSONL 审计事件、HMAC 标识、失败策略、文件锁和轮转
internal/commandflags/          # 命令层共享 flag 与补全注册
internal/commands/images/       # load/save 镜像导入导出命令
internal/commands/pull/         # pull 镜像拉取、导入和重新推送命令
internal/commands/reverse/      # reverse/rerun 命令入口和输出包装
internal/commands/backup/       # backup/restore 容器备份、迁移包和恢复命令
internal/commands/diagnostics/  # report、registry、volume、image tree 等诊断命令
internal/completion/            # shell 补全命令和 Docker 资源补全
internal/docker/                # Docker API client 和镜像/容器管理封装
internal/report/                # text/json/markdown/html/SARIF、findings、阈值和报告门禁
internal/runcontrol/            # 命令级并发、外层超时、启动速率和累计资源项控制
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
