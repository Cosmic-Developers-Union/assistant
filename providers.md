# providers 优化点（全局 + 各供应商）

本文件是 `config.json` 里 `providers` / `optimizations` 的手写素材：**框架只做透传合并**，
下面是各接入点的推荐取值与坑位。你按实际套餐/网络环境取舍后粘贴，密钥用占位符替换。

## 0. 怎么用

```jsonc
{
  "optimizations": { /* 全局优化点：所有会话生效，见第 1 节 */ },
  "default_provider": "zhipu",
  "providers": { "zhipu": { /* 供应商覆盖：同名键压过全局，见第 2 节 */ } }
}
```

- 生效顺序：assistant 托管默认 < `optimizations`（全局） < 选中 provider < 命令行（`--model` 等）。
- 选择粒度：`repo.provider` > `instance.provider` > `default_provider`；微信桥 `weixin.provider`。
- 供应商定义可以写在两处：`config.json` 的 `providers`，或**一个 provider 一个文件**放在
  `<配置目录>/providers/<名字>.json`（内容即 provider 对象：`env`/`settings`/`mcp`；重名报错）。
  文件形式不会被写回 `config.json`，适合密钥分离、逐家手写维护。
- 少数供应商需要在会话启动时做动态调整（如 opencode 的会话请求头）：由**代码级特化**
  处理，一个供应商一个文件（`internal/provider/`），见第 2 节 opencode。
- 只作用于运行时会话（临时 `--settings`/`--mcp-config`），不写仓库 `.claude/settings.json`。
- `env` 里的值也会缺省注入该 provider 自定义的 MCP server（server 自身 `env` 优先）。
- 已由 assistant 托管、**不要重复配置**：`BASH_DEFAULT_TIMEOUT_MS`、`BASH_MAX_TIMEOUT_MS`、
  `BASH_MAX_OUTPUT_LENGTH`、`MCP_TIMEOUT`、`MAX_MCP_OUTPUT_TOKENS`、只读命令放行；
  会话固定 `--permission-mode auto`、`--setting-sources project`、`--strict-mcp-config`、`--max-turns 300`。
- 我们的会话是无人值守 headless：**别配 `settings.model` 之外的交互项**（主题/快捷键/statusLine 等无效）。

## 1. 全局优化点（`optimizations`）

跨供应商通用、随模型/网络微调的项：

| 键 | 建议 | 说明 |
| --- | --- | --- |
| `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` | `"1"` | 关掉遥测/自动更新等非必要请求（无人值守、省钱、防被网关计费） |
| `DISABLE_TELEMETRY` / `DISABLE_ERROR_REPORTING` | `"1"` | 同上，双保险（老版本变量名） |
| `DISABLE_AUTOUPDATER` | `"1"` | 容器内不需要自更新（如镜像已固定版本） |
| `API_TIMEOUT_MS` | `"3000000"` | 第三方网关长思考 + 长 diff 常见超时来源；官方可留默认 |
| `CLAUDE_CODE_SUBAGENT_MODEL` | 便宜/快档 | 子 agent 与摘要类任务钉到廉价模型，省钱 |
| `CLAUDE_CODE_EFFORT_LEVEL` | `"high"` 或 `"max"` | 评审质量优先；网关不支持 effort 时无效（可删） |
| `CLAUDE_CODE_AUTO_COMPACT_WINDOW` | 视模型：`1000000`/`262144`/`512000` | 纯整数（`500k` 会被当 500 用）；也被用来纠正 `[1m]` 模型 ID 的窗口 |
| `CLAUDE_CODE_MAX_CONTEXT_TOKENS` | 与上同 | 显式覆盖上下文窗口（第三方模型 ID 未识别时用） |
| `MAX_THINKING_TOKENS` | `"0"` 关 / 正整数固定预算 | `0` 在第三方=省略参数，行为同官方「关闭」 |
| `CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING` | `"1"` | 关自适应思考，回退固定预算（对 Fable/Sonnet 5/Opus 4.7+ 无效） |
| `ENABLE_TOOL_SEARCH` | `true` 或 `auto:5` | **仅当**网关能正确转发 `tool_reference`；第三方 base_url 默认关闭 tool search |
| `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS` | `"1"` | 网关对 beta header 报 400 时打开（注意：会同时关掉 tool search） |
| `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY` | `"1"` | 需要 `/model` 列出网关模型时（v2.1.129+，且 ID 以 `claude`/`anthropic` 开头才收录） |

