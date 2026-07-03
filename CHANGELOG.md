# Changelog

本文档记录 `docker-manager` 当前发布候选版本的功能、修复、结构调整、已完成优化和已知非阻断项。临时优化清单已归档到本文档，后续不再维护 `OPTIMIZATION_AND_EXTENSIONS.md`。

## v2.0.0 - 2026-07-03

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
  - Linux/macOS 包只包含 shell 安装/卸载脚本。
  - Windows 包只包含 PowerShell 安装/卸载脚本。
- 发布包包含二进制、`README.md`、`LICENSE`、`dm.yaml.example`、`INSTALL.md`、安装脚本和卸载脚本。
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
