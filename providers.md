# providers 优化点（内置预设 + 全局 + 各供应商）

`assistant` 为常见供应商内置了**开箱即用预设**：选一个名字 + 只写 `api_key`（或
`auth_token`）就能接入，端点、令牌变量名、模型档位映射、超时与上下文窗口由预设补齐。
改完配置跑一次 `assistant validate`：它会指出没被任何地方引用的 provider（配了却没生效）、
缺 `api_key` 的 provider，以及缺身份/用途令牌的平台。

## 0. 开箱即用

写进 `config.json`：

```json
{
  "default_provider": "zhipu",
  "providers": { "zhipu": { "api_key": "your-zai-api-key" } }
}
```

或**一个 provider 一个文件** `<配置目录>/providers/zhipu.json`：

```json
{ "api_key": "your-zai-api-key" }
```

内置预设一览（别名同样可用；`api_key`/`auth_token` 写哪个都行）：

| provider 名（别名） | 端点 | 令牌写入 | 预设附带的默认 | 备注 |
| --- | --- | --- | --- | --- |
| `anthropic`（claude、official） | 官方 `api.anthropic.com` | `ANTHROPIC_API_KEY` | 无（Claude Code 默认最优） | 官方模型档位自动跟随 |
| `zhipu`（glm、zai、z.ai） | `https://api.z.ai/api/anthropic` | `ANTHROPIC_AUTH_TOKEN` + `Z_AI_API_KEY` | 全档 `glm-5.3-flash[1m]`、1M 压缩窗口、`API_TIMEOUT_MS=3000000`、关非必要流量 + **视觉理解 MCP** | glm-5.3-flash 绝对优先；主档想用 glm-5.3[1m] 可覆盖 |
| `bigmodel`（zhipu-cn） | `https://open.bigmodel.cn/api/anthropic` | 同上 | 同上（MCP `Z_AI_MODE=ZHIPU`） | 国内 bigmodel 端点 |
| `kimi`（kimi-code、kimi-coding） | `https://api.kimi.com/coding/` | `ANTHROPIC_API_KEY` | 全档 `kimi-for-coding`、窗口/压缩 262144、子 agent 同档 | Kimi Code 订阅；升级套餐后按需覆盖为 `k3[1m]` |
| `moonshot`（kimi-platform、moonshot-ai） | `https://api.moonshot.cn/anthropic` | `ANTHROPIC_AUTH_TOKEN` | 全档 `kimi-k3`、压缩窗口 262144 | 开放平台（key 与 Kimi Code 不通用；国际站改 `api.moonshot.ai`） |
| `minimax` | `https://api.minimax.io/anthropic` | `ANTHROPIC_AUTH_TOKEN` + `MINIMAX_API_KEY` | 全档 `MiniMax-M3[1m]`、压缩窗口 1000000 + **coding-plan MCP** | 自动 cache；512K/M2.7 档自行覆盖 |
| `minimax-cn`（minimaxi） | `https://api.minimaxi.com/anthropic` | 同上 | 同上（MCP host `api.minimax.cn`） | 国内端点 |
| `opencode`（zen、opencode-zen、opencode-go） | `https://opencode.ai/zen` | `ANTHROPIC_AUTH_TOKEN` | Claude 档位（网关原生提供，无需映射） | 自动注入 `x-opencode-session` + `x-session-affinity` 会话头（见第 2 节） |
| `openai`（gpt） | **无**，必须自配 `ANTHROPIC_BASE_URL` | `ANTHROPIC_AUTH_TOKEN` | 无 | OpenAI 无 Anthropic 端点，base_url 指向翻译代理（LiteLLM 等），否则配置校验直接报错 |

预设还自带供应商官方 MCP（随会话 `--mcp-config` 注入，不需要额外安装步骤）：

- 运行环境需要 `node`/`npx`（智谱视觉理解）与 `uv`/`uvx`（MiniMax coding-plan）：
  daemon 镜像已内置（`images/review/Dockerfile`），宿主机运行请自行安装；
- 首次会话会下载 MCP 包（npx/uvx 缓存挂在 `${HOME}/.cache`，重启不重复下载）；
- 关闭某个预设 MCP：`"mcp": {"zai-mcp-server": null}`（同名 server 也按此覆盖）。

覆盖与关闭：

- 用户 `env`/`settings`/`mcp` 覆盖预设同名键；`env` 里给**空串**即删除该预设默认，
  例如不想要某个模型映射：`"env": {"ANTHROPIC_DEFAULT_HAIKU_MODEL": ""}`。
- 模型 ID 随版本/套餐滚动：换档就在 provider `env` 覆盖 `ANTHROPIC_MODEL` 与
  `ANTHROPIC_DEFAULT_*_MODEL`（预设里已给四档 + `CLAUDE_CODE_SUBAGENT_MODEL`）。
- 未识别的 provider 名**没有预设**，行为是纯手写透传（可当任意自定义网关用）。
- 预设与用户配置都只进运行时会话（临时 `--settings`/`--mcp-config`），不写仓库文件。

