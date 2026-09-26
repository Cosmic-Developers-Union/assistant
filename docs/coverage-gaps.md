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

`internal/claudecfg.AssistantCommand` 曾列在此（~78%）：`os.Executable` 的错误分支
需要故障注入。现已抽出可覆盖的包级 `AssistantExecutable` var，三个分支（取不到
路径、空路径、相对路径）都有测试，覆盖率 92.3%。同理 `ClaudeVersion` /
`SupportsBare` 的探测改经 `Prober` 接口，失败与缓存路径不再需要造真脚本。

## 三、SDK / 标准库不会失败的返回

| 包 | 函数 | 原因 |
|---|---|---|
| `internal/setup` | `NewAdmin` 95.7%、`NewRepoClient` 95.5% | `gitea.NewClient`（SDK v1.2.0）**永不返回 error**：只应用 setup 用到的三个 `ClientOption`，而 `SetToken`/`SetHTTPClient`/`SetUserAgent` 恒返回 nil（只有 `UseSSHCert`/`UseSSHPubkey` 可能失败，setup 不用） |
| `internal/setup` | `RandomPassword` 75% | Go 1.24+ 的 `crypto/rand.Read` 不返回错误（失败即 panic），无法注入 |
| `internal/setup` | `do` 89.3%、`userTokenRequest` 89.3% | `json.Marshal`（入参类型固定）/ `http.NewRequestWithContext`（host 已由 `validate` 保证可解析）/ `io.ReadAll`（httptest 下无法构造读取失败） |
| `internal/status` | `getJSON` 的相关分支 | 同上；**已覆盖** 403→`PermissionError`、非 2xx 带响应体、JSON 解析失败带路径、`out` 为 nil 与空体不解析 |

## 四、需要真实进程 / 网络 / 计时器

会话执行已统一在 `internal/claude`：`Runner` 接口是唯一需要注入的执行点，生产实现
`execRunner` 是唯一启动子进程的地方。它的错误/超时/信号路径由
`internal/claude/runner_test.go` 用 `sh` 驱动的**真实子进程**覆盖（那是它唯一该被真实
驱动的地方）；dispatcher 与 daemon 只注入假 Runner（`stubRunner`），不再写假可执行
脚本。

| 包 | 函数 | 处理方式 |
|---|---|---|
| `internal/dispatcher` | `RunSession` 的真实 `claude` 调用 | 逻辑经包级 `SessionRunner`（可替换，读写有锁——RunLoop 并发起会话）测到；真实调用交给 `make test-e2e` |
| `internal/daemon` | 真实 `claude` 调用 | 经 `ChatConfig.Claude` 注入假 Runner；此前是 `RunClaude` 旁路，会绕过真实 spawn 路径，已删除 |
| `internal/setup` | `personal_token.go` 的 `CheckRedirect` | 仅服务端返回 3xx 时调用，需真实重定向链路 |

## 五、`cmd/assistant` 的剩余缺口

该包阈值 90%，实测 **89.0%**（3284 条语句，未覆盖 360），**距阈值还差 32 条**。

| 文件 | 未覆盖语句 | 主要形态 |
|---|---|---|
| `cmd/assistant/daemon.go` | 104 | `runWeixinLogin`（交互式扫码登录）等 |
| `cmd/assistant/dispatch.go` | 103 | 调度器装配后的回调路径（需真实待办与 Gitea 往返） |
| `main.go` | 25 | `main()` 全体：signal 接线 + `ExecuteContext` + `os.Exit`，无返回路径 |
| `config.go` | 16 | `readAPIKey` 的 pty 分支、备份/写盘失败臂 |
| `serve.go` / `login_identity.go` / `init.go` / `login.go` 等 | 各 12–15 | 注缝之外的 IO 失败臂、真 pty |

**注意措辞**：这些是「当前注入缝之外」的路径，不是「原则上不可达」。其中两类确有
可达路径，只是各有代价：

- `main.go` 的 25 条可用**子进程**方式驱动（`go test` 里 exec 自己编译出的二进制、
  发信号、断言退出码），代价是引入一个真实子进程测试装置；
- `daemon.go` / `dispatch.go` 的 207 条（占未覆盖的 57%）需要真实扫码或真实 Gitea
  往返，只能走 `make test-e2e`。

按 `AGENTS.md`：这类边界用可注入的缝把逻辑测到、真实调用交给 e2e，**不为凑数字写
无意义的测试**。因此本包的缺口如实登记；是否需要为那 32 条引入子进程装置，是取舍
问题而非「漏测」。

## 六、形式化规格约束的语义核

按 `AGENTS.md`：`internal/dispatcher` 的循环守卫/去重与 `internal/status/verify.go`
（对应 `formal/Dispatcher.lean` 的 `completeOk`）无论包整体百分比起伏都应接近
100%。这两处的语义由形式化规格钉住，改动必须同步
`spec/ReviewStateMachine.tla` 并让 `lean formal/Dispatcher.lean` 与 TLC 重新通过。
