package dispatcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
)

// writeSessionMCPConfig：assistant 指定的 gitea server 恒注入（覆盖仓库里的同名
// 条目），仓库其它 server 保留，provider 原生 MCP 最后合并；仓库没有 .mcp.json
// 也照常生成——评审不依赖仓库内容。
func TestWriteSessionMCPConfig(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "repo.json")
	if err := os.WriteFile(base, []byte(`{"mcpServers": {
		"gitea": {"command": "stale-assistant", "args": ["mcp", "gitea"]},
		"project": {"command": "project-mcp"}
	}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// 无 provider mcp：也生成会话配置，gitea 条目由 assistant 注入
	generated, err := writeSessionMCPConfig(dir, base, claudecfg.Overrides{Env: map[string]string{"A": "b"}})
	if err != nil {
		t.Fatalf("writeSessionMCPConfig: %v", err)
	}
	if generated == base {
		t.Fatal("会话 MCP 配置必须是新生成的合并文件，不能复用仓库文件")
	}
	servers := readMCPServers(t, generated)
	gitea, _ := servers[claudecfg.MCPServerGitea].(map[string]any)
	if gitea["command"] != claudecfg.AssistantCommand() {
		t.Errorf("gitea server 应由 assistant 指定：%+v", gitea)
	}
	if args, _ := gitea["args"].([]any); len(args) != 2 || args[0] != "mcp" || args[1] != "gitea" {
		t.Errorf("gitea server args 不对：%+v", gitea)
	}
	if _, ok := servers["project"]; !ok {
		t.Errorf("仓库其它 server 应保留：%+v", servers)
	}

	// provider mcp：合并进同一文件，同名覆盖，stdio server 继承 provider env
	merged, err := writeSessionMCPConfig(dir, base, claudecfg.Overrides{
		Env: map[string]string{"GATEWAY_KEY": "secret"},
		MCP: map[string]any{
			"search": map[string]any{"command": "search-mcp"},
			"gitea":  map[string]any{"command": "provider-gitea"},
		},
	})
	if err != nil {
		t.Fatalf("writeSessionMCPConfig: %v", err)
	}
	servers = readMCPServers(t, merged)
	if gitea, _ := servers["gitea"].(map[string]any); gitea["command"] != "provider-gitea" {
		t.Errorf("provider 同名 server 应覆盖 assistant 默认：%+v", servers)
	}
	search, _ := servers["search"].(map[string]any)
	searchEnv, _ := search["env"].(map[string]any)
	if searchEnv["GATEWAY_KEY"] != "secret" {
		t.Errorf("provider env 未注入 server：%+v", searchEnv)
	}

	// 仓库没有 .mcp.json：gitea 条目照样在，评审不因仓库缺文件而失去工具面
	missing, err := writeSessionMCPConfig(dir, filepath.Join(dir, "absent.json"), claudecfg.Overrides{})
	if err != nil {
		t.Fatalf("缺少 base 时应可用 assistant 注入的 MCP: %v", err)
	}
	if _, ok := readMCPServers(t, missing)[claudecfg.MCPServerGitea]; !ok {
		t.Error("仓库缺 .mcp.json 时 gitea server 仍应注入")
	}

	// base 非法 JSON 直接报错，不静默丢弃仓库工具面
	if err := os.WriteFile(base, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSessionMCPConfig(dir, base, claudecfg.Overrides{}); err == nil {
		t.Error("非法 base 应报错")
	}
}

// writeSessionMCPConfig / writeClaudeSessionSettings / createSessionConfigDir 的
// 失败路径：读不动、写不进、目录建不出来时必须报错退出，不能静默吞掉——
// 否则会话会在没有工具面或没有权限放行的状态下跑起来。
func TestClaudeConfigWriteFailures(t *testing.T) {
	dir := t.TempDir()

	t.Run("base 存在但不是普通文件：读取错误原样上抛", func(t *testing.T) {
		sub := filepath.Join(dir, "as-dir")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		// 目录不能当文件读，且不是 ErrNotExist：走 !os.IsNotExist 分支
		if _, err := writeSessionMCPConfig(dir, sub, claudecfg.Overrides{}); err == nil ||
			!strings.Contains(err.Error(), "读取 MCP 配置") {
			t.Errorf("error = %v, want 读取 MCP 配置失败", err)
		}
	})

	t.Run("目标目录不存在：写 MCP 配置报错", func(t *testing.T) {
		if _, err := writeSessionMCPConfig(filepath.Join(dir, "absent-dir"), "", claudecfg.Overrides{}); err == nil ||
			!strings.Contains(err.Error(), "写入合并后的 MCP 配置") {
			t.Errorf("error = %v, want 写入合并后的 MCP 配置失败", err)
		}
	})

	t.Run("目标目录不存在：写会话配置报错", func(t *testing.T) {
		if _, err := writeClaudeSessionSettings(filepath.Join(dir, "absent-dir"), claudecfg.Overrides{}); err == nil ||
			!strings.Contains(err.Error(), "写入会话配置") {
			t.Errorf("error = %v, want 写入会话配置失败", err)
		}
	})

	t.Run("正常路径落盘且权限收紧", func(t *testing.T) {
		settings, err := writeClaudeSessionSettings(dir, claudecfg.Overrides{})
		if err != nil {
			t.Fatalf("writeClaudeSessionSettings: %v", err)
		}
		info, err := os.Stat(settings)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("settings 权限 = %v, want 0600（会话配置含环境变量，不外泄）", info.Mode().Perm())
		}
		mcp, err := writeSessionMCPConfig(dir, "", claudecfg.Overrides{})
		if err != nil {
			t.Fatalf("writeSessionMCPConfig: %v", err)
		}
		if mcpInfo, err := os.Stat(mcp); err != nil || mcpInfo.Mode().Perm() != 0o600 {
			t.Errorf("mcp.json 权限 = %v/%v, want 0600", mcpInfo.Mode().Perm(), err)
		}
	})

	t.Run("配置目录创建成功且 cleanup 清干净", func(t *testing.T) {
		created, cleanup, err := createSessionConfigDir()
		if err != nil {
			t.Fatalf("createSessionConfigDir: %v", err)
		}
		if _, err := os.Stat(created); err != nil {
			t.Fatalf("配置目录应存在: %v", err)
		}
		cleanup()
		if _, err := os.Stat(created); !os.IsNotExist(err) {
			t.Errorf("cleanup 后目录应消失, stat err = %v", err)
		}
	})
}

// sessionProjectsDir / sessionMountDirs 的空值口径：SessionDir 为空时不产出
// 挂载点（否则 docker 会拿到空字符串路径），MCP / settings 路径为空同理。
func TestSessionProjectsDirEmptyAndMountDirsSkipsEmpty(t *testing.T) {
	if got := sessionProjectsDir(Config{}); got != "" {
		t.Errorf("sessionProjectsDir(空 SessionDir) = %q, want 空", got)
	}
	if got := sessionProjectsDir(Config{SessionDir: "/root/claude"}); got != "/root/claude/projects" {
		t.Errorf("sessionProjectsDir = %q, want /root/claude/projects", got)
	}
	// 空 SessionDir + 无路径选项：没有任何挂载点
	if dirs := sessionMountDirs(SessionOptions{}); len(dirs) != 0 {
		t.Errorf("sessionMountDirs = %v, want 空", dirs)
	}
	// 三者齐全时去重后保持 MCP / settings / projects 的顺序
	dirs := sessionMountDirs(SessionOptions{
		MCPConfigPath: "/cfg/mcp.json",
		SettingsPath:  "/cfg/settings.json",
		Config:        Config{SessionDir: "/root/claude"},
	})
	want := []string{"/cfg", "/root/claude/projects"}
	if len(dirs) != len(want) {
		t.Fatalf("sessionMountDirs = %v, want %v", dirs, want)
	}
	for i := range want {
		if dirs[i] != want[i] {
			t.Errorf("dirs[%d] = %q, want %q", i, dirs[i], want[i])
		}
	}
}

func readMCPServers(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	servers, _ := document["mcpServers"].(map[string]any)
	if servers == nil {
		t.Fatalf("MCP 配置缺少 mcpServers：%s", data)
	}
	return servers
}
