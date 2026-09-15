# assistant

一个 Go 静态二进制，两种角色：

1. **仓库辅助机器人**（Gitea Actions 事件 / schedule 驱动）：机械、启发式地处理 Issue、PR 的例行事务（标签体系维护、Issue 标签规范、PR 评审状态同步、已批准 PR 自动合并）。
2. **评审会话调度引擎**（宿主机 systemd 常驻）：检测 Gitea 待办并自动拉起 headless 评审会话，直到 review 完成。

两种角色共享 `GITEA_HOST` / `GITEA_ACCESS_TOKEN` / `GITEA_REPOSITORY` 配置约定与 Gitea API 客户端。`claude` CLI 是调度引擎的外部运行时依赖（不在本仓库内），需在宿主机安装并登录；其余能力全部由二进制自带。

## 命令

| 命令 | 角色 | 说明 |
| --- | --- | --- |
| `assistant setup` | 初始化 | 建机器人账号/令牌、配协作者与分支保护、补齐标签，并写入 `config.json` |
| `assistant login` | 初始化 | **唯一登录入口**：`host + username + password`（无参数则交互式询问），校验身份并按身份派生用途令牌写入 `credentials.json`；`login list`/`login remove` 查看与清除 |
| `assistant repos` | 初始化 | 管理 `config.json` 中登记的仓库：`list` / `add --dir` / `remove`（只改本地，不触碰服务端） |
| `assistant init` | 初始化 | 只初始化当前仓库：复用平台凭据与 ai/merge 账号，配保护/标签/secret，并自动登记进 `config.json` |
| `assistant deinit` | 初始化 | init 的反命令：从 `config.json` 移除当前仓库；`--purge` 同清理服务端（保护/协作者/secret） |
| `assistant actions` | 初始化 | 为配置中的仓库写入 Actions secrets（merge 令牌等；身份约定 ai/merge，无需 variable） |
| `assistant install` | 开发者 | 在仓库检出内配置 skills / AGENTS.md / workflow / 各 AI CLI 的 MCP |
| `assistant uninstall` | 开发者 | 移除 `install` 写入的内容（只触碰带 marker 的） |
| `assistant doctor` | 开发者 | 两级体检：本地 install 产物 + 服务端（分支保护/标签/协作者/merge 令牌），只读 |
| `assistant validate` | 部署 | 校验用户配置：config.json（平台/仓库/provider/微信桥）与 credentials.json（身份与用途令牌），只读不联网 |
| `assistant config init` | 部署 | 模板补全 config.json：补 `$schema`、按登录身份预填平台、指定 provider 与密钥；旧文件备份为 `.bak` |
| `assistant mcp gitea` | 开发者 | MCP 包装层：自动检测项目站点与开发者令牌后拉起 gitea-mcp |
| `assistant mcp daemon` | 开发者 | 自举的 daemon 状态 MCP：自动发现运行中的 `assistant run`，只读查询队列/会话/结果 |
| `assistant weixin` | 初始化 | 微信对话桥（openclaw ilink 协议）：`login` 扫码、`status` 查看 |
| `assistant check` | 机器人 | 按标签检索待 triage 的 Issue 和待 review 的 PR（只读） |
| `assistant sync` | 机器人 | 规范 Issue 标签并把 PR 原生评审状态同步为状态标签（单次执行） |
| `assistant automerge` | 机器人 | 维护官方评审请求（merge 管理员身份）并合并门禁全绿的已批准 PR（一次至多一个，squash） |
| `assistant run` | 调度引擎 | 长驻 daemon：调度主循环 + 只读状态 API + 可选微信对话桥 |
| `assistant list` | 调度引擎 | 只读列出当前待办（验证 host/仓库/令牌/标签链路） |
| `assistant review <n>` | 调度引擎 | 立即评审单个 PR（跳过检测，端到端调试用） |
| `assistant triage <n>` | 调度引擎 | 立即分诊单个 Issue（跳过检测） |

`assistant --help` 查看全部选项。

### Shell 补全

Cobra 内置的补全命令已启用（命令、旗标与文件路径）：

```bash
# bash（当前会话）
source <(assistant completion bash)
# bash（持久）
assistant completion bash > /etc/bash_completion.d/assistant

# zsh（持久，写入 fpath）
assistant completion zsh > "${fpath[1]}/_assistant"
# fish
assistant completion fish > ~/.config/fish/completions/assistant.fish
```

`assistant completion --help` 查看全部说明。

## 多实例配置（config.json）

落点只有两个文件：**`config.json` 是用户配置**（平台、仓库、provider、微信桥——你手写），**`credentials.json` 由 assistant 管理**（`login`/`setup` 派生的身份与用途令牌，别手改）。

不指定配置文件时，所有命令维持环境变量单实例模式（`GITEA_HOST` / `GITEA_ACCESS_TOKEN` / `GITEA_REPOSITORY`）。要同时管理多台 Gitea、多个仓库，写一份 `config.json`。查找顺序：`--config` > `ASSISTANT_CONFIG` > 平台标准配置目录 `<UserConfigDir>/Cosmic-Developers-Union/assistant/config.json`（Linux `~/.config`、macOS `~/Library/Application Support`、Windows `%AppData%`）；**不读当前目录 `config.json`**（避免检出里的同名文件被误当运行配置），示例见 `config.example.json`。机器人命令与调度命令都会按 instance × repo 迭代：

凭据**不在** `config.json` 里：本地身份与用途令牌写在同目录的 `credentials.json`（0600，位置可用 `ASSISTANT_CREDENTIALS` 覆盖；`assistant login list` 查看），`config.json` 只描述管理哪些实例与仓库。

配置辅助（`assistant config init`）——**只补缺失项，绝不覆盖你已经写下的值**：

1. 写 `$schema: ./config.schema.json`，并把随二进制分发的 schema 放到 `config.json` 旁边（编辑器补全与悬停文档，离线可用；私有站点拉不到仓库 URL，所以就近落地）；
2. 用 `credentials.json` 里已登录的平台预填 `instances`（`reviewer: ai` / `merger: merge` 是约定值）；
3. `--provider minimax --api-key-stdin` 写入 provider 与 `default_provider`，密钥从 stdin 读（终端下隐藏输入，不进 shell 历史）；
4. 旧文件备份为 `config.json.bak`，原子写入 0600，写完立即跑一遍校验；重复执行无改动，`--dry-run` 只打印差异与合并结果。

```bash
assistant config init                                        # 补 $schema + 预填平台
assistant config init --provider minimax --api-key-stdin     # 顺带写入供应商与密钥
assistant config init --dry-run                              # 先看会改什么
```

```json
{
  "instances": [
    {
      "host": "https://gitea.example.com",
      "reviewer": { "name": "ai" },
      "merger": { "name": "merge" },
      "repos": [
        "owner/repo",
        { "name": "owner/another", "dir": "/srv/another" }
      ]
    }
  ]
}
```

`config.json` **不含 Gitea 令牌**（登录与 setup 派生的凭据都在 `credentials.json`）；AI 供应商的 `api_key` 属于用户配置，就写在 `providers` 里：

