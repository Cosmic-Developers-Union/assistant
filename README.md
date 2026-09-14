# assistant

一个 Go 静态二进制，两种角色：

1. **仓库辅助机器人**（Gitea Actions 事件 / schedule 驱动）：机械、启发式地处理 Issue、PR 的例行事务（标签体系维护、Issue 标签规范、PR 评审状态同步、已批准 PR 自动合并）。
2. **评审会话调度引擎**（宿主机 systemd 常驻）：检测 Gitea 待办并自动拉起 headless 评审会话，直到 review 完成。

两种角色共享 `GITEA_HOST` / `GITEA_ACCESS_TOKEN` / `GITEA_REPOSITORY` 配置约定与 Gitea API 客户端。`claude` CLI 是调度引擎的外部运行时依赖（不在本仓库内），需在宿主机安装并登录；其余能力全部由二进制自带。

## 命令

| 命令 | 角色 | 说明 |
| --- | --- | --- |
| `assistant setup` | 初始化 | 建机器人账号/令牌、配协作者与分支保护、补齐标签，并写入 `config.json` |
| `assistant login` | 初始化 | OAuth 登录登记/刷新平台（写入 `admin_oauth`）；`login list`/`login remove` 管理平台 |
| `assistant repos` | 初始化 | 管理 `config.json` 中登记的仓库：`list` / `add --dir` / `remove`（只改本地，不触碰服务端） |
| `assistant init` | 初始化 | 只初始化当前仓库：复用平台凭据与 ai/merge 账号，配保护/标签/secret，并自动登记进 `config.json` |
| `assistant deinit` | 初始化 | init 的反命令：从 `config.json` 移除当前仓库；`--purge` 同清理服务端（保护/协作者/secret） |
| `assistant actions` | 初始化 | 为配置中的仓库写入 Actions secrets（merge 令牌等；身份约定 ai/merge，无需 variable） |
| `assistant install` | 开发者 | 在仓库检出内配置 skills / AGENTS.md / workflow / 各 AI CLI 的 MCP |
| `assistant uninstall` | 开发者 | 移除 `install` 写入的内容（只触碰带 marker 的） |
| `assistant doctor` | 开发者 | 两级体检：本地 install 产物 + 服务端（分支保护/标签/协作者/merge 令牌），只读 |
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

不指定配置文件时，所有命令维持环境变量单实例模式（`GITEA_HOST` / `GITEA_ACCESS_TOKEN` / `GITEA_REPOSITORY`）。要同时管理多台 Gitea、多个仓库，写一份 `config.json`。查找顺序：`--config` > `ASSISTANT_CONFIG` > 平台标准配置目录 `<UserConfigDir>/Cosmic-Developers-Union/assistant/config.json`（Linux `~/.config`、macOS `~/Library/Application Support`、Windows `%AppData%`）；**不读当前目录 `config.json`**（避免检出里的同名文件被误当运行配置），示例见 `config.example.json`。机器人命令与调度命令都会按 instance × repo 迭代：

```json
{
  "instances": [
    {
      "host": "https://gitea.example.com",
      "admin_token": "admin-personal-access-token",
      "admin_oauth": { "client_id": "...", "refresh_token": "..." },
      "reviewer": { "name": "ai", "token": "created-by-assistant-setup" },
      "merger": { "name": "merge" },
      "repos": [
        "owner/repo",
        {
          "name": "owner/another",
          "dir": "/srv/another",
          "merger_token": "created-by-assistant-setup"
        }
      ]
    }
  ]
}
```