全局块示例（按需删行）：

```json
"optimizations": {
  "env": {
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    "DISABLE_TELEMETRY": "1",
    "DISABLE_ERROR_REPORTING": "1",
    "DISABLE_AUTOUPDATER": "1",
    "CLAUDE_CODE_EFFORT_LEVEL": "high"
  },
  "settings": {
    "permissions": { "deny": ["Bash(rm -rf:*)"], "defaultMode": "acceptEdits" }
  }
}
```

> `permissions` 与托管放行是并集合并；`deny` 会随 provider 覆盖保留（这是我们托管里没写的空白项）。

## 2. 各供应商优化点

模型档位映射变量：`ANTHROPIC_DEFAULT_OPUS_MODEL` / `..._SONNET_MODEL` / `..._HAIKU_MODEL` / `..._FABLE_MODEL`
（另有 `..._MODEL_NAME` 只改显示名）。Claude Code 内部按场景取档，**漏配某档会让该档任务失败或静默回退**。
下列表里的值随版本滚动，写死前建议跑一次 `/status` 与 `/model` 核对；**以官方最新文档为准**。

### anthropic（官方）

- 接入：`ANTHROPIC_API_KEY`（或宿主 OAuth；容器内建议 key）。无需 base_url。
- 优化点：
  - 提示缓存自动生效，无需配置；长评审靠缓存省钱，别开 `DISABLE_PROMPT_CACHING`。
  - 质量优先：`CLAUDE_CODE_EFFORT_LEVEL=max`、`MAX_THINKING_TOKENS` 拉高（若无自适应思考）。
  - 省钱：`CLAUDE_CODE_SUBAGENT_MODEL` 指 Haiku 档；`CLAUDE_CODE_MAX_OUTPUT_TOKENS` 压低到实际需要。
  - 1M 上下文：按官方当前支持的模型 ID 写法（含 `[1m]` 后缀）+ `CLAUDE_CODE_AUTO_COMPACT_WINDOW`。
- 粘贴块：

```json
"anthropic": {
  "env": {
    "ANTHROPIC_API_KEY": "sk-ant-...",
    "CLAUDE_CODE_EFFORT_LEVEL": "max",
    "CLAUDE_CODE_SUBAGENT_MODEL": "claude-haiku-4-5",
    "MAX_THINKING_TOKENS": "31999"
  },
  "settings": { "model": "claude-sonnet-5" }
}
```

### zhipu（GLM / Z.ai）

- 接入：Anthropic 兼容端点 `https://api.z.ai/api/anthropic`（国内如用 bigmodel，请自行核对是否
  `https://open.bigmodel.cn/api/anthropic`）；令牌 `ANTHROPIC_AUTH_TOKEN`。
- 优化点：
  - `API_TIMEOUT_MS=3000000`（官方脚本默认值，长会话必需）。
  - `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`（官方推荐）。
  - 1M 上下文：模型名加 `[1m]`（如 `glm-5.3[1m]`）+ `CLAUDE_CODE_AUTO_COMPACT_WINDOW=1000000`。
  - 官方建议**不要硬编码模型映射**（套餐模型升级时自动跟随）；若套餐含 Flash，则 Haiku 档指 Flash。
  - 老账号可能只开了 OpenAI 协议权限（Coding Plan 历史遗留），此类账号需换 OpenAI 兼容工具，Claude Code 走不通。
- 粘贴块：