- `reviewer`（`ai`）：内容评审者账号名。
- `merger`（`merge`）：状态评审者（会签/合并）账号名。
- `repos`：仓库清单。字符串是 `owner/name` 简写（有 `dir`/`provider` 时写对象）；调度引擎的 `review`/`triage` 会话需要本地检出，多仓库时给出 `dir`（单仓库可省略，退回启动目录的检出）。
- `provider`（可选）：生效的供应商名，逐级回退 `repo.provider` > `instance.provider` > `default_provider`；定义与用法见下文「多 provider（供应商）」。
- 路径默认值按仓库隔离：日志 `<检出>/logs`、锁 `<检出>/dispatcher.lock`、worktree `<临时目录>/agent-dispatcher/<owner>-<repo>/worktrees`。
- 机器人账号的令牌在凭据库里按 `(host, 账号, purpose=review|merge)` 唯一存放（一个站点一个账号一条令牌）。
- `config.json` 含 provider 的 `api_key`、`credentials.json` 含 Gitea 令牌，都以 0600 写入，请勿提交到版本库（`.gitignore` 已忽略常见位置，建议自行确认）。

### 多 provider（供应商）

`providers` 是供应商配置，**直接写在 `config.json` 里**（它就是用户配置的一部分）：**内置了开箱即用预设**（`zhipu`/`glm`、`kimi`、`moonshot`、`minimax`、`opencode`/`zen`、`anthropic`、`openai` 等），预设补齐端点、令牌变量、模型档位、超时与上下文窗口——多数情况下只写 `api_key`（或 `auth_token`）即可，其余键（`env`/`settings`/`mcp`）按需覆盖预设同名取值：

- `env`：注入会话环境变量（可含密钥）；provider 自定义的 `mcp` server 会缺省继承它（server 自身 `env` 优先）；
- `settings`：Claude Code 原生 settings 片段（如 `model`、`apiKeyHelper`、`awsAuthRefresh`），合并进会话 `--settings`（`env`/`permissions` 逐键合并，其余顶层键覆盖托管默认）；
- `mcp`：原生 MCP server 定义（`.mcp.json` 形态），合并进会话 `--mcp-config`，同名 server 由 provider 覆盖（仓库既有的 `gitea` 等不受影响）。

未识别的键原样保留，随你手写扩展。示例：

```json
{
  "default_provider": "zhipu",
  "providers": {
    "zhipu": { "api_key": "your-zai-api-key" },
    "opencode": {
      "api_key": "sk-zen-...",
      "settings": { "model": "claude-sonnet-5" }
    }
  }
}
```

预设还会带上供应商官方 MCP（智谱视觉理解、MiniMax coding-plan 等，随会话注入；daemon 镜像已内置 `npx`/`uvx`）。未识别的 provider 名没有预设，按纯手写透传处理（`env`/`settings`/`mcp` 原样合并）；`env` 里给空串可删除预设默认，`mcp` 里给 `null` 可关闭预设 MCP。内置预设、各家的覆盖要点与坑位见 [`providers.md`](providers.md)。

选择粒度逐级回退：`repo.provider` > `instance.provider` > `default_provider`；微信对话桥用 `weixin.provider`（缺省回退 `default_provider`）。引用了未定义的名字直接报错，不会静默用错供应商。改完配置先跑 `assistant validate`：它会指出没被任何地方引用的 provider（配了但没生效）、缺 `api_key` 的 provider、缺身份或用途令牌的平台。

**provider 只有 `config.json` 一个落点**（它就是用户配置）；`<配置目录>/providers/` 目录不再被读取，遗留文件会在 `assistant validate` 里报出来。

`optimizations` 是跨供应商通用的全局优化点（同 `env`/`settings`/`mcp` 三段）：对所有会话打底生效，选中 provider 的同名取值覆盖其上（`repo.provider` 亦不例外）。适合放时长、上下文窗口、遥测开关、子 agent 模型等横向调优：

```json
{
  "optimizations": {
    "env": { "CLAUDE_CODE_EFFORT_LEVEL": "high", "API_TIMEOUT_MS": "3000000" }
  },
  "default_provider": "zhipu"
}
```

各家供应商的推荐优化点与坑位（1M 上下文、模型档位映射、tool search、beta header 等）见 [`providers.md`](providers.md)。

**只作用于运行时会话**：provider 覆盖进评审/分诊/对话会话的临时 `--settings` 与 `--mcp-config`，绝不写入仓库 `.claude/settings.json`/`.mcp.json`——密钥不进 git，仓库保持供应商无关。`run --dry-run` 会打印每个目标生效的 provider 与覆盖项数（值不落日志）。

少数供应商需要在会话启动时做动态调整（例如 opencode 网关要求的 `x-opencode-session` 会话请求头，经 `ANTHROPIC_CUSTOM_HEADERS` 注入）：这由代码级特化处理——`internal/provider/` 下一个供应商一个文件，实现 `Handler` 并在 `init` 注册，即可在会话启动前读写 `env`/`settings`/`mcp`；没有注册 handler 的 provider 原样通过。

### 平台管理：assistant login

`login` 是**唯一登录入口**，只负责身份与凭据，不做账号/仓库初始化（那是 `setup`）。没有参数时进入交互式询问，依次问 host、username、password（密码隐藏输入）：

```bash
assistant login                                       # 交互式：host → username → password
assistant login https://gitea.example.com --user alice            # 只缺密码时在终端隐藏输入
assistant login https://gitea.example.com --user alice --password-stdin --totp 123456  # 非交互/双因素

assistant login list                                  # 平台、身份与各用途令牌（只列令牌名，不显示令牌值）
assistant login remove https://gitea.example.com       # 移除平台条目与该站点的本地凭据（不触碰 Gitea 侧）
assistant login --rotate                              # 忽略已存令牌，重建该账号的用途令牌（同名旧令牌在站点上删除）
```

登录确定两件事：**这台站点以谁登录**（凭据库的 `identity`，含 `is_admin`），以及**派生哪些用途令牌**：

| purpose | 何时派生 | 用途 | scope |
| --- | --- | --- | --- |
| `mcp` | 总是 | 编辑器/CLI 里的 gitea MCP 工具面 | repository/issue 读写 + `read:user` |
| `admin` | 仅当账号是实例管理员 | `setup`/`init`/`actions` 等管理操作 | 上面的 + `write:admin` |

令牌名按 `assistant-<purpose>-<host>-<账号>` 确定性生成；重复登录时仍然有效的令牌原样复用（不新建、不需要密码），失效或 `--rotate` 时先删除站点上的同名旧令牌再新建，因此不会越登越多。Gitea 的建令牌端点只接受账号自己的密码（Basic Auth），所以令牌只能由登录派生——**没有 OAuth、tea 复用或手工录入等第二种方式**。

**能力边界**：非管理员账号同样可以登录并使用 MCP 工具面，但 `setup`/`init`/`actions` 会直接拒绝——登录时记录的是身份事实，授权判定仍以服务端为准（`setup.NewAdmin` 在线核对 `is_admin`）。review/merge 能力属于 `setup` 创建的 `ai`/`merge` 机器人账号（协作者权限、`MERGE_TOKEN` secret、分支保护与合并白名单），因此同样只由管理员开通。要管理就用**管理员账号**登录。

host 可省略：取配置中唯一实例；否则在带 Gitea remote 的检出内探测（`/api/v1/version`，多 remote 逐个尝试）；都推断不出来时交互式询问。凭据落点是 `config.json` 同目录的 `credentials.json`（0600，可用 `ASSISTANT_CREDENTIALS` 覆盖）。

### 仓库初始化：assistant init

`setup` 面向整台实例（账号/令牌/多仓库），`init` 面向当前仓库——在仓库检出内运行即可，平台必须已由 `login`/`setup` 登记：

```bash
assistant init                 # 复用 remote 推导仓库；也可以用 --repo owner/name
assistant init --create-repos  # 仓库不存在时创建
assistant init --dry-run
assistant deinit               # 仅从 config.json 移除当前仓库
assistant deinit --purge       # 同时删除分支保护、移除 ai/merge 协作者、删除 MERGE_TOKEN secret
```