- `admin_token`（可选）：高权限令牌（repo admin），用于 `automerge` 读取分支保护、收窄必要检查 context。用 OAuth 初始化时不写 access token（短期有效），而是写入 `admin_oauth`（`client_id` + `refresh_token`），运行期按需刷新；两者都缺失时 `automerge` 回退为「任何失败 context 即阻塞」的严格模式。
- `admin_oauth`（可选，setup 自动写入）：OAuth2 刷新凭据。`access_token` 不落盘，`refresh_token` 长期有效；`check`/`sync`/`automerge` 在需要读分支保护时现场换取短期令牌。
- `reviewer`（`ai`）：内容评审者，其令牌**全实例唯一**——同一站点同时只有一个评审主机（dispatcher 的单飞锁是进程内的，令牌唯一从凭据层面兜底）；`setup` 每次运行都会把该账号其余令牌收敛删除。
- `merger`（`merge`）：状态评审者（会签/合并）。**令牌按仓库独立**，存放在每个 repo 条目的 `merger_token`：仓库级 Actions workflow 各自使用自己项目的令牌，互不影响、可独立轮换。令牌名由仓库全名哈希派生（`assistant-<sha256 前 8 位>`），稳定且便于识别；`setup` 只清理同名旧令牌与历史共享名，不动其他仓库的令牌。
- `repos`：仓库清单。字符串是 `owner/name` 简写（有 `dir`/`merger_token` 时写对象）；调度引擎的 `review`/`triage` 会话需要本地检出，多仓库时给出 `dir`（单仓库可省略，退回启动目录的检出）。
- `provider`（可选）：生效的供应商名，逐级回退 `repo.provider` > `instance.provider` > `default_provider`；定义与用法见下文「多 provider（供应商）」。
- 路径默认值按仓库隔离：日志 `<检出>/logs`、锁 `<检出>/dispatcher.lock`、worktree `<临时目录>/agent-dispatcher/<owner>-<repo>/worktrees`。
- 文件含令牌，`setup` 以 0600 写入；请勿提交到版本库（`.gitignore` 已忽略常见位置，建议自行确认）。

### 多 provider（供应商）

`providers` 是手写维护的供应商配置：不同供应商（Anthropic 官方、Anthropic 兼容网关、Bedrock/Vertex 等）的 env/settings/mcp 格式各异，框架只做原样透传合并，不解释供应商语义：

- `env`：注入会话环境变量（可含密钥）；provider 自定义的 `mcp` server 会缺省继承它（server 自身 `env` 优先）；
- `settings`：Claude Code 原生 settings 片段（如 `model`、`apiKeyHelper`、`awsAuthRefresh`），合并进会话 `--settings`（`env`/`permissions` 逐键合并，其余顶层键覆盖托管默认）；
- `mcp`：原生 MCP server 定义（`.mcp.json` 形态），合并进会话 `--mcp-config`，同名 server 由 provider 覆盖（仓库既有的 `gitea` 等不受影响）。

未识别的键原样保留，随你手写扩展。示例：

```json
{
  "providers": {
    "gateway": {
      "env": {
        "ANTHROPIC_BASE_URL": "https://gateway.example.com/api/anthropic",
        "ANTHROPIC_AUTH_TOKEN": "gateway-auth-token"
      },
      "settings": { "model": "glm-4.6" },
      "mcp": {
        "search": { "command": "npx", "args": ["-y", "@example/search-mcp"] }
      }
    }
  },
  "default_provider": "gateway"
}
```

选择粒度逐级回退：`repo.provider` > `instance.provider` > `default_provider`；微信对话桥用 `weixin.provider`（缺省回退 `default_provider`）。引用了未定义的名字直接报错，不会静默用错供应商。

**只作用于运行时会话**：provider 覆盖进评审/分诊/对话会话的临时 `--settings` 与 `--mcp-config`，绝不写入仓库 `.claude/settings.json`/`.mcp.json`——密钥不进 git，仓库保持供应商无关。`run --dry-run` 会打印每个目标生效的 provider 与覆盖项数（值不落日志）。

### 平台管理：assistant login

`login` 只负责平台条目与 OAuth 凭据，不做账号/仓库初始化（那是 `setup`）：

```bash
assistant login https://gitea.example.com     # 优先复用 tea CLI 登录（--token 可显式指定），否则 OAuth 写入/刷新 admin_oauth
assistant login list                          # 列出平台：凭据类型（oauth/token/none）、仓库数、ai/merge 账号
assistant login remove https://gitea.example.com   # 移除平台条目（不触碰 Gitea 侧账号/仓库）
```