## 1. 全局优化点（`optimizations`）

跨供应商通用、需要按模型/网络微调的项（对所有会话打底，provider 覆盖其上）：

| 键 | 建议 | 说明 |
| --- | --- | --- |
| `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` | `"1"` | 关掉遥测/自动更新等非必要请求（无人值守、省钱、防被网关计费） |
| `DISABLE_TELEMETRY` / `DISABLE_ERROR_REPORTING` | `"1"` | 同上，双保险（老版本变量名） |
| `DISABLE_AUTOUPDATER` | `"1"` | 容器内不需要自更新（镜像已固定版本） |
| `API_TIMEOUT_MS` | `"3000000"` | 第三方网关长思考 + 长 diff 常见超时来源；官方可留默认 |
| `CLAUDE_CODE_SUBAGENT_MODEL` | 便宜/快档 | 子 agent 与摘要类任务钉到廉价模型，省钱 |
| `CLAUDE_CODE_EFFORT_LEVEL` | `"high"` 或 `"max"` | 评审质量优先；网关不支持 effort 时无效（可删） |
| `CLAUDE_CODE_AUTO_COMPACT_WINDOW` | 视模型：`1000000`/`512000`/`262144` | 纯整数（`500k` 会被当 500 用）；也被用来纠正 `[1m]` 模型 ID 的窗口 |
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

> `permissions` 与托管放行是并集合并；`deny` 会随 provider 覆盖保留（托管里没写这一项）。

## 2. 各供应商覆盖要点（预设之上）

`anthropic`

- 优化点：提示缓存自动生效（别开 `DISABLE_PROMPT_CACHING`）；质量优先 `CLAUDE_CODE_EFFORT_LEVEL=max`；
  省钱把 `CLAUDE_CODE_SUBAGENT_MODEL` 指 Haiku 档、`CLAUDE_CODE_MAX_OUTPUT_TOKENS` 压到实际需要。
- 覆盖样例：`{"api_key": "sk-ant-...", "env": {"CLAUDE_CODE_EFFORT_LEVEL": "max", "CLAUDE_CODE_SUBAGENT_MODEL": "claude-haiku-4-5"}}`

`zhipu` / `bigmodel`

- 预设按官方推荐：全档 `glm-5.3-flash[1m]`（glm-5.3-flash 绝对优先）、
  `CLAUDE_CODE_AUTO_COMPACT_WINDOW=1000000`、`API_TIMEOUT_MS=3000000`、关非必要流量。
- 主档想用 glm-5.3[1m]（官方示例配置）：
  `{"env": {"ANTHROPIC_MODEL": "glm-5.3[1m]", "ANTHROPIC_DEFAULT_SONNET_MODEL": "glm-5.3[1m]", "ANTHROPIC_DEFAULT_OPUS_MODEL": "glm-5.3[1m]", "ANTHROPIC_DEFAULT_FABLE_MODEL": "glm-5.3[1m]"}}`；
  Haiku 保持 `glm-5.3-flash[1m]`。
- **视觉理解 MCP**（官方 vision-mcp-server）随预设注入：`npx -y @z_ai/mcp-server@latest`
  （`@latest` 避免 npx 缓存旧版本），`Z_AI_API_KEY` 由 `api_key` 自动写入，
  `Z_AI_MODE` 按平台（z.ai=`ZAI`，bigmodel=`ZHIPU`）。
  工具：`ui_to_artifact`、`extract_text_from_screenshot`、`diagnose_error_screenshot`、
  `understand_technical_diagram`、`analyze_data_visualization`、`ui_diff_check`、
  `image_analysis`、`video_analysis`（本地视频 ≤8M）。
  注意：Claude Code 走 GLM Coding Plan 时服务端已内置 `image_analysis`，装全量工具才有其余 7 个；
  使用时把图片放本地目录再用路径提问（直接粘贴图片不会走 MCP）。
- 老账号（2025-09-30 前订阅）可能只有 OpenAI 协议权限，Claude Code 走不通。

`kimi` / `moonshot`

- 两种账号、端点与 key 不通用：Kimi Code 订阅（`api.kimi.com/coding/`，`ANTHROPIC_API_KEY`）与
  Moonshot 开放平台（`api.moonshot.cn/anthropic`，`ANTHROPIC_AUTH_TOKEN`）。
- 1M 上下文：`ANTHROPIC_MODEL=k3[1m]` + `CLAUDE_CODE_AUTO_COMPACT_WINDOW=1048576`、
  `CLAUDE_CODE_MAX_CONTEXT_TOKENS=1048576`；256K 档 `k3-256k` + `262144`。
- `kimi-k2.7-code` **强制开启思考**（未开 thinking 会 400），适合主模型、别放 Haiku 档。
- 覆盖样例：`{"api_key": "sk-...", "env": {"ANTHROPIC_MODEL": "k3[1m]", "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1048576"}}`

