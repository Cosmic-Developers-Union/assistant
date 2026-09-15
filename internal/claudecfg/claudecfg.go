// Package claudecfg 定义 assistant 对 Claude Code 的托管配置：项目级
// .claude/settings.json（assistant install 写入/核对）与 headless 会话的
// 独立 --settings / --mcp-config（dispatcher 与对话桥生成）共用同一份环境
// 变量与权限放行，避免漂移。
//
// 托管默认之外支持 provider 覆盖（Overrides）：供应商（Anthropic 官方、兼容
// 网关、Bedrock/Vertex 等）的 env/settings/mcp 原样透传合并，只作用于运行时
// 会话配置，绝不写入仓库文件。
package claudecfg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
)

// Env 是托管环境变量：把评审/开发会话的时长与输出上限钉死，键名与取值对齐
// Claude Code 官方环境变量文档。
var Env = map[string]string{
	// 长测试/构建：默认 2 分钟太低，默认上限与最大上限分别抬到 5/30 分钟
	"BASH_DEFAULT_TIMEOUT_MS": "300000",
	"BASH_MAX_TIMEOUT_MS":     "1800000",
	// 测试日志回读上限（官方允许的最大值 150000 字符）
	"BASH_MAX_OUTPUT_LENGTH": "150000",
	// gitea MCP：大 diff / 长 Issue 的执行超时与输出上限
	"MCP_TIMEOUT":           "30000",
	"MAX_MCP_OUTPUT_TOKENS": "50000",
}

// Allow 是托管权限放行：gitea MCP + 只读/低风险命令（写入类命令仍走权限判定）。
var Allow = []string{
	"mcp__gitea",
	"mcp__gitea__*",
	"Bash(assistant:*)",
	"Bash(git status:*)",
	"Bash(git diff:*)",
	"Bash(git log:*)",
	"Bash(git show:*)",
	"Bash(git branch:*)",
	"Bash(git rev-parse:*)",
	"Bash(git worktree list:*)",
}

// SettingSources 是 headless 会话加载的设置来源：只认项目级（随仓库提交的
// 评审基线），不读操作者的用户级与本地私有设置。MCP 工具面另由
// --strict-mcp-config + --mcp-config 钉死。
const SettingSources = "project"

// CleanupPeriodDays 是会话文本记录的保留天数（Claude Code 默认 30 天即被清理
// 扫描删除；评审/分诊记录要留作审计，钉到 ~10 年，provider 覆盖可改）。
const CleanupPeriodDays = 3650

// ProjectDirName 把候选名折成 CLAUDE_CODE_PROJECT_DIR_NAME 的合法值（官方规则：
// 1-64 位字母/数字/连字符/下划线，非法字符替换为 -），超长截断并附哈希；折叠
// 后为空时返回 "assistant"。
func ProjectDirName(name string) string {
	var builder strings.Builder
	for _, character := range strings.TrimSpace(name) {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
			builder.WriteRune(character)
		default:
			builder.WriteRune('-')
		}
	}
	sanitized := strings.Trim(builder.String(), "-")
	if sanitized == "" {
		return "assistant"
	}
	if len(sanitized) > 64 {
		sum := sha256.Sum256([]byte(name))
		sanitized = sanitized[:55] + "-" + hex.EncodeToString(sum[:4])
	}
	return sanitized
}

// TranscriptPath 返回会话文本记录路径（与 Claude Code 存储规则一致）。
func TranscriptPath(configDir, projectDirName, sessionID string) string {
	if configDir == "" || projectDirName == "" || sessionID == "" {
		return ""
	}
	return filepath.Join(configDir, "projects", projectDirName, sessionID+".jsonl")
}

// SessionEnv 返回启动 claude 时必须写进**进程环境**的两项（settings.env 无法
// 设置 CLAUDE_CODE_PROJECT_DIR_NAME，CLAUDE_CONFIG_DIR 同理）：让文本记录固定
// 落在项目目录名下，而不是随 worktree 路径漂移或被删除。任一项缺失时返回空。
func SessionEnv(configDir, projectDirName string) []string {
	if strings.TrimSpace(configDir) == "" || strings.TrimSpace(projectDirName) == "" {
		return nil
	}
	return []string{
		"CLAUDE_CONFIG_DIR=" + configDir,
		"CLAUDE_CODE_PROJECT_DIR_NAME=" + projectDirName,
	}
}