host 可省略：取配置中唯一实例，否则在带 Gitea remote 的检出内探测（`/api/v1/version`，多 remote 逐个尝试）。**登录优先复用本机 tea CLI 在该站点的登录令牌**（读 `~/.config/tea/config.yml`，可用 `TEA_CONFIG` 覆盖；只读，不做网络探测），没有时走 OAuth。OAuth 默认复用 Gitea 内置 `tea` 公共客户端（回调 `http://127.0.0.1:<随机端口>`，公共客户端允许任意 loopback 端口）；`--oauth-client-id/--oauth-client-secret` 可换成自建应用（confidential 应用需提供密钥），`--oauth-scope` 指定授权 scope（缺省不带）。Gitea 会拒绝与已有授权记录 scope 不一致的请求（`a grant exists with different scope`）：到 `<host>/user/settings/applications` 撤销旧授权，或用 `--oauth-scope` 与旧记录对齐。

### 仓库初始化：assistant init

`setup` 面向整台实例（账号/令牌/多仓库），`init` 面向当前仓库——在仓库检出内运行即可，平台必须已由 `login`/`setup` 登记：

```bash
assistant init                 # 复用 remote 推导仓库；也可以用 --repo owner/name
assistant init --create-repos  # 仓库不存在时创建
assistant init --dry-run
assistant deinit               # 仅从 config.json 移除当前仓库
assistant deinit --purge       # 同时删除分支保护、移除 ai/merge 协作者、删除 MERGE_TOKEN secret
```

`init` 做的事：复用或补齐平台上的 `ai`/`merge` 账号与令牌 → 配协作者（ai write / merge admin）→ 分支保护（同 `setup` 口径）→ 标签体系 → 写入仓库级 `MERGE_TOKEN` secret → 把仓库条目（含 `merger_token`）自动加入 `config.json`。`deinit --purge` 是其反操作；本地安装产物（skills/AGENTS.md/workflow/MCP）用 `assistant uninstall` 清理。两者都支持 `--dry-run`。

### 仓库登记：assistant repos

`repos` 只管理 `config.json` 里的 `instances[].repos`（本地登记，不触碰服务端），适合先登记、再用 `setup` 统一补齐服务端：

```bash
assistant repos list                                  # 列出所有平台已登记仓库（dir / merger_token 状态）
assistant repos list --host https://gitea.example.com
assistant repos add owner/repo --host https://gitea.example.com --dir /srv/repo   # 登记（已存在则更新 dir）
assistant repos remove owner/repo --host https://gitea.example.com                # 移除登记（不触碰服务端）
```

`--host` 缺省时按「检出 remote 对应的平台 → 已登记该仓库的平台 → 唯一实例」推断；`--dir` 缺省时若当前检出的 remote 指向该仓库，自动登记检出根目录（一个实例下多个仓库时建议显式 `--dir`）。登记后用 `assistant setup --host <H>` 按配置补齐协作者/分支保护/标签/`MERGE_TOKEN`。

### 初始化：assistant setup

`setup` 把「评审 → 批准 → 会签 → 自动合并」闭环需要的一切配置好，幂等可重跑。管理员凭据支持多种方式（按优先级）：

```bash
# 1. 管理员令牌（最直接；会写入配置供 automerge 使用）
assistant setup --host https://gitea.example.com \
  --admin-token <管理员令牌> \
  --repos owner/repo[,owner/another] --create-repos

# 2. 令牌文件（避免 shell 历史/进程列表泄露）
assistant setup --host https://gitea.example.com \
  --admin-token-file /run/secrets/gitea-admin-token --repos owner/repo

# 3. OAuth2 浏览器登录（授权码 + PKCE；令牌不落盘，默认复用内置 tea 公共客户端）
assistant setup --host https://gitea.example.com --oauth --repos owner/repo

# 4. 管理员账号密码现场换取长期令牌（缺 --admin-password 时交互式输入，不回显）
assistant setup --host https://gitea.example.com \
  --admin-user <管理员账号> --repos owner/repo
```

管理员凭据会**优先复用配置中已有的**：`admin_token` 直接使用；只有 `admin_oauth` 时先用 refresh token 换取短期令牌；都不可用且未提供令牌/密码时才需要 `--oauth` 浏览器登录。因此配置就绪后重复运行 `setup` 不会反复要求登录；确实要换账号时加 `--relogin` 强制重新授权。

OAuth 登录说明：默认使用 Gitea 内置的 `tea` 公共客户端；内置客户端在部分已发布版本上仍是 confidential（会提示 Unregistered Redirect URI），此时在 Gitea「设置 → 应用 → 创建 OAuth2 应用」建一个**公共**客户端（重定向 URI 填 `http://127.0.0.1`，或与 `--oauth-port` 完全一致的 `http://127.0.0.1:<端口>`），再用
`--oauth --oauth-client-id <Client ID>`（如有密钥再给 `--oauth-client-secret`）重跑。access token 不落盘，`refresh_token` 会写入 `admin_oauth` 供运行期刷新。