```json
"zhipu": {
  "env": {
    "ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic",
    "ANTHROPIC_AUTH_TOKEN": "your_zai_key",
    "API_TIMEOUT_MS": "3000000",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1000000",
    "ANTHROPIC_MODEL": "glm-5.3[1m]",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "glm-5.3[1m]",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "glm-5.3[1m]",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "glm-5.3-flash[1m]",
    "ANTHROPIC_DEFAULT_FABLE_MODEL": "glm-5.3[1m]"
  }
}
```

### kimi（Moonshot / Kimi Code）

- 两种账号、端点与 key 不通用：
  - 开放平台：`https://api.moonshot.cn/anthropic`（国内）/ `https://api.moonshot.ai/anthropic`（国际），
    `ANTHROPIC_AUTH_TOKEN`。
  - Kimi Code（订阅制）：`https://api.kimi.com/coding/`，`ANTHROPIC_API_KEY`。
- 优化点：
  - 1M 上下文：`ANTHROPIC_MODEL=k3[1m]` + `CLAUDE_CODE_AUTO_COMPACT_WINDOW=1048576`、
    `CLAUDE_CODE_MAX_CONTEXT_TOKENS=1048576`；256K 档用 `k3-256k` + `262144`。
  - `CLAUDE_CODE_SUBAGENT_MODEL` 钉同档；`CLAUDE_CODE_EFFORT_LEVEL=high`（官方示例）。
  - `kimi-k2.7-code` **强制开启思考**（未开 thinking 会 400），适合主模型、别放 Haiku 档。
  - 建议 `ENABLE_TOOL_SEARCH=true` 前先确认网关转发 `tool_reference`（Moonshot 文档提及该变量）。
- 粘贴块：

```json
"kimi": {
  "env": {
    "ANTHROPIC_BASE_URL": "https://api.moonshot.ai/anthropic",
    "ANTHROPIC_AUTH_TOKEN": "your_moonshot_key",
    "ANTHROPIC_MODEL": "kimi-k3[1m]",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "kimi-k3[1m]",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "kimi-k3[1m]",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "kimi-k2.7-code",
    "ANTHROPIC_DEFAULT_FABLE_MODEL": "kimi-k3[1m]",
    "CLAUDE_CODE_SUBAGENT_MODEL": "kimi-k3[1m]",
    "CLAUDE_CODE_EFFORT_LEVEL": "high",
    "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1048576",
    "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "1048576"
  }
}
```

### minimax

- 接入：`https://api.minimax.io/anthropic`（国际）/ `https://api.minimaxi.com/anthropic`（国内，另有
  `api.minimax.cn`）；`ANTHROPIC_AUTH_TOKEN`（订阅 Token Plan 的 key）。
- 优化点：
  - 模型 `MiniMax-M3[1m]`（1M，官方示例 `CLAUDE_CODE_AUTO_COMPACT_WINDOW=1000000`；512K 档配 `512000`），
    或 `MiniMax-M2.7-highspeed` 通勤快档。
  - 自动 cache 生效，无需配置；思考默认开启（`/config` 可关）。
  - 配置前清掉宿主/其他 profile 残留的 `ANTHROPIC_*`（我们的框架天然隔离，宿主 shell 无关）。
- 粘贴块：

```json
"minimax": {
  "env": {
    "ANTHROPIC_BASE_URL": "https://api.minimax.io/anthropic",
    "ANTHROPIC_AUTH_TOKEN": "your_minimax_key",
    "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1000000",
    "ANTHROPIC_MODEL": "MiniMax-M3[1m]",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "MiniMax-M3[1m]",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "MiniMax-M3[1m]",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "MiniMax-M3[1m]"
  }
}
```

### opencode（OpenCode Zen / Go 网关）

> 如果你指的是 opencode CLI 本体而不是 Zen 网关：当前会话执行器是 `claude`，provider 只能作用于
> Anthropic 兼容端点，CLI 本体不在本框架内。

