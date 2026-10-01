# assistant 设计规范

单一二进制 `assistant`,不引入第二个常驻程序。操作面:

- `instance` — 平台接入管理 (gitea / qq / weixin / telegram)
- `mcp` — AI 会话的工具面 (stdio)
- `action` — 仓库自动化 (在 CI 里按事件与定时运行)
- `assistant run` — 常驻进程:评审调度 + 消息通道

## 1. instance 子命令

实例 = 一个已接入的平台连接:名字、地址、账号、令牌。

```shell
# 列出已添加的实例
assistant instance list

# 添加 Gitea;参数缺省时进入交互式模式
# 不支持 --password(避免进 shell 历史),非交互场景用 --password-file 注入
assistant instance add gitea --name instance-name --url https://... --username username

# 其他平台遵循各自的登录方式,使用相同的模块
assistant instance add qq ...
assistant instance add weixin ...
assistant instance add telegram ...

# 移除与查看
assistant instance remove instance-name
assistant instance show instance-name
```

- `add` 对缺失的参数逐项交互询问;密码只在登录时使用,不落盘、不回显。
- `remove` 只删本地实例条目,不动平台侧的账号。

### 实例的存放:credentials.json

用户级文件,与当前目录无关。Linux 落
`~/.config/Cosmic-Developers-Union/assistant/credentials.json`
(`ASSISTANT_CREDENTIALS` 可显式指定位置):

```json
{
  "instances": {
    "gitea": [],
    "qq": []
  }
}
```

按类型分键:每种类型有自己的字段集,校验按类型严格进行 (未知字段直接报错);
新增平台只需定义该类型的字段与登录方式,不牵动其他类型。

## 2. mcp 子命令

```shell
assistant mcp gitea
```

自动检测当前项目的 Gitea 实例与当前开发者的访问令牌,再以 stdio 拉起
gitea-mcp (默认 `go run gitea.com/gitea/gitea-mcp@latest`)。

host 检测顺序:

1. `--host`
2. `GITEA_HOST`
3. 当前检出内的 Gitea remote 探测 (多个上游时选命中的)
4. 实例配置中唯一的 gitea 实例
5. 凭据库中唯一的站点

无上游且登记了多个平台时显式报错,不猜。

token 检测顺序:

1. `--token`
2. `GITEA_ACCESS_TOKEN`
3. `GITEA_ACCESS_TOKEN_FILE`
4. `assistant instance add <host> --username <账号>` 登记的该站点凭据

凭据库与当前目录无关——从任何项目启动都解析同一份登录状态。

## 3. action 子命令

```shell
# 合并门禁全绿且分支未过期的已批准 PR (一次运行至多一个,squash;供 CI schedule 驱动)
assistant action automerge

# 规范 Issue 标签,并把 PR 原生评审状态同步为状态标签 (单次执行,供 CI 事件驱动)
assistant action label-sync
```

Flags: `--dry-run`

两个动作的共同语义:

- 单次执行、无状态、幂等:门禁不满足时本轮跳过,重复运行无副作用。
- 只处理显式指定的那一个仓库,不扫描、不发现令牌可见的其他仓库。workflow 用
  环境变量把三样东西交给命令,给了什么就处理什么,简单、清晰、可预测:
  `GITEA_HOST` 站点地址,`GITEA_ACCESS_TOKEN` 本次令牌,`GITEA_REPOSITORY`
  要处理的仓库 (owner/name)。形态与旧版 `assistant sync` 的 workflow 一致:

```yaml
steps:
  - run: assistant action label-sync --verbose
    env:
      GITEA_HOST: ${{ github.server_url }}
      GITEA_ACCESS_TOKEN: ${{ secrets.GITHUB_TOKEN }}
      GITEA_REPOSITORY: ${{ github.repository }}
```

- 逐 PR/Issue 处理,一处失败不中断其余,错误最后聚合上报。
- 账号只是惯例,不是前提:内容评审者惯用 `ai`,状态评审者/合并者惯用 `merge`;
  命令本身只要求令牌有相应的权限:

    - label-sync 用 CI 内置令牌 (gitea-actions) 就够:建删标签、改 Issue 标签、
      读评审状态。
    - automerge 的令牌必须允许合并;维护官方评审请求时还要能指定 reviewer (Gitea 只允许 PR 作者或仓库管理员这么做),所以惯例上用
      merge 账号的令牌,
      经 secret 注入。

### 3.1 规范标签体系

全仓库统一使用下面这套标签。label-sync 的收敛步骤负责维护它:补齐缺失、
强制互斥、删除体系外。`type/`、`priority/`、`status/`、`awaiting/` 四个前缀
下的标签互斥:同一条 Issue 或 PR 上,同一前缀的标签至多保留一个。
`duplicate`、`wontfix`、`needs-info` 没有前缀,不参与互斥。

type/,条目类型:

