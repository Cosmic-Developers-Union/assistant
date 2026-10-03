# Gitea 接入与项目工具

接入有两个边界：instance 管用户级账号凭据，project 管当前 Git 项目。
run 的服务连接由 config.yaml 手写，不依赖个人凭据库，也没有项目白名单。
下面命令不会触碰其他站点；先用 --dry-run 查看操作。

## 站点管理员创建机器人

先登记管理员实例（首次登录需要能创建个人令牌的账号密码）：

```sh
assistant instance add gitea --name work --url https://gitea.example.com --username admin
assistant instance provision gitea --admin work --dry-run
assistant instance provision gitea --admin work
```

交互提示中按一次 Ctrl+C 即取消退出（退出码 130），密码不会回显，退出前恢复终端状态。
SIGTERM 也会取消输入并退出（退出码 143）；取消输入不会写入凭据。

缺省创建 ai、merge，实例名为 work-ai、work-merge。新账号使用随机密码，
发放 read:user、read:organization、write:repository、write:issue 的限定令牌；
持久凭据包含令牌和加密密码，密码不以明文落盘、不回显。平台账号不是站点管理员。
管理员个人实例的令牌另含 write:admin，才可创建站点账号；不会向机器人授予该范围。
凭据放在用户级 credentials.json，目录 0700、文件 0600。列表和 show 不显示令牌。

重复执行时验证并复用已有令牌，不每次发令牌，也不删除账号的其他令牌。
每个账号接入成功立即保存；如果下一步失败，先前成功的账号可在重试时复用。

已有账号但未登记、或本地令牌失效时，不自动重置账号密码；管理员登录已保存加密密码时，
可直接为已有用户发令牌。也可以提供目标账号密码文件接入：

```sh
assistant instance provision gitea --admin work \
  --reviewer-password-file /secure/ai-password \
  --merger-password-file /secure/merge-password
# 给登录账号创建并保存新令牌，无需再输入用户名和密码：
assistant instance create-token work
# 管理员为其他用户发令牌，保存为独立实例供 MCP 使用：
assistant instance create-token work --user ai --name work-ai
# 只读取已保存令牌，通过原有环境变量交给 MCP：
GITEA_ACCESS_TOKEN="$(assistant instance token work-ai)" assistant mcp gitea
```

首次登录成功即创建令牌供 MCP 使用，并以 AES-GCM 加密密码保存到 credentials.json。
按用户指定采用内置密钥；持有二进制者可以提取密钥解密，这不提供独立于二进制的
安全边界。文件仍为 0600，目录 0700；list/show 不显示密码或令牌。
旧记录仅有令牌时，重新运行 instance add gitea --name work 登录一次补齐加密密码，
站点和用户名复用原记录，登录失败保留旧凭据。密码变更后也用该方式重新登录。
instance token 只向 stdout 输出已保存令牌，不联网、不询问密码、不修改平台或本地状态。
create-token 可用 --password-file 覆盖已保存密码；启用两步验证时用 --otp 提供当前验证码。
未指定 --user 时更新登录账号令牌；指定其他用户时，缺省保存为“登录实例名-用户名”，
可用 --name 指定名称。管理员密码不会复制到目标账号，旧平台令牌不自动撤销。

也可用 instance add gitea 单独登记 ai 或 merge。创建账号成功但发令牌/落盘失败，
命令会报错，不能把它当成完整成功；管理员应恢复账号凭据后重试，工具不隐式回滚
平台账号或重置密码。账号/email/name 可用相应 flags 明确指定。

## 当前项目标签、协作者与分支保护

在 Git 项目里选择对应站点的管理员或仓库管理员实例：

```sh
assistant project configure --instance work --dry-run
assistant project configure --instance work --required-checks build,test
assistant project labels --instance work       # 只规范标签
```

从所选实例站点匹配 Git remote（GitHub origin 不会盖过 Gitea remote），多个不同
仓库 remote 报错；无法推导时用 --repo owner/name。--dir 可指定项目目录。

configure 规范完整标签体系；ai 加为 write 协作者，merge 加为 admin 协作者。
merge 需要仓库管理员权限登记他人评审请求，但不成为站点管理员；默认分支保护
禁止直接推送、双批准、批准随提交过期、阻止驳回/未回应请求/落后分支，合并白名单
只含 merge，管理员也不能绕过。已有保护规则更新时保留未指定的检查上下文。
必要检查需显式提供；不要把 assistant 自身的 workflow context 配为必要项。

