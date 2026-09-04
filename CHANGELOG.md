# Changelog

本文档记录 `docker-manager` 已发布版本的功能、修复、结构调整、已完成优化和已知非阻断项。临时优化清单已归档到本文档，后续不再维护 `OPTIMIZATION_AND_EXTENSIONS.md`。

## Unreleased

### 配置与多环境

- 新增命名 profile/context，可在同一配置文件中保存多套 Docker endpoint、TLS、proxy、registry CA 和输出目录，并通过 `--profile`、`DM_PROFILE` 或 `default_profile` 切换。
- profile 按字段覆盖顶层 base 配置，保留显式空字符串、`false` 和空列表语义；配置来源输出可区分 base、profile、环境变量和命令行参数。
- 新增精确 `host[:port]` 匹配的 per-registry policy，支持独立 CA、proxy/no-proxy、timeout、凭据操作范围、Bearer realm allowlist 和显式 plain HTTP。
- Registry policy 隔离应用于 pull 源、批量镜像、目标 registry 预检、`dm registry` 和 `dm doctor --registry`；Docker daemon 执行 push 时仍使用 daemon 自己的 CA/代理配置。
- Pull 与 diagnostics 共用有界 registry CA 加载器：拒绝链接/reparse、特殊文件和混合 PEM，并限制单文件 16 MiB、目录 256 项及累计 32 MiB。

### 规模化运行和自动化

- 新增一次命令共享的运行 controller：`operation_concurrency`、`operation_timeout`、`operation_rate_limit`、`operation_max_items` 分别控制只读任务并发、整条命令外层 deadline、任务启动速率和跨资源类型累计 item 预算；diagnostics、backup、reverse/rerun 提供对应命令 flag，显式 flag 优先于 profile/base 配置。
- 将累计 item 预算接入 health/logs/network/volumes/prune/doctor/tree/diff、backup、reverse/rerun 和 pull 批量入口；需要修改 Docker 或发布文件的路径在工作开始前预留完整目标预算，超限时失败关闭。
- health、logs 和 report all 新增流式日志读取预算，默认每个容器 `16 MiB`、整条命令 `256 MiB`，支持 `max_log_bytes`/`max_total_log_bytes` 和命令 flag 覆盖，硬上限分别为 `256 MiB`/`4 GiB`。
- Docker multiplex 日志按固定大小缓冲区解复用并在底层读取前预留剩余预算，避免全量 raw/stdout/stderr 多份驻留、并发超读及恶意超大 frame 触发等尺寸内存分配。
- health、logs、network、volumes、prune 和 report all 新增统一 findings/metrics 模型、`--fail-on none|note|warning|error`、可重复 `--threshold metric=max` 及 SARIF 2.1.0 输出；未启用策略时保持既有输出，JSON/机器输出不混入文本尾部。
- 统一自动化退出码：成功 `0`、运行/配置/输出/审计错误 `1`、仅报告策略门禁失败 `2`、SIGINT/context cancel `130`；组合错误保持运行错误优先。
- 新增可选 JSONL 审计 lifecycle，记录系统/声明操作人、profile、HMAC endpoint、分页候选集、mutation 授权/拒绝、结果、耗时和错误分类。`safe` 默认只保留脱敏标识，`full` 额外保留经过 strict 脱敏和长度限制的详情。
- 审计失败支持 `warn`、`deny-mutation`、`fail`（及 `--audit-required`）策略；文件 sink 使用 Unix 私有模式、跨进程锁、完整事件边界轮转、持久 HMAC key，并拒绝数据/key/lock/rotation 路径中的 symlink、junction/reparse 和非普通文件；Windows 访问边界由部署目录 ACL 配合约束。
- 配置 threshold 改为严格 `scope.metric=max` 命名空间；补齐 doctor 超时/取消传播、pull batch state/report 发布前审计授权、同一 root 重复执行隔离，以及显式审计 flag 对参数/flag/配置加载失败的 lifecycle。
- 本地全量门禁、规模化运行回归和 Docker 只读验收已完成；不同环境与构建批次的详细范围统一记录在 `docs/TESTING.md`，不在此处展开内部运行信息。