`init` 做的事：复用或补齐平台上的 `ai`/`merge` 账号与令牌 → 配协作者（ai write / merge admin）→ 分支保护（同 `setup` 口径）→ 标签体系 → 写入仓库级 `MERGE_TOKEN` secret → 把仓库条目自动加入 `config.json`。`deinit --purge` 是其反操作；本地安装产物（skills/AGENTS.md/workflow/MCP）用 `assistant uninstall` 清理。两者都支持 `--dry-run`。

### 仓库登记：assistant repos

`repos` 只管理 `config.json` 里的 `instances[].repos`（本地登记，不触碰服务端），适合先登记、再用 `setup` 统一补齐服务端：

```bash
assistant repos list                                  # 列出所有平台已登记仓库（dir / merge 令牌状态）
assistant repos list --host https://gitea.example.com
assistant repos add owner/repo --host https://gitea.example.com --dir /srv/repo   # 登记（已存在则更新 dir）
assistant repos remove owner/repo --host https://gitea.example.com                # 移除登记（不触碰服务端）
```

`--host` 缺省时按「检出 remote 对应的平台 → 已登记该仓库的平台 → 唯一实例」推断；`--dir` 缺省时若当前检出的 remote 指向该仓库，自动登记检出根目录（一个实例下多个仓库时建议显式 `--dir`）。登记后用 `assistant setup --host <H>` 按配置补齐协作者/分支保护/标签/`MERGE_TOKEN`。

### 初始化：assistant setup

`setup` 把「评审 → 批准 → 会签 → 自动合并」闭环需要的一切配置好，幂等可重跑。它**不接收任何凭据参数**：管理员令牌来自 `assistant login` 写入凭据库的 `purpose=admin`，所以先用**管理员账号**登录：

```bash
assistant login https://gitea.example.com --user <管理员账号>

assistant setup --host https://gitea.example.com --repos owner/repo[,owner/another] --create-repos
```

非管理员身份运行 `setup` 会被直接拒绝（本地身份事实 + 服务端 `is_admin` 双重校验）。

流程：

1. 用凭据库里的 admin 令牌校验管理员身份；
2. 复用/创建 `reviewer`（默认 `ai`）与 `merger`（默认 `merge`）账号（随机密码不落盘）；
3. 为两个账号各收敛出**唯一一条**令牌（`assistant`），写入凭据库的 `purpose=review` / `purpose=merge`；建令牌端点仅接受 Basic Auth，必要时以管理员身份重置机器人密码后创建；
4. 把两个账号加为仓库协作者：reviewer 写权限，**merger 管理员权限**（合并白名单成员 + 分支保护读取），并收敛标签体系（补齐缺失、scoped 互斥、删除不在体系内的标签，与 `sync` 完全相同口径）；
5. 在默认分支配置分支保护：`required approvals=2`（内容批准 + 状态会签）、**合并白名单只含 merger（只有它可以合入）**、驳回阻塞（`block_on_rejected_reviews`）、过期批准作废、落后分支阻塞、**勾选「管理员须遵守分支保护规则」（`block_admin_merge_override`，管理员也不得绕过）**，以及**「有官方审核阻止了代码合并」（`block_on_official_review_requests`）**：合并 job（merge 管理员身份）会把 `/review`、`@ai` 提及登记为正式评审请求，内容评审者提交 review 后由它撤回遗留请求、门禁解除（Gitea 只允许 PR 作者或仓库管理员选择 reviewer，`gitea-actions` 会被拒绝，因此请求维护不在 sync）。注意：**团队评审请求**不会因成员 review 自动清除，需人工移除后合并才解除；API 的 `requested_reviewers` 字段有显示滞后，不代表门禁实际状态；
6. 把实例与仓库写回 `config.json`（落点：`--config` / `ASSISTANT_CONFIG`，否则平台标准配置目录），机器人令牌写入 `credentials.json`。`--create-repos` 会在仓库不存在时自动创建私有仓库（auto_init，默认分支 main）。

常用开关：`--dry-run`（只输出计划）、`--allow-admin-override`（放开管理员绕过，缺省关闭）、`--reviewer` / `--merger`（账号名）、`--required-approvals`、`--email-domain`。

## 仓库辅助机器人

- `sync`：**强制收敛标签体系**——补齐缺失标签、把 scoped 标签设为互斥、删除不在规范体系内的标签；规范 open Issue 标签（含 duplicate/wontfix 自动关闭）、把 PR 评审状态同步为状态标签。`status/review` 即「评审请求中」标记：原生 review 请求、`@ai`/`@reviewer` 提及、行首 `/review` 命令任一出现就进评审队列，不再被门禁（冲突/落后/必要检查失败）阻断——门禁只在 `automerge` 合并时校验，避免「检查被取消」直接卡住评审。单次执行、幂等，由 Gitea Actions 事件驱动运行（只用内置令牌）。官方评审请求的登记/撤回由 `automerge`（merge 身份）承担，见下文。
- `check`：按标签检索待处理项并输出报告。默认立即返回；`--wait` 持续轮询直到出现待办，`--timeout` 轮询到超时为止（两者互斥，时长支持 `d` / `w` 单位，如 `1d1m1s`）；`--interval` 控制间隔（默认 15s），Ctrl+C 可随时中断。
- `automerge`：把带 `status/approved` + `awaiting/merge` 标签且门禁全绿（分支未落后 main、无冲突、必要检查通过）的 PR squash 合并；一次运行至多合并一个 PR。由 schedule 工作流每 5 分钟驱动。

### 退出码

| 退出码 | 含义                                                             |
| ------ | ---------------------------------------------------------------- |
| `0`    | 正常结束                                                         |
| `1`    | 瞬时错误（网络不通、Gitea 暂不可用）或其他运行失败               |
| `78`   | 配置类致命错误：认证失败（HTTP 401/403）或 `GITEA_HOST` 配置错误 |

三个子命令均为单次执行，不做进程内重试；CI 由事件和 schedule 兜底重跑。

### 环境变量

- `GITEA_HOST`: Gitea 服务器地址（必需，完整的 HTTP/HTTPS URL）
- `GITEA_ACCESS_TOKEN`: Gitea 访问令牌（必需，需要读写仓库权限）
- `GITEA_BRANCH_PROTECTION_TOKEN`: 分支保护读取专用令牌（可选，仅宿主/env 手动运行需要；仓库 workflow 不需要——
  merge job 的令牌本身是仓库管理员协作者）。分支保护端点要求 repo admin 权限；未配置时读取被拒（HTTP 403）
  会自动回退为「任何失败 context 即阻塞」的严格模式
- `GITEA_STATE_REVIEWER`: 状态评审者账号名（可选覆盖，缺省约定 `merge`）。sync 按作者角色区分内容/状态两条
  review 通道，automerge 以该角色会签
- `GITEA_STATE_TOKEN`: 状态评审者令牌（可选覆盖，与 `GITEA_STATE_REVIEWER` 配套）
- `GITEA_REPOSITORY`: 指定仓库（可选，格式: owner/name，用于 CI 环境锁定仓库）

优先级: `--repo` 命令行参数 > `GITEA_REPOSITORY` 环境变量。

## 仓库级 Actions 与容器镜像

仓库自动化由**每个仓库自己的 Actions workflow** 驱动（事件 + schedule），因此需要仓库级 Actions 配置。`assistant setup` 只负责实例与仓库本身（账号/令牌/协作者/分支保护/标签）；仓库 Actions 配置由独立命令写入：

```bash
assistant actions --config /etc/assistant/config.json [--dry-run]
```

