# 发布检查清单

本清单只记录发布操作需要逐项确认的事项。测试方法和历史验收结论统一维护在 [TESTING.md](TESTING.md)。

## 1. 代码状态

- [ ] 工作区只包含本次发布需要的代码和文档变更。
- [ ] 不包含临时测试报告、内网凭据、私有证书、打包产物或本地二进制。
- [ ] `CHANGELOG.md` 已更新版本变化、已完成优化和已知非阻断项。
- [ ] `README.md` 只保留面向用户的构建、安装和功能说明。

## 2. 本地检查

- [ ] `go version` 为 Go 1.27.0 或当前更高的受支持补丁版本。
- [ ] `gofmt` 无待格式化文件。
- [ ] 仓库文本通过 `scripts/text-check.go`：UTF-8 无 BOM、LF、末尾换行且无 `U+FFFD`。
- [ ] `go test ./...` 通过。
- [ ] 覆盖率门禁通过：全局至少 70%，配置、认证、runconfig、targets、Docker SDK 和 completion 关键包达到 [TESTING.md](TESTING.md) 的独立阈值。
- [ ] `go vet ./...` 通过。
- [ ] 可用时执行 `go test -race ./...` 或 `scripts/check.* -Race`。
- [ ] `staticcheck ./...` 和 `govulncheck ./...` 阻断门禁通过。
- [ ] ShellCheck 完整通过，未使用 `--no-shellcheck` / `-NoShellCheck` 绕过发布门禁。
- [ ] `git diff HEAD --check` 无尾随空格、冲突标记或补丁格式问题。

推荐命令:

```bash
bash scripts/check.sh --race
```

Windows:

```powershell
.\scripts\check.ps1 -Race
```

## 3. 构建和发布包

- [ ] 开发构建通过: `scripts/dev-build.sh --vet` 或 `scripts/dev-build.ps1 -Vet`。
- [ ] 发布打包通过: `scripts/package-release.sh --version vX.Y.Z` 或 `scripts/package-release.ps1 -Version vX.Y.Z`。
- [ ] `dist/<version>-<commit>/checksums.txt`、`release-manifest.json`、`release-summary.md` 已生成并通过结构化 manifest/digest 校验。
- [ ] 每个发布归档包含二进制、`README.md`、`CHANGELOG.md`、`docs/TESTING.md`、`docs/RELEASE_CHECKLIST.md`、`docs/DOCKER_API_MIGRATION.md`、`LICENSE`、`dm.yaml.example`、`INSTALL.md` 和目标平台对应的安装/卸载脚本。
- [ ] Windows 包只包含 PowerShell 安装/卸载脚本；Linux/macOS 包只包含 shell 安装/卸载脚本。
- [ ] Darwin 发布说明标明 Go 1.27 构建产物最低支持 macOS 13。

## 4. 安装和卸载

- [ ] Linux/macOS 默认安装路径、自定义安装路径、配置目录、数据目录可用。
- [ ] Windows 默认安装路径、自定义安装路径、配置目录、用户级环境变量可用。
- [ ] Windows 多安装目录按 `install.json` 所有权链恢复环境变量；用户后续改写的 `DM_CONFIG`、`DM_HOME`、`DM_OUTPUT_DIR` 和 `PATH` 不被卸载清空。
- [ ] Windows install/uninstall/purge 对路径链和树内 junction/reparse 在 mutation 前拒绝，外部目标未被修改。
- [ ] 默认 completion 安装行为符合平台预期。
- [ ] `--no-completion` / `-NoCompletion` 可关闭 completion 安装。
- [ ] 卸载默认保留配置和数据，`--purge` / `-Purge` 会清理配置和数据。
- [ ] 重复安装不会破坏已有配置，除非显式传入覆盖配置选项。

## 5. 验收

- [ ] 已按 [TESTING.md](TESTING.md) 完成当前发布所需的本地、远程或企业环境验收。
- [ ] Linux completion 无 Docker 门禁通过，bash-completion、zsh、fish 加载用例均非 SKIP。
- [ ] PowerShell 7 和 Windows PowerShell 5.1 均通过 completion dot-source 和 `TabExpansion2` 用例。
- [ ] Docker completion 使用 `--require-docker` / `-RequireDocker` 通过，容器、镜像、volume 候选均非 SKIP，且无临时资源残留。
- [ ] base、`default_profile`、`DM_PROFILE` 和显式 `--profile` 的选择优先级通过；completion 查询选中 profile 的 Docker endpoint。
- [ ] 至少使用两个隔离 registry 验证 policy 不串用 CA、代理、realm allowlist 或凭据；HTTPS 私有 CA 成功且错误 CA 失败，CA 目录链接/混合 PEM/数量和大小越界均失败关闭。
- [ ] `plain_http` 只对精确匹配的 registry 生效，显式 `--plain-http=false` 可覆盖配置；未配置的 registry 不降级到 HTTP。
- [ ] 已分别验证 `dm` 直连 registry policy 和 Docker daemon push 的 CA/代理配置，发布说明没有混淆两条链路。
- [ ] 破坏性命令测试只作用于测试 label、测试容器、测试 volume 或临时 registry。
- [ ] image/volume prune 未传 `--allow-non-atomic-delete` 时零删除，显式确认后只删除固定测试候选。
- [ ] 分卷备份成功后 commit manifest/digest 完整且无 pending/staging 残留；中断恢复不删除 foreign replacement。
- [ ] restore/rerun 回滚只清理稳定 ID 与本次 owner label 同时匹配的候选；volume probe 结束后无 helper 容器残留。
- [ ] 远程测试完成后已清理临时容器、volume、network、registry 和测试目录。
- [ ] 新增失败、跳过或非阻断项已记录到 `CHANGELOG.md`。

## 6. 回滚

- [ ] 当前版本和上一版本发布归档及 checksum 可追溯。
- [ ] 已验证卸载当前版本后安装上一版本可以执行 `dm version`。
- [ ] 回滚说明没有依赖本地临时路径。