### Linux 安装和 Darwin 发布边界

- `scripts/install.sh`/`uninstall.sh` 明确只支持 Linux，非 Linux 系统会提前拒绝；Darwin 归档不再包含这两个脚本，包内 `INSTALL.md` 改为二进制手动安装/卸载步骤。Linux 包继续携带 shell 脚本，Windows 包继续携带 PowerShell 脚本。
- Linux `install.env` 升级为私有 manifest v3，并由卸载器按 allowlist 和严格 quoting 规则作为数据解析，不再 source。manifest 的 128-bit token 与 config/data 两个 `.docker-manager-managed` marker 绑定；purge 要求三者的版本、角色、绝对路径和 token 一致。v2 可继续执行保留 config/data 的普通卸载，但 purge 必须先重新安装迁移到 v3。
- config/data 安装路径拒绝系统/用户/XDG 宽根目录、彼此重叠或包含安装产物；purge 在首次删除前预检两个完整目录树，并通过 `/proc/self/mountinfo` 拒绝目标本身及后代 mount，以及 `root != /` 的祖先 bind/subtree view。对 `root=/` 祖先仅在当前 namespace 可见的重复 `(major:minor, fstype, source)` 身份下拒绝，以保留独立文件系统挂载兼容性；source 已卸载或被其他 namespace 隐藏时无法可靠区分 root bind，属于已记录残余边界。symlink、特殊文件和外来 owner 仍失败关闭。
- root profile 只在内容精确匹配时覆盖/删除，普通用户 profile 拒绝畸形 marker；卸载侧在替换前复核 inode、内容和 mode，安装侧当前复核 inode。普通用户 profile 的 mode 显式恢复；其 ACL/xattr 在 GNU `cp --preserve=all` 路径会保留，`cp -p` 回退仅为尽力保留，不声明跨环境保证。
- `install.sh --build --dry-run` 不再要求 Go，也不创建 build/install 目录或目标文件；Linux installer、manifest v3、completion 和 Bash 兼容性定向检查已通过，详细方法见 `docs/TESTING.md`。

### P0-P2 回查修复

