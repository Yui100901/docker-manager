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

## Linux 安装器定向回归

`scripts/install.sh` 和 `scripts/uninstall.sh` 当前只支持 Linux；两者必须在非 Linux 系统上提前拒绝。Darwin 发布归档只包含 `dm` 二进制、配置示例和文档，不得包含 Linux shell installer；包内 `INSTALL.md` 必须给出 `install -m 0755` 和手动删除二进制的步骤。Linux/Windows 发布包仍分别携带 shell/PowerShell 安装脚本。

Linux installer 变更后至少覆盖以下边界：

- `install.sh --build --dry-run` 在 `PATH` 中没有 Go、源码目录不存在 `bin/` 时也应成功输出计划，并保持源码树、prefix、config 和 data 路径均未创建。
- 新安装生成 mode `0600` 的 `install.env` manifest v3；卸载器将其作为有大小上限、key allowlist、无重复字段、严格 quoted value 的数据读取，绝不 shell source。config/data 各有 mode `0600` 的 `.docker-manager-managed` marker，且角色、绝对路径、uid 和 128-bit token 必须与 manifest 匹配；manifest 中的 gid 目前只做十进制格式校验，不作为 marker 归属证明（setgid 父目录可能使实际 gid 不同）。
- v2 manifest 可执行普通卸载并保留 config/data；`--purge` 必须失败且在任何删除前提示重新运行 `install.sh` 迁移到 v3。可执行语句、未知/重复 key、公开 mode、畸形 value、marker 被替换或 token/path 不匹配均应失败关闭且不执行注入内容。
- config/data 拒绝 `/root`、`/etc`、`/var/lib`、用户/XDG 根等宽路径，拒绝彼此重叠和包含 prefix/bin/libexec/completion/profile。purge 必须先完整预检两个目标树，再删除 wrapper/profile；目标本身或任一后代是 mount，或祖先是 `root != /` 的 bind/subtree view，均应保持安装不变。对 `root=/` 祖先只在当前 namespace 可见的重复 `(major:minor, fstype, source)` 身份下拒绝，独立文件系统单一挂载应继续可用；source 已卸载或被其他 namespace 隐藏时 mountinfo 无法可靠区分 root bind，需作为残余风险记录并要求生产 purge 前稳定挂载拓扑。树内存在 symlink、特殊文件、不可遍历目录或其他 uid 所有的条目时同样保持安装不变。
- root profile 只接受受管内容的精确匹配；普通用户 profile 的 marker 缺失、重复、嵌套或次序错误应在 mutation 前拒绝。卸载侧对 snapshot 后和发布前的 inode、内容、mode 变化再次 fail-closed；安装侧当前发布前复核 inode。普通用户 profile 的原 mode 必须恢复，GNU `cp --preserve=all` 可用时 ACL/xattr 应保持一致；`cp -p` 回退只作尽力保留，不能把 ACL/xattr 写成所有 Linux 环境的硬保证。
- 兼容性测试应至少用 Bash 3.2 对脚本执行 `-n`，并实际执行无 completion、`--completion all`、manifest v3 与只读 `0400` profile 的安装/卸载流程。

2026-09-02 服务器 run `/root/dm-installer-final-20260902-140133` 已完成上述 installer-only 定向矩阵 17 PASS；`scripts/e2e.sh --mode install` 为 19 PASS / 10 XFAIL / 0 FAIL。另从固定为 2,542,620 字节、SHA-256 `3fa9daf85ebf35068f090ce51283ddeeb3c75eb5bc70b1a4a7cb05868bfe06a4` 的官方源码构建 Bash 3.2.57，语法检查及三个实际安装场景全部通过。该 run 没有完成或声明最终 Docker container/image/network/volume before/after 集合比较，也不作为服务器目录或 Docker 资源清理完成的证据。

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
- pull 输出覆盖已有普通文件的 rename 发布；backup 输出覆盖已有 artifact 的拒绝。两者还应覆盖 symlink/junction、并发 writer、取消、close/sync/rename 失败。Pull 在 rename 前失败时旧目标不变且只清理本事务 staging；Unix rename 后的父目录 sync 失败应返回错误，但允许新归档已经发布。
- 分卷备份覆盖 `.parts.pending.json` 文件锁、崩溃后重试、已提交 manifest、外来同名 part/marker 替换和 staging 所有权冲突；恢复只能删除 `os.SameFile` 证明属于旧事务的文件。
- restore/rerun 创建结果不确定与回滚测试必须注入同名外来容器；只有稳定 ID 和本次 128-bit owner label 同时匹配的候选可被删除。
- volume docker-run probe 使用 digest-pinned 默认 helper、无网络、只读 rootfs/volume、drop capabilities、`no-new-privileges`，并验证失败和取消后 helper 容器清理。
- image/volume prune 缺少 `--allow-non-atomic-delete` 时在首个删除请求前失败；container-only 候选不要求该选项。