身份是**约定**：内容评审者恒为 `ai`，状态评审者/合并者恒为 `merge`（宿主机 `config.json` 仍可覆盖，但仓库级自动化只承诺约定部署）。每个仓库只需要一个 secret：

| 名称 | 类型 | 值 | workflow 中的环境变量 |
| --- | --- | --- | --- |
| `MERGE_TOKEN` | secret | merge 令牌（`(host, merge)` 唯一；评审请求维护/分支保护读取/会签/合并） | automerge job 的 `GITEA_ACCESS_TOKEN` |

同一站点的所有仓库写同一个 merge 令牌值；令牌轮换后每个仓库都要重新写入（`assistant actions` 幂等覆写）。

管理员凭据取自 `credentials.json`（`purpose=admin`，即管理员账号登录派生）。

**令牌轮换与 secret 同步**：Gitea 的 Actions secret 值**只写不可读**，无法读取仓库里现有值来比对差异，所以本命令每次运行都幂等覆写全部 secret（唯一代价是审计记录）。这很重要：当 `setup` 因为配置丢失等原因重建了机器人令牌（旧令牌会被唯一性收敛删除）时，仓库里的旧 secret 会立即失效——**必须重新运行 `assistant actions`**，否则仓库 workflow 会以陈旧令牌静默 401。

### 容器镜像

`Dockerfile` 打出带 shell 的最小静态二进制镜像（act_runner 以 `sh -c` 执行 `run:` 步骤，distroless 无 shell 不可用）：

```yaml
# 仓库内 .gitea/workflows/assistant.yml（示意）：两个 job，两种权限模型
jobs:
  sync:                 # 只用内置令牌（gitea-actions 权限模型）：标签/评审请求意图
    runs-on: ubuntu-latest
    container: { image: ghcr.io/<owner>/assistant:latest }
    steps:
      - run: assistant sync --verbose
        env:
          GITEA_HOST: ${{ github.server_url }}
          GITEA_ACCESS_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          GITEA_REPOSITORY: ${{ github.repository }}
  automerge:            # 只需要 merge 令牌：评审请求维护 + 分支保护读取 + 会签 + 合并
    needs: sync
    runs-on: ubuntu-latest
    container: { image: ghcr.io/<owner>/assistant:latest }
    steps:
      - run: assistant automerge --verbose
        env:
          GITEA_HOST: ${{ github.server_url }}
          GITEA_ACCESS_TOKEN: ${{ secrets.MERGE_TOKEN }}
          GITEA_REPOSITORY: ${{ github.repository }}
```

> 合并白名单启用后只有 `merge` 账号能合入：人类与内置 Actions 令牌都会被拒绝，自动合并 job 必须把 `GITEA_ACCESS_TOKEN` 指向 merge 令牌。官方评审请求的登记/撤回也放在 merge job：Gitea 只允许 **PR 作者或仓库管理员**选择 reviewer，`gitea-actions` 会被拒绝（"Doer can't choose reviewer"），因此 sync（内置令牌）只做标签，不碰请求。merge 是仓库管理员协作者，分支保护读取也用同一个令牌，无需额外 secret。

- 本地构建：`make image`（`IMAGE=ghcr.io/<owner>/assistant:dev` 可指定标签）
- 发布：`.github/workflows/publish-image.yml` 在 GitHub 上把镜像推送到 `ghcr.io/<owner>/assistant`（main 推 `latest`，tag 推语义化版本，另附 `sha-*`）

## 开发者仓库配置（assistant install）

在仓库检出内运行：

```bash
assistant install              # 配置全部（claude/opencode/codex）
assistant install --dry-run    # 只输出将要写入的内容
assistant install --image ...  # 覆盖 workflow 容器镜像
assistant install --skills-source ...  # skills CLI 安装源（缺省本仓库；none 关闭技能管理）
assistant doctor               # 两级体检：本地 install 产物 + 服务端（分支保护/标签/协作者/令牌）
assistant uninstall            # 移除 assistant 生成的内容
```

写入内容：

- **review 技能**：源码在 `skills/review/SKILL.md`，安装/更新/卸载交给 skills CLI
  （`bunx skills add Cosmic-Developers-Union/assistant --skill review --copy -a claude-code -a opencode -a codex -y`），
  落盘 `.claude/skills/review/`、`.agents/skills/review/`；
- `AGENTS.md`：assistant 段落（内容源在 `content/agents.md`，追加，不覆盖用户已有内容）；
- `CLAUDE.md`：Claude Code 的项目说明，内容稳定为 `@AGENTS.md` 导入（用户自有文件不覆盖）；
- `.assistant/review.md`：项目评审约定（install 维护托管段落，段落外用户内容保留）；评审会话把它作为附加 system 提示词注入；
- `.gitea/workflows/assistant.yml`：单文件两个 job——sync（内置令牌）与 automerge（merge 令牌）；旧版 `automerge.yml` 带 marker 时自动清理；
- MCP：Claude（`.mcp.json` + `.claude/settings.json`：放行 `mcp__gitea*`、托管会话环境变量与只读命令白名单）、opencode（`opencode.json`，`gitea_*` 放行）、Codex（全局 `~/.codex/config.toml`，`approval_policy = "never"` 自动放行；若你已配置该键则保留）。zcode 暂不支持。

Claude 的项目级 `.claude/settings.json` 是**增量托管**：只写下面这些键，其余键（含用户自定义的 `env`/`permissions.deny`）原样保留，`uninstall` 也只摘除仍是我们写入的值：

| 键 | 内容 | 用途 |
| --- | --- | --- |
| `env` | `BASH_DEFAULT_TIMEOUT_MS=300000`、`BASH_MAX_TIMEOUT_MS=1800000`、`BASH_MAX_OUTPUT_LENGTH=150000`、`MCP_TIMEOUT=30000`、`MAX_MCP_OUTPUT_TOKENS=50000` | 长测试/构建、测试日志回读、gitea MCP 大 diff 的输出上限 |
| `permissions.allow` | `mcp__gitea*`、`Bash(assistant:*)`、只读 git（`status`/`diff`/`log`/`show`/`branch`/`rev-parse`/`worktree list`） | 评审场景常用只读命令免打扰；写入类命令仍走权限判定 |
| `enableAllProjectMcpServers` | `true` | 自动启用 `.mcp.json` 的项目 MCP |

`assistant doctor` 逐项输出 `OK / MISSING / OUTDATED / UNMANAGED / LEGACY / SKIPPED`：本地对照当前模板/镜像检查缺失、过期、非托管、遗留；服务端按配置/remote 定位实例——多个 remote 会逐个探测 `/api/v1/version`（GitHub/GitLab 等非 Gitea 自动跳过），**凭据用该实例在 `credentials.json` 中的 `purpose=admin` 令牌（管理员账号 `assistant login` 派生），当前仓库未登记在 `repos[]` 也照查**；逐项核对分支保护策略、标签体系、协作者权限（ai write / merge admin）与 `MERGE_TOKEN` secret。每项检查独立执行，单项权限不足只标记 `SKIPPED`，不阻断其余检查。有问题时退出码 1。

### MCP 包装层与开发者令牌

所有 MCP 配置都指向 `assistant mcp gitea`（不写死 token）：

- host：`--host` > `GITEA_HOST` > 多 remote 探测（origin 优先，`/api/v1/version` 判定 Gitea）；
- token：`--token` > `GITEA_ACCESS_TOKEN` > `GITEA_ACCESS_TOKEN_FILE` > 凭据库里该站点的 `purpose=mcp` 令牌。环境变量优先是为了让评审会话的显式注入覆盖本地登录；显式 token 文件缺失或为空时直接报错。
- 实例配置：`--config` > `ASSISTANT_CONFIG` > `~/.config/Cosmic-Developers-Union/assistant/config.json`；凭据库默认与其同目录（`ASSISTANT_CREDENTIALS` 可覆盖）。MCP 凭据独立于 admin/review/merge 用途令牌。

