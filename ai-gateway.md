# AI 网关（`cmd/ai-gateway`）

assistant 套件里的自托管 API 网关：对客户端暴露 **Anthropic Messages 协议**，
向上游 Anthropic 兼容厂商（zhipu / kimi / minimax / opencode / OpenAI 翻译代理 /
Anthropic 官方）转发，按路由链故障转移。它与 `assistant` 是**同一仓库、不同进程、
不同镜像**：网关是数据面（持厂商密钥、逐请求代理），assistant 是控制面（调度 +
对话），互不拖累发布与故障域。

## 协议面与透传语义

| 客户端路径 | 协议 | 上游要求 |
| --- | --- | --- |
| `POST /v1/messages`、`/v1/messages/count_tokens` | `anthropic` | 上游声明 `protocols: ["anthropic"]` |
| `POST /v1/chat/completions` | `openai-chat` | 上游声明 `openai-chat` |
| `POST /v1/responses` | `openai-responses` | 上游声明 `openai-responses` |

**只做同协议透传**（不做跨协议翻译）：路由按「虚拟模型 + 客户端协议」过滤上游，
未声明该协议的上游不参与（否则 400 并提示检查 `routes`/`protocols`）。请求体
**默认逐字节透传**——键序、空白、转义都不动；只有在「模型映射命中」或「需要写
`prompt_cache_key`/`metadata.user_id`」时才重新解析序列化（语义等价）。请求头
除 hop-by-hop 与客户端凭据外原样保留，另加上游鉴权、静态头、会话注入与 UA 规范化。

## 客户端工具（claude / codex / dsh）

一个工具一个文件（`internal/aigateway/tool_*.go`），识别后用于日志、指标与 UA
规范化：

| 工具 | 协议 | 会话来源 | 说明 |
| --- | --- | --- | --- |
| `claude`（Claude Code） | Anthropic Messages | 请求体 `metadata.user_id`，兼容 `x-session-id` | UA `claude-cli/*` 原样保留 + 网关后缀 |
| `codex`（Codex CLI） | OpenAI Responses | `session_id` 头（部分版本缺失 → 内容派生兜底） | `originator: codex_*` / UA 识别 |
| `dsh`（DeepSeek Harness） | OpenAI Chat Completions | `x-dsh-session` / `session_id`（部分路径缺失 → 兜底） | UA/`x-app` 识别 |

识别出工具后若客户端 UA 是通用 SDK/库名（`OpenAI/Python`、`httpx/`、`go-http-client`…）
或缺失，会被改写为工具专属署名——满足 OpenCode Go「客户端应使用自身专属 UA」
的约束。

## OpenCode Go/Zen 使用约束（网关侧保证）