- 单次 pull 与 batch 现在共用归档输出目录生命周期 scope，batch 内部并发归档复用已持有的锁；不同 metadata 文件但共享输出目录的 batch，以及 standalone 与 batch，不能并发覆盖同一归档。锁定期间持续复核目录和 lifecycle lock marker 身份；Unix 锁定已打开的父目录 inode，Windows marker handle 禁止 delete sharing，unlink/替换 marker 不能绕过活跃 scope。
- Pull batch 在审计授权后、读取 resume state 和任何 registry/Docker 回调前获取 state/report 生命周期锁；安全读取 state 时锚定父目录、拒绝链接/reparse/非普通文件并限制为 `64 MiB`。state/report 使用随机排他 `0600` staging、发布前身份复核和 Unix 目录同步；路径精确冲突、hardlink、大小写碰撞、祖先/后代拓扑及辅助锁冲突均在回调前失败。`.dm-pull-`/`.docker-manager-pull-` 在原始 basename 和 Windows canonical/实际 8.3 basename 上均为内部保留 namespace；standalone 在首个 HTTP 请求前拒绝，batch 在 callback 前拒绝。Windows 还拒绝设备/扩展 namespace、设备名、尾点/尾空格、unsafe UNC server/share、尚不存在且形似 DOS 8.3 短名的组件和启用 per-directory case sensitivity 的现存父目录；已存在祖先解析成长路径后，仅在检测到实际 short-name alias 冲突时拒绝。
- Tar 归档在同目录私有 staging 完成 close 和文件 sync 后 rename；rename 前失败保留旧目标。Unix rename 后父目录 sync 失败会返回错误且 batch 不记录 success，但新归档可能已经发布；Windows 的 `0600` 不等价于私有 ACL，rename 也不声明具有与 Unix 相同的文件系统语义。
- Resume state 的成功项新增版本化 fingerprint：归档绝对路径、大小、SHA-256、目标 OS/arch 和 effective Docker load。旧 state 缺 fingerprint、归档缺失/篡改，或路径、平台、load 语义变化时均安全重拉并迁移；成功回调未产生可验证归档时不会记录 success。
- State commit protocol 升至 v2。写 state 前持久化固定 `.dm-pull-state-untrusted.marker`，payload 为版本、state basename 原始字节 hex 和 128-bit transaction；state rename/目录同步成功后按 marker 文件身份、transaction 和 owner 清理。残留 marker 或 protocol 0/1 success 保守重拉；其他 owner、畸形、超限或被替换 marker 在 pull/exists callback 前失败关闭，marker 创建/清理持久化失败不会被误报为可信 success。
- Pull batch 的 state/report 持久化按 `none/basic/strict` 脱敏，state 写失败不会在后续发布中回写为假成功；deadline report 包含全部已计划项，包括尚未调度项。Report I/O 后重新检查 context，state、report 和 context 错误通过 `errors.Join` 同时保留；context canceled 分支继续不发布 report。
- Backup/restore 的 `--max-items` 统一累计 container 与去重后的 custom network、named volume；backup 在任何输出前复核已计费容器身份和资源集合，restore 多输入 text/structured dry-run 在产生 stdout 或访问 Docker 前完成全部预算预留，并复用已验证 manifest 快照。
- 顶层、profile、per-registry、显式和环境 registry proxy 均要求允许的 scheme 与非空 hostname；配置中的空白值、首尾空白和 fragment 会早期拒绝。HTTPS auth realm、Bearer challenge/allowlist 及带 scheme 的 `--to` 同样拒绝空 hostname，doctor 对非法环境代理给出 warning。非法 audit rotation 在打开 sink 前固定返回退出码 `1`，不受 `warn` 降级；`doctor` 不接受位置参数，`load`/`save` 最多接受一个路径。
- E2E registry 数据改用带归属标签的显式 named volume，清理前复核归属并使用 `docker rm -fv --`，避免匿名 volume 泄漏。
- 修复确认式目录 restore 预检失败时的临时快照泄漏：`prepareRestoreBackup` 现在在返回错误后仍保留并执行已创建 snapshot cleanup；新增隔离 `TMPDIR` 回归，普通 `-count=20`、race 及服务器不访问 Docker 的定向回归均通过。
- 最终扩展受影响 regex 在 Windows normal/race 分别通过（12.631s/13.344s），5 个 marker 高风险用例 `-count=20` 通过（14.941s）；3 个 deadline 用例改用确定性 deadline context 后 normal/race 各 `-count=50` 通过（22.994s/45.240s）。Pull vet、staticcheck v0.8.1、变更 Go 文件 gofmt 和 `git diff --check` 均通过；gosec v2.28.0 全仓为 52 项已分诊告警（含 Windows API 必需的 3 条 G103），新增 G115 已归零。
- 空 hostname URL 边界在 appconfig、pull、diagnostics 的新增用例中通过 normal `-count=50` 和 MinGW race `-count=20`；三包全部 `Proxy|Realm|PushTarget` 用例及 CLI 四条配置消费路径的 normal/race 通过，四个受影响包的 vet、staticcheck v0.8.1 和 Linux/amd64 交叉编译通过。
- 本地全仓检查、覆盖率门禁、安装/卸载 smoke、PowerShell completion 和五平台发布打包均已通过；归档器回归覆盖 tar/zip mode、路径安全、符号链接拒绝和 Unix 执行位保留。尚未覆盖的 Docker 版本矩阵、真实 TLS、企业 registry/OIDC、真机运行和破坏性 E2E 继续作为发布前外部门禁。

### 维护与依赖