开发者用**自己的账号**登录一次，assistant 自动创建/复用 mcp 长期令牌（密码只用于本次认证，不落盘）：

```sh
assistant login https://gitea-a.example.com --user alice
assistant login https://gitea-b.example.com --user bob
```

令牌名按 `assistant-mcp-<host>-<账号>` 确定性派生（`--token-name` 可覆盖），scope 为 `read:repository`、`write:repository`、`read:issue`、`write:issue`、`read:user`。旧令牌仍然有效时原样复用；失效或 `login --rotate` 时先删除站点上的同名旧令牌再新建，因此每个 (站点, 账号, `mcp`) 始终只有一条。`assistant login list` 可查看身份与各用途令牌（只列名字，不显示令牌值）。

评审会话不从这里取令牌：dispatcher 显式注入 `GITEA_HOST` / `GITEA_ACCESS_TOKEN`（reviewer 令牌）/ `ASSISTANT_CONFIG` 给会话进程，会话内 MCP 因此始终以 `ai` 身份提交 review。

MCP 未配置登录时提示 `assistant login <host> --user <账号>`，stdio 启动不交互式等待密码，也不会回退到历史全局令牌文件。

gitea-mcp 默认以 `go run gitea.com/gitea/gitea-mcp@latest -t stdio -S ...` 启动，可用 `GITEA_MCP_BIN` 指已安装的二进制、`GITEA_MCP_SCOPES` 调整 scope 列表。

## 评审会话调度引擎

```
评审请求 = 原生 review 请求 | @ai/@reviewer 提及 | 行首 /review 命令
（sync 归一为 status/review，即「评审请求中」）

检测（只读标签）                    每待办一个会话                      完成判定（简单规则）
─────────────────                 ─────────────────────              ─────────────────────
status/review PR ─────────────▶  review pr #N（起始提示词） ──────▶  起点之后 reviewer 的新 review
status/triage     Issue ───────▶  triage issue #N                  ▶  status/triage 标签已移除
```

- **会话工作区与当前目录解耦**：PR 评审在 `refs/pull/<N>/head` 的独立 worktree 里进行；Issue 分诊同样在基线 head 的 detach worktree 里跑。worktree 默认在 `<系统临时目录>/agent-dispatcher/<host>-<owner>-<repo>/worktrees/{pr,issue}-<N>`（`--worktree-root` 可改），会话结束立即移除——宿主检出只被 fetch/worktree 命令触碰，所有实验都在临时目录。
- **评审标准锚定基线，PR 不可自改**：worktree 建成后把 PR 自带的 `.claude/` **整体删除**，再用宿主检出的 `.claude/` 覆盖（skills、settings）——worktree 按 PR head 检出，随带的 `.claude` 是 PR 自己的版本，照单全收等于允许 PR 改弱自己被审的规则。宿主检出没有 `.claude/` 也照常起会话：settings、MCP 与评审协议本来就由 assistant 注入（见下文），仓库没装脚手架不构成门槛，只是少了仓库自己的项目级调优。
- **受管克隆（config.json 模式）**：`instances[].repos` 未配置 `dir` 的仓库用受管克隆——落点 `<数据目录>/Cosmic-Developers-Union/assistant/repos/<host>/<owner>/<name>`（`XDG_DATA_HOME`，缺省 `~/.local/share`）。`run` 启动时缺失自动 `git clone`（用 reviewer/admin 令牌认证，origin 保持无凭据 URL），之后每轮检测前强制对齐 `origin/<基线分支>`（`--base-branch` / `DISPATCH_BASE_BRANCH`，缺省 `main`）——`git fetch --prune origin` → `git checkout -f -B <基线> origin/<基线>` → `git clean -fd`，任何本地分叉一律丢弃。日志与单飞锁在检出外的状态目录 `<数据目录>/.../state/<host>/<owner>/<name>/`，不会被 clean 波及。**仓库长什么样都不阻碍评审**：只有克隆失败（网络/权限）才跳过，缺少 `.mcp.json`、`.claude/`、`AGENTS.md` 之类 install 产物只提示一句——推送后在下一轮镜像同步里自动生效，不需要删克隆重来。全部仓库都被跳过时 daemon 保持常驻（状态 API 与对话可用，状态里带 `skip_reason`）。
- **镜像同步（显式 `dir` 的共享检出）**：`--sync-mirror` / `DISPATCH_SYNC_MIRROR=1` 开启后同样每轮强制对齐基线；受管克隆恒为开。共享开发检出勿开启。
- **会话驱动**：`claude -p` 子进程，命令面与手工运维一致（`--permission-mode auto`、`--autocompact auto`、stream-json 输出）。无人值守必需项：`--strict-mcp-config --mcp-config` 注入 **assistant 自己指定的 gitea MCP**（`assistant mcp gitea`，绝对路径启动，与仓库里有没有 `.mcp.json` 无关；仓库其它 MCP server 保留，provider 原生 server 合并进来），外加 `--max-turns` / abort 超时兜底 runaway 会话。
- **独立会话配置**：每次会话在临时目录生成 `--settings`（同一份 env/权限放行，见上文 install 的托管键）与 `--mcp-config`，并加 `--setting-sources project`（只加载项目级设置）；**不读取也不写入任何用户级配置**。会话配置根是 assistant 托管的 `<配置目录>/claude`（`instances.ClaudeDir`，`CLAUDE_CONFIG_DIR` 可覆盖）——不是用户的 `~/.claude`，会话记录与 claude 全局配置都落在这里，跟宿主上的 Claude 安装互不影响。需要 Claude Code 2.1+（`claude --help` 能看到这些参数；评审镜像安装的是 latest）。
- **AI 凭据由配置提供**：会话不读 `~/.claude` 的 OAuth 登录态，凭据来自 config.json 的 `providers`/`optimizations`（`api_key`/`auth_token` 简写落到 `ANTHROPIC_AUTH_TOKEN`/`ANTHROPIC_API_KEY`，见上文「多 provider」），或进程环境里的同名变量。daemon 启动时自检：`claude` 可执行文件与版本、会话配置根、每个 provider 的凭据来源，并用一个 `max_tokens=1` 的最小请求**实测端点**——密钥与端点不配套（典型：MiniMax 国内账号配了国际端点）会当场报 `HTTP 401` 并给出排查提示，而不是等每条会话静默重试几分钟。
- **项目评审约定与协议**：assistant 内置的 review 协议（`skills/review/SKILL.md`：PR 审查协议、Issue 分诊协议、标签体系）作为 `--append-system-prompt` 打头注入会话——仓库没 install 过也照常生效；宿主基线检出的 `.assistant/review.md`（项目自有约定，PR 自带的版本不生效）附在其后，上限 32000 字符，超出截断。
- **进度两路落点**：控制台实时显示会话 init 与编号的工具调用；完整明细实时写待办日志——stdout 按 stream-json 逐行解析，assistant 文本原样、工具调用记 `🔧 名称`。
- **一请求一会话，head 漂移即作废**：同一 instance+仓库+PR/Issue 同时至多一个会话；每个请求只拉起一个会话，完成（reviewer 已提交 review / triage 标签已移除）后在标签被 sync 收敛前不再重复拉起——连续 `@ai`、`/review` 不会造成重复会话。验证未过则本轮放行，等待下一轮检测；head 已被作者推进则本轮评审作废，下一轮以新 head 重开。
- **有界并发（`--concurrency` / `DISPATCH_CONCURRENCY`，缺省 8）**：一轮待办至多同时跑 N 个会话；轮与轮之间是天然 barrier——同一 PR/Issue 同一时刻至多一个会话。
- **单飞**：`dispatcher.lock` 记 PID（原子创建），同机第二实例拒绝启动，死 PID 残留自动接管（`review`/`triage` 一次性命令共用此锁）。
- **多实例**：使用 `config.json` 时，`run` 为每个 instance × repo 启动一个独立循环（各自加锁、各自 worktree 根），任一循环失败即整体退出。启动前逐 instance 做健康检查：版本端点可达 + reviewer/merger/admin 令牌认证通过，任一不可用则拒绝启动（不带病运行）。
- **优雅退出**：首个 SIGINT/SIGTERM 等当前待办处理完；二次信号强杀。

