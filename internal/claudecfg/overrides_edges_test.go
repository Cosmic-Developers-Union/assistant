package claudecfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SessionSettingsMap 的退化分支：settings.env 为 nil 时 provider env 仍要落位
// （不能就地 nil map 写入 panic 或静默丢弃）。
func TestSessionSettingsEnvFallbacks(t *testing.T) {
	settings := SessionSettingsMap(Overrides{
		Env:      map[string]string{"ANTHROPIC_BASE_URL": "https://gw.example.com"},
		Settings: map[string]any{"env": nil},
	})
	env, _ := settings["env"].(map[string]any)
	if env == nil {
		t.Fatalf("env 应为对象：%+v", settings["env"])
	}
	if env["ANTHROPIC_BASE_URL"] != "https://gw.example.com" {
		t.Errorf("provider env 未落位：%+v", env)
	}
	if env["MCP_TIMEOUT"] != Env["MCP_TIMEOUT"] {
		t.Errorf("托管 env 应保留：%+v", env)
	}

	// settings.env 是字符串而非对象：折成空对象后 provider env 仍要落位
	settings = SessionSettingsMap(Overrides{
		Env:      map[string]string{"A": "1"},
		Settings: map[string]any{"env": "不是对象"},
	})
	env, _ = settings["env"].(map[string]any)
	if env == nil || env["A"] != "1" {
		t.Errorf("非对象 env 应被折成对象：%+v", settings["env"])
	}
	if _, err := SessionSettingsWith(Overrides{Settings: map[string]any{"env": "不是对象"}}); err != nil {
		t.Errorf("序列化应正常：%v", err)
	}
}

// MergeMCPServers 的退化条目：null 表示移除同名基线 server，非对象条目原样透传，
// 都不该 panic 也不该污染其它 server。
func TestMergeMCPServersDegenerateEntries(t *testing.T) {
	base := map[string]any{
		"mcpServers": map[string]any{
			"gitea":  map[string]any{"command": "assistant", "args": []any{"mcp", "gitea"}},
			"daemon": map[string]any{"command": "assistant", "args": []any{"mcp", "daemon"}},
		},
	}
	document := MergeMCPServers(base, Overrides{MCP: map[string]any{
		"gitea":  nil,           // 移除基线里的 gitea
		"raw":    "some-string", // 非对象条目：原样透传
		"absent": nil,           // 不存在同名项时删除应为 no-op
	}})
	servers, _ := document["mcpServers"].(map[string]any)
	if _, ok := servers["gitea"]; ok {
		t.Errorf("null 应移除同名 server：%+v", servers)
	}
	if _, ok := servers["daemon"]; !ok {
		t.Errorf("其它 server 不该被牵连：%+v", servers)
	}
	if servers["raw"] != "some-string" {
		t.Errorf("非对象条目应原样透传：%+v", servers["raw"])
	}
	if len(servers) != 2 {
		t.Errorf("servers = %+v", servers)
	}

	// 空 mcpServers 文档：新建 server 后文档结构完整
	created := MergeMCPServers(map[string]any{}, Overrides{MCP: map[string]any{"only": map[string]any{"command": "x"}}})
	servers, _ = created["mcpServers"].(map[string]any)
	if _, ok := servers["only"]; !ok {
		t.Errorf("空文档应新建 server：%+v", created)
	}

	// provider env 存在但 server 不是 stdio（无 command）：不注入 env
	document = MergeMCPServers(nil, Overrides{
		Env: map[string]string{"API_KEY": "k"},
		MCP: map[string]any{"remote": map[string]any{"type": "http", "url": "https://x"}},
	})
	servers, _ = document["mcpServers"].(map[string]any)
	remote, _ := servers["remote"].(map[string]any)
	if _, ok := remote["env"]; ok {
		t.Errorf("非 stdio server 不该注入 env：%+v", remote)
	}
}

// mergeSettings 逐键合并的退化形态：env 段不是对象、permissions 段不是对象、
// allow 段不是数组，都不该 panic，托管默认要保持可用。
func TestMergeSettingsDegenerateFragments(t *testing.T) {
	settings := SessionSettingsMap(Overrides{Settings: map[string]any{
		"env":         "不是对象",
		"permissions": "也不是对象",
	}})
	env, _ := settings["env"].(map[string]any)
	if env["MCP_TIMEOUT"] != Env["MCP_TIMEOUT"] {
		t.Errorf("托管 env 应保留：%+v", env)
	}
	permissions, _ := settings["permissions"].(map[string]any)
	allow, _ := permissions["allow"].([]string)
	if len(allow) == 0 || allow[0] != "mcp__gitea" {
		t.Errorf("托管 allow 应保留：%+v", permissions["allow"])
	}

	// permissions.allow 非数组：托管 allow 不受影响，字符串形态被接受为单元素
	settings = SessionSettingsMap(Overrides{Settings: map[string]any{
		"permissions": map[string]any{"allow": "Bash(echo:*)", "deny": []any{"Bash(rm:*)"}},
	}})
	permissions, _ = settings["permissions"].(map[string]any)
	allow, _ = permissions["allow"].([]string)
	found := false
	for _, value := range allow {
		if value == "Bash(echo:*)" {
			found = true
		}
	}
	if !found {
		t.Errorf("字符串形态的 allow 应被接受：%+v", allow)
	}
	if _, ok := permissions["deny"]; !ok {
		t.Errorf("deny 应被合并：%+v", permissions)
	}
	// permissions 段不是对象时也不该 panic，托管 allow 仍在
	settings = SessionSettingsMap(Overrides{Settings: map[string]any{"permissions": 42}})
	permissions, _ = settings["permissions"].(map[string]any)
	if allow, _ := permissions["allow"].([]string); len(allow) == 0 {
		t.Errorf("托管 allow 应保留：%+v", permissions)
	}
}

