# 当前覆盖率与测试边界

架构重构后的逐包门禁为核心 ≥90%、适配器 ≥80%。`make cover` 同时要求测试全绿。
最近一次验证：runtime 91.0%、status 91.5%、cli 92.0%、credentials 92.1%、
claude 95.5%、integration 97.7%、integration/gitea 100%；QQ 88.3%、微信 84.4%、
Telegram 92.2%、MCP 92.9%、project 93.8%、skills 100%。后续修改以实际门禁输出为准。

cmd/assistant 是只调用命令层后 os.Exit 的进程入口，单测不能直接调用退出语句。
它登记在 scripts/coverage.sh 的 entry_points，而非伪装成资源包；构建后的二进制
另做帮助与错误退出冒烟。业务行为和信号处理由 internal/cli 测试。

尚未完全覆盖的系统边界包括：

- credentials.Save、runtime.atomicFile 的磁盘写入、Sync、Close 等系统失败。
  已测试创建/替换失败与权限契约；不引入只为制造磁盘错误的生产抽象。
- runtime 会话归档与 worktree 的实际磁盘/子进程突发故障。已测不可信路径、
  非普通文件、缺失记录、远端失败、head 漂移及收尾顺序，不能穷举操作系统故障。
- Claude 进程的真实模型/账号登录：进程协议用替身验证，JSON 流和启动器独立测试；
  未用生产模型凭据执行真实评审。
- QQ/微信/Telegram 的真实账号和外网服务：协议用本地服务器与伪造消息验证，
  登录方式及真实平台权限需集成环境验收。
- Gitea 原生 auto-merge 的真实检查转绿排定：SDK 请求契约、状态判定、双批准、未知合并响应
  和 dry-run 写拦截都有测试，生产站点验收必须使用独立仓库。
  账号/限定令牌、标签、协作者/保护配置、Actions secret 与全站 @ 召唤已在临时
  Gitea 1.27 跑 TestProjectGiteaBootstrapAndCrossRepositoryMention，通过旧提及停止/新提及
  再入队的真实平台验证。

真实 S3 边界已补充 `TestSessionS3RoundTrip`，在临时 MinIO 验证主记录与子代理
上传及换本地根恢复。无测试环境变量时跳过，`make test-e2e-s3` 会启动环境执行。
并发核心另跑 race 检查；形式化规格另跑 Lean/TLC，覆盖率不能代替这些验证。

agent-runtime 是独立 module，仅有接口类型，没有引擎或 TUI 实现；它单独检查
编译/vet，不算主线行为覆盖，也不冒充已完成的轻量运行时。
