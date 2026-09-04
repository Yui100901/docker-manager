# 使用指南（试用版）

本文面向第一次使用 `docker-manager` 的管理员，提供一条可复现的上手路径、命令选择建议和常见故障处理。命令的完整参数以当前二进制的 `dm <command> --help` 为准；配置字段和安全边界的详细约束见 [README.md](../README.md)。源码仓库的配置模板名为 `.dm.yaml.example`，发布包中的对应文件名为 `dm.yaml.example`。

## 1. 试用前提

| 项目 | 要求 |
| --- | --- |
| 运行二进制 | Linux amd64/arm64、Windows amd64 或 Darwin amd64/arm64 发布包；从源码构建需要 Go 1.27.0 或更高版本 |
| Docker daemon | `tree`、`health`、`network`、`logs`、`diff`、`volumes`、`registry`、`doctor`、`save`、`load`、`backup`、`restore` 和 `rerun` 按命令需要可访问的 daemon |
| Registry | `pull` 需要源 registry；`pull --to` 还需要目标 registry 和 Docker daemon 的 push 权限 |
| 权限 | 运行用户需要读取 Docker socket/远程证书、写入输出目录；安装脚本另外需要对应的系统权限 |
| 网络 | 访问 registry、认证服务和（如使用）代理；离线导入/恢复不需要 registry，但仍可能需要 Docker daemon |

试用版聚焦基础镜像迁移、备份恢复和诊断流程。真实企业 registry/OIDC、多个 Docker 版本、真实 TLS/mTLS 生产拓扑、真机平台兼容性和破坏性 E2E 仍应在目标环境单独验收，不能仅凭本指南视为已覆盖。

## 2. 五分钟快速开始

### 使用发布包

下载与目标平台匹配的归档及发布目录中的 `checksums.txt`，先在解压前核对归档，再按包内 `INSTALL.md` 安装。`checksums.txt` 位于归档旁，不在归档内部；安装后执行：

```bash
dm version
dm config validate
dm doctor --check-e2e=false --format markdown
```

Windows PowerShell 使用相同的 `dm` 命令；路径和安装步骤以包内 `INSTALL.md` 为准。

需要配置文件时，将发布包中的 `dm.yaml.example` 复制为 `.dm.yaml`，或通过 `--config` 指定其他路径。

在归档所在目录校验发布文件（文件名必须与 `checksums.txt` 中一致）。下面的 `-c` 命令会逐项检查清单，因此只有目录中已下载所有列出的归档时才会全部通过；只下载一个平台时，应按实际归档文件名筛选对应 checksum 行后再校验：

```bash
# Linux
sha256sum -c checksums.txt

# macOS（没有 sha256sum 时）
shasum -a 256 -c checksums.txt
```

PowerShell：

```powershell
$archiveName = 'dm_1.2.3_windows_amd64.zip' # 替换为实际下载的归档文件名
$archivePath = Join-Path . $archiveName
if (-not (Test-Path -LiteralPath $archivePath -PathType Leaf)) { throw "archive not found: $archiveName" }
$line = @(Get-Content .\checksums.txt | Where-Object {
  $_ -match ('^\s*[0-9a-fA-F]{64}\s+\*?' + [regex]::Escape($archiveName) + '\s*$')
})
if ($line.Count -ne 1) { throw "checksums.txt must contain exactly one entry for: $archiveName" }
$expected = (($line[0].Trim() -split '\s+')[0]).ToLowerInvariant()
$actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash.ToLowerInvariant()
if ($actual -ne $expected) { throw "checksum mismatch: $archiveName" }
```

目录中有多个平台归档时，按实际文件名逐个运行上述校验，不要依赖“第一条 checksum”。

### 从源码运行

```bash
cp .dm.yaml.example .dm.yaml
go run . version
go run . config validate
go run . doctor --check-e2e=false
```

Windows:

```powershell
Copy-Item .dm.yaml.example .dm.yaml
go run . version
go run . config validate
go run . doctor --check-e2e=false
```