// describe.go 的退化行：server 值不是对象时只出名字；name 行与 url/command 行的
// 取值都必须与文档一致（这是「声明了什么 MCP」的唯一展示口径）。
func TestMCPServerLinesDegenerate(t *testing.T) {
	document := map[string]any{"mcpServers": map[string]any{
		"plain":    "just-a-string", // 值不是对象：只出名字
		"remote":   map[string]any{"type": "http", "url": "https://mcp.example.com"},
		"noDetail": map[string]any{"type": "stdio"}, // 无 url 也无 command：冒号后为空
	}}
	lines := MCPServerLines(document)
	want := []string{
		"noDetail：",
		"plain",
		"remote：https://mcp.example.com",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("MCPServerLines =\n%s\nwant=\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	// env 为空对象时不输出 env 行；url 优先于 command
	document = map[string]any{"mcpServers": map[string]any{
		"both": map[string]any{"url": "https://x", "command": "cmd", "env": map[string]any{}},
	}}
	lines = MCPServerLines(document)
	if strings.Join(lines, "\n") != "both：https://x" {
		t.Errorf("url 应优先且空 env 不出行：%v", lines)
	}

	// mcpServers 不是对象：按空处理
	if MCPServerLines(map[string]any{"mcpServers": 42})[0] != "（无）" {
		t.Error("mcpServers 非对象时应显示（无）")
	}
}

// JSONLines 的退化路径：无法序列化的值（如 map 里带不可编码的值）返回 nil；
// 值本身为 nil 也返回 nil。
func TestJSONLinesUnencodable(t *testing.T) {
	if got := JSONLines(map[string]any{"bad": make(chan int)}, 10); got != nil {
		t.Errorf("不可序列化应返回 nil：%v", got)
	}
	if got := JSONLines(nil, 10); got != nil {
		t.Errorf("nil 应返回 nil：%v", got)
	}
	// 正常值不该受影响
	if lines := JSONLines(map[string]any{"k": "v"}, 10); len(lines) == 0 {
		t.Error("正常值应有行")
	}
}

// ConfigDirEnv 只按去空白后的值判空，但嵌入的是**原值**：确认这条边界（空白路径
// 视为未配置，非空白路径原样透传到变量值里）。
func TestConfigDirEnvRawValue(t *testing.T) {
	if got := ConfigDirEnv("  /tmp/cfg  "); len(got) != 1 || got[0] != "CLAUDE_CONFIG_DIR=  /tmp/cfg  " {
		t.Errorf("ConfigDirEnv 嵌入原值 = %q", got)
	}
}

// AssistantCommand 的缓存语义：首次调用即定值，多次调用稳定不变（会话间不漂移），
// 且结果要么是绝对路径、要么是 PATH 回退名。
func TestAssistantCommandStable(t *testing.T) {
	first := AssistantCommand()
	if first == "" {
		t.Fatal("AssistantCommand() 不该为空")
	}
	for range 5 {
		if again := AssistantCommand(); again != first {
			t.Fatalf("多次调用应稳定：%q vs %q", again, first)
		}
	}
	if first != "assistant" && !filepath.IsAbs(first) {
		t.Errorf("应是绝对路径或回退名：%q", first)
	}
}

// SessionSettingsWith 写文件的形态：缩进两空格、以换行结尾、含托管 env 与权限。
func TestSessionSettingsShape(t *testing.T) {
	encoded, err := SessionSettingsWith(Overrides{Env: map[string]string{"A": "1"}})
	if err != nil {
		t.Fatalf("SessionSettingsWith: %v", err)
	}
	if !strings.HasSuffix(string(encoded), "\n") {
		t.Error("应以换行结尾")
	}
	if !strings.Contains(string(encoded), "\n  \"env\"") {
		t.Errorf("应两空格缩进：\n%s", encoded)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("回读失败：%v", err)
	}
	env, _ := decoded["env"].(map[string]any)
	if env["A"] != "1" || env["BASH_MAX_OUTPUT_LENGTH"] != Env["BASH_MAX_OUTPUT_LENGTH"] {
		t.Errorf("env = %+v", env)
	}
	if decoded["enableAllProjectMcpServers"] != false {
		t.Errorf("enableAllProjectMcpServers = %v", decoded["enableAllProjectMcpServers"])
	}
}

// 确认测试不写开发者真实目录：这里只核对 os.UserConfigDir 未被本包触碰，
// 以及本包没有任何写入用户配置根的行为（托管配置由调用方落盘）。
func TestNoWritesToUserConfigDir(t *testing.T) {
	root, err := os.UserConfigDir()
	if err != nil {
		t.Skip("本机没有用户配置目录")
	}
	target := filepath.Join(root, "Cosmic-Developers-Union", "assistant")
	before, statErr := os.Stat(target)
	// 只构造配置，不做任何落盘
	_, _ = SessionSettingsWith(Overrides{Env: map[string]string{"A": "1"}})
	_, _ = SessionSettings()
	after, afterErr := os.Stat(target)
	if statErr == nil && afterErr == nil && before.ModTime() != after.ModTime() {
		t.Errorf("本包不该改动用户配置目录：%s", target)
	}
}