- 将项目构建基线升级到 Go 1.27.0。
- 将 Moby client 更新到 v0.5.1、Docker connections 更新到 v0.8.1、compress 更新到 v1.19.2。
- 将项目 YAML v3 import 从停止维护的 `gopkg.in/yaml.v3` 迁移到 `go.yaml.in/yaml/v3` v3.0.5。
- 将 OpenTelemetry HTTP instrumentation 更新到 v0.70.0，OpenTelemetry 核心模块更新到 v1.45.0，并同步更新实际构建链中的传递依赖。
- 将 staticcheck 更新到支持 Go 1.27 的 v0.8.1。
- 将 GitHub Actions checkout、setup-go 和 upload-artifact 更新到 v7。
- 发布脚本统一使用仓库内置的 Go 归档器生成 tar.gz/zip，统一执行路径、符号链接和文件权限校验，不再依赖宿主机的 `zip` 或 `tar` 打包命令。

## v2.2 - 2026-07-09

GitHub Release:

- Tag: `v2.2`
- Commit: `23b8b34737c398c08af76fa75c6f8ae36b1ed326`
- GitHub 发布时间: `2026-07-09T01:28:26Z`
- Release 摘要: 完整迁移 Docker API，整理代码，部分耗时命令进行并发化优化。

提交范围: `v2.1..v2.2`

### 重点变化

- 完成只读报告类和预览类路径的并发优化:
  - `dm report all` 并发执行 health、network、logs、volumes 和 prune 子报告，最终仍按用户选择顺序聚合输出。
  - `dm reverse` 的容器 inspect、volume/network metadata inspect 接入 context 和有界并发。
  - `dm tree` / image tree 对关联容器 inspect 使用有界并发。
  - `dm backup --dry-run` 的 network/volume metadata 检查改为有界并发。
  - `dm restore --dry-run` / 结构化恢复计划中，多容器计划、端口冲突扫描、network/volume 差异预览改为有界并发。
- 保持下载、导入、导出类命令的保守并发策略:
  - 未扩大 `pull/save/load` 默认并发。
  - `pull` 仍保留单镜像 layer 下载并发和批量模式显式 `--concurrency`。
  - `save/load` 与实际 `restore` 默认保持串行，避免磁盘 IO、Docker daemon 和破坏性操作风险。
- `dm prune` 增强:
  - 支持 `--only container|image|volume|build-cache` 限定资源类型。
  - prune 预览和 apply 路径继续保留安全边界，`--apply` 必须配合 `--confirm`。
- 统一并发和取消行为:
  - 新增并复用 `internal/parallel` 的 error-returning 有界并发 helper。
  - 补齐 context 传递，取消时尽量停止剩余只读任务并返回清晰错误。
  - e2e 增加取消测试模式，覆盖 pull、backup、restore、logs、prune dry-run、reverse 等路径。
- 报告输出增强:
  - 新增 `dm report all` 聚合报告。
  - 报告聚合输出支持 text/json/markdown/html。
  - 敏感信息脱敏策略扩展为 `none|basic|strict`，管理员场景默认不脱敏，显式开启时再处理。
- 备份包能力增强:
  - 恢复前差异预览和结构化恢复计划输出。
  - 支持恢复计划中检查目标镜像、network、volume、端口冲突和容器冲突。
  - 支持备份包可选加密、zstd 压缩相关归档支持和分卷归档选项。
- 代码结构继续收敛:
  - 拆分 `backup`、`diagnostics`、`reverse` 中的大文件，降低单文件复杂度。
  - 抽取共享 command flag、target selection、docker config merge、敏感信息处理和 report defaults。
  - 收敛快捷命令与完整命令的构造方式，避免 flag 漂移。
- Registry sync 扩展曾在开发过程中尝试实现，但已通过 revert 撤回:
  - 当前主线继续保留 `dm pull --to` 的轻量镜像搬运能力。
  - 保留 `dm registry` / `dm report registry` / `dm doctor --registry` 的检查能力。
  - 不在 v2.2 主线保留完整 registry sync 产品化命令，避免偏离工具核心边界。

### 验证

- `go test ./...` 通过。
- `go vet ./...` 通过。
- `CGO_ENABLED=1 go test -race ./...` 通过。
- 使用真实 Docker VM 对 `report all`、`backup --dry-run`、`backup --bundle`、`restore --dry-run` 完成定向验证。
- 先前 VM full/destructive、install、cancel、completion 和 Windows 远程 Docker API 验收继续作为 v2.2 基线。