### 会话记录（持久化，可续接）

每个待办的评审/分诊会话都有**稳定的会话记录位置与 ID**，不再随 `/tmp` worktree 消失：

- **稳定 ID**：由站点 + 仓库 + 待办 + 锚点（PR 的 head / Issue 的标题）确定性派生 UUID。同一待办重试复用同一记录并自动 `claude -p --resume <id>` 续接（保留上一轮上下文）；head 推进即派生新 ID 开新记录。
- **稳定位置**：会话进程环境注入 `CLAUDE_CONFIG_DIR` + `CLAUDE_CODE_PROJECT_DIR_NAME`（官方「自己命名项目目录」机制），记录固定落
  `<配置目录>/claude/projects/<assistant-站点-仓库>/<session-id>.jsonl`，与启动目录/ worktree 路径解耦；Docker 评审形态会把该 `projects` 目录挂进容器，记录仍留在宿主。
- **自定义标题**：`--name` 设为 `review <owner>/<repo>#12@<head7>`、`triage <owner>/<repo>#7`；微信对话会话为 `chat-<8 位哈希>`（同一微信用户跨轮稳定）。用 `claude --resume "<id 或标题>"` 随时人工查看；每轮日志会记录文本记录路径。
- **保留期**：托管会话设置里 `cleanupPeriodDays=3650`（官方默认 30 天会被清理扫描删掉；provider `settings` 可覆盖）。
- 不再传 `--no-session-persistence`：会话记录是审计与排障的一等数据。
- 容器部署下 `CLAUDE_CONFIG_DIR=<配置目录>/claude`：它就是挂载进去的 assistant 配置目录里的 `claude/`，记录与 claude 全局配置一起持久化（首次运行自动创建）；**不挂载也不读写 `~/.claude`**，凭据由 provider 配置提供。

### 观测：--debug 与实时会话日志

`assistant run --debug`（或 `DISPATCH_DEBUG=1`）打开详细日志；docker compose 的 daemon 默认已经带上该参数。不开也能看到关键线索，开了之后连原始事件都进日志：

| 来源 | 不开 debug | 开 debug |
| --- | --- | --- |
| 微信对话 | 收到消息（用户/文本）、`claude: …` 实时文本、`🔧 工具名`、`API 错误：HTTP 401 …`、回复（字数+摘要） | 追加会话命令行、每条 claude 原始 stream 事件（截断）、结束统计（subtype/turns/cost） |
| 评审/分诊会话 | 会话启动、assistant 文本、`🔧` 工具调用、结果与验证结论 | 追加 `[debug]` 事件行、settings/MCP/文本记录路径、stderr 尾部 |
| 启动自检 | claude 版本、会话配置根、每个 provider 的凭据来源 + 端点实测结果 | 同上（自检始终实测端点，与 debug 无关） |

对话会话用 `--output-format stream-json --verbose`：claude 的每条消息（文本、工具调用、API 报错）都会**实时**落到 daemon 日志，所以「发了消息没反应」时直接 `docker compose logs -f` 就能看到卡在哪一步（模型拒绝、认证失败、还是工具在跑）。

### 校验配置：assistant validate

改完 `config.json`（或 `credentials.json`）先跑一次，再重启 daemon：

```bash
assistant validate                 # 用默认配置；--config / ASSISTANT_CONFIG 可指定
```

只读、不联网、不改文件，逐行给出结论（`OK / WARN / ERROR / SKIP`），有问题以退出码 1 结束。检查项：

- `config.json` 本身：JSON 合法性、平台与仓库、`default_provider`、微信桥状态；
- **provider**：被引用的 provider 能否解析、有没有凭据（`api_key`/`auth_token`）、端点与模型、MCP 数量，以及**定义了却没被任何地方引用**的 provider（配了但没生效，最常见的失误）；
- **credentials.json**：每个平台是否有登录身份，`review`/`merge`/`admin`/`mcp` 用途令牌是否齐（缺 review/merge 会指向 `assistant setup --host <host>`）；
- **`$schema`**：相对路径（`./config.schema.json`）指向的文件是否真的存在——缺失只提示，`assistant config init` 会补上。

```
$ assistant validate
OK        /home/ge/.config/.../config.json — 1 个平台、1 个仓库、1 个 provider 定义
OK        providers.minimax — 内置预设；端点 https://api.minimax.io/anthropic；模型 MiniMax-M3[1m]；env 8 / settings 0 / mcp 1；凭据：provider env ANTHROPIC_AUTH_TOKEN（被 default_provider 引用）
WARN      credentials https://gitea.example.com — 身份 developer（管理员）；令牌：admin/review/merge；缺 mcp（…）
OK        校验通过：config.json 与 credentials.json 可用于 assistant run（另有 1 条提示，不阻断启动）
```

### daemon 模式：状态 API 与微信对话桥

`assistant run` 是 daemon：除调度主循环外，还提供只读状态 API 与可选的微信对话桥。

```bash
assistant run                                    # 默认在 127.0.0.1:8770 提供状态 API
assistant run --api-listen none                  # 关闭状态 API
assistant weixin login                           # 扫码登录微信 Bot（凭据写入 config.json 的 weixin 节）
assistant run --weixin                           # 启动微信对话桥（或 config.json 设 weixin.enabled=true）
```