未找到隐式 `.dm.yaml` 时，程序使用内置默认值；显式传入 `--config` 或设置 `DM_CONFIG` 后，如果文件不存在或无法解析，命令会失败。建议把真实配置放在权限受限目录，不要把口令、私钥或生产 endpoint 写入公开仓库。

### 第一次只读验证

确认 `doctor` 没有阻断项后，先运行只读报告和 dry-run。下面的镜像树示例假定 Docker daemon 已有 `busybox:latest`；干净主机请先执行 `dm pull busybox:latest --load`，或替换为本机已有的镜像引用。`reverse`/`backup` 示例假定已有名为 `my-container` 的容器，`restore` 示例假定 `./backup-or-archive` 已存在；没有这些资源时请跳过对应行，或替换为后文生成的实际路径：

```bash
dm report all --format markdown
dm image tree busybox:latest --format markdown
dm reverse --filter 'name:my-container' --pretty
dm backup my-container --dry-run
dm restore ./backup-or-archive --dry-run --format markdown
dm prune --format markdown
```

需要分享输出时加 `--redact-profile strict`。工具默认面向管理员，默认脱敏策略是 `none`，所以日志、环境变量和 endpoint 可能包含敏感信息。

## 3. 命令选择

| 命令 | 默认行为 | 会写入或修改什么 | 首次使用建议 |
| --- | --- | --- | --- |
| `pull` / `image pull` | 从 registry 下载并生成 tar | 写归档；`--load` 会导入 Docker，`--to` 会 push | 先对一个小镜像使用 `--output-dir`，再启用 batch |
| `image save` / `save` | 从本地 Docker 导出镜像 | 写 tar 文件 | 先 `--dry-run`，再指定独立目录 |
| `image load` / `load` | 递归扫描 tar 并导入 Docker | 修改本地 Docker 镜像 | 只传可信目录，必要时先检查归档来源 |
| `image tree` / `tree` | 展示镜像层、历史和引用 | 只读 | 用 `--format json` 给脚本消费 |
| `reverse` | 根据 inspect 生成 `docker run`/compose | 默认只输出；`--save` 写文件 | 先 `--pretty`，分享前使用 strict 脱敏 |
| `rerun` | 停止、替换并重建容器 | 修改 Docker；无 `--confirm` 不执行 | 先 `--dry-run`，只对明确的容器名或 filter 使用 |
| `backup` | 导出 inspect、镜像和资源元数据 | 写备份目录/离线包 | 先 `--dry-run`；多个容器需决定分开或 `--merge` |
| `restore` | 生成恢复计划 | 默认只读；`--confirm` 才创建/替换资源 | 先 `--dry-run` 并核对 checksum，再使用唯一 `--name` |
| `health`、`network`、`logs`、`diff`、`volumes` | 生成诊断报告 | 只读（`volumes --size-mode docker-run` 会短暂创建 helper） | 先用 `--format json` 保存基线 |
| `prune` | 生成候选清单 | 只有 `--apply --confirm` 才删除 | 先固定 `--only`、`--filter` 和 `--protect-label` |
| `registry`、`doctor` | 检查配置、网络和凭据 | 只读；不会自动修复 | 指定精确 `host[:port]`，不要带 scheme/path |
| `config`、`completion`、`version` | 查看本地配置/生成脚本/打印版本 | 不访问 Docker（completion 的动态候选除外） | 出问题时先运行 `config show --effective --show-source` |

`dm image <subcommand>` 与顶层 `dm pull/save/load/tree` 是同一功能的两种入口；`dm report <subcommand>` 与顶层诊断快捷命令同理。旧名称如 `logs-scan`、`inspect-diff`、`prune-report` 不再作为兼容别名。

## 4. 镜像迁移

### 单个镜像

```bash
dm pull alpine:latest --output-dir ./images
dm pull alpine:latest --load --output ./images/alpine.tar
```

`--timeout`（默认 30s）只限制连接、TLS 握手和响应头；`--total-timeout`（默认 1h，最大 24h）覆盖一个镜像从 manifest 到归档、load 或 push 的完整流程。所有 `--max-*-bytes` 参数使用十进制整数，不能写 `2G`；配置中的日志预算才接受 `K/M/G/T` 二进制后缀。