| 标签            | 说明                         |
|-----------------|------------------------------|
| `type/bug`      | 需要修复的问题               |
| `type/feature`  | 新功能请求                   |
| `type/refactor` | 不改变外部行为的内部结构整理 |
| `type/task`     | 开发或维护任务               |

priority/,优先级:

| 标签            | 说明         |
|-----------------|--------------|
| `priority/high` | 需要优先处理 |
| `priority/low`  | 可延后处理   |

status/,状态。前四个用于 Issue,后四个用于 PR:

| 标签                       | 说明                             |
|----------------------------|----------------------------------|
| `status/triage`            | Issue 等待分诊                   |
| `status/confirmed`         | Issue 已确认有效                 |
| `status/in-process`        | Issue 正在处理                   |
| `status/blocked`           | Issue 处理受阻                   |
| `status/in-progress`       | PR 开发中                        |
| `status/review`            | PR 等待代码审查,即「评审请求中」 |
| `status/changes-requested` | PR 需要修改                      |
| `status/approved`          | PR 已批准合并                    |

awaiting/,轮到谁行动,由 PR 的 status/ 状态推出:

| 标签                | 说明                                                   |
|---------------------|--------------------------------------------------------|
| `awaiting/author`   | 等待 PR 作者处理 (对应 in-progress、changes-requested) |
| `awaiting/reviewer` | 等待 reviewer 评审 (对应 review)                       |
| `awaiting/merge`    | 等待人类工程师合并 (对应 approved)                     |

无前缀的三个收尾标签:

| 标签         | 说明                                                                                      |
|--------------|-------------------------------------------------------------------------------------------|
| `duplicate`  | Issue 与已有记录重复。label-sync 见到即自动关闭                                           |
| `wontfix`    | Issue 不计划处理。label-sync 见到即自动关闭                                               |
| `needs-info` | 需要报告者补充信息。此时不挂 status/triage,已有的会被移除;报告者补齐后重新加回,才进入分诊 |

`status/triage` 与 `status/review` 同时是评审调度 (assistant run) 的待办
标记:前者排队分诊会话,后者排队评审会话。

### 3.2 label-sync 的工作流程

label-sync 处理 GITEA_REPOSITORY 指定的仓库,做三件事:先收敛标签体系,再
规范 open Issue 的标签,最后把 open PR 的评审状态同步成状态标签。

第一步,收敛标签体系。

1. 对照 3.1 的规范标签体系检查仓库现有的标签,缺哪个就补哪个。
2. `type/`、`priority/`、`status/`、`awaiting/` 四个前缀下的标签是互斥标签:
   同一条 Issue 或 PR 上,同一前缀的标签至多保留一个。
3. 删除规范之外的标签。assistant 强制维护完整的标签集,手工加的历史标签会让
   状态判定出现歧义,所以不在规范里的一律删掉。

第二步,规范 open Issue 的标签。逐个 Issue 按下面的顺序判断,命中一条就处理
完,再看下一个。

1. 带 duplicate 或 wontfix 标签的,直接关闭,其他标签不动。
2. 挂着 PR 专用标签的,把这些标签摘掉,Issue 上不该出现它们。
3. 带 needs-info 的,移除 status/triage。needs-info 表示在等报告者补充信息;
   等对方补齐后重新加回 status/triage,才会进入分诊。
4. 没有任何状态标签的,补上 status/triage。

第三步,把 open PR 的评审状态同步成状态标签。逐个 PR 处理。

先判定这个 PR 有没有评审意图,满足下面任意一条就算有:

1. 有还没回应的官方评审请求。reviewer 只有在当前 head 上提交过正式 review,
   才算回应了这个请求;团队请求一律视为未回应。
2. 有比最新内容结论更晚的复审请求记录。评审者回应过之后,原生「请求评审」
   按钮改变不了任何字段,这条时间线记录是新一轮请求留下的唯一证据。
3. 最新内容结论之后,有人用 @ai 或 @reviewer 提及,或在行首写了 /review 命令。
   结论之前的提及不计数,否则历史提及会反复触发评审。

然后按下面的顺序定标签:

1. WIP 或 draft 的 PR,说明作者还在开发,冲突、落后、检查失败这些门禁问题
   此时都不适用。有评审意图的标 status/review,没有的标 status/in-progress。
2. 其他 PR 看内容评审者的最新结论来定 (惯例是 `ai` 账号);状态评审者的会签与
   门禁驳回 (惯例是 `merge` 账号) 不算内容结论。有评审意图而且没有更新的结论,
   标 status/review;最新结论是 APPROVED,标 status/approved;最新结论是
   CHANGES_REQUESTED,标 status/changes-requested;最新结论是 COMMENT,说明
   评审者留下了讨论、在等作者回应,同样标 status/changes-requested;既没有
   结论也没有评审意图,标 status/in-progress。
3. PR 与目标分支冲突,或落后于目标分支的:如果现在标的是 status/in-progress,
   改标 status/changes-requested,让作者先处理;如果已经有评审结论,保持不动。