仓库清单可省略：`--repos` 与配置文件里都没有仓库时，只初始化实例（建号、令牌、OAuth 凭据），不触碰任何仓库；之后再次运行 `setup` 补齐仓库即可。

流程：

1. 校验管理员身份；
2. 复用/创建 `reviewer`（默认 `ai`）与 `merger`（默认 `merge`）账号（随机密码不落盘）；
3. 收敛令牌：reviewer 全实例唯一（删除账号下其余令牌）；merger **按仓库独立**——每个项目一个 `assistant-<hash8>` 令牌，只清理同名旧令牌与历史共享名，不动其他仓库。建令牌端点仅接受 Basic Auth，必要时以管理员身份重置机器人密码后创建；
4. 把两个账号加为仓库协作者：reviewer 写权限，**merger 管理员权限**（合并白名单成员 + 分支保护读取），并收敛标签体系（补齐缺失、scoped 互斥、删除不在体系内的标签，与 `sync` 完全相同口径）；
5. 在默认分支配置分支保护：`required approvals=2`（内容批准 + 状态会签）、**合并白名单只含 merger（只有它可以合入）**、驳回阻塞（`block_on_rejected_reviews`）、过期批准作废、落后分支阻塞、**勾选「管理员须遵守分支保护规则」（`block_admin_merge_override`，管理员也不得绕过）**，以及**「有官方审核阻止了代码合并」（`block_on_official_review_requests`）**：合并 job（merge 管理员身份）会把 `/review`、`@ai` 提及登记为正式评审请求，内容评审者提交 review 后由它撤回遗留请求、门禁解除（Gitea 只允许 PR 作者或仓库管理员选择 reviewer，`gitea-actions` 会被拒绝，因此请求维护不在 sync）。注意：**团队评审请求**不会因成员 review 自动清除，需人工移除后合并才解除；API 的 `requested_reviewers` 字段有显示滞后，不代表门禁实际状态；
6. 把结果写回 `config.json`（0600；落点：`--config` / `ASSISTANT_CONFIG`，否则平台标准配置目录）。`--create-repos` 会在仓库不存在时自动创建私有仓库（auto_init，默认分支 main）。

常用开关：`--dry-run`（只输出计划）、`--relogin`（强制 OAuth 重新登录）、`--allow-admin-override`（放开管理员绕过，缺省关闭）、`--reviewer` / `--merger`（账号名）、`--required-approvals`、`--email-domain`。

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
| `MERGE_TOKEN` | secret | merge 令牌（评审请求维护/分支保护读取/会签/合并） | automerge job 的 `GITEA_ACCESS_TOKEN` |

管理员凭据取自 `config.json`（`admin_token`，或 `admin_oauth` 刷新出的短期令牌）。

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

`assistant doctor` 逐项输出 `OK / MISSING / OUTDATED / UNMANAGED / LEGACY / SKIPPED`：本地对照当前模板/镜像检查缺失、过期、非托管、遗留；服务端按配置/remote 定位实例——多个 remote 会逐个探测 `/api/v1/version`（GitHub/GitLab 等非 Gitea 自动跳过），**凭据复用该实例在 `config.json` 中的 admin（`setup`/`login` 写入，OAuth 现场刷新短期令牌），当前仓库未登记在 `repos[]` 也照查**；逐项核对分支保护策略、标签体系、协作者权限（ai write / merge admin）与 `MERGE_TOKEN` secret。每项检查独立执行，单项权限不足只标记 `SKIPPED`，不阻断其余检查。有问题时退出码 1。

### MCP 包装层与开发者令牌

所有 MCP 配置都指向 `assistant mcp gitea`（不写死 token）：

- host：`--host` > `GITEA_HOST` > 多 remote 探测（origin 优先，`/api/v1/version` 判定 Gitea）；
- token：`--token` > `GITEA_ACCESS_TOKEN` > `GITEA_ACCESS_TOKEN_FILE` > `~/.config/Cosmic-Developers-Union/assistant/token` > `~/.config/mmc/gitea-token` > tea CLI 配置（`~/.config/tea/config.yml` 中同站点登录）。