`minimax` / `minimax-cn`

- 预设为 M3 1M 档；512K 档：`ANTHROPIC_*_MODEL=MiniMax-M3` + `CLAUDE_CODE_AUTO_COMPACT_WINDOW=512000`。
- 快档：`MiniMax-M2.7-highspeed`（204800）。思考默认开启（`/config` 可关）。
- 自动 cache，无需配置。
- **coding-plan MCP**（官方配置）随预设注入：`uvx minimax-coding-plan-mcp`，
  `MINIMAX_API_KEY` 由 `api_key` 自动写入，`MINIMAX_API_HOST` 按平台
  （国际 `https://api.minimax.io`，国内 `https://api.minimax.cn`）；
  官方 `mcpServers` 里 `"MINIMAX_API_KEY": "MINIMAX_API_KEY"` 的占位由框架替代，
  不用手填。

`opencode`（OpenCode Zen / Go 网关）

- **会话请求头（必须）**：Go/Zen 端点自 2026-09-06 起要求每个会话一个稳定的
  `x-opencode-session`（缺失可能被拒），OpenCode 客户端还默认带 `x-session-affinity`
  （缓存亲和）。预设已内置代码级特化：评审/分诊每会话一个 UUID、微信对话复用稳定会话
  UUID，经 `ANTHROPIC_CUSTOM_HEADERS` 注入；你在 `env` 显式配置的同名头优先。
  特化实现 `internal/provider/opencode.go`，网关改名/新增要求时只改该文件。
- Anthropic 协议档（`claude-*`、`qwen3.7-max/plus`）可直接用；Zen 表里标
  `/chat/completions` 的模型（deepseek/minimax/glm/gpt/grok/gemini）需要翻译代理。
- 免费模型（如 `big-pickle`）可能用你的提示词训练，评审私有代码前评估。
- `ENABLE_TOOL_SEARCH` 先保持默认（第三方 base_url 默认关闭）；确认 Zen 转发
  `tool_reference` 再开。

`openai`

- 必须自备翻译网关（LiteLLM、claude-code-router、自建代理），`ANTHROPIC_BASE_URL` 指向它；
  缺 base_url 时配置加载直接报错（这就是预设「开箱即用」的边界：OpenAI 没有可直连的端点）。
- 代理要求：暴露 `/v1/messages`（与 `count_tokens`）、原样转发 `anthropic-beta`/`anthropic-version`；
  非常规鉴权用 `ANTHROPIC_CUSTOM_HEADERS`（换行分隔 `Key: Value`）。
- 代理对 beta header 报 400 → `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`（tool search 同时关闭）。
- 四档模型映射必须显式配齐（OpenAI 模型名与 Claude 档位无默认对应）；子 agent 钉 mini。
- 覆盖样例：`{"api_key": "sk-...", "env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:4000", "ANTHROPIC_DEFAULT_OPUS_MODEL": "gpt-5.2", "ANTHROPIC_DEFAULT_SONNET_MODEL": "gpt-5.2", "ANTHROPIC_DEFAULT_HAIKU_MODEL": "gpt-5.2-mini", "CLAUDE_CODE_SUBAGENT_MODEL": "gpt-5.2-mini"}}`

## 3. 特殊行为怎么加（代码级特化）

只有需要在**会话启动时动态调整**（会话级请求头、按会话取令牌、动态 MCP 等）才需要写代码；
一个供应商一个文件：`internal/provider/<名字>.go`，实现 `Handler` 并 `Register`：

```go
type myHandler struct{}

func init() { Register(myHandler{}) }

func (myHandler) Name() string { return "myprovider" }
func (myHandler) Aliases() []string { return []string{"my-alias"} }

func (myHandler) Prepare(s *Session) error {
    // s.Provider / s.Kind("review"/"triage"/"chat") / s.ID（本次会话 UUID）
    s.EnsureHeader("x-my-session", s.ID)            // → ANTHROPIC_CUSTOM_HEADERS
    s.Env["SOME_PER_SESSION_TOKEN"] = fetchToken()  // 也可改 settings / mcp
    return nil
}
```

## 4. 评审场景的经验取值

- 评审要**长上下文 + 长工具链**：优先 1M/大窗口档 + `API_TIMEOUT_MS` 拉满 +
  `CLAUDE_CODE_AUTO_COMPACT_WINDOW` 与模型窗口一致（否则大 diff 提前压缩丢上下文）。
- 质量优先：主档/子档都别用 Free/Flash；`CLAUDE_CODE_EFFORT_LEVEL=high|max`。
- 分诊（triage）偏短任务：Haiku 档指快模型即可，省钱。
- MCP 工具面：gitea MCP 工具较多，网关转发 `tool_reference` 时可用 `ENABLE_TOOL_SEARCH=auto:5` 省上下文。
- 变更 provider 后重启 daemon 生效；`assistant run --dry-run` 显示生效的 provider（含「内置预设」标记）
  与覆盖计数（值不落日志）。
