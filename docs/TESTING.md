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

Linux installer 的 Bash 兼容性、manifest、completion 和安装/卸载边界已完成定向检查；详细执行命令见本节，具体环境信息不随公开文档记录。

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

当前状态：E2 代码和本机可执行门禁已完成；远程 Docker、registry 和外部平台矩阵按发布版本单独验收，历史运行结果不跨构建批次合并。

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

远程复测按构建批次记录；公开文档只保留覆盖范围和结论，具体主机、运行目录和临时资源信息留在本地验收清单。

## 远程 Docker 验收

建议在干净 Docker 主机上使用临时目录执行:

```bash
export DM_TEST_ROOT="${TMPDIR:-/tmp}/dm-test-$(date +%Y%m%d-%H%M%S)"
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
DM_E2E_DM_BIN="$DM_TEST_ROOT/dm" bash scripts/e2e.sh
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

- 本地 Go 1.27 门禁、全仓 test/race/vet/build、依赖与静态分析、覆盖率、安装/卸载和 PowerShell completion 定向检查已通过。
- 远程 Docker 只读与 registry、batch resume/tamper、installer/mount、审计和 prune 定向检查已通过；测试资源按归属清理，未将不同构建批次的结果合并为单一发布证明。
- 五平台发布归档的 manifest/checksum、路径安全、平台文件头、Unix 执行位和普通文件权限均已独立复核；归档器自身有 tar/zip、符号链接和边界路径回归测试。
- Docker 24/27/29 版本矩阵、真实 TLS/mTLS、企业 registry/OIDC、真机运行、真实 case-sensitive NTFS 和 full/destructive E2E 仍需在对应外部环境完成；这些项目未完成前不视为公开发布放行。

## 已知非阻断项

- linux/arm64、darwin/amd64、darwin/arm64 已完成交叉编译产物生成，但尚未做真机运行验证。
- 真实 Harbor OIDC 登录、权限映射和审计链路仍需 Keycloak 完整部署或企业 OIDC 环境复测。
- 数百级大规模资源压测建议在专用环境单独运行。
