# Docker API 依赖迁移清单

本文档记录从旧 Docker Go SDK 迁移到新 Moby 拆分模块时的影响面和执行清单。目标是先明确会导致哪些代码调整，再按阶段实施迁移。

## 背景

当前项目使用:

```go
github.com/docker/docker v28.3.3+incompatible
```

当前 SDK 内置 Docker Engine API 默认版本为 `1.51`，项目创建 Docker client 时启用了 API version negotiation，因此默认会与目标 Docker daemon 自动协商版本。

后续建议迁移到:

```go
github.com/moby/moby/client v0.5.0
github.com/moby/moby/api v1.55.0
```

`github.com/moby/moby/client` 是新拆分出的 Docker/Moby client 模块，配套 API types 来自 `github.com/moby/moby/api`。这不是简单的 patch 升级，而是一次 Docker SDK 依赖体系迁移。

过渡阶段说明: 当前 `main` 仍需要旧 `github.com/docker/docker` SDK 参与编译，`github.com/moby/moby/client v0.5.0` 会拉高 `github.com/docker/go-connections` 到 `v0.7.0`，导致旧 SDK 编译失败。因此第一阶段先固定 `github.com/moby/moby/client v0.4.0` 与旧 SDK 共存；待业务命令完全迁移并移除旧 SDK 后，再升级到 `v0.5.0`。

## 当前扫描结果

当前工程中直接引用 `github.com/docker/docker` 的范围:

| 范围 | 数量 |
| --- | ---: |
| Go 文件 | 47 |
| import 引用 | 约 116 |
| 主要业务目录 | `internal/commands/diagnostics`、`internal/commands/backup`、`internal/commands/reverse`、`internal/docker` |

按目录统计:

| 目录 | 文件数 | 说明 |
| --- | ---: | --- |
| `internal/commands/diagnostics` | 26 | health、network、volume、prune、logs、doctor、image tree 等报告命令 |
| `internal/commands/backup` | 7 | backup/restore、image load/save、network/volume/container recreate |
| `internal/commands/reverse` | 5 | inspect 到 run/compose 输出 |
| `internal/docker` | 3 | 项目内 Docker client、镜像和容器封装 |
| `internal/commands/images` | 2 | image save/load |
| `internal/resourcefilter` | 2 | 容器、镜像、volume 筛选 |
| `internal/runconfig` | 1 | inspect 配置解析 |
| `internal/completion` | 1 | Docker 资源补全 |

按 import 类型统计:

| 旧 import | 数量 | 新 import |
| --- | ---: | --- |
| `github.com/docker/docker/api/types/container` | 37 | `github.com/moby/moby/api/types/container` |
| `github.com/docker/docker/api/types/volume` | 17 | `github.com/moby/moby/api/types/volume` |
| `github.com/docker/docker/client` | 15 | `github.com/moby/moby/client` |
| `github.com/docker/docker/api/types/network` | 13 | `github.com/moby/moby/api/types/network` |
| `github.com/docker/docker/api/types/image` | 11 | `github.com/moby/moby/api/types/image` |
| `github.com/docker/docker/api/types/mount` | 6 | `github.com/moby/moby/api/types/mount` |
| `github.com/docker/docker/api/types/filters` | 5 | 迁移为 `github.com/moby/moby/client.Filters` |
| `github.com/docker/docker/api/types` | 5 | 多数需要迁移为 `client` result/options 或 `api/types/system` |
| `github.com/docker/docker/api/types/build` | 2 | `github.com/moby/moby/api/types/build` |
| `github.com/docker/docker/api/types/registry` | 2 | `github.com/moby/moby/api/types/registry` |
| `github.com/docker/docker/pkg/stdcopy` | 2 | `github.com/moby/moby/api/pkg/stdcopy` |
| `github.com/docker/docker/api/types/strslice` | 1 | `github.com/moby/moby/api/types/strslice` |

## 会导致的主要调整

### 1. import 路径变化

