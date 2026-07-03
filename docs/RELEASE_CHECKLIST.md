# 发布检查清单

本清单只记录发布操作需要逐项确认的事项。测试方法和历史验收结论统一维护在 [TESTING.md](TESTING.md)。

## 1. 代码状态

- [ ] 工作区只包含本次发布需要的代码和文档变更。
- [ ] 不包含临时测试报告、内网凭据、私有证书、打包产物或本地二进制。
- [ ] `CHANGELOG.md` 已更新版本变化、已完成优化和已知非阻断项。
- [ ] `README.md` 只保留面向用户的构建、安装和功能说明。

## 2. 本地检查

- [ ] `gofmt` 无待格式化文件。
- [ ] `go test ./...` 通过。
- [ ] `go vet ./...` 通过。
- [ ] 可用时执行 `go test -race ./...` 或 `scripts/check.* -Race`。
- [ ] `git diff --check` 无尾随空格、冲突标记或补丁格式问题。

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
- [ ] `dist/checksums.txt`、`dist/release-manifest.json`、`dist/release-summary.md` 已生成。
- [ ] 每个发布归档包含二进制、`README.md`、`LICENSE`、`dm.yaml.example`、`INSTALL.md` 和目标平台对应的安装/卸载脚本。
- [ ] Windows 包只包含 PowerShell 安装/卸载脚本；Linux/macOS 包只包含 shell 安装/卸载脚本。

## 4. 安装和卸载

- [ ] Linux/macOS 默认安装路径、自定义安装路径、配置目录、数据目录可用。
- [ ] Windows 默认安装路径、自定义安装路径、配置目录、用户级环境变量可用。
- [ ] 默认 completion 安装行为符合平台预期。
- [ ] `--no-completion` / `-NoCompletion` 可关闭 completion 安装。
- [ ] 卸载默认保留配置和数据，`--purge` / `-Purge` 会清理配置和数据。
- [ ] 重复安装不会破坏已有配置，除非显式传入覆盖配置选项。

## 5. 验收

- [ ] 已按 [TESTING.md](TESTING.md) 完成当前发布所需的本地、远程或企业环境验收。
- [ ] 破坏性命令测试只作用于测试 label、测试容器、测试 volume 或临时 registry。
- [ ] 远程测试完成后已清理临时容器、volume、network、registry 和测试目录。
- [ ] 新增失败、跳过或非阻断项已记录到 `CHANGELOG.md`。

## 6. 回滚

- [ ] 当前版本和上一版本发布归档及 checksum 可追溯。
- [ ] 已验证卸载当前版本后安装上一版本可以执行 `dm version`。
- [ ] 回滚说明没有依赖本地临时路径。