// Overrides 是 provider 对会话配置的运行时覆盖，三段均原样透传（框架只做
// 合并，不解释供应商语义），支持不同供应商的各异格式：
//   - Env：会话环境变量（可含密钥），合并进 settings.env 与 provider 自定义的
//     MCP server env（server 自身取值优先）；
//   - Settings：Claude Code 原生 settings 片段，浅合并进会话 settings（env 与
//     permissions 逐键合并，其余顶层键覆盖托管默认）；
//   - MCP：原生 MCP server 定义（.mcp.json 形态），合并进 --mcp-config，同名
//     server 由 provider 覆盖。
type Overrides struct {
	Env      map[string]string
	Settings map[string]any
	MCP      map[string]any
}

// Empty 报告是否没有任何覆盖（用于跳过临时配置生成）。
func (o Overrides) Empty() bool {
	return len(o.Env) == 0 && len(o.Settings) == 0 && len(o.MCP) == 0
}

// Counts 返回 env/settings/mcp 三项覆盖数量（日志与状态展示用）。
func (o Overrides) Counts() (env, settings, mcp int) {
	return len(o.Env), len(o.Settings), len(o.MCP)
}

// ComposeOverrides 叠加两层覆盖（high 优先，例如 provider 覆盖全局优化点）：
// env 逐键合并、settings 走同一套浅合并规则、mcp 同名由 high 覆盖。
func ComposeOverrides(low, high Overrides) Overrides {
	if low.Empty() {
		return high
	}
	if high.Empty() {
		return low
	}
	composed := Overrides{}
	if len(low.Env) > 0 || len(high.Env) > 0 {
		env := make(map[string]string, len(low.Env)+len(high.Env))
		for key, value := range low.Env {
			env[key] = value
		}
		for key, value := range high.Env {
			env[key] = value
		}
		composed.Env = env
	}
	if len(low.Settings) > 0 || len(high.Settings) > 0 {
		settings := map[string]any{}
		mergeSettings(settings, low.Settings)
		mergeSettings(settings, high.Settings)
		composed.Settings = settings
	}
	if len(low.MCP) > 0 || len(high.MCP) > 0 {
		servers := make(map[string]any, len(low.MCP)+len(high.MCP))
		for name, server := range low.MCP {
			servers[name] = server
		}
		for name, server := range high.MCP {
			servers[name] = server
		}
		composed.MCP = servers
	}
	return composed
}

// SessionSettingsMap 生成 headless 会话的完整设置视图：托管 env/权限默认，
// 叠加 provider 覆盖，extraAllow 追加到权限放行（调用方自有工具面）。
func SessionSettingsMap(overrides Overrides, extraAllow ...string) map[string]any {
	settings := map[string]any{
		"env": envDocument(Env),
		"permissions": map[string]any{
			"allow": appendAllow(append([]string(nil), Allow...), extraAllow...),
		},
		// 项目级 .mcp.json 不自动启用：会话只用 --mcp-config 注入的服务器
		"enableAllProjectMcpServers": false,
		// 文本记录保留期（默认 30 天太短，审计需要；默认值可被 provider 覆盖）
		"cleanupPeriodDays": CleanupPeriodDays,
	}
	mergeSettings(settings, overrides.Settings)
	if len(overrides.Env) > 0 {
		environment, _ := settings["env"].(map[string]any)
		if environment == nil {
			environment = map[string]any{}
		}
		for key, value := range overrides.Env {
			environment[key] = value
		}
		settings["env"] = environment
	}
	return settings
}

// SessionSettings 生成 headless 会话的独立配置 JSON（--settings 的内容），
// 无 provider 覆盖（等价 SessionSettingsWith(Overrides{})）。
func SessionSettings() ([]byte, error) {
	return SessionSettingsWith(Overrides{})
}