基础替换:

```go
github.com/docker/docker/client
```

改为:

```go
github.com/moby/moby/client
```

API types 基本改为:

```go
github.com/moby/moby/api/types/...
```

`stdcopy` 改为:

```go
github.com/moby/moby/api/pkg/stdcopy
```

### 2. Docker client 方法签名变化

新 `github.com/moby/moby/client` 将部分 options 和 result 类型移动到了 `client` 包内，不能只替换 import。

典型变化:

| 旧写法 | 新写法 |
| --- | --- |
| `container.ListOptions` | `client.ContainerListOptions` |
| `image.ListOptions` | `client.ImageListOptions` |
| `volume.ListOptions` | `client.VolumeListOptions` |
| `network.ListOptions` | `client.NetworkListOptions` |
| `types.DiskUsageOptions` | `client.DiskUsageOptions` |
| `filters.NewArgs()` | `client.Filters{}` 或 `make(client.Filters)` |

返回值也有变化:

| 旧行为 | 新行为 | 调整 |
| --- | --- | --- |
| `ContainerList` 直接返回 `[]container.Summary` | 返回 `client.ContainerListResult` | 使用 `.Items` |
| `ImageList` 直接返回 `[]image.Summary` | 返回 `client.ImageListResult` | 使用 `.Items` |
| `VolumeList` 返回 volume list response | 返回 `client.VolumeListResult` | 使用 `.Items` 和 `.Warnings` |
| `DiskUsage` 返回 `types.DiskUsage` | 返回 `client.DiskUsageResult` | 调整 prune 报告构建逻辑 |
| `Ping` 返回 `types.Ping` | 返回 `client.PingResult` | 调整 doctor 检查接口 |
| `ServerVersion` 返回 `types.Version` | 返回 `client.ServerVersionResult` | 调整 doctor 检查接口 |
| `VolumesPrune` 返回 `volume.PruneReport` | 返回 `client.VolumePruneResult` | 使用 `.Report` |

### 3. filters 模型变化

旧 SDK 使用:

```go
filters.NewArgs()
filters.Arg(...)
```

新 client 使用:

```go
client.Filters{}
make(client.Filters).Add("label", "env=test")
```

会影响:

- `dm prune`
- `dm volumes`
- 使用 Docker API filters 的补全和报告逻辑

需要特别检查筛选条件序列化是否保持一致。

### 4. doctor/prune 的根 types 变化

旧代码中这些类型来自 `github.com/docker/docker/api/types`:

```go
types.Ping
types.Version
types.DiskUsage
types.DiskUsageOptions
```

迁移后需要分别改为:

```go
client.PingResult
client.ServerVersionResult
client.DiskUsageResult
client.DiskUsageOptions
```

影响:

- `dm doctor` 的 Docker ping/version 检查
- `dm prune` 的磁盘占用、镜像、容器、volume 和 build cache 汇总
- 相关 fake service 和单元测试

### 5. backup/restore 需要重点验证

`backup/restore` 使用了较多 Docker API:

- `ContainerList`
- `ContainerInspect`
- `ImageSave`
- `ImageLoad`
- `NetworkInspect`
- `NetworkCreate`
- `VolumeInspect`
- `VolumeCreate`
- `ContainerCreate`
- `ContainerStart`
- `ContainerRemove`

迁移后需要验证:

- inspect JSON 结构是否保持兼容
- restore 时 `ContainerCreate` 的 config、hostConfig、networkingConfig 是否仍可直接传入
- `ImageLoad` 返回流读取逻辑是否保持一致
- network/volume 已存在时的错误判断是否保持一致
- dry-run 输出是否不受影响

### 6. diagnostics/report 影响最大

`internal/commands/diagnostics` 是迁移面最大的模块，需要逐项验证:

- `dm doctor`
- `dm health`
- `dm network`
- `dm logs`
- `dm diff`
- `dm prune`
- `dm volumes`
- `dm registry`
- `dm tree`