输出目录必须是专用目录。程序会拒绝输出路径链中的 symlink、junction/reparse、目录或其他非普通目标，并在 staging 完成、关闭和同步后再发布归档。目标已有普通文件时不会在 staging 失败前覆盖它。

### 批量镜像与恢复

镜像列表是 UTF-8 文本，每行一个引用；空行和以 `#` 开头的行会被忽略：

```text
# images.txt
alpine:latest
busybox:latest
registry.example.com/team/app:2026.09
```

```bash
dm pull --file images.txt --output-dir ./images \
  --concurrency 2 --retries 2 --resume \
  --state-file ./state/pull-state.json \
  --report ./state/pull-report.json
```

`--resume` 只跳过 state 中 fingerprint 与当前归档、平台和 load/push 语义都匹配的成功项；state 缺失、过期、被替换或归档 digest 改变时会保守重拉。`--skip-existing` 只适用于带 `--to` 的批量 push，表示目标 manifest 已存在时跳过下载后的 push。state、report 和归档应放在彼此不冲突的专用路径，避免把它们放在输出目录的同名/嵌套位置。

### 重新推送到 Registry

```bash
dm pull alpine:latest --to registry.example.com/team --output-dir ./images
dm pull --file images.txt --to http://registry.local:5000/team \
  --concurrency 2
```

`--to` 的导入、tag 和 push 由 Docker daemon 完成；目标写成 `http://...` 只会为 `dm` 的目标 registry 预检选择明文协议，最终 Docker push 仍由 daemon 执行。源 registry 的 `proxy`、CA 和认证策略只控制 `dm` 的 HTTP 请求，不能替代 daemon 侧的 registry CA、insecure registry 和代理配置。若源 registry 也确实是明文 HTTP，再显式使用 `--plain-http`；该 flag 会影响源 registry，请不要与默认 HTTPS 的公共镜像列表混用。未配置的 registry 不会自动降级。

## 5. 备份与恢复

### 备份目录和离线包

```bash
dm backup web --output-dir ./backups/web
dm backup web --bundle --bundle-output ./out/web-backup.tar.gz
dm backup web --bundle --encrypt \
  --passphrase-file ./secrets/backup.pass \
  --bundle-output ./out/web-backup.tar.gz
dm backup web api --merge --bundle \
  --bundle-output ./out/batch-backup.tar.gz
```

普通备份目录通常包含 `manifest.json`、`container.inspect.json`、`docker-compose.yml`、网络和 volume 元数据，以及可选的 `images/`。bundle 还包含 `README.md`、`restore.sh` 和 `checksums.txt`；签名 bundle 另有 `checksums.txt.sig`。`--no-image` 只跳过镜像 tar，不会跳过 inspect 或网络/volume 元数据。volume 备份当前只保存 Docker volume 元数据，不包含 volume 内的数据。

`--encrypt`、`--split-size` 和 `--signing-key` 只能与 `--bundle` 一起使用。口令文件、签名私钥和最终 bundle 路径必须位于备份输出目录之外；口令文件应设置为仅当前用户可读。分卷成功的提交标志是 `<bundle>.parts.json` 中 `commit: complete`，且不存在 `.parts.pending.json` 或 staging 残留。

### 恢复流程

推荐按以下顺序操作：

```bash
# 1. 完整性检查；dry-run 会在 checksums.txt 存在时校验
dm restore ./out/web-backup.tar.gz --dry-run --format json

# 2. 使用独立名称在隔离环境恢复
dm restore ./out/web-backup.tar.gz --name web-restored --confirm
```

dry-run 会校验存在的 `checksums.txt`，但缺少该文件时仍可生成计划；实际恢复默认要求完整有效的 `checksums.txt`，只有显式 `--skip-checksum` 才会跳过。实际恢复必须显式 `--confirm`。目标容器已经存在时，默认拒绝；确认过快照和端口后才使用 `--replace`。普通 `--replace` 会临时保留旧容器，候选容器达到 running/healthy 后才提交；若同时使用 `--no-start`，候选不会启动或等待就绪就提交，替换完成后保持停止状态，原先运行中的旧容器仍会先停止并在提交后删除。失败时只清理由本次事务的稳定 ID 和 owner label 证明属于本次事务的候选。