与当前开发者绑定，与管理员/实例配置无关：换项目自动换 host，换人自动换 token。gitea-mcp 默认以 `go run gitea.com/gitea/gitea-mcp@latest -t stdio -S ...` 启动，可用 `GITEA_MCP_BIN` 指已安装的二进制、`GITEA_MCP_SCOPES` 调整 scope 列表。

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
- **评审标准锚定基线，PR 不可自改**：worktree 建成后立即以宿主检出的 `.claude/` 整体覆盖（skills、settings）——worktree 按 PR head 检出，随带的 `.claude` 是 PR 自己的版本，照单全收等于允许 PR 改弱自己被审的规则。宿主检出缺 `.claude/` 时拒绝起会话（fail-closed）。
- **受管克隆（config.json 模式）**：`instances[].repos` 未配置 `dir` 的仓库用受管克隆——落点 `<数据目录>/Cosmic-Developers-Union/assistant/repos/<host>/<owner>/<name>`（`XDG_DATA_HOME`，缺省 `~/.local/share`）。`run` 启动时缺失自动 `git clone`（用 reviewer/admin 令牌认证，origin 保持无凭据 URL），之后每轮检测前强制对齐 `origin/<基线分支>`（`--base-branch` / `DISPATCH_BASE_BRANCH`，缺省 `main`）——`git fetch --prune origin` → `git checkout -f -B <基线> origin/<基线>` → `git clean -fd`，任何本地分叉一律丢弃。日志与单飞锁在检出外的状态目录 `<数据目录>/.../state/<host>/<owner>/<name>/`，不会被 clean 波及。受管克隆要求仓库已提交 `assistant install` 产物（`.mcp.json`、`.claude/settings.json`）：缺失的仓库会被**跳过并记录原因**（daemon 保持常驻，状态 API 里该仓库 `ready=false` + `skip_reason`，对话里可直接问「为什么没在跑」），补齐并推送后重启即可；所有仓库都未就绪时 daemon 也不会退出，只提示修复方向。
- **镜像同步（显式 `dir` 的共享检出）**：`--sync-mirror` / `DISPATCH_SYNC_MIRROR=1` 开启后同样每轮强制对齐基线；受管克隆恒为开。共享开发检出勿开启。
- **会话驱动**：`claude -p` 子进程，命令面与手工运维一致（`--permission-mode auto`、`--autocompact auto`、stream-json 输出）。无人值守必需项：`--strict-mcp-config --mcp-config` 从宿主仓库 `.mcp.json` 显式注入 gitea MCP，外加 `--max-turns` / abort 超时兜底 runaway 会话。
- **独立会话配置**：每次会话在临时目录生成 `--settings`（同一份 env/权限放行，见上文 install 的托管键），并加 `--setting-sources project`（只加载项目级设置）、`--no-session-persistence`（不落会话历史）；**不读取也不写入任何用户级配置**（`~/.claude`）。需要 Claude Code 2.1+（`claude --help` 能看到这些参数；评审镜像安装的是 latest）。
- **项目评审约定**：宿主基线检出的 `.assistant/review.md`（及其中的 install 托管段落）作为 `--append-system-prompt` 注入会话，PR 自带的版本不生效——与 `.claude/` 同口径，防止 PR 改弱自己被审的规则；内容上限 32000 字符，超出截断。
- **进度两路落点**：控制台实时显示会话 init 与编号的工具调用；完整明细实时写待办日志——stdout 按 stream-json 逐行解析，assistant 文本原样、工具调用记 `🔧 名称`。
- **一请求一会话，head 漂移即作废**：同一 instance+仓库+PR/Issue 同时至多一个会话；每个请求只拉起一个会话，完成（reviewer 已提交 review / triage 标签已移除）后在标签被 sync 收敛前不再重复拉起——连续 `@ai`、`/review` 不会造成重复会话。验证未过则本轮放行，等待下一轮检测；head 已被作者推进则本轮评审作废，下一轮以新 head 重开。
- **有界并发（`--concurrency` / `DISPATCH_CONCURRENCY`，缺省 8）**：一轮待办至多同时跑 N 个会话；轮与轮之间是天然 barrier——同一 PR/Issue 同一时刻至多一个会话。
- **单飞**：`dispatcher.lock` 记 PID（原子创建），同机第二实例拒绝启动，死 PID 残留自动接管（`review`/`triage` 一次性命令共用此锁）。
- **多实例**：使用 `config.json` 时，`run` 为每个 instance × repo 启动一个独立循环（各自加锁、各自 worktree 根），任一循环失败即整体退出。启动前逐 instance 做健康检查：版本端点可达 + reviewer/merger/admin 令牌认证通过，任一不可用则拒绝启动（不带病运行）。
- **优雅退出**：首个 SIGINT/SIGTERM 等当前待办处理完；二次信号强杀。

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
- **微信对话桥**：按 [openclaw-weixin ilink 协议](https://github.com/Tencent/openclaw-weixin/blob/main/docs/protocol_zh_CN.md) 长轮询收消息、typing 状态与分块回复。`assistant weixin login` 会把服务端返回的扫码内容直接在终端渲染成**可扫的二维码**（半块字符），并同时给出链接兜底；二维码过期自动刷新（上限 3 次）。每条会话（微信 session）对应一个**稳定的 claude 会话**：首轮 `claude -p --session-id <uuid>`，之后 `--resume <uuid>`，会话映射持久化在 `<配置目录>/chat/sessions.json`；会话只挂一个**自举的 daemon MCP**（工具面严格限定，见 `internal/daemon/chat.go`），不读写用户级 claude 配置。
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
- **路径一致挂载**：worktree 与宿主 `.mcp.json`、临时会话 `--settings` 所在目录按相同绝对路径挂进容器，claude 的 `--mcp-config`、`--settings`、`--plugin-dir` 等参数无需改写；`.claude/` 已由 dispatcher 以宿主基线覆盖进 worktree（PR 自带的评审规则不起作用）。
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

`--token` / `GITEA_ACCESS_TOKEN` 一处配置、两处消费，且**决定评审身份**：

1. **dispatcher 自身**：按标签检测待办、完成判定验证（读 review 与标签），只需读权限。
2. **评审会话**：gitea MCP 子进程继承同一组 `GITEA_HOST` / `GITEA_ACCESS_TOKEN`，会话提交的 Pull Request Review 以该令牌的账号身份落库——「reviewer 名下出现新 review」正是完成判定的依据。

因此令牌必须是 reviewer 账号（默认 `ai`）的令牌：换成其他账号，检测与验证照常工作，但提交的 review 不再匹配 `--reviewer`，PR 会被反复重开会话。令牌无法自动检测，必须显式提供（命令行传参可见于进程列表，推荐环境变量或 `config.json`）；`assistant setup` 会自动生成。

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

daemon 整体跑在容器里：宿主机**用户目录原样挂载**（claude 登录态、config.json、受管克隆都在里面，容器内外路径一致），`/tmp` 用 **tmpfs**（评审 worktree 全在 `/tmp`，落内存、退出即清），状态 API 只发布到宿主 `127.0.0.1:8770`：

```bash
make compose-up         # 预建挂载点并启动（等价：docker compose up -d，首次会构建镜像）
docker compose logs -f
docker compose exec assistant assistant weixin login   # 首次扫码（终端直接渲染二维码）
```

`docker compose up` 会从 `images/review/Dockerfile` 的 `daemon` target 构建镜像（与评审镜像共用 `review-env` 层，无需从 registry 拉取预构建镜像）；`make daemon-image` 可单独构建。

- 挂载是**细粒度**的（不用整目录 `$HOME`）：`~/.claude`（登录态）、`~/.config/Cosmic-Developers-Union/assistant`（config.json / daemon.json / chat 会话）、`~/.local/share/Cosmic-Developers-Union/assistant`（受管克隆与状态）读写，`~/.config/tea`（凭据）只读；git 身份与 `docker.sock` 按需（compose 内已注释示例）。
- 挂载点需先在宿主存在（docker 对不存在的路径会建 root 属主目录）：用 `make compose-up` 会自动预建，或手动 `mkdir -p ~/.claude ~/.config/Cosmic-Developers-Union/assistant ~/.local/share/Cosmic-Developers-Union/assistant`（跑过一次 `assistant login`/`setup` 也会生成）。
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