### 主要提交

- `23b8b34` 扩大只读命令并发范围。
- `9b009c2` 拆分文件。
- `ba754ea` 部分代码复用和文件整理。
- `c3d91fa` 收敛命令入口，统一创建命令实例。
- `7902297` Revert registry sync expansion。
- `2211467` 可选加密或分卷归档。
- `f4df638` 备份恢复冲突检查和计划输出。
- `e1a7b75` 报告输出聚合，给出所有报告。
- `197b0dd` 添加 zstd 压缩支持。
- `124ad11` 统一命令的 context 传递。
- `1cb87fa` 进一步统一并发命令的取消行为。
- `91985fd` 优化 prune，允许通过 `--only` 指定资源。

## v2.1 - 2026-07-07

GitHub Release:

- Tag: `v2.1`
- Commit: `e76bb086e8599ea1725603f8ec15cd1fd83c5025`
- GitHub 发布时间: `2026-07-07T07:29:56Z`
- Release 摘要: 迁移 Docker API 调用到新版本 `docker/docker -> moby/moby`。

提交范围: `v2.0..v2.1`

### 重点变化

- 完成 Docker API 依赖迁移:
  - 将业务代码从旧 `github.com/docker/docker` SDK 直接依赖迁移到 Moby 拆分 API/client 模块。
  - 统一 Docker client 工厂和类型转换入口。
  - backup、restore、diagnostics、reverse/rerun、completion、image save/load 等本地 Docker API 调用均完成迁移。
- 从 `go.mod` 中移除旧 Docker SDK 的直接依赖，减少依赖冲突和未来升级风险。
- 补齐迁移文档:
  - 新增并完善 [docs/DOCKER_API_MIGRATION.md](docs/DOCKER_API_MIGRATION.md)。
  - 记录阶段 1-6 的迁移范围、类型变化、验证路径和已知注意事项。
- 修复网络报告中的 IP 输出问题，适配 Moby API 类型变化后的网络/IPAM 字段解析。
- 完成迁移后的测试和验收:
  - 本地单元测试通过。
  - VM full/destructive 测试通过。
  - Windows 侧远程 Docker API 路径验证通过。

### 主要提交

- `e76bb08` 测试，并修复网络 IP 输出问题。
- `1fca504` 修改大量代码，从 mod 文件中删除旧的直接依赖。
- `e302e51` 完成大量的类型、API 迁移。
- `109f991` 同步修改部分 type 以及验证测试。
- `a27269e` 完成大部分命令调用的迁移。
- `b0a0b5b` 进一步迁移 API 封装层。
- `a83a2fa` 添加新依赖替换部分旧的依赖。
- `fc03d37` 列出文档，计划将 `docker/docker` 迁移至 `moby/moby`。

## v2.0 - 2026-07-03

GitHub Release:

- Tag: `v2.0`
- Commit: `dc5c3ac169e3a3dbffcc1407d8c5917c78c494c9`
- GitHub 发布时间: `2026-07-03T03:06:41Z`
- Release 摘要: 优化并添加大量功能，包括镜像操作、资源报告等。

### 发布状态

- 当前无已确认的 P0/P1/P2 阻断待办。
- 本地静态检查已通过: `go test ./...`、`go vet ./...`、`go test -race ./...`、`scripts/check.ps1 -Race`、`git diff --check`。
- 干净 Ubuntu 24.04 / Docker 29.1.3 VM 完整验收通过:
  - `install`: 14 PASS / 5 XFAIL / 0 FAIL。
  - `destructive/full`: 48 PASS / 12 XFAIL / 0 FAIL。
  - 测试后无 `dm_e2e_*` 容器、volume 或测试镜像残留。
- 企业 registry、Harbor、Nexus、Artifactory/JCR、代理/CA、completion、取消行为和中等规模资源场景已完成验收。详细记录见 [docs/TESTING.md](docs/TESTING.md)。

### 新增功能