默认会阻止 privileged、host namespace、host bind、device 和部分驱动选项等高风险 HostConfig。只有在独立确认备份来源和目标主机后，才同时使用 `--confirm --allow-dangerous-config`（兼容别名 `--allow-unsafe-host-config`）。`--skip-checksum` 仅适用于已在外部验证备份包内容完整性的兼容场景；checksum 能检测内容变化，但不能证明签发者。需要认证签名者时，提供位于备份根外的 `--trusted-public-key` 验证 Ed25519 对 `checksums.txt` 的签名；签名验证与包内文件 checksum 校验是两个独立步骤。

恢复输入、展开总量、JSON 累计大小和分卷数都有硬上限；`--max-archive-size`、`--max-expanded-size`、`--max-json-size`、`--max-parts` 只能进一步下调，不能上调。恢复前请确认目标端口未被占用、网络/volume 名称符合预期，并保留 dry-run JSON 作为变更记录。
`--trusted-public-key` 与 `--skip-checksum` 不能同时使用。

## 6. 诊断和清理

### 聚合报告

`dm report all` 默认运行 health、network、logs、volumes 和 prune dry-run。可缩小范围并控制日志成本：

```bash
dm report all --include health,network --format json
dm report all --skip logs --max-items 1000
dm report all --log-tail 100 --log-context 2 --log-since 2h
dm report all --volume-size-mode api --prune-only container
```

`--include` 与 `--skip` 接受逗号分隔值；未知名称会失败，同一名称同时出现时由 `--skip` 优先移除，过滤后没有可运行报告才会失败。聚合报告里的 prune 永远是 dry-run，不会删除资源。`health`、`logs`、`network`、`volumes`、`prune` 和 `report all` 这六个自动化报告入口支持 `text`、`json`、`markdown`、`html` 和 `sarif`；其他命令以各自 `--help` 为准。机器消费时使用纯 JSON/SARIF 文件，不要把 verbose 日志混入 stdout。

### 日志、volume 和 helper

单独执行的 `logs` 默认扫描最近 500 行，单独执行的 `health` 默认 100 行；`report all --log-tail` 默认 200 行（同时启用 `--health-logs` 时也用于 health 子报告）。`logs --tail -1`、`health --log-tail -1` 和 `report all --log-tail -1` 表示读取全部日志；`logs --context` 输出命中行前后的上下文。单容器日志预算默认 16 MiB，整条命令默认 256 MiB；达到预算会停止读取并返回运行错误。

`logs --since` 和 `report all --log-since` 可用 `30m`、`2h` 或 RFC3339；`--context` 只属于 `logs` 子命令。

`volumes --size-mode api` 只使用 Docker API。`local-go` 在本机只读遍历，`docker-run` 使用只读、无网络、drop capabilities、`no-new-privileges` 的 helper 容器；默认 helper 是 digest 固定的 busybox，且必须已存在于目标 Docker。使用 `--size-image` 时，调用方必须自行固定并信任镜像。命令取消或失败后应检查 `com.docker-manager.volume-probe`，不应有残留 helper。

### Prune 安全流程

```bash
dm prune --only container --filter 'label=env=test' --format markdown
dm prune --only container --filter 'label=env=test' \
  --protect-label keep --apply --confirm
```

默认只生成候选报告。`--apply` 必须再配合 `--confirm`；`--only`、`--filter`、`--until` 和 `--protect-label` 应尽量固定到测试 label。Docker API 对 image/volume 没有 compare-and-delete：只要候选包含 image 或 volume，还必须显式 `--allow-non-atomic-delete`；缺少该选项时不会执行任何删除。即使传入该选项，inspect 与 delete 之间仍存在竞态，生产环境应在低变更窗口执行并保留报告。

## 7. 配置、环境和多环境

查看最终值和来源：

```bash
dm config show --effective --show-source
dm config show --effective --show-source --format json
dm config profiles
```