重点关注:

- Docker API result 包装类型
- volume size helper 容器日志解析
- health/logs 中的 `stdcopy`
- image tree 对 image/container inspect 字段的读取
- prune apply 的安全边界和 dry-run 行为

### 7. completion 和 resourcefilter 需要保持远程 Docker 行为

补全和本地资源筛选会调用 Docker API:

- 容器列表
- 镜像列表
- volume 列表

迁移时需要确保:

- `--docker-host`、`.dm.yaml docker_host`、`DOCKER_HOST` 仍生效
- 补全读取远程 Docker endpoint 的行为不回退到本地
- 通配符和 filter 语法不变

## 建议迁移步骤

### 阶段 1: 依赖和核心 client

- [x] 新增过渡期 `github.com/moby/moby/client v0.4.0`
- [x] 新增 `github.com/moby/moby/api v1.55.0`
- [ ] 移除直接依赖 `github.com/docker/docker`
- [x] 迁移 `internal/docker/client.go`，新增 `NewMobyClient`
- [x] 保留旧 `NewClient` 兼容入口，避免业务命令在同一阶段被迫整体迁移
- [x] 确认 `DOCKER_HOST`、`DOCKER_TLS_VERIFY`、`DOCKER_CERT_PATH`、`DOCKER_API_VERSION` 仍兼容
- [x] 确认 `client.WithAPIVersionNegotiation()` 行为仍符合预期

阶段 1 状态: 已完成可编译的过渡式迁移。旧 `NewClient` 仍返回 `github.com/docker/docker/client.Client`，新 `NewMobyClient` 返回 `github.com/moby/moby/client.Client`。后续阶段按模块迁移业务命令，最后再移除旧 SDK 并升级到 `github.com/moby/moby/client v0.5.0`。

### 阶段 2: 项目 Docker 封装层

- [x] 迁移 `internal/docker/container.go`
- [x] 迁移 `internal/docker/image.go`
- [x] 对 `ContainerList`、`ImageList`、`NetworkList`、`ContainerInspect`、`NetworkInspect`、`VolumeInspect` 等 result 包装做内部适配
- [x] 优先让上层业务尽量少感知 SDK 差异

阶段 2 状态: 已完成。`ContainerManager` 和 `ImageManager` 内部改用 `NewMobyClient`，但公开方法仍返回旧 `github.com/docker/docker/api/types/...` 类型，避免 `images`、`reverse` 和 `pull mirror` 在同一阶段被迫迁移。当前通过 `internal/docker/compat.go` 做 JSON 结构转换；后续阶段完成上层模块迁移后，可移除这些兼容转换。

### 阶段 3: diagnostics

- [x] 迁移 doctor 的 `Ping` / `ServerVersion`
- [x] 迁移 prune 的 `DiskUsage` / filters / volume prune
- [x] 迁移 health/logs 的 `ContainerLogs` 和 `stdcopy`
- [x] 迁移 volume/network/image tree/report 相关 Docker API 调用
- [x] 更新并验证 fake service 和单元测试

阶段 3 状态: 已完成 Docker API 调用路径迁移。`internal/commands/diagnostics` 不再直接创建旧 `github.com/docker/docker/client.Client`，统一通过 `docker.NewMobyClient` 调用 Docker daemon。为控制迁移边界，报告结构和单元测试仍保留旧 `github.com/docker/docker/api/types/...` 类型，service 层负责将 Moby result/options 转换为当前报告逻辑使用的结构。

### 阶段 4: backup/restore

- [x] 迁移 backup/restore 相关 Docker API 调用路径
- [x] 验证 image save/load
- [x] 验证 network/volume metadata restore
- [x] 验证 container recreate
- [x] 验证 checksum、bundle、dry-run、replace、no-start 行为