// SessionSettingsWith 是 SessionSettings 的 provider 覆盖版本。
func SessionSettingsWith(overrides Overrides) ([]byte, error) {
	encoded, err := json.MarshalIndent(SessionSettingsMap(overrides), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// MergeMCPServers 把 provider 的原生 MCP server 定义合并进 base 文档（就地
// 修改并返回同一 map）：
//   - 同名 server 由 provider 覆盖；
//   - provider 定义的 stdio server（含 command）缺省继承 provider env，server
//     自身 env 优先——原生 MCP 需要供应商凭据时无需在 mcp 段重复手写密钥；
//
// base 的既有 server（如仓库的 gitea）不受影响。base 为 nil 时新建文档。
func MergeMCPServers(document map[string]any, overrides Overrides) map[string]any {
	if len(overrides.MCP) == 0 {
		return document
	}
	if document == nil {
		document = map[string]any{}
	}
	servers, _ := document["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	for name, raw := range overrides.MCP {
		if raw == nil {
			// null 表示移除基线同名 server（可关闭预设自带的供应商 MCP）
			delete(servers, name)
			continue
		}
		server, ok := raw.(map[string]any)
		if !ok {
			servers[name] = raw
			continue
		}
		merged := cloneDocument(server)
		if _, stdio := merged["command"]; stdio && len(overrides.Env) > 0 {
			environment, _ := merged["env"].(map[string]any)
			if environment == nil {
				environment = map[string]any{}
			}
			// server 自身 env 优先：先铺 provider env 再回填
			for key, value := range overrides.Env {
				if _, exists := environment[key]; !exists {
					environment[key] = value
				}
			}
			merged["env"] = environment
		}
		servers[name] = merged
	}
	document["mcpServers"] = servers
	return document
}

// mergeSettings 把 provider 原生 settings 片段浅合并进托管设置：env 与
// permissions 逐键合并（provider 优先，allow 取并集），其余顶层键直接覆盖。
func mergeSettings(settings map[string]any, fragment map[string]any) {
	for key, value := range fragment {
		switch key {
		case "env":
			environment, _ := settings["env"].(map[string]any)
			if environment == nil {
				environment = map[string]any{}
			}
			for envKey, envValue := range asDocument(value) {
				environment[envKey] = envValue
			}
			settings["env"] = environment
		case "permissions":
			permissions, _ := settings["permissions"].(map[string]any)
			if permissions == nil {
				permissions = map[string]any{}
			}
			for permissionKey, permissionValue := range asDocument(value) {
				if permissionKey != "allow" {
					permissions[permissionKey] = permissionValue
					continue
				}
				allow := stringList(permissions["allow"])
				allow = appendAllow(allow, stringList(permissionValue)...)
				permissions["allow"] = allow
			}
			settings["permissions"] = permissions
		default:
			settings[key] = value
		}
	}
}

func envDocument(env map[string]string) map[string]any {
	document := make(map[string]any, len(env))
	for key, value := range env {
		document[key] = value
	}
	return document
}

// asDocument 把任意 JSON 值折成 map[string]any（非对象返回空）。
func asDocument(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case map[string]string:
		return envDocument(typed)
	}
	return map[string]any{}
}

// stringList 把任意 JSON 值折成字符串列表（非数组/非字符串元素忽略）。
func stringList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		switch typed := value.(type) {
		case []string:
			return append([]string(nil), typed...)
		case string:
			return []string{typed}
		}
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func appendAllow(values []string, additions ...string) []string {
	for _, addition := range additions {
		exists := false
		for _, value := range values {
			if value == addition {
				exists = true
				break
			}
		}
		if !exists {
			values = append(values, addition)
		}
	}
	return values
}

// cloneDocument 深拷贝 JSON 文档（provider 定义会被多个会话复用，避免合并时
// 相互污染配置里的原始 map）。
func cloneDocument(document map[string]any) map[string]any {
	encoded, err := json.Marshal(document)
	if err != nil {
		return document
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		return document
	}
	return clone
}