## E2 定向回归

E2 规模化运行和自动化代码变更后至少运行：

```bash
go test -count=1 ./internal/runcontrol ./internal/parallel ./internal/report ./internal/audit ./internal/appconfig ./internal/cli
go test -count=1 ./internal/commands/diagnostics ./internal/commands/backup ./internal/commands/reverse ./internal/commands/pull ./internal/commands/images
go test -race -count=1 ./internal/runcontrol ./internal/parallel ./internal/report ./internal/audit ./internal/appconfig ./internal/cli
go test -race -count=1 ./internal/commands/diagnostics ./internal/commands/backup ./internal/commands/reverse ./internal/commands/pull ./internal/commands/images
```

定向用例应确认：

- `operation_concurrency`、`operation_timeout`、`operation_rate_limit` 和 `operation_max_items` 可由 base/profile 配置，命令级 `--concurrency`、`--operation-timeout`、`--rate-limit`、`--max-items` 显式值优先；省略 concurrency 使用共享默认 `8`，显式 `0` 可关闭；超限、NaN/Inf、负数和超过 `64`/`24h`/`1000`/`100000` 的值失败。
- 四个统一 runtime flag 只出现在 diagnostics、backup/restore 和 reverse/rerun 叶子；pull 仅继承配置 controller 并保留原 `--concurrency`/`--timeout`/`--total-timeout` 语义，config/version/completion/image load/save 不创建 controller。
- 同一命令的并发 semaphore、启动 pacer、外层 deadline 和累计 item counter 在嵌套/兄弟任务间共享；取消、nil context、回调 panic 和 Acquire 失败均不会泄漏 lease 或卡住后续任务。
- health/logs 按过滤后的 container 数量计费，network 累计 container+network，volumes 累计 volume+container，prune 在 apply 前对固定候选集计费；backup/reverse/rerun/pull 在 Docker mutation 或目标文件写入前预留完整目标预算。
- health、logs、report all 以流式方式执行 `16 MiB` 单容器和 `256 MiB` 全命令日志预算；底层读取不超过剩余额度，恰好达到单容器或累计上限也返回预算错误，multiplex frame 使用固定缓冲区且伪造超大 frame 不触发等尺寸分配；配置和 flag 覆盖生效，单容器/累计超限、并发竞争、取消、读取错误均停止继续读取。配置硬上限分别为 `256 MiB` 和 `4 GiB`。
- health/logs/network/volumes/prune/report all 的 `--fail-on`、可重复 `--threshold metric=max`、配置默认值及命令行优先级正确；配置 threshold 必须使用严格 `scope.metric=max`，单报告自动路由并剥离前缀，report all 保留前缀；未知/重复指标失败，实际值严格大于 maximum 才失败。
- 未启用策略时既有 text/JSON/Markdown/HTML 输出不增加门禁尾部；JSON 是单个可解析文档。SARIF 输出符合 2.1.0，findings/rules/results、invocation 状态和 threshold properties 完整且顺序稳定。
- 退出码严格区分：成功 `0`，运行/配置/渲染/审计错误 `1`，仅报告门禁失败 `2`，SIGINT/context cancel `130`；门禁错误与运行错误组合时保持 `1`，取消保持 `130`。
- JSONL 审计覆盖 start、分页 candidates、rejected/authorized、finish 和取消结果；同一 root 重复执行生成独立 run；显式 `--audit-file` 对参数缺失、未知 flag 和配置加载失败也生成失败 lifecycle；`safe` 不泄露 endpoint、资源名、凭据或原始错误，`full` 仍执行 strict 脱敏和长度限制，同一 key 下 HMAC 标识稳定。
- `warn` 只警告一次并继续；`deny-mutation` 允许只读但在 authorized 事件无法落盘时保持零 mutation；`fail`/`--audit-required` 对打开、start、authorized、finish 和 close 失败返回运行错误。
- Unix 新建审计目录、JSONL、key 和 lock 分别使用 `0700`/`0600`，Windows 部署 ACL 要求有记录；`audit_max_bytes` 为 `0` 或至少 `65536`；跨进程锁和完整 JSONL 事件轮转有效，数据/key/lock/rotation 的 symlink、junction/reparse、非普通文件、替换竞态及恶意同名轮转目标均失败关闭。
- doctor 的累计 item 预算、外层 timeout 和 cancel 必须作为运行错误传播；pull batch 的默认/显式 state 与 report 文件必须在任何 pull/exists 或写入前完成一次 filesystem 审计授权，授权失败时文件和 `.tmp` 均不存在。

本地 CLI 可补充以下最小检查；第二条要求 Docker 中至少有一个容器，预期只因门禁返回 `2`，不会修改 Docker：