阶段 4 状态: 已完成过渡式迁移。`internal/commands/backup` 的真实 Docker API 调用已改为 `docker.NewMobyClient` 和 `github.com/moby/moby/client` options/result；为控制迁移边界，backup manifest、archive、restore 编排和测试仍保留旧 `github.com/docker/docker/api/types/...` 类型，由 service 层负责转换。后续阶段完成 reverse、completion、resourcefilter 等模块迁移后，再统一移除旧 SDK 类型。

### 阶段 5: reverse/rerun、completion、filter

- [x] 迁移 reverse/rerun 的 inspect 类型
- [x] 迁移 runconfig parser 的 container config 类型
- [x] 迁移 completion 的 list result 处理
- [x] 迁移 resourcefilter 的 container/image/volume summary 类型
- [x] 确认筛选和通配符行为不变

阶段 5 状态: 已完成。`internal/docker.ContainerManager`、`ImageManager`、`reverse/rerun`、`runconfig`、`completion`、`resourcefilter` 和 `images save` 的本地资源处理已切到 `github.com/moby/moby/api` / `github.com/moby/moby/client` 类型。由于 backup 与 diagnostics 的报告结构仍处在旧 SDK 类型边界，当前在调用 `runconfig` / `resourcefilter` 前保留轻量转换；后续迁移这些报告结构后可删除桥接。

### 阶段 6: 测试和验收

- [x] `go test ./...`
- [x] `go vet ./...`
- [x] `scripts/check.ps1` 或 `scripts/check.sh`
- [x] 本地无 Docker 环境 smoke test
- [x] 本地 Docker 环境 smoke test
- [x] 远程 Docker endpoint 测试
- [x] backup/restore 小容器迁移测试
- [x] prune dry-run / apply 安全边界测试
- [x] completion 读取远程 Docker 资源测试

阶段 6 状态: 已完成。Windows 本地 `scripts/check.ps1` 已通过；VM `192.168.31.57` 使用不可达 `DOCKER_HOST` 完成 smoke 9 PASS / 5 XFAIL；同一 VM 在 Docker 29.1.3 上完成 destructive/full 48 PASS / 12 XFAIL，覆盖本地 registry、pull/save/load、reverse/rerun、backup/restore、report 和 prune apply 安全边界；Windows 侧通过 `--docker-host tcp://192.168.31.57:2375` 验证远程 doctor、reverse、health、logs、prune dry-run 以及容器/镜像 completion。

## 风险评估

| 风险 | 等级 | 说明 |
| --- | --- | --- |
| 编译破坏 | 高 | options/result 类型变化较多 |
| 报告字段变化 | 中 | 新 API types 字段可能变化，尤其是 image、disk usage、network |
| backup/restore 行为变化 | 中高 | restore 依赖 inspect config 与 create API 的兼容性 |
| completion 行为变化 | 中 | result 包装和 Docker endpoint 配置需要复测 |
| 运行时兼容性 | 中 | 需要验证旧 Docker daemon 自动协商行为 |
| 发布风险 | 高 | 不建议直接进入 `v2.0.x` 维护分支 |

## 推荐版本策略

- `release/v2.0`: 保留旧 `github.com/docker/docker` SDK，只做 bugfix 和发布修复。
- `main`: 执行 Moby client 迁移，作为后续 `v2.1` 或 `v3.0` 候选。
- 如果迁移过程中发现命令行为或报告结构需要调整，建议归入 `v3.0`。

## 迁移完成判定

满足以下条件后，可认为 Docker API 依赖迁移完成:

- [ ] 全项目无 `github.com/docker/docker` import。
- [ ] `go.mod` 不再直接 require `github.com/docker/docker`。
- [x] `go test ./...` 通过。
- [x] `dm doctor` 能显示 Docker daemon API 信息。
- [x] `dm health`、`dm network`、`dm volumes`、`dm prune` 能正常输出报告。
- [x] `dm backup` / `dm restore` 能完成小容器端到端恢复。
- [x] `dm completion` 能读取当前 Docker endpoint 的资源。
- [x] 远程 Docker endpoint 行为与迁移前一致。