标签规范化会删除规范之外的标签，与 goal.md 的 action 规则相同。
--reviewer/--merger 可以配置账号名称；需要自定义内容评审/合并流程时须同步 CI 身份约定。

## 安装和卸载 Gitea Actions

```sh
assistant project install action --dry-run
assistant project install action
assistant project uninstall action
```

安装只在当前 Git 项目生成 .gitea/workflows/assistant.yml，不读取站点配置、
凭据或 Git remote，也不启用平台 Actions 或写入 secret。没有登录、没有 merge
账号、甚至凭据文件无效时，仍可生成。账号与平台设置属于站点接入工作。
workflow 使用平台提供的 GITEA_HOST / GITEA_REPOSITORY / GITHUB_TOKEN，并引用
仓库 Actions secret MERGE_TOKEN；运行前在平台配置该 secret，生成器不接触它。

生成物自动绑定当前 assistant 构建版本，两个任务使用同一固定镜像标签。
发布构建使用发布标签，未打标签的构建使用 sha-<完整 commit>；本地 Make 构建、
Release 与镜像发布沿用相同规则。无需 --version，也不提供 --instance、--repo
或 --merge-instance。工具升级不改变已经提交的 workflow；重新生成并提交才更新。
对应版本镜像由发布流程提供，本地新 commit 的镜像须发布后才能由 runner 拉取。

存在同名的非托管 workflow 则拒绝覆盖；重复安装可更新自己的 workflow。
--dry-run 不写文件。本地文件的安装/卸载需提交推送后生效；命令不替用户提交或推送。
卸载只删除带 managed-by: assistant 标记的 workflow，不关闭仓库 Actions、不删其他
workflow，也不移除平台账号、权限或 secret。

## 安装和卸载 MCP

```sh
assistant project install mcp
assistant project install mcp --force   # 备份后修复错误 JSON 或覆盖同名冲突
assistant project uninstall mcp
```

.mcp.json 内新增命名 server gitea，默认启动参数为 assistant mcp gitea。
安装只需要本地 Git 项目，无需凭据、Gitea remote 或运行配置；站点与令牌在 MCP
启动时按现有规则解析，缺失或歧义届时报错。MCP 不增加 --instance 参数；同一站点
有多个账号时，通过原有 GITEA_ACCESS_TOKEN 或 GITEA_ACCESS_TOKEN_FILE 明确提供令牌。

保留所有其他 server 和顶层字段；同名 server 若不是本工具的启动配置则拒绝
覆盖或卸载。缺失、空白文件和空对象可直接初始化；错误 JSON 或错误结构默认保留
原文件并报告修复方式。安装的 --force / -f 会先保存原始字节到同目录的
.mcp.json.assistant-backup-<UUID>（权限 0600），报告备份路径，再修复配置。
可解析配置只覆盖 gitea 或损坏的 mcpServers 字段，其余字段和 server 保留；
整个 JSON 无法解析时备份后重建。--force --dry-run 不写配置或备份。
卸载不支持 --force。读写限制在当前项目根，symlink 越界仍报错。
卸载不删除用户的其他配置。
重新安装会将此前本工具生成的 assistant-gitea 配置迁移为 gitea；卸载也兼容旧名称。
此前生成的 --instance 启动参数会在重新安装时移除。
原 assistant mcp gitea 的显式 host/token 与 remote 探测方式继续可用。

## 召唤与验证

run 使用服务令牌的真实账号身份，标签待办扫描所有可见仓库，全站 mentioned_by
搜索捕捉 @ 该账号的 PR。公开仓库即使无 workflow、无协作者关系也可发现，实际
写 review 仍由平台权限决定。历史提及在该账号正式回应后不再触发，新 @ 评论可
再次召唤；draft/WIP 仍按契约排除。

单测覆盖账号/令牌、读写边界、配置归属、权限与保护字段及错误恢复。隔离 Gitea
端到端用例 TestProjectGiteaBootstrapAndCrossRepositoryMention 验证真实接入与跨仓库
召唤；无测试环境时跳过，make test-e2e 自动起环境。

新的轻量聊天/TUI 引擎由独立任务实现，接口在 [agent-runtime](../agent-runtime/README.md)。
这组接入命令不依赖新引擎，也不提前更换主线的评审运行时。