```bash
dm health --format sarif --fail-on error > health.sarif
dm health --format json --threshold total=0 > health.json; test "$?" -eq 2
dm health --threshold unsupported_metric=0 >/dev/null 2>&1; test "$?" -eq 1
```

远程 E2 只读验收应在执行前后分别记录 container/image/volume/network 数量，并覆盖：配置/profile 来源、四类运行控制、三个日志读取入口、六个自动化报告入口、纯 JSON、SARIF、退出码 `0/1/2/130` 以及审计 JSONL 写入。审计文件和 key 写在独立临时目录，检查 JSONL 每行可解析、序列递增和私有权限后删除。`deny-mutation`/`fail` 的失败注入优先由本地 fake sink 单测覆盖；如需远程验证，必须使用隔离测试资源并在首个 Docker mutation 前证明调用数为零。

当前状态：E2 代码和本机可执行门禁已完成；`192.168.31.40` 的 39 PASS / 0 FAIL 是 2026-08-27 历史 E2 基线。2026-09-02 17:05 已使用包含最终安装脚本和 root-bind 检测的工作树完成最新服务器定向复测，结果和资源快照见“已完成验收记录”；Docker 24/27/29 真实 daemon matrix、TLS 2376 与企业 registry 联调仍属于发布前外部证据。

## P0-P2 回查定向回归

2026-09-01 的回查先验证本次受影响的问题面；最终又补跑了本机可执行的全仓门禁，但仍不替代 Linux、Docker/服务器或 full/destructive E2E。复测必须继续使用隔离 `GOMODCACHE`/`GOCACHE`，并按平台分别运行普通、race、重复和交叉编译检查。

Pull 和 URL 输入边界的最小定向命令基线：

```powershell
$affected = '^Test.*(PullBatch|TarArchive|PullArchive|AtomicJSON|WindowsPathInspection|PullJSON|Reserved).*$'
$markerRepeat = '^TestRunPullBatch(ResumeRepullsAfterUncertainStateCommit|StateMarkerCreateSyncFailurePreventsStateWrite|StateMarkerClearFailureRemainsUntrusted|DoesNotClearReplacedStateMarker|WindowsResumeDetectsUntrustedMarkerAcrossStatePathCase)$'
$urlBoundary = 'Proxy|Realm|PushTarget'
$cliConfig = '^(TestLoadAppConfig|TestConfigValidateAndShowSources|TestDoctorUsesSelectedProfileForConfigChecks|TestRootCommandLoadsDMConfigDefaults)$'
go test -count=1 ./internal/commands/pull -run $affected
go test -count=1 ./internal/appconfig ./internal/commands/pull ./internal/commands/diagnostics -run $urlBoundary
go test -count=1 ./internal/cli -run $cliConfig
go test -race -count=1 ./internal/commands/pull -run $affected
go test -count=20 ./internal/commands/pull -run $markerRepeat
go test -race -count=1 ./internal/appconfig ./internal/commands/pull ./internal/commands/diagnostics -run $urlBoundary
go test -race -count=1 ./internal/cli -run $cliConfig
go vet ./internal/appconfig ./internal/commands/pull ./internal/commands/diagnostics ./internal/cli
go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./internal/appconfig ./internal/commands/pull ./internal/commands/diagnostics ./internal/cli

cmd /d /c 'set GOOS=linux&& set GOARCH=amd64&& set CGO_ENABLED=0&& go test -c -o "%TEMP%\dm-p0p2-appconfig-linux-amd64.test" ./internal/appconfig'
cmd /d /c 'set GOOS=linux&& set GOARCH=amd64&& set CGO_ENABLED=0&& go test -c -o "%TEMP%\dm-p0p2-pull-linux-amd64.test" ./internal/commands/pull'
cmd /d /c 'set GOOS=linux&& set GOARCH=amd64&& set CGO_ENABLED=0&& go test -c -o "%TEMP%\dm-p0p2-diagnostics-linux-amd64.test" ./internal/commands/diagnostics'
```

Windows 的 race 命令需要可用的 MinGW GCC，并显式设置 `CGO_ENABLED=1`、`CC=gcc` 和 `CXX=g++`。重复测试应使用精确 `-run` 和大于 1 的 `-count`，至少覆盖 lifecycle lock marker replacement、state-untrusted marker replacement/owner/protocol/create/clear failure、standalone/batch archive scope、reserved standalone pre-network、Windows canonical/实际 8.3 alias/case-sensitive directory、resume fingerprint 和 deadline/persistence 错误组合；不要用 `go test ./...` 代替本节的定向范围。

定向用例应确认：