4. 写标签时同时维护两个维度,都由上面同一次判定导出。status/ 前缀按结论保留
   一个;awaiting/ 前缀表示轮到谁行动:status/review 配 awaiting/reviewer,
   status/approved 配 awaiting/merge,其余一律配 awaiting/author。

### 3.3 automerge 的工作流程

automerge 处理 GITEA_REPOSITORY 指定的仓库,一次运行至多合并一个 PR,合并
方式是 squash。合并不看标签,标签只是 label-sync 维护出来的观测产物;所有
门禁都按当前 head 实时判定。

第一步,维护官方评审请求。这一步的令牌必须能指定 reviewer,因为 Gitea 只允许
PR 作者或仓库管理员这么做 (惯例上用 merge 账号的令牌)。

1. 评论里出现评审意图的 (用 @ai 提及,或行首写 /review 命令),补登记为正式
   评审请求。
2. 内容评审者已经回应过的遗留请求,撤回。遗留指的是:请求发生在最新内容结论
   之前,而且不是新一轮复审。
3. 读不到请求发生时间的,这一轮不撤回。宁可让门禁多等一轮,也不能把真实请求
   误撤掉。

第二步,逐个找可以合并的 PR。PR 按编号从小到大,先来先合并。每个 PR 先重读
一次最新状态 (前面的列表结果只是快照,等到处理它时状态可能已经变了),再按
顺序过下面的门禁,任何一条不满足就跳过它,继续看下一个:

1. 已关闭、草稿、标题带 WIP 的,跳过。
2. 与目标分支冲突的,跳过。
3. 落后于目标分支的,跳过。main 前移之后旧的批准就过期了,作者需要 rebase
   之后重新评审。
4. 内容评审者没有在当前 head 上批准的,跳过。批准指:提交过 APPROVED 的
   review、没有被撤销、并且指向当前 head。
5. 必要检查有失败的,跳过。

第三步,处理通过了全部门禁的 PR,分两种情况:

1. 必要检查还在运行:先会签,再武装 Gitea 原生 auto-merge。检查从运行变成
   全绿没有任何事件可以触发 CI,武装之后由 Gitea 在全绿的瞬间直接合并。
   分支保护没有配置必要检查的,不武装,保持严格门禁等下一轮。
2. 必要检查全部通过:会签,然后 squash 合并。合并完成后本轮直接结束,后面的
   PR 都不再看 (main 已经前移,其余 PR 全部过期)。

第四步,门禁失效时撤销武装。PR 关闭、批准被撤销、检查失败、落后目标分支,
任何一种情况发生时,把这个 PR 之前武装的原生 auto-merge 撤掉。Gitea 的排定
不会自己失效,不撤的话检查一转绿就会合并,绕过内容门禁。

关于会签,补充说明三点:

1. 会签指用合并者身份的令牌对当前 head 提交一条正式批准 (惯例上是 `merge`
   账号)。会签和合并是同一个动作,不存在盖了章还等着的中间状态。
2. 会签让 required approvals 计满 (内容批准加状态会签),同时覆盖此前可能存在
   的门禁驳回。同一个 head 已经会签过的,不重复盖章。
3. 武装路径下,会签提前到武装之前做:Gitea 在检查变绿后尝试合并时会重新校验
   批准门禁,所以武装那一刻批准就必须已经满足。

必要检查按下面的口径统计:

1. 分支保护配置了必检 context 的,只统计配置里的那些。
2. 没配置的,所有 context 都算必要,但要排除 assistant 自己的工作流:sync 的
   运行被取消时 Gitea 会留下 failure 状态,不排除的话一次取消就会造成自我驳回。
3. 分支保护读不到的 (令牌权限不足),回退成严格模式:任何一个 context 失败都算
   门禁不过。

## 4. assistant run

### 4.1 config.yaml:本次 run 跑什么、怎么跑

run 的全部行为由 config.yaml 决定。它只回答两个问题:跑什么 (哪些仓库、哪些
通道),怎么跑 (节奏与上限)。平台怎么接入、凭据是什么,由 credentials.json
负责,config.yaml 按名字引用实例——两个文件各管各的,合起来才是完整运行。

读取规则:`--config` 显式指定路径,缺省当前目录的 `config.yaml`;未知字段
直接报错 (与 credentials.json 同一口径的严格校验)。

```yaml
connects:
  gitea: { type: gitea, url: https://gitea.vincentge.top, token: "{{GITEA_TOKEN}}" }
mcp:
  gitea: { cmd: assistant, args: [ mcp, gitea ], env: { } }
bots:
  reviewer:
    kind: gitea-review
    use: { gitea: gitea }
    with: { identity: ai, prompt: "" }
    workspace: { type: worktree , repo: "{{event.repo}}", ref: "{{event.pr.ref}}" }
    agent: { system: "", mcp: [ gitea ], model: "" }
  triage:
    kind: triage
    use: { gitea: gitea }
    workspace: { type: worktree , repo: "{{event.repo}}" }
    agent: { system: "", mcp: [ gitea ], model: "" }
```