优先级如下：

| 范围 | 优先级/说明 |
| --- | --- |
| 配置文件路径 | `--config` > `DM_CONFIG` > 当前目录 `.dm.yaml`；显式不存在即失败 |
| profile | `--profile` > `DM_PROFILE` > `default_profile` > 不选择 profile |
| 普通字段 | 命令 flag > 选中 profile 中显式字段 > 顶层配置 > 环境变量/内置默认值 |
| Docker endpoint | `--docker-*` > profile > 顶层配置 > `DOCKER_*` > 本机默认 endpoint |
| Registry proxy | 命令 flag > 精确 registry policy > `.dm.yaml proxy`/标准 proxy 环境变量 |

profile 只覆盖显式出现的字段；显式空字符串、`false` 和 `[]` 可以清除继承值。配置加载是严格 schema：未知字段、错误类型、YAML 多文档、非 mapping 根节点和非法值都会失败。建议先在测试 profile 中验证，再切换生产 profile：

```bash
dm --profile staging doctor --check-e2e=false --format json
dm --profile staging pull alpine:latest --output-dir ./images/staging
```

常用环境变量：

| 变量 | 用途 |
| --- | --- |
| `DM_CONFIG` | 默认配置文件路径；显式设置后文件缺失会失败 |
| `DM_PROFILE` | 选择 profile，优先于 `default_profile` |
| `DOCKER_HOST` | Docker daemon endpoint（`tcp://`、`unix://`、`npipe://`） |
| `DOCKER_TLS_VERIFY` | TCP daemon 的服务端证书校验开关 |
| `DOCKER_CERT_PATH` | `ca.pem`、`cert.pem`、`key.pem` 目录 |
| `DOCKER_API_VERSION` | 固定 Docker API 版本；省略则自动协商 |
| `DOCKER_CONFIG` | Docker `config.json` 所在目录 |
| `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` | `dm pull` 和 registry 直连检查使用的标准代理变量 |
| `SSL_CERT_FILE`/`SSL_CERT_DIR` | `doctor` 检查的系统 CA 提示；不会替代 per-registry CA 配置 |
| `XDG_CONFIG_HOME`/`XDG_DATA_HOME` | Linux 非 root 安装器的默认目录 |
| `DM_HOME`/`DM_OUTPUT_DIR` | 安装器写入的环境提示；真正的配置和输出仍以 `DM_CONFIG`/命令参数为准 |

Docker daemon 的 TCP 代理和 registry 直连代理是两条独立链路。配置中的顶层 `proxy` 不控制 daemon client；`pull --to` 的最终 push 也不使用 `dm` 的 registry CA 来替代 daemon 信任目录。

## 8. 输出、审计和退出码

`health`、`logs`、`network`、`volumes`、`prune` 和 `report all` 支持 `--format text|json|markdown|html|sarif`；`backup`、`restore`、`doctor`、`registry`、`tree` 等其他命令不提供 SARIF，具体格式以各自 `--help` 为准。未启用门禁时，JSON 是单一可解析文档；SARIF 固定为 2.1.0。`--fail-on none|note|warning|error` 和可重复的 `--threshold metric=max` 只在报告成功生成后评估。

进程退出码：

| 代码 | 含义 |
| ---: | --- |
| `0` | 命令成功，或报告生成且门禁通过 |
| `1` | 配置、连接、执行、渲染、审计或参数等运行错误 |
| `2` | 报告已生成，但 `fail-on`/`threshold` 门禁未通过 |
| `130` | 收到 SIGINT 或 context cancel |

运行错误和门禁失败同时出现时返回 `1`；取消优先返回 `130`。需要 JSON 日志时使用全局 `--log-json`，它不会改变业务报告的 `--format`。

启用 `audit_file` 或 `--audit-file` 后会写 JSONL lifecycle。`safe` 只保留 HMAC 标识和错误分类，`full` 才保留经过 strict 脱敏和长度限制的显示值。`audit_on_error=deny-mutation` 允许只读命令继续，但在授权事件无法落盘时拒绝 mutation；`fail`/`--audit-required` 会拒绝无法完整落盘的命令。审计目录、key、lock 和轮转文件应放在受限目录，不能随报告或 bundle 发布。