- 新增 `dm backup` / `dm restore`:
  - 支持容器 inspect、compose、镜像 tar、network/volume 元数据备份。
  - 支持离线迁移包、批量备份、分离包、合并包、包内 README、restore 脚本和 checksum。
  - `restore` 默认先校验 checksum，再接触 Docker。
- 新增 `dm rerun`:
  - 从 `reverse` 中拆分破坏性重建能力。
  - 支持 `--dry-run` 和 `--confirm`。
  - 实际停止、删除并重建容器前必须显式确认。
  - 执行前自动保存容器 inspect JSON。
- `dm reverse` 改为只读命令:
  - 输出 `docker run` 或 compose 配置。
  - 支持批量容器输出。
  - 补齐 labels、dns、dns_search、extra_hosts、cap_add、cap_drop、security_opt、privileged、devices、ulimits、logging 等解析字段。
  - 生成 `docker run` 时增加 shell quoting。
- 新增诊断报告命令:
  - `dm health`
  - `dm network`
  - `dm logs`
  - `dm diff`
  - `dm prune`
  - `dm volumes`
  - `dm registry`
  - `dm doctor`
- 新增镜像分析命令:
  - `dm tree` / `dm image tree` 展示 RootFS layer、构建历史、每层大小占比和最大 layer 排名。
- 新增 `dm version`，构建脚本通过 ldflags 注入 version、commit 和 build date。
- 新增 `.dm.yaml` 和 `DM_CONFIG` 配置支持。
- 新增 Docker API endpoint 配置:
  - 支持 `DOCKER_HOST`、`DOCKER_TLS_VERIFY`、`DOCKER_CERT_PATH`、`DOCKER_API_VERSION`。
  - 支持 `.dm.yaml` 的 `docker_host`、`docker_tls_verify`、`docker_cert_path`、`docker_api_version`。
  - 支持全局参数 `--docker-host`、`--docker-tls-verify`、`--docker-cert-path`、`--docker-api-version` 覆盖配置。
- 新增 bash、zsh、fish、PowerShell completion。
- Shell completion 的容器、镜像和 volume 候选会按当前 Docker endpoint 查询。

### 镜像能力增强

- `dm pull` / `dm image pull` 支持:
  - `--output`
  - `--output-dir`
  - `--load`
  - `--to <registry-or-prefix>`
  - `--file`
  - `--concurrency`
  - `--retries`
  - `--resume`
  - `--skip-existing`
  - `--report`
  - `--plain-http`
  - `--docker-config`
  - `--proxy`
  - `--verbose-http`
- 修复镜像名解析逻辑，避免误判带端口的 registry，例如 `localhost:5000/nginx:latest`。
- 使用 manifest `mediaType` 或响应 `Content-Type` 判断单架构 manifest 与多架构 index。
- 支持未压缩 tar layer，避免从 Docker 29 本地 registry 拉取镜像时固定按 gzip 解压。
- 下载 layer 后校验 digest，避免生成损坏或不可信的镜像 tar。
- `image pull` 失败时向命令层返回非零退出码，便于脚本和 CI 判断失败。
- 默认使用环境变量代理，未设置则直连，并支持 `--proxy` 强制指定代理。
- 支持匿名 registry、Basic challenge、Bearer token challenge、Docker config `auths`、`credHelpers` / `credsStore`。
- `image pull --to` 支持 `http://` / `https://` 目标前缀解析；源 registry 的 `--plain-http` 与目标 registry 协议语义已拆分。
- `image pull --to` 增加认证和推送前检查，提前发现目标 registry 连通性或凭据问题。
- `image pull` 网络失败不再 panic，底层 HTTP 日志默认降噪，仅 `--verbose-http` 输出。
- `image load` 只导入 `.tar`、`.tar.gz`、`.tgz` 镜像文件。
- `image load`、`image save`、`image pull` 增加进度输出和最终汇总。
- `image save` 支持按镜像名、tag、ID、digest、label 和通配符筛选导出，并支持 `--dry-run`。
- 修复 `image save --merge` 输出路径，尊重用户传入的 `[path]`。
- `image save` 批量导出时聚合错误，任意镜像导出失败后命令返回非零退出码。

