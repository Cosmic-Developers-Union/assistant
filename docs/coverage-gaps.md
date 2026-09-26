# 覆盖率实测与不可达边界（如实报告）

本文件记录覆盖率门禁落地后，**仍然无法在单元测试里触达**的语句及其原因。
按 `AGENTS.md` 的约定：覆盖率是必要条件而非充分条件，达不到数字时如实说明
是哪些函数、什么原因，**不用无意义的测试堆数字**。

测量方式：`GOCACHE=/tmp/assistant-gocache GOTMPDIR=/tmp go test ./<pkg>/ -coverprofile=<file>`
后 `go tool cover -func=<file>`。门禁入口 `make cover`（阈值表见 `scripts/coverage.sh`）。

## 一、需要故障注入缝才可达的写入路径

这些函数的结构是「建临时文件 → 写 → fsync → rename」，中间每一步失败都要
清理并报错。触发它们需要让 `os.CreateTemp` / `*os.File.Write` / `Sync` /
`Close` / `os.Rename` 失败——标准库不提供这类注入点，且**不允许为测试改产品
代码**（会引入仅为测试存在的间接层，反而降低可读性）。

| 包 | 函数 | 覆盖率 | 未覆盖的语句 |
|---|---|---|---|
| `internal/credentials` | `Save` | 69.2% | `temp.Chmod` / `temp.Write` / `temp.Sync` / `temp.Close` / `os.Rename` 的失败分支 |
| `internal/instances` | `SaveBytes` | 64.3% | `CreateTemp` / `Write` / `Close` / `Rename` 的失败分支 |

`os.CreateTemp` 无法被诱导返回 `IsDir` 句柄，`Write` 到已打开的普通文件在
tempdir 里也不会失败。**已覆盖**的部分：空/非法内容被 `Validate` 拒绝、
父路径是普通文件（`MkdirAll` 失败）时报错而不是静默丢内容、成功路径的 0600
权限位与无 `.tmp` 残留。

## 二、依赖构建期常量的守卫

| 包 | 函数 | 覆盖率 | 原因 |
|---|---|---|---|
| `internal/agents` | `builtin` 的 panic 守卫 | ~96% | `//go:embed` 的 `builtin.json` 损坏、缺少 main 预设——只能在构建期破坏嵌入文件触发 |
| `internal/claudecfg` | `AssistantCommand` | ~78% | `os.Executable` / `filepath.Abs` 的错误分支需要故障注入 |

## 三、SDK / 标准库不会失败的返回

| 包 | 函数 | 原因 |
|---|---|---|
| `internal/setup` | `NewAdmin` 95.7%、`NewRepoClient` 95.5% | `gitea.NewClient`（SDK v1.2.0）**永不返回 error**：只应用 setup 用到的三个 `ClientOption`，而 `SetToken`/`SetHTTPClient`/`SetUserAgent` 恒返回 nil（只有 `UseSSHCert`/`UseSSHPubkey` 可能失败，setup 不用） |
| `internal/setup` | `RandomPassword` 75% | Go 1.24+ 的 `crypto/rand.Read` 不返回错误（失败即 panic），无法注入 |
| `internal/setup` | `do` 89.3%、`userTokenRequest` 89.3% | `json.Marshal`（入参类型固定）/ `http.NewRequestWithContext`（host 已由 `validate` 保证可解析）/ `io.ReadAll`（httptest 下无法构造读取失败） |
| `internal/status` | `getJSON` 的相关分支 | 同上；**已覆盖** 403→`PermissionError`、非 2xx 带响应体、JSON 解析失败带路径、`out` 为 nil 与空体不解析 |

## 四、需要真实进程 / 网络 / 计时器

| 包 | 函数 | 处理方式 |
|---|---|---|
| `internal/dispatcher` | `RunSession` 真实 `claude` 调用 | 逻辑经 `Deps` 注入缝测到（`RunSession` 是 `func(SessionRequest) SessionOutcome` 字段）；真实调用交给 `make test-e2e` |
| `internal/daemon` | `chat.go: runStreaming` | 同属会话桥接；需要的部分经注入缝覆盖，真实流式进程走 e2e |
| `internal/setup` | `personal_token.go` 的 `CheckRedirect` | 仅服务端返回 3xx 时调用，需真实重定向链路 |

## 五、形式化规格约束的语义核

按 `AGENTS.md`：`internal/dispatcher` 的循环守卫/去重与 `internal/status/verify.go`
（对应 `formal/Dispatcher.lean` 的 `completeOk`）无论包整体百分比起伏都应接近
100%。这两处的语义由形式化规格钉住，改动必须同步
`spec/ReviewStateMachine.tla` 并让 `lean formal/Dispatcher.lean` 与 TLC 重新通过。