- **状态 API（只读）**：`GET /healthz`（无鉴权）与 `/api/v1/{status,sessions,queue,results}`（`Authorization: Bearer <token>`）。启动时端点凭据写入 `<配置目录>/daemon.json`（0600），daemon 退出即删除。
- **自举 MCP**：`assistant mcp daemon` 从 `daemon.json` 自动发现运行中的 daemon（`ASSISTANT_DAEMON_ENDPOINT` / `ASSISTANT_DAEMON_ADDR` 可覆盖），提供 `daemon_status`、`list_sessions`、`list_queue`、`recent_results` 四个只读工具——「当前有多少个 PR 在 review、状态如何」直接问即可。
- **微信对话桥**：按 [openclaw-weixin ilink 协议](https://github.com/Tencent/openclaw-weixin/blob/main/docs/protocol_zh_CN.md) 长轮询收消息、typing 状态与分块回复。`assistant weixin login` 会把服务端返回的扫码内容直接在终端渲染成**可扫的二维码**（半块字符），并同时给出链接兜底；二维码过期自动刷新（上限 3 次）。每条会话（微信 session）对应一个**稳定的 claude 会话**：首轮 `claude -p --session-id <uuid>`，之后 `--resume <uuid>`，会话映射持久化在 `<配置目录>/chat/sessions.json`；会话只挂一个**自举的 daemon MCP**（工具面严格限定，见 `internal/daemon/chat.go`），上下文只有显式注入的 system 提示词与 settings，不读写任何用户级 claude 配置。claude 支持 `--bare` 时对话会话用它跑最小模式（跳过 hooks、插件同步、CLAUDE.md 自动发现与记忆），启动日志会写明 `bare=开/关`；`weixin.enabled=false` 或没有 `weixin` 配置时 daemon 明确打印桥未启用的原因。
- **谁能对话**：`weixin.admin_users` 白名单；留空时只允许扫码登录的用户（`login_user_id`），两者都为空则忽略所有消息（fail-closed）。
- 每条消息处理串行（同一会话的消息排队），不同会话最多 4 路并发；单轮对话超时默认 3 分钟（`weixin.session_timeout_ms`）。

### Docker 评审环境（可选）

会话可以跑进容器，宿主机只需 docker（免装 claude 与语言工具链）：

```bash
make review-image        # 构建评审镜像（REVIEW_IMAGE=... 可改标签）
assistant run --docker-image ghcr.io/cosmic-developers-union/assistant-review:latest
assistant review 42 --docker-image ...     # 一次性调试同样支持
# 或环境变量：DISPATCH_DOCKER_IMAGE / DISPATCH_DOCKER_NETWORK（如 host）
```

- 镜像定义在 `images/review/Dockerfile`：gitea runner 基础镜像 + bun/skills CLI、Claude Code、Go 工具链与常用构建工具，缓存目录统一到 `/root`。
- **路径一致挂载**：worktree、会话 `--settings`/`--mcp-config` 所在目录与 assistant 二进制按相同绝对路径挂进容器（MCP 就是 `assistant mcp gitea`，所以镜像不必自带 assistant），claude 的 `--mcp-config`、`--settings`、`--plugin-dir` 等参数无需改写；`.claude/` 已由 dispatcher 把 PR 自带的那份删掉、再以宿主基线覆盖（基线没有就留空，协议由 assistant 注入）。
- **认证透传**：`ANTHROPIC_*` / `CLAUDE_*` 及代理变量按白名单 `-e KEY` 从宿主环境继承，其余环境不进容器。
- **超时兜底**：SIGTERM docker 客户端不会停容器，dispatcher 额外按容器名执行 `docker kill`。
- **网络**：容器默认 bridge；MCP 需要回连宿主机上的服务时用 `--docker-network host`（或自定义网络）。

### 命令行

```
assistant run             长驻 daemon（部署形态；--interval/--timeout/--api-listen/--weixin 可调）
assistant run --dry-run   只读演练：先打印生效配置（host/repo/令牌掩码/会话/节奏/路径/基线），再列出将执行的待办并逐步说明
                          将发生的动作（日志、worktree、会话、超时、完成判定、
                          放行、清理），零副作用
assistant list            只读列出当前待办（快速验证 host/仓库/令牌/标签链路）
assistant review <n>      立即评审单个 PR（跳过检测，端到端调试用）
assistant triage <n>      立即分诊单个 Issue
```

配置优先级：**命令行参数 > 环境变量 > git remote 自动检测 / 默认值**。

- `--repo-dir` 是单目标检出覆盖：环境变量单实例模式下替代启动目录推导（remote 也从该目录读）；config.json 模式下覆盖 `repo.dir`（按共享检出处理，不克隆/不强制镜像），配置里有多仓库时必须用 `--repo owner/name` 收敛到唯一仓库。
- **host 与仓库缺省从 remote 探测推导**（环境变量单实例模式）：按顺序检查各 remote（origin 优先），用 `/api/v1/version` 探测 Gitea 站点，GitHub/GitLab 等会被跳过；http(s) remote（如 `http://gitea.example.com:3000/owner/repo.git`）可完整推出 API 根地址与 owner/repo，ssh/scp remote 只可靠推出仓库（host 以 `http://<主机名>` 尽力猜测），探测不命中时用 `--host` / `GITEA_HOST` 显式指定。
- 时长参数（`--interval` / `--timeout` 及对应 `DISPATCH_*_MS` 环境变量）接受 `30s` / `10m` / `1h` / `2d` 或毫秒裸数字。
- 路径默认值：config.json 模式锚定受管路径（受管克隆 + 状态目录，见上文）；环境变量单实例模式与显式 `dir` 的共享检出锚定检出根（`git rev-parse --show-toplevel`，与启动 cwd 无关）：日志 `<检出根>/logs`、锁 `<检出根>/dispatcher.lock`；worktree 一律在系统临时目录（按 `<host>-<owner>-<repo>` 隔离）。
- 其余环境变量：`DISPATCH_LOG_DIR`、`DISPATCH_WORKTREE_ROOT`、`DISPATCH_LOCK_FILE`、`DISPATCH_MODEL`、`DISPATCH_REVIEWER`、`DISPATCH_CLAUDE_BIN`。

### 访问令牌的作用

调度引擎与评审会话使用**同一条 reviewer 令牌**，并且这条绑定是显式注入的：

1. **dispatcher 自身**：按标签检测待办、完成判定验证（读 review 与标签），只需读权限。
2. **评审会话**：启动会话时 dispatcher 把 `GITEA_HOST`、`GITEA_ACCESS_TOKEN`（reviewer 令牌）与 `ASSISTANT_CONFIG`（当前 `config.json` 路径；环境变量单实例模式为空）显式写入会话进程环境。会话内 `assistant mcp gitea` 因此始终以 reviewer 账号（默认 `ai`）提交 review——完成判定只承认 `--reviewer` 名下的 review。docker 形态按 `-e` 键把这三项透传进容器（值由宿主进程环境继承）。

这条绑定不能靠环境继承：`config.json` 模式下 daemon 进程环境通常没有 Gitea 变量，会话 MCP 会退回开发者自己的 `mcp` 令牌（`assistant login --user` 写入），评审就不再以 reviewer 身份落库，PR 会被反复重开会话。开发者的 `mcp` 令牌只服务本地交互会话（自己在编辑器/CLI 里读写 Issue/PR）。

因此 reviewer 令牌必须是 reviewer 账号的令牌：换成其他账号，检测与验证照常工作，但提交的 review 不再匹配 `--reviewer`。令牌由 `assistant setup` 自动生成；环境变量单实例模式下由 `--token` / `GITEA_ACCESS_TOKEN` 提供。

### 部署（专用开发机）

依赖：claude CLI 已安装并登录（PATH 可见；否则用 `--claude-bin` 指绝对路径）。二进制本身无其他运行时依赖。

```bash
make build                        # 或 make push 发布到 generic package registry
# 单实例（环境变量）
export GITEA_ACCESS_TOKEN=<ai 账号令牌>
export DISPATCH_SYNC_MIRROR=1     # 每轮把宿主检出强制对齐 origin/main（评审标准的数据源）
./assistant run                   # 在仓库检出内任意目录启动（路径默认值锚定检出根）

# 多实例（config.json，由 assistant setup 生成；仓库 dir 需为本地检出）
./assistant run --config /etc/assistant/config.json
```

#### 容器化部署（docker compose）

daemon 整体跑在容器里：**宿主构建的二进制直接挂载进去**（`make build` 的产物以只读方式覆盖 `/usr/local/bin/assistant`，镜像里不再内置副本，升级不必重建镜像），assistant 的配置目录（config.json/credentials.json/会话记录）与受管克隆挂进容器（容器内外路径一致），`/tmp` 用 **tmpfs**（评审 worktree 全在 `/tmp`，落内存、退出即清），状态 API 只发布到宿主 `127.0.0.1:8770`：

```bash
make compose-up         # 构建 ./assistant（linux/amd64 静态）→ 预建挂载点 → 启动/重建容器
docker compose logs -f
docker compose exec -it assistant assistant weixin login   # 首次扫码（-it 必需：二维码 + 验证码输入）

make build && docker compose up -d --force-recreate    # 升级：重新构建二进制并让容器换上新的一份
```

镜像只用 `images/review/Dockerfile` 的 `review` target（评审环境：claude / go / bun / uv / node，与评审会话镜像共用 `review-env` 层，无需从 registry 拉取预构建镜像）。容器入口固定指向挂载进来的二进制：**只替换文件不会重启进程**，所以升级要用 `docker compose up -d --force-recreate`（即 `make compose-up`）或 `docker compose restart assistant`。想让镜像自带二进制（离线分发、不走挂载）时用 `make daemon-image` 构建 `daemon` target。

- **AI 凭据来自 assistant 配置**：容器**不挂载 `~/.claude`**，会话不读宿主的 OAuth 登录态。评审/分诊/对话会话的凭据取自 `config.json` 的 `providers`/`optimizations`（`api_key`/`auth_token` 简写，见上文「多 provider」）；进程环境里的 `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN` 只是兜底（compose 内已注释示例）。改完配置先 `docker compose exec assistant assistant validate` 校验，再 `docker compose restart assistant`（启动日志也会打印 claude 版本、会话配置根与凭据来源，缺凭据直接告警）。
- **会话配置根**：`<配置目录>/claude`（`instances.ClaudeDir`，`CLAUDE_CONFIG_DIR` 可覆盖）——会话文本记录与 claude 的全局配置都在这里，随配置目录一起挂载/备份，与宿主上的 Claude 安装互不影响。
- **二进制挂载**：`${ASSISTANT_BINARY:-./assistant}` → `/usr/local/bin/assistant:ro`（相对路径按 compose 文件所在目录解析，默认就是仓库根目录下 `make build` 的产物）。`make compose-up` 先构建再启动，因此不存在挂载点缺失；手工 `docker compose up` 前请确认该文件已存在——路径不存在时 docker 会把它建成**目录**，容器启动会报 `is a directory`。`make build` 用 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` 构建，静态链接，不依赖容器基座的 C 库。
- 挂载是**细粒度**的（不用整目录 `$HOME`）：`~/.config/Cosmic-Developers-Union/assistant`（config.json / credentials.json（`assistant login` 派生的用途令牌）/ daemon.json / chat 会话 / claude 会话配置根）、`~/.local/share/Cosmic-Developers-Union/assistant`（受管克隆与状态）、`~/.cache`（MCP 与工具链缓存）读写；git 身份与 `docker.sock` 按需（compose 内已注释示例）。
- 挂载点需先在宿主存在（docker 对不存在的路径会建 root 属主目录）：用 `make compose-up` 会自动预建，或手动 `mkdir -p ~/.config/Cosmic-Developers-Union/assistant ~/.local/share/Cosmic-Developers-Union/assistant`（跑过一次 `assistant login`/`setup` 也会生成）。
- 微信桥要显式开启：`assistant weixin login`（容器里用 `docker compose exec -it ...`）会把 `weixin.enabled=true` 与 Bot 凭据写进 config.json；没有 `weixin` 配置或 `enabled=false` 时 daemon 会明确打印「微信桥未启用」及原因，不再静默跳过。对话会话走 claude 的 `--bare` 最小模式（探测到支持时；上下文只有显式注入的 system 提示词、settings 与自举 daemon MCP）。
- UID/GID/HOME 由 compose 插值（缺省 `1000:1000` + 宿主 `$HOME`）；以宿主用户运行，写回挂载目录的文件属主不变。请在 `.env` 或环境里按需 `export ASSISTANT_UID=$(id -u) ASSISTANT_GID=$(id -g)`。
- `/tmp` 为 `tmpfs`（`exec,mode=1777,size=16g`）：评审 worktree 全在内存盘、退出即清；16G 是上限，占用受宿主可用内存约束，可调。
- 需要 `--docker-image` 把评审会话再放进容器时，打开 `docker.sock` 挂载（docker-out-of-docker）；宿主上的 Gitea 可通过 `extra_hosts: host.docker.internal:host-gateway` 访问（文件内已注释示例）。

systemd 示例（`WorkingDirectory` 建议仓库检出根；systemd 的最小 PATH 通常不含 claude 安装目录，`--claude-bin` 用绝对路径）：

```ini
[Unit]
Description=review agent dispatcher
After=network-online.target

[Service]
WorkingDirectory=/srv/repo
Environment=DISPATCH_CLAUDE_BIN=/usr/local/bin/claude
Environment=DISPATCH_SYNC_MIRROR=1
EnvironmentFile=/etc/assistant.env
ExecStart=/srv/repo/assistant run
Restart=on-failure
KillSignal=SIGTERM
TimeoutStopSec=1800

[Install]
WantedBy=multi-user.target
```

`/etc/assistant.env` 至少包含 `GITEA_ACCESS_TOKEN=<ai 账号令牌>`；多实例形态改用 `--config /etc/assistant/config.json`（文件含令牌，注意 0600 权限）。

### 日志

每个待办一个归档：`logs/<kind>-<number>-<时间戳>.log`——起始提示词、worktree/head、会话 init、实时进度（assistant 文本与 `🔧` 工具调用）、结果 JSON（turns、cost、permission denials）、验证结论。

## 形式化规格

- `formal/Dispatcher.lean`：调度语义的 Lean 4 形式化规格（时间线建模、完成判定谓词）。机器检验的性质包括：作者 push 不同代码时完成判定不可能通过（A1）、更新 PR 说明对完成判定无影响（A2）、head 漂移本轮作废、同一待办每次入队至多一个会话、入队受 pending 与并发上限双重守卫。

  ```bash
  lean formal/Dispatcher.lean   # 退出码 0 即全部证明通过（纯 core Lean，无需 mathlib）
  ```

- `spec/ReviewStateMachine.tla`：PR 评审状态机的 TLA+ 规格（标签体系与状态流转）。

规格与实现语言无关；改变这些语义的实现改动必须同步修改模型并让证明重新通过。

## 开发

```bash
make test          # go test -v ./...
make build-local   # 构建本地平台二进制（用于开发测试）
make install       # 先构建再安装到本机（缺省 /usr/local/bin；非 root 自动 sudo）
make build         # 构建 Linux/amd64 静态二进制（与 CI 发布产物相同）
make push          # 手动发布 latest 到 generic package registry（引导/紧急修复；需要 GITEA_HOST / GITEA_ACCESS_TOKEN）
make clean         # 清理构建产物
```

### 端到端测试（临时 Gitea）

`test/gitea/docker-compose.yaml` 提供一次性 Gitea + act_runner（SQLite，HTTP `127.0.0.1:3300`）：`up.sh` 会构建本地 `assistant:e2e` 镜像、启动 runner 并注册，e2e 覆盖 `setup` 全流程、真实 workflow 执行（issues/pull_request/issue_comment/pull_request_review 事件驱动的 sync + automerge）与 admin/write/read 三种权限协作者的交互：

```bash
make test-e2e      # 构建镜像 → 起 Gitea + runner → 跑 go test -tags e2e ./test/e2e/... → 清理
make gitea-up      # 只启动，保留现场手动调试（凭据写入 test/e2e/.env）
make gitea-down    # 停止并清除数据卷
```

需要 Docker（runner 通过 /var/run/docker.sock 以容器 job 执行 workflow）；e2e 测试在缺少凭据时自动跳过（`go test ./...` 不受影响）。

发布的是统一二进制 `assistant`。仓库内的 Gitea Actions 工作流（发布、`sync`、`automerge`）需相应把下载/执行目标从旧的 `gitea-assistant` 改为 `assistant`；发布步骤仍使用带 `write:package` scope 的 PAT secret。

## 许可

MIT