参考 [OpenCode Go 文档](https://opencode.ai/docs/zh-cn/go/)：客户端应发送典型的
编程 Agent 流量、使用专属 UA、并在 `x-opencode-session` 中发送**每段对话稳定的
会话 ID**。网关对 opencode 上游的保证：

1. **会话头永远存在**：客户端带（`x-opencode-session`/`x-session-affinity`/`session_id`/
   `metadata.user_id`…）就透传/规范化；没带就按内容确定性派生——包括 dsh 这类
   “部分调用路径缺失会话头”的工具也自动补齐。
2. **专属 UA**：识别 claude/codex/dsh 并规范化为其专属署名（见上表）。
3. **会话头保留**：转发时不清除任何会话头（只替换鉴权与 hop-by-hop）。
4. **约束提醒**：账号用途/转售/限流等是上游条款，网关只在日志与 `/status` 里
   提供 `tool` / `session` 归因，便于自我审计。

opencode 上游示例（Go 三协议全开）：

```json
{
  "name": "opencode-go",
  "base_url": "https://opencode.ai/zen/go",
  "token": "sk-zen-...",
  "protocols": ["anthropic", "openai-chat", "openai-responses"],
  "session": {
    "headers": ["x-opencode-session", "x-session-affinity"],
    "body_field": "prompt_cache_key",
    "metadata_user_id": true
  }
}
```

## 数据驻留（完整保留请求与响应）

`residency.enabled` + `residency.dir` 打开后，每个请求归档为：

```
<dir>/<YYYY-MM-DD>/<时间>-<随机>/
  request.meta.json        请求元信息（头已脱敏：Authorization/x-api-key/Cookie/会话头）
  client-request.body      客户端原始请求体（逐字节）
  upstream-request.body    改写后的上游请求体（仅改写时）
  response.meta.json       响应元信息（状态/头/字节数/耗时/错误）
  upstream-response.body   上游响应体（SSE 流式边收边写，完整保留）
```

- 元信息含 `session`/`session_source`/`tool`/`key`/`protocol`/`model`/`upstream`/耗时，
  方便按会话或工具归因；凭据一律不入盘。
- 流式响应不占内存（边收边写文件），适合长会话大 diff。
- 目录保留策略由运维决定（`find` + 定期清理或挂载到专用盘）。

## 为什么不是 LiteLLM/new-api

通用网关做协议转换与渠道管理很好，但**解决不了“会话”这件事**：不同 Agent 与
供应商对会话的载体完全不同，且很多客户端什么都不带。本网关的核心是统一会话：

| 来源 | 载体 | 示例 |
| --- | --- | --- |
| Claude Code | 请求体 `metadata.user_id`（JSON 字符串里的 `session_id`） | `{"session_id":"...","device_id":"..."}` |
| OpenCode（Go/Zen） | 请求头 `x-opencode-session`（2026-09-06 起强制） | 缺失可能被拒 |
| Fireworks / Cloudflare 等 | 请求头 `x-session-affinity` | 同会话路由到同副本，缓存才命中 |
| OpenAI 兼容端点 | 请求体 `prompt_cache_key` | 提示缓存亲和 |
| 其他工具 | **什么都不带** | 靠内容派生兜底 |

**抽取 → 规范化 → 注入**：

1. **抽取**（优先级）：
   - 显式请求头候选（默认 `x-session-id`、`x-session-affinity`、`x-opencode-session`、
     `x-claude-session-id`，可配置）；
   - Anthropic `metadata.user_id`：兼容 JSON 字符串（提取 `session_id`，忽略其余键）
     与纯字符串两种形态；
   - **内容派生**：`HMAC-SHA256(secret, 客户端密钥名 + 模型 + system + 第一条 user 消息)`。
     Anthropic 协议是无状态的（每轮重发完整历史），首条 user 消息在整段对话中不变，
     因此不需要任何客户端配合即可得到跨轮稳定的会话键；压缩/改写首条消息时换新键，
     与提示缓存的前缀语义一致。
2. **规范化**：统一折成 UUIDv5 布局文本（超长/含控制字符的显式值改为其摘要），
   可直接作为头或 JSON 字段。
3. **注入**（每个上游独立配置）：
   - `headers`：`["x-opencode-session", "x-session-affinity"]` 等；
   - `body_field`：`prompt_cache_key`；
   - `metadata_user_id: true`：按 Anthropic 原生格式写回 `metadata.user_id.session_id`，
     **保留既有的 `device_id` 等键**。

这样同一条对话在任意上游都落到同一亲和标识：缓存命中率与成本归因不受客户端实现
差异影响。

## 快速开始

```bash
make gateway                                  # 构建 ./ai-gateway
cp ai-gateway.example.json ~/.config/Cosmic-Developers-Union/assistant/ai-gateway.json
$EDITOR ~/.config/Cosmic-Developers-Union/assistant/ai-gateway.json   # 填厂商 key
./ai-gateway                                  # 缺省读 <配置目录>/ai-gateway.json
# 或 ./ai-gateway --config ./ai-gateway.json --listen 127.0.0.1:8780
```

assistant 侧只加一个 provider（无需预设，纯透传）：

```json
"providers": {
  "gateway": {
    "api_key": "gateway-virtual-token",
    "env": { "ANTHROPIC_BASE_URL": "http://127.0.0.1:8780" }
  }
}
```

`default_provider` 或 `repo.provider` 指到它即可；assistant 的评审/分诊/微信对话
全部经网关，模型档位映射、密钥轮换、故障转移都在网关上做。

## 配置参考

```jsonc
{
  "listen": "127.0.0.1:8780",          // 只绑环回；对外暴露请自行加 TLS/反代
  "session": {
    "secret": "随机 32 字节 base64",     // 内容派生 HMAC 密钥（集群内一致）
    "headers": ["x-session-id", "..."]   // 覆盖默认会话头候选顺序
  },
  "keys": [                             // 接入密钥（虚拟 key）
    { "name": "assistant", "token": "gateway-virtual-token", "models": ["*"] }
  ],
  "upstreams": [
    {
      "name": "zhipu",
      "base_url": "https://api.z.ai/api/anthropic",  // Anthropic Messages 兼容根
      "token": "your_zai_key",
      "auth": "bearer",                 // 缺省 bearer（Authorization: Bearer）；或 x-api-key
      "headers": {},                    // 额外静态头（可选）
      "models": {                       // 虚拟模型 → 上游模型（未命中原样透传）
        "claude-sonnet-5": "glm-5.3-flash[1m]"
      },
      "session": { "metadata_user_id": true },   // 会话注入规则
      "timeout_ms": 3000000              // 单次上游超时（缺省 30 分钟）
    }
  ],
  "routes": {                            // 虚拟模型 → 上游优先级（按序故障转移）
    "claude-sonnet-5": ["zhipu", "minimax", "anthropic"],
    "*": ["zhipu", "anthropic"]          // 兜底
  }
}
```

端点：`POST /v1/messages`、`POST /v1/messages/count_tokens`（同一路由与注入）、
`GET /v1/models`（列出 `routes` 的虚拟模型，供 `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY`）、
`GET /healthz`、`GET /status`（请求/失败/会话来源/上游计数）。

路由语义：

- 按 `routes[model]` 顺序尝试，`*` 兜底；上游返回 **429 / 5xx 或连接失败**才切下一个，
  **4xx 原样回传**（不掩盖客户端错误）；
- 客户端凭据不会透传给上游（网关用上游自己的 `token`）；`anthropic-beta`、
  `anthropic-version` 等头原样透传；
- 响应（含 SSE 流式）实时回传并逐块 flush；
- 日志一行一次尝试：`session=… source=… tool=… key=… protocol=… model=… upstream=… status=… bytes=…`。

## 安全

- `keys[].token` 是唯一对外凭据；上游厂商密钥只存在网关配置里（0600）。
- `session.secret` 用于内容派生；换密钥会让“无显式会话”的客户端换一批会话键。
- 网关默认只监听环回。要跨机使用：加 TLS 终止反代（或 `--listen` + 防火墙），
  并确保 `keys` 足够强。
- 网关不做内容审计/落库；提示与补全不存储。

## 路线图

- **跨协议翻译**（当前是「同协议透传 + 路由」；需要 OpenAI→Anthropic 之类转换时，
  先接 LiteLLM 作为 `openai-proxy` 上游，或后续在网关内做翻译层）；
- **复用 `internal/provider` 预设**：把 zhipu/opencode 等厂商的端点、模型映射、会话
  规格收敛为一份「一供应商一文件」的共享注册表，assistant 与网关同源；
- **成本归因**：解析响应 `usage`，按会话/密钥/上游累计，供 assistant 的
  `daemon_status` 或微信问答展示；
- **compose service**：与 daemon 同栈部署（独立镜像、独立重启策略）。