- 接入：`ANTHROPIC_BASE_URL=https://opencode.ai/zen`（Claude Code 会请求 `/v1/messages`）+ Zen API key。
- **会话请求头（必须）**：Go/Zen 端点自 2026-09-06 起要求每个会话一个稳定的 `x-opencode-session`
  （缺失可能被拒），OpenCode 客户端还默认带 `x-session-affinity`（缓存亲和）。Claude Code 没有原生
  会话头开关，用 `ANTHROPIC_CUSTOM_HEADERS`（换行分隔 `Key: Value`）注入——**这一项已内置代码级
  特化**：只要 provider 名是 `opencode` / `zen` / `opencode-zen` / `opencode-go`（不区分大小写），
  评审/分诊每次会话一个 UUID、微信对话复用其稳定会话 UUID；你在 `env` 里显式配置的同名头优先，
  不会被覆盖。特化实现见 `internal/provider/opencode.go`，网关改名/新增要求时只改该文件。
- 优化点：
  - Anthropic 协议档（`claude-*`、`qwen3.7-max/plus` 等）可直接用；Zen 表里标 `/chat/completions`
    的模型（deepseek/minimax/glm/gpt/grok/gemini 等）需要本地/自建翻译代理转成 Anthropic Messages。
  - 免费模型（如 `big-pickle`）可能用你的提示词训练，评审私有代码前评估。
  - `ENABLE_TOOL_SEARCH` 先保持默认（第三方 base_url 默认关闭）；确认 Zen 转发 `tool_reference` 再开。
- 粘贴块（可作为 `<配置目录>/providers/opencode.json` 文件内容）：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://opencode.ai/zen",
    "ANTHROPIC_AUTH_TOKEN": "sk-zen-...",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-5",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-5",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "claude-haiku-4-5"
  }
}
```

### openai

- 接入：OpenAI 官方没有供 Claude Code 用的 Anthropic Messages 端点，需要**翻译网关**
  （LiteLLM、claude-code-router、自建 OpenAI→Anthropic 代理等）；`ANTHROPIC_BASE_URL` 指向代理。
- 优化点：
  - 代理需要暴露 `/v1/messages`（与 `count_tokens`），并原样转发 `anthropic-beta`/`anthropic-version`。
  - 代理鉴权非常规时用 `ANTHROPIC_CUSTOM_HEADERS`（换行分隔 `Key: Value`）。
  - 代理不认 beta header 报 400 → `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`（tool search 同时关闭）。
  - 模型映射必须显式配齐四档（OpenAI 模型名与 Claude 档位无默认对应）；子 agent 钉 `gpt-*-mini`。
  - 工具调用/流式/thinking 回放是自建代理最容易翻车的地方：评审会话开 `--permission-mode auto`，
    代理需完整支持 tool_use/tool_result 往返，先跑 `assistant review --dry-run` 之外的小 PR 验证。
- 粘贴块（以 LiteLLM 代理为例）：

```json
"openai": {
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:4000",
    "ANTHROPIC_AUTH_TOKEN": "sk-litellm-master-key",
    "API_TIMEOUT_MS": "3000000",
    "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "gpt-5.2",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "gpt-5.2",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "gpt-5.2-mini",
    "CLAUDE_CODE_SUBAGENT_MODEL": "gpt-5.2-mini"
  }
}
```

## 3. 评审场景的经验取值

- 评审要**长上下文 + 长工具链**：优先 1M/大窗口档 + `API_TIMEOUT_MS` 拉满 + `CLAUDE_CODE_AUTO_COMPACT_WINDOW`
  与模型窗口一致（否则大 diff 提前触发压缩丢上下文）。
- 质量优先的评审：主档/子档都别用 Free/Flash；`CLAUDE_CODE_EFFORT_LEVEL=high|max`。
- 分诊（triage）偏短任务：Haiku 档指快模型即可，省钱。
- MCP 工具面：gitea MCP 工具较多，若网关转发 `tool_reference` 可开 `ENABLE_TOOL_SEARCH=auto:5` 省上下文；
  否则保持关闭（第三方 base_url 默认）。
- 变更 provider 后重启 daemon 生效；`assistant run --dry-run` 可核对生效的 provider/优化点计数（值不落日志）。