## 9. 安装、升级和回滚

1. 下载与平台匹配的归档，使用 `checksums.txt` 校验 SHA-256。
2. 阅读包内 `INSTALL.md`；Linux 使用 `scripts/install.sh`，Windows 使用 `scripts/install.ps1`，Darwin 手动安装二进制。
3. 安装后运行 `dm version` 和 `dm doctor --check-e2e=false`。
4. 升级时重复运行安装脚本；默认保留已有配置，只有显式 `--overwrite-config`（Linux）或 `-OverwriteConfig`（Windows）才替换配置文件。
5. 卸载默认保留配置和数据；确认不再需要后才使用 `--purge`/`-Purge`。升级失败时可安装上一版本归档，先执行 `dm version` 和只读 doctor，再恢复业务。

安装器会拒绝通过 symlink、junction/reparse 或特殊文件越过安装边界。不要把 config/data 指向系统根、用户根、共享挂载或包含其他应用的目录；Windows 多安装时不要手工删除 `install.json`，否则卸载会按失败关闭处理。

## 10. 常见问题

| 现象 | 检查和处理 |
| --- | --- |
| `config file ... does not exist` | 检查 `--config`、`DM_CONFIG`；隐式 `.dm.yaml` 才允许缺失并使用默认值 |
| `unknown field` 或 `multiple YAML documents` | 对照源码仓库的 `.dm.yaml.example`（发布包为 `dm.yaml.example`），删除未知键、YAML merge key 和第二个文档 |
| Docker endpoint 连接失败 | 运行 `dm config show --effective --show-source`；核对 `DOCKER_HOST`、证书目录和 `dm doctor` 的 transport |
| TLS 启用但证书缺失 | `docker_tls_verify=true` 不能回退到明文；准备完整 `ca.pem/cert.pem/key.pem` 或显式关闭该配置后重新评估风险 |
| registry 401/403 | 先运行 `dm registry host:port --format json`；检查 `DOCKER_CONFIG`、credential helper、`credential_scope` 和 auth realm |
| registry URL 被拒绝 | 参数只接受规范化 `host[:port]`；不要传 `https://`、路径、userinfo、query 或 wildcard |
| pull 被预算/超时中止 | 调整单镜像 `--max-*`、`--timeout`、`--total-timeout` 或 profile 的日志/运行预算；只能在硬上限内取值 |
| restore 报 checksum mismatch | 重新取得完整 bundle，或先在外部验证备份包内容后再考虑 `--skip-checksum` |
| restore 报签名验证失败 | 确认 `checksums.txt.sig` 与 `checksums.txt` 匹配，并使用签发方对应的 `--trusted-public-key`；checksum 本身不认证签名者 |
| restore 报容器/端口冲突 | 先 dry-run，改用唯一 `--name`，确认端口和网络后再 `--replace` |
| prune 不执行删除 | 检查是否同时提供 `--apply --confirm`；image/volume 候选还需 `--allow-non-atomic-delete` |
| 报告 JSON 无法解析 | 不要把 stderr/verbose 合并到 stdout；使用 `--format json`，需要日志时另行重定向 stderr |
| completion 没有动态候选 | 静态 shell completion 仍可用；检查当前 profile 的 Docker endpoint 和 daemon 权限，必要时运行 `dm completion <shell>` 重新生成 |

## 11. 相关文档

- [README.md](../README.md)：功能总览、完整配置表、命令速查和安全边界。
- `.dm.yaml.example`（源码仓库）/ `dm.yaml.example`（发布包）：可复制的配置模板和字段注释。
- [TESTING.md](TESTING.md)：本地、远程 Docker、registry 和外部平台验收方法。
- [RELEASE_CHECKLIST.md](RELEASE_CHECKLIST.md)：发布、安装、审计和回滚核对项。
- [DOCKER_API_MIGRATION.md](DOCKER_API_MIGRATION.md)：Docker client/API 迁移说明。