### 报告和安全边界

- 为报告类命令增加统一 `--format text|json|markdown|html`。
- 新增 `dm prune` / `dm report prune` 和 `--apply --confirm`，支持 `--only`、`--filter`、`--until`、`--protect-label` 安全边界。
- 默认处理全部本地资源的命令增加数量提示，覆盖 `reverse`、`health`、`network`、`logs`。
- 容器、镜像和 volume 筛选统一接入 `internal/resourcefilter`，支持 keyed filter、通配符、大小写不敏感匹配、前缀匹配和候选字段生成。
- 补齐筛选语法文档和示例，覆盖容器、镜像、volume 和 prune filter 差异。
- `network` 深度关联 `NetworkInspect` 和 `ContainerInspect`，补齐 network ID、labels、options、IPAM subnet/gateway、IPv6、attachable、ingress、endpoint ID、gateway、driver opts、仅暴露未发布端口等字段。
- `health` 增加容器镜像、网络、挂载、端口、日志驱动和日志可读性字段。
- `volumes` 增加 volume 大小统计和容器引用关联:
  - 本地 Linux 优先 Go 原生统计。
  - 远程 Docker 或 Docker Desktop 自动回退到 helper 容器。
- `diff` 支持 `--redact-secrets`，分享报告时可脱敏 env、label、cmd、entrypoint、healthcheck 和 log config 等字段。
- 管理员场景默认不脱敏，保留显式脱敏选项。
- `backup`、`restore`、`logs`、`prune` 的 SIGINT/context cancel 行为已补强:
  - Docker API 调用、归档、解压、checksum、日志扫描和 prune 汇总路径会向上返回 `context.Canceled`。
  - CLI 统一输出 `操作已取消` 并返回 130。
- 远程 Docker 安全提示增强:
  - 只读报告输出 `docker_endpoint` / 来源 Docker。
  - `rerun`、`restore`、`prune --apply` 执行前输出目标 Docker endpoint。
  - 未确认错误也包含远程地址。

### 命令树整理

- 命令命名空间收敛:
  - 镜像类: `dm image pull/save/load/tree`
  - 报告类: `dm report health/network/logs/diff/prune/volumes/registry`
  - 容器类: `dm reverse`、`dm rerun`、`dm backup`、`dm restore`
- 为常用叶子命令保留二级入口:
  - `dm pull`
  - `dm save`
  - `dm load`
  - `dm tree`
  - `dm health`
  - `dm network`
  - `dm logs`
  - `dm diff`
  - `dm prune`
  - `dm volumes`
  - `dm registry`
- 合并 volume 报告命令: `dm volume ls-unused` 移入 `dm report volumes`，删除顶层 `volume` 入口。
- 删除未发布版本中的旧兼容命令名:
  - `logs-scan`
  - `inspect-diff`
  - `prune-report`
  - `registry-login-check`
- 删除兼容 flag 和位置参数:
  - 全局 `--json`
  - `backup --output`
  - `backup --include-image`
  - `backup [legacy-backup-dir]`
  - `reverse --filter-default-envs`
  - `reverse --merge-ports`

### 构建、安装和发布

- 新增开发构建脚本:
  - `scripts/dev-build.sh`
  - `scripts/dev-build.ps1`
- 新增发布打包脚本:
  - `scripts/package-release.sh`
  - `scripts/package-release.ps1`
- 新增安装/卸载脚本:
  - `scripts/install.sh`
  - `scripts/install.ps1`
  - `scripts/uninstall.sh`
  - `scripts/uninstall.ps1`
- 安装脚本支持自定义安装目录、配置目录、数据目录、环境变量、completion、dry-run 和 purge 卸载。
- Windows 安装入口改为直接安装 `dm.exe`。
- 发布包按平台裁剪脚本:
  - Linux 包只包含 shell 安装/卸载脚本。
  - Windows 包只包含 PowerShell 安装/卸载脚本。
  - Darwin 包不包含平台不兼容的安装/卸载脚本，按包内 `INSTALL.md` 手动安装二进制。