- 单次和 batch 归档共用输出目录 lifecycle scope；不同 metadata batch、standalone/batch 竞争和已持锁输出均在归档覆盖前失败。持锁期间替换或 unlink lifecycle lock marker 不能获得第二个有效 scope。
- Tar rename 前的 close、文件 sync、身份复核或 rename 失败保留旧目标；Unix rename 后父目录 sync 失败返回错误且 batch 不记录 success，但断言允许新归档已经发布。
- Resume 成功项的版本化 fingerprint 同时校验归档绝对路径、大小、SHA-256、OS/arch 和 effective Docker load；旧 state、缺失/篡改归档及任一字段变化都会重拉并迁移，未生成可验证归档不能写 success。
- State commit protocol v2 在写 state 前持久化固定 `.dm-pull-state-untrusted.marker`，payload 包含版本、state basename 原始字节 hex 和 128-bit transaction；清理前校验 marker 文件身份、transaction 和 owner。残留 marker 或 protocol 0/1 success 必须保守重拉；其他 owner、畸形、超限、被替换 marker 以及创建/清理持久化失败必须在 callback 前失败关闭或保持 untrusted。
- `.dm-pull-`/`.docker-manager-pull-` 在原始 basename 和 Windows canonical/实际 8.3 basename 上均为内部保留 namespace；standalone 必须在首个 registry HTTP 请求前拒绝并复用已解析输出，batch 必须在 pull/exists callback 前拒绝 state/report/archive。
- Windows 拒绝设备/扩展 namespace、设备名、尾点/尾空格、unsafe UNC server/share、尚不存在且形似 DOS 8.3 短名的组件，以及任一启用 per-directory case sensitivity 的现存父目录；已存在祖先解析成长路径后，应只在实际 short-name alias 发生路径冲突时拒绝，而不是无条件拒绝所有已存在的 8.3 alias。普通长路径仍可用。Windows `0600` 只表示 Go mode，不等价于私有 ACL，rename 也不假定具有与 Unix 相同的原子/持久化语义。
- Base、profile 和 per-registry proxy 只有原始空字符串表示未配置；空白值、首尾空白、缺 scheme、空 hostname、fragment 和不支持的 scheme 均在网络请求前失败。显式及环境 pull/registry-login proxy 在交给 HTTP transport 前执行同样的 hostname 边界检查，doctor 对非法环境代理给出 warning；HTTPS auth realm、Bearer challenge/allowlist 和带 scheme 的 `--to` 也拒绝空 hostname，合法 IPv6 hostname 保持可用。
- Deadline report 包含每个计划项及未调度项；report I/O 后到达的 context 错误，以及 state/report/context 同时失败时，可通过 `errors.Is` 观察全部 joined cause。显式 context canceled 路径仍不发布 report。

服务器最终复测以 2026-09-02 17:05 的 `/root/dm-server-eval-20260902-servergap-1705` 为准：脚本和 linux/amd64 主程序均记录 SHA-256，安装回归、v2 legacy migration 和目标资源快照均使用同一最新脚本集；测试前后按稳定字段比较 container/image/network/volume 集合。此前 11:00、09:37 及更早工作树的远端结果只能作为阶段证据，不能替代本次最终代码复测。

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
dm pull --file "$DM_TEST_ROOT/images.txt" --to "$DM_REGISTRY/dm-batch" --plain-http --concurrency 1 --retries 1 --resume --output-dir "$DM_TEST_ROOT/pulled-batch" --state-file "$DM_TEST_ROOT/pull-state.json" --report "$DM_TEST_ROOT/pull-report.json"
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

### Profile 和 per-registry policy

配置扩展至少覆盖以下定向用例：

- 旧扁平 YAML 行为不变；`--profile > DM_PROFILE > default_profile > base`，未知或非法 profile 失败。
- profile 省略字段继承 base，显式空字符串、`false` 和 `[]` 可以清除继承值；Docker flag 仍覆盖 profile，并保持显式空值清除 `DOCKER_*` 回退。
- `dm config validate` 校验所有 profile/policy；`config show --effective --show-source` 正确报告 active profile 及 base/profile/flag/env/default 来源。
- Docker completion 使用 selected profile endpoint；base、`DM_PROFILE`、`default_profile` 和显式 `--profile` 均有覆盖。
- 同一批次访问两个 registry 时分别使用自己的 CA、proxy/no-proxy、timeout、credential scope 和 realm allowlist，源 registry 与 `--to` 目标策略互不串用。
- 自签 HTTPS registry 使用正确 policy CA 成功，缺失/错误 CA 失败；未匹配 registry 不继承其他 registry 的 CA。CA loader 必须拒绝 symlink/reparse、非普通文件、空或混合 PEM，并覆盖单文件 16 MiB、目录 256 项和累计 32 MiB 上限。
- `plain_http` 仅由精确 registry policy 或显式 flag 开启，`--plain-http=false` 能覆盖 policy，HTTPS 不会静默降级。
- `credential_scope` 分别限制 pull、push 和 login；空列表不向 registry 或 Docker daemon 传递凭据，realm allowlist 不能跨 policy 合并。

