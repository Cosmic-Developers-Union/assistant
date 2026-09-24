# config.json —— 唯一的用户配置

`assistant` 的全部配置只有一份 config.json：三个池（providers / agents /
channels，**大量配置**）加 N 个 runtime（**少量运行**——每个 runtime 集合一
个 main agent、若干 subagents、若干通道与一棵独立运行树）。凭据不在这里：
机器人令牌写在各通道字段，助手派生与用途令牌由 assistant 管理在
credentials.json（0600）。

```jsonc
{
  "$schema": "./config.schema.json",

  // ── 池：定义一次，多处引用 ──
  "providers":  { "zhipu": { "api_key": "…" } },
  "agents":     { "qa": { "description": "测试问答", "system_prompt": "…", "provider": "zhipu" } },
  "channels": [
    { "type": "gitea", "host": "https://gitea.example.com",
      "reviewer": "ai", "merger": "merge",
      "repos": ["acme/repo", {"name": "acme/b", "dir": "/local/b"}] },
    { "type": "weixin", "name": "work", "bot_token": "…" },
    { "type": "telegram", "bot_token": "…", "enabled": false }
  ],

  // ── 运行时：集合 agent + provider + channel ──
  "runtimes": {
    "main": {
      "main_agent": "main",                    // 缺省内置通用主 agent
      "subagents": ["ops", "coder", "writer", "review"],   // 缺省全部内置子代理
      "channels": ["gitea", "weixin/work"],    // 键 = type 或 type/name
      "provider": "zhipu",

      // 运行参数（原 run.yaml 并入；每 runtime 一棵独立 $root 树）
      "root": "",                    // 缺省 config.json 同目录的 data/
      "repos_dir": "",               // 缺省 $root/repos
      "review_root": "",             // 缺省 $root/review
      "review_name_template": "",    // 缺省 "{instance-name}-{username-or-org}--{name}-{pr|issue}-{index}"
      "sessions_dir": "",            // claude 会话记录归档根；留空不归档
      "sessions_name_template": "",
      "state_dir": "",               // 缺省 $root/state
      "state_file": "",              // 缺省 $state-dir/state.sqlite3（默认开启；"off" 关闭）
      "chat_dir": "",                // 对话状态（conversations.json 等）；缺省 $root/chat
      "claude_dir": "",              // claude 配置根；$CLAUDE_CONFIG_DIR 优先，缺省 $root/claude
      "api_listen": "127.0.0.1:8770",
      "interval_ms": 30000,
      "session_timeout_ms": 1800000,
      "concurrency": 8
    }
  },
  "default_runtime": "main"
}
```

## 落点（显式模式）

assistant 是工具不是常驻应用：配置按 `--config` → `ASSISTANT_CONFIG` →
**当前目录的 `./config.json`** 定位，运行产物收在配置旁边。**凭据是例外**：
`credentials.json` 是用户级状态（这台机器上的当前用户是谁），不是项目级——
按 XDG Base Directory / Windows Known Folders / macOS Library 规范落在平台
标准配置目录，从任何目录启动的 MCP/CLI 都解析同一份登录态，绝不锚定 cwd
（`ASSISTANT_CREDENTIALS` 可显式覆盖）。

| 文件/目录 | 落点 | 说明 |
| --- | --- | --- |
| `config.json` | 配置目录（缺省当前目录） | 唯一的用户配置 |
| `credentials.json` | 平台标准配置目录（Linux `~/.config/Cosmic-Developers-Union/assistant/`） | login/setup 派生的用途令牌（0600） |
| `daemon.json` | 同目录（配置目录） | 运行中 daemon 的端点发现文件 |
| `config.schema.json` | 同目录（配置目录） | `assistant config init` 写入（编辑器补全） |
| `data/` | 同目录（`runtime.root` 缺省） | 运行树：repos/state/review/chat/claude |

把项目入库时排除它们（`.gitignore`）：`config.json`、`daemon.json`、
`data/` 等——参考仓库根的 `.gitignore`（`credentials.json` 在用户配置目录，
天然不在仓库里）。

## 运行与边界

- `assistant run [--runtime <名>]` 零旗标起一个运行时：装配它引用的通道与
  agent，状态与产物全部落在该 runtime 的 `$root` 树下（repos / state /
  review / chat / claude），备份只看一个目录。多 runtime = 多部署形态，
  互不串扰（daemon 端点文件 `daemon.json` 在配置目录，MCP 会话以
  `ASSISTANT_CONFIG` 继承同一份配置）。
- 路径字段支持 `${VAR:-default}` 环境变量展开与 `$root` / `$state-dir`
  自引用，相对路径锚定到 config.json 所在目录（随配置目录一起挂载/备份）。
- provider 解析链：agent 自身（仅独立执行）> main agent provider >
  `runtime.provider` > 全局 `default_provider`。
- gitea 通道由调度引擎接管：work item（`review pr #N` / `triage issue #N`）
  以 **review agent** 定义独立执行会话（评审工作区按 `review_root` 与
  `review_name_template` 落位）。

## 迁移（旧配置零改动可跑）

- **旧 instances**：载入时自动迁移为 gitea 通道（host / reviewer / merger /
  repos 原样带入），任何写回即固化为规范形，`instances` 节从此消失。
- **旧 run.yaml**：已并入 config.json。`assistant config migrate` 导入
  （运行参数 → runtime、monitor → gitea 通道、provider 简写 → providers），
  旧文件改名 `run.yaml.migrated`；`run --run` 旗标已退役并给出指引。
- **无 runtimes**：`ResolveRuntime` 即时合成 `main`（全部通道 + 内置 main
  agent + 全部内置子代理），不落盘——通道增删不会留下悬空引用。
- 环境变量单实例模式（`GITEA_HOST` / `GITEA_ACCESS_TOKEN` / `GITEA_REPOSITORY`）
  保留，供 CI 与无配置的一次性运行。