- 发布包包含二进制、`README.md`、`LICENSE`、`dm.yaml.example` 和 `INSTALL.md`；Linux/Windows 包另外包含对应平台的安装及卸载脚本。
- `scripts/package-release.*` 生成按平台命名的归档、`checksums.txt`、`release-manifest.json`、`release-summary.md` 和包内 `INSTALL.md`。
- `checksums.txt` 会保留仍存在于发布目录中的历史归档校验行，重新生成同名归档时自动替换该行，便于回滚核验。
- 新增轻量静态检查脚本 `scripts/check.sh` 和 `scripts/check.ps1`。
- 新增 Windows 本地 smoke 脚本 `scripts/local-test.ps1`。
- `scripts/e2e.sh` 增加 `smoke`、`full`、`destructive`、`install` 分层执行模式。
- `scripts/e2e.sh` 在 full/destructive 模式中增加 Docker runtime 前置探针，创建或启动测试容器超时时直接报告环境阻塞。
- `go.sum` 不再被 `.gitignore` 忽略，源码发布和 CI 构建可以锁定依赖校验。

### 代码结构和维护性

- 拆分 `internal/commands/pull`，按命令构建、runner、registry/auth、下载、归档、mirror、代理和类型定义划分文件。
- 拆分 `internal/commands/backup`，按命令构建、备份执行、恢复、归档、checksum、bundle artifacts、Docker service 和类型定义划分文件。
- 拆分 `internal/commands/diagnostics/prune`，将 prune 类型、服务执行和文本输出从命令构建中分离。
- 拆分 `internal/commands/diagnostics/doctor`，按命令入口、类型、Docker service、配置/代理/CA 检查、Docker 检查、磁盘检查、registry/toolchain 检查和输出汇总划分文件。
- 拆分 `diagnostics` 中 `health`、`network`、`volume` 的文本输出和 Docker service 适配。
- 抽取 `internal/appconfig`，统一 `.dm.yaml`、`DM_CONFIG` 和 Docker endpoint 默认配置解析。
- 抽取 `internal/commandflags`，统一命令层共享 flag 与补全注册，避免 `internal/report` 依赖 Cobra。
- 抽取 `internal/runconfig`，让 reverse/rerun 和 backup 共享容器 inspect 到 `docker run` / compose 的解析模型。
- 抽取 `internal/registryauth`，统一 Docker config、auths、credential helper 和基础认证 header 解析。
- 抽取 `internal/textfmt`，统一字节大小和下载速率格式化。
- 拆分 `internal/report/render.go`，将 markdown/html 渲染与共享反射格式化工具分离。
- 收敛 `image pull` 包级全局状态，新增 `PullRunner` 以便命令执行和测试注入依赖。
- 抽象统一 Docker client 工厂，让 backup、report、volume、image tree、registry 等命令复用统一入口。
- 补齐关键复杂流程注释，覆盖 `PullRunner`、registry 认证、pull mirror、backup manifest 兼容、restore 替换安全边界和 prune apply 确认边界。
- 清理无信息量的历史 `@Author` / `@Date` 文件头注释。
- 修复历史乱码注释和用户可见日志，README 重写为 UTF-8。

### 文档整理

- README 精简为项目功能、构建、安装、配置和命令说明，不再展开测试报告。
- 新增 `docs/TESTING.md`，集中维护本地检查、远程 Docker 验收、企业 registry 验收和历史测试结论。
- 精简 `docs/RELEASE_CHECKLIST.md`，只保留发布操作核对项。
- 删除冗余的远程测试跳转文档，远程测试正文统一维护在 `docs/TESTING.md`。
- 删除临时功能扩展清单 `OPTIMIZATION_AND_EXTENSIONS.md`，已完成项归档到本 changelog。

### 已知非阻断项

- linux/arm64、darwin/amd64、darwin/arm64 已完成交叉编译产物生成，但尚未做真机运行验证。
- 真实 Harbor OIDC 登录、权限映射和审计链路仍需 Keycloak 完整部署或企业 OIDC 环境复测。
- 数百级大规模资源压测建议在专用环境单独运行。