Registry policy 控制 `dm` 直接发出的 `/v2/`、manifest、blob、token 和 push 前预检请求。最终 `docker push` 由 Docker daemon 执行，仍需在 daemon 侧配置 registry CA、insecure registry 和代理；两条链路必须分别验收。

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

- 2026-09-02 11:00 最终服务器定向复测：`192.168.31.40` 为 Ubuntu 22.04、Docker Server 28.1.1、API 1.49；run `/root/dm-server-eval-20260902-1100` 的主程序 SHA-256 为 `fe8b3480b1aaee52fd0c2e2251385d150b0fd5809e63d9e9949c4599c611b8b6`，backup/pull/appconfig/diagnostics 测试二进制 SHA-256 依次为 `8af8936772c537417613de12c4ccdce3bc95ea510af02ccb9f7f63ace999b3e0`、`81b95bb82a558d554409b3a1f354cf8de06106344f311ba8c1c149a0a9f76fd7`、`06a867e4366b6e1ef79fbc8a88da0cc0009bad56e9f352fd4ac2050b0a728d65`、`3d4e7b94894ca525e6b5170c471c0a6df5b654109313288a6c1055e82511d657`。appconfig、diagnostics、pull affected 和 backup cleanup（restore cleanup `-count=20`）全部 PASS。
- 2026-09-02 17:05 最新 Linux 安装/挂载边界复测：服务器 run `/root/dm-server-eval-20260902-servergap-1705/e2e-install-latest` 使用当前脚本和主程序；`scripts/e2e.sh --mode install` 为 `19 PASS / 10 XFAIL / 0 FAIL`。`uninstall rejects bind-mounted ancestor` 同时覆盖普通 bind ancestor、`root=/` tmpfs-root bind ancestor 和独立 tmpfs filesystem 三个子场景，全部 PASS；completion all/bash/none/restore、v3 marker/manifest、失败回滚和 purge 拒绝场景均按预期。脚本 SHA-256：install `b414afc59ec986b8d0a87e509cf99e38ea42fc28fc17c62aeb3a15258625f2a4`、uninstall `6615b022bbc0ce1b034885b2d1d6e785d64bc2eae9dc9ddf4aabe4393aee4f8e`、e2e `8eb5199f9599d49f3722a620ea0f26ba965b093b7ad401800b5d17dfe0d6c8dc`，主程序 `2dd982ca178572969bb5a7eb33ea292ff21edaf8b2e4693db31bdcbe2cd2fc2d`。服务器 ShellCheck 0.8 和 Bash syntax 均返回 0；e2e run 使用唯一工作目录，完成后本轮创建的测试资源已清理。
- 同一 17:05 run 的 v2 legacy completion 边界为 `V2_LEGACY_TAIL_PASS`：前导/尾部/连续 `:` 空字段在 verify、普通卸载和 purge 路径均拒绝；合法 v2 legacy-only manifest 可 verify、普通卸载保留 config/data，并可由重装自动推导 completion base、迁移到 v3 后完成 purge。该 harness 使用的临时 manifest/marker/completion 和工作目录已按归属清理。
- 2026-09-02 14:01 Linux installer 定向复测：服务器 run `/root/dm-installer-final-20260902-140133` 为 17 PASS；同一变更集的 `scripts/e2e.sh --mode install` 为 19 PASS / 10 XFAIL / 0 FAIL。固定大小和 SHA-256 的 Bash 3.2.57 完成语法检查及空/全 completion、manifest v3、只读 profile 场景。该 installer-only run 未执行也未声明 Docker 资源 before/after 集合比较或最终清理。
- 同一 11:00 run 的 doctor、health/network/logs/volumes、`report all` JSON/SARIF、JSONL audit 和 threshold 检查均按预期；`report all --max-items 1000` 返回 `1` 属预算超限预期，扩大到 `100000` 返回 `0`。测试前后 container/image/network/volume 计数均为 `227/7012/19/129`，四类稳定资源集合 diff 为 0。
- P0-02 真实 CLI 的 registry standalone 与 batch 匿名链路均 PASS；P1-07 prune 的 container/volume/image guard 场景均 PASS。`volume docker-run` 使用显式 `busybox:latest` helper 通过且 helper 残留为 0，但固定 digest helper 在服务器不存在，因此不宣称默认 digest helper 已远程验证。
- Backup/restore 预算验证中，`--max-items=2` 在无输出、无 Docker mutation 下失败，`--max-items=3` 成功；真实 batch resume 首次成功、第二次跳过、篡改后第三次重新拉取，state protocol v2、fingerprint 和 `0600` 权限均通过。脚本 cleanup 等价 harness 保留 foreign resource，仅清理 owned resource。
- 2026-09-02 09:37 阶段性服务器定向复测（旧构建，保留为阶段证据）：`192.168.31.40` 为 Ubuntu 22.04、Docker Server 28.1.1、API 1.49；run `/root/dm-server-eval-20260902-0937` 使用旧主程序 SHA-256 `ae082618e7e211f34d161d01dce299a4b7d4b17ec739ab0985b4bc317791aa4d`，appconfig/pull/diagnostics 测试二进制 SHA-256 分别为 `0a13ec8ddf587c74a7988fa2a33f49d336ac5566a5630be79e931c90e44ea66f`、`213c52013e585150dd27a6d916839a8c90ddf285121b21d2a1ef7127b41ebbcd`、`99553f4dd6027f2b0eb5db79e0aeffe89522599c7f28b18b5c7fb77b5170e9d6`。Pull 受影响 regex、state-marker 重复集合、关键替换用例以及 appconfig/diagnostics URL 边界测试全部 PASS，无 FAIL；该 run 创建且经归属确认的测试二进制、日志、审计和工作目录均已清理。服务器上更早验收遗留的 `/tmp/dm-restore-*`、`/root/dm-e2e-source-*.tar`、`/root/pull-state.json` 及无归属标签的 `dm_debug_*` 容器不属于该 run，未在本轮擅自删除。
- 上述 09:37 阶段性 run 的只读验收覆盖最终二进制的 `doctor`、health、network、logs、volumes (`--size-mode api`)、prune dry-run 和 `report all` JSON；JSON 文档可解析，审计 JSONL 的 start/finish 序列连续（1/2），events/key 权限为 `0600`。`--threshold total=0` 返回 `2`，未知指标返回 `1`，共享 `--max-items` 超限在任何 Docker 写操作前返回 `1`；扩大到足够预算后聚合报告返回 `0`。服务器未安装默认的 digest-pinned volume helper，因此该阶段只验证 API 模式。
- 上述 09:37 阶段性 run 的 Docker 资源稳定证据：测试前后 container/image/network/volume 计数为 `227/6988/19/129`（image 为 `docker image ls -aq` 行数）；container、image、network 稳定字段哈希依次为 `b9cfd8ff715c9d60bb523cd249af6b0b3f42ce3a6d02478d4311682ca57a1387`、`9653893121cb0f5e7028f0dd097faa56f96aa124a6757fa850f431d3919d073a`、`b049346c9d13c1e6798479e5fb990b15e33a1b5e75289cedb159e85e40230507`；volume 使用 `Name/Driver/Mountpoint/Scope/Labels/Options` 规范化集合比较且无差异。本轮唯一命名或带归属标记的测试容器、volume、镜像标签和 network 均无残留。
- 同日 10:33 的后续只读复核（阶段状态）计数为 `227/7004/19/129`。09:37 阶段测试结束后服务器在 10:15-10:19 出现 `emergency-adapter-hub` 的新镜像/回滚标签，解释了增加的 16 个 image 行；该后续状态不用于替代阶段 run 的前后快照，也不据此将外部新增资源归因于测试。
- 上述阶段性 P0-02 以唯一命名的本地 `registry:2` 做了匿名 `pull -> load -> tag -> push -> manifest` 定向链路，空 Docker config 下命令返回 `0`，目标 manifest 可读取且目标镜像可由 Docker inspect 验证；临时 registry/volume/标签均已删除，四类资源集合恢复。P1-07 以唯一 label 的 stopped container 验证未确认 dry-run 不删除、`--apply --confirm --max-items 1000` 只删除该临时候选并在清理后无残留。
- 上述阶段性 P0-03 服务器仅执行 transport 契约检查：显式 Unix endpoint 成功，缺失 TLS 证书目录和不可用裸 TCP endpoint 均报告 failed/warning 且不回退到本地 Unix endpoint；未宣称真实 TLS/mTLS 2376 通过。服务器现状报告另有 3 个 unhealthy、95 个 logs unavailable 和 1122 个 public bindings，这些是现有 Docker 资源的运维告警，不是本次代码测试失败。
- 2026-09-02 restore cleanup 阶段性修复回归（旧构建）：“修复后二进制” SHA-256 `a2476c868ceefeb89ff3568ec0b62ae347b252824bc1043335d5f0f02fe099dc` 在隔离 `TMPDIR` 下执行确认式空目录 restore，因缺少 checksum 返回退出码 `1`，snapshot 临时目录为空；将 `DOCKER_HOST` 指向不可用的 `tcp://127.0.0.1:1` 作为误访问探针，未发生 Docker 回调，远程工作目录按哨兵清理。该条目保留为阶段证据，不代表 11:00 run 的构建。
- 2026-09-01 最终差异复核补齐 URL `Host` 非空但 `Hostname()` 为空的 P2 边界：配置 base/profile/per-registry proxy、显式及环境 pull/registry-login proxy、doctor 环境诊断、配置与 pull auth realm、Bearer challenge/allowlist 和带 scheme 的 `--to` 均 fail closed，并保留合法 IPv6 proxy。新增空 hostname 用例在 appconfig、pull、diagnostics 中通过 normal `-count=50` 和 MinGW race `-count=20`；三包全部 `Proxy|Realm|PushTarget` 用例及 CLI 四条配置消费路径的 normal/race 通过，四包 vet、staticcheck v0.8.1、gofmt 和 `git diff HEAD --check` 通过。测试过程发现 Windows 环境变量名大小写不敏感，修正了先设置 `HTTPS_PROXY` 又清空 `https_proxy` 的测试顺序后稳定通过。
- 2026-09-01 P0-P2 回查的阶段性本地定向验证使用隔离 Go 缓存完成：appconfig proxy、CLI audit rotation、backup/restore item budget、doctor 位置参数和 image load/save 参数的普通与 MinGW race 分组均为 5 个包全部 `ok`，相同 5 个包的 `go vet` 通过；`bash -n scripts/e2e.sh` 通过。本机无 ShellCheck，因此本轮未把 ShellCheck 记为通过。
- 最终扩展 regex `^Test.*(PullBatch|TarArchive|PullArchive|AtomicJSON|WindowsPathInspection|PullJSON|Reserved).*$` 的 Windows normal/race 分别 PASS（12.631s/13.344s）；5 个 marker 高风险用例 `-count=20` PASS（14.941s）。3 个 deadline 用例原先使用 100ms 墙钟超时，在全仓 race 压力下会于安全 preflight 完成前到期；改为 callback 内确定性触发 `DeadlineExceeded` 后 normal/race 各 `-count=50` PASS（22.994s/45.240s），未改变取消后不启动新工作的生产语义。
- Go 1.27.0 下 `scripts/check.ps1 -Race -NoShellCheck`、全仓 test/race/vet/build、gofmt、302 个仓库文本文件、PowerShell AST、Git Bash syntax 和 `git diff HEAD --check` 通过。默认 `local-test.ps1` 为 27 PASS / 6 XFAIL / 1 SKIP / 0 FAIL；`-NoEnvironment -SkipRace` 为 25/6/3/0，race 已由前者和全仓门禁覆盖。测试前后 User/Process `DM_CONFIG`、`DM_HOME`、`DM_OUTPUT_DIR` 的存在性和值一致。
- `go mod tidy -diff` 无差异，隔离 `go mod verify` 输出 `all modules verified`；staticcheck v0.8.1 全仓、actionlint v1.7.12 和显式 `go build ./...` 通过，govulncheck v1.7.0 输出 `No vulnerabilities found.`。gosec v2.28.0 扫描 165 个文件、36,069 行，最终为 52 项已分诊告警（G103 3、G122 2、G204 2、G301 2、G302 3、G304 40；HIGH/MEDIUM/LOW 为 2/47/3）；Windows 路径缓冲区增加 32768 UTF-16 单元上限后 G115 为 0。既有两条 G122 私有工作目录归档 TOCTOU 继续列入维护清单，不作为本轮新阻断。
- 覆盖率门禁为 76.45%（13998/18309），7 个关键包全部通过阈值。PowerShell 7 和 5.1 无 Docker completion 均为 6 PASS / 0 FAIL / 1 显式 SKIP；默认 Windows smoke 的 Docker 不可用路径按预期为 XFAIL。
- `package-release.ps1 -NoTest` 成功构建 linux/amd64、linux/arm64、windows/amd64、darwin/amd64、darwin/arm64 五个平台并通过结构化 manifest/checksum 校验；Windows 归档解压后的 `dm version`、包内 install/uninstall smoke 通过，四个 Unix tar.gz 均包含 CHANGELOG 和三份要求的 docs。发布产物只用于本地验证，未提交或上传。
- 本机交叉编译的 linux/amd64、`CGO_ENABLED=0` test binary 上传前构建记录：appconfig 为 6,750,860 字节、SHA-256 `7e820a53d2e3241516fc08a94659de3c76b5af55e55018a574862c35248682ac`；pull 为 18,472,457 字节、SHA-256 `6279420e40bef553f4897eee0782d21c99d87446aba35cff12f3fe494272bfbc`；diagnostics 为 17,265,217 字节、SHA-256 `7febfc6b1e04e0008c064a91c32de1d7c58d3648b6ad2e539808033f370f4618`。服务器实际执行的上传副本 SHA 以本节首条 2026-09-02 记录为准；两组 SHA 属于不同构建批次，不作同一文件的等值声明。对应的本机临时构建目录已清理，历史大体量验证目录因占用/权限问题保留。
- 2026-09-01 的中间版本曾在 `192.168.31.40` 以 `run=/root/dm_p0p2_targeted_20260901171830_34157bef` 执行 Linux 定向回归：pull/appconfig test binary SHA-256 分别为 `34157bef8d0cb7dfe28d277cf260e745d7ce845eb981d7de474e47bb399385df` / `e396d436f5b3ae9004c5c0f2e0e675d5ff03fb09cee8bed9f7a739bc6ca44748`，11 个 pull 顶层测试（resume 含 7 个子场景）和 4 个 proxy 顶层测试全部 PASS；Docker 四类资源集合前后完全一致，计数 `227/6943/129/19`，远端目录已清理。此后继续新增 state protocol v2/固定 untrusted marker、内部命名空间、Windows canonical/8.3/case-sensitive 目录和 deadline 测试修复，因此不作为最终代码证据。
- 2026-09-01 状态记录时本机无 Docker CLI、无可用 WSL 发行版和 ShellCheck，服务器暂不可用；`fsutil file SetCaseSensitiveInfo <temp> enable` 返回 `The request is not supported.`，空探针目录已清理，因此真实 case-sensitive NTFS 集成仍待其他卷/环境。2026-09-02 已补做最终 Linux/服务器定向复测，但本轮仍未运行 full/destructive E2E；2026-08-27 的 E2 全量结果仍是历史基线，不代表自动覆盖 2026-09-01 的新增修复。
- 2026-08-27 E2 规模化运行和自动化：Go 1.27.0 下全包 test/race/vet/build、`go mod tidy -diff`、隔离 `go mod verify`、staticcheck v0.8.1、govulncheck v1.7.0、gosec v2.28.0、ShellCheck、Bash/PowerShell 语法、文本、completion 和覆盖率门禁全部通过；覆盖率为 75.94%（12977/17088），7 个关键包均通过门槛。gosec 仍为既有 52 项（G304 41、G301 3、G302 3、G122 2、G204 2、G306 1），没有新增告警；全新隔离 `GOMODCACHE` 下的 verify/test/build 复验也通过。Windows smoke 普通模式为 27 PASS / 6 XFAIL / 1 SKIP，`-NoEnvironment` 为 26 PASS / 6 XFAIL / 2 SKIP，PowerShell completion 为 6 PASS / 0 FAIL / 1 SKIP，测试前后用户级和进程级 `DM_*` 状态一致。
- `192.168.31.40`（Docker 28.1.1 / API 1.49）使用 SHA-256 `2768f696fb89b98c44f99ec8196a83a68fe94c858ace4e6bc959e610c08432b5` 的 Go 1.27.0 linux/amd64 二进制完成 E2 只读验收 39 PASS / 0 SKIP / 0 FAIL；覆盖 profile/source、并发/timeout/rate/max-items、health/logs/network/volumes/prune/report all、JSON/SARIF、日志预算、退出码 0/1/2/130 和 JSONL 审计。前后 container/image/volume/network 集合及计数 `221/6082/129/19` 完全一致，审计目录/JSONL/key/lock 权限为 0700/0600/0600/0600；远端旧轮与本轮目录以及本地 E2 二进制、报告、审计、缓存和临时脚本均已清理。
- 2026-08-27 E1 配置和多环境扩展：Go 1.27.0 下最终全包 test/race/vet/build、`go mod tidy -diff`、隔离 module verify、staticcheck v0.8.1、govulncheck v1.7.0、gosec v2.28.0、ShellCheck v0.11.0、文本和 completion 门禁通过；覆盖率为 74.17%（10756/14502），7 个关键包均通过门槛。gosec 仍为既有 52 项且规则分布不变。
- `192.168.31.40`（Docker 28.1.1 / API 1.49）完成 E1 profile/config/completion/doctor/health/prune 只读验收 46 PASS / 0 FAIL；profile 选择优先级、显式空 profile、旧扁平 YAML、严格坏配置拒绝和来源显示均通过。最终 endpoint 分离修复另以最新 linux/amd64 二进制完成 10 PASS / 0 FAIL，确认 `registry-1.docker.io` 不会被 policy alias 改写。每轮容器、镜像、volume、network 计数前后分别保持一致，远端和本地临时目录均已清理。
- 2026-08-24 Go 1.27 迁移：Go 1.27.0 下全包 test/race/vet/build、staticcheck v0.8.1、ShellCheck v0.11.0、actionlint、文本门禁和覆盖率门禁通过；全仓覆盖率为 73.41%（9916/13508），关键包均高于 CI 阈值。
- 依赖迁移后 govulncheck v1.7.0 扫描 Go 1.27 标准库和 32 个模块，输出 `No vulnerabilities found.`；gosec v2.28.0 仍为既有 52 条分诊项，规则分布未变化。
- Go 1.27 发布打包生成并验证 linux/amd64、linux/arm64、windows/amd64、darwin/amd64、darwin/arm64 五个平台归档、checksum 和 manifest；`192.168.31.40` 的 Docker 28.1.1 / API 1.49 完成 smoke、doctor、health 和 prune 只读预览，容器、volume、镜像计数前后相同，测试目录已清理。
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
