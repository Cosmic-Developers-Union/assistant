package dispatcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
)

// 稳定会话 ID：同待办同锚点确定、head 变化换新、格式为合法 UUID。
func TestSessionIDDeterministic(t *testing.T) {
	first := SessionID("https://gitea.example.com", "acme/repo", KindPull, 12, "abc1234")
	again := SessionID("https://gitea.example.com", "acme/repo", KindPull, 12, "abc1234")
	if first != again {
		t.Errorf("同待办应派生同一 ID：%q vs %q", first, again)
	}
	if len(first) != 36 || first[14] != '5' {
		t.Errorf("ID 不是 v5 布局：%q", first)
	}
	changedHead := SessionID("https://gitea.example.com", "acme/repo", KindPull, 12, "def5678")
	if changedHead == first {
		t.Error("head 变化应换新会话记录")
	}
	if SessionID("https://other.example.com", "acme/repo", KindPull, 12, "abc1234") == first {
		t.Error("站点不同不应复用 ID")
	}
	if SessionID("https://gitea.example.com", "acme/repo", KindIssue, 12, "abc1234") == first {
		t.Error("待办类型不同不应复用 ID")
	}
}

// 会话标题：PR 带 head 短标识，Issue 用 triage 前缀。
func TestSessionTitle(t *testing.T) {
	if got := SessionTitle(KindPull, "acme/repo", 12, "abcdef1234567890"); got != "review acme/repo#12@abcdef1" {
		t.Errorf("SessionTitle() = %q", got)
	}
	if got := SessionTitle(KindPull, "acme/repo", 12, ""); got != "review acme/repo#12" {
		t.Errorf("SessionTitle() = %q", got)
	}
	if got := SessionTitle(KindIssue, "acme/repo", 7, ""); got != "triage acme/repo#7" {
		t.Errorf("SessionTitle() = %q", got)
	}
}

// 文本记录持久化：首次创建（--session-id），已有记录则 --resume 续接；记录
// 路径固定在 SessionDir/projects/<project> 下，并把项目目录名写进进程环境。
func TestRunSessionPersistsTranscript(t *testing.T) {
	dir := t.TempDir()
	mcpConfigPath := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcpConfigPath, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "args.txt")
	envFile := filepath.Join(dir, "env.txt")
	bin := fakeClaude(t, dir, `printf '%s\n' "$@" > '`+argsFile+`'
printf '%s' "$CLAUDE_CODE_PROJECT_DIR_NAME" > '`+envFile+`'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"x","result":"ok"}'`)

	sessionDir := filepath.Join(dir, "claude")
	config := testConfig(func(config *Config) {
		config.ClaudeBin = bin
		config.SessionDir = sessionDir
		config.SessionProject = "assistant-acme-repo"
	})
	sessionID := "11111111-2222-4333-8444-555555555555"
	outcome := RunSession(SessionOptions{
		Config:        config,
		Prompt:        "review pr #1",
		Cwd:           dir,
		MCPConfigPath: mcpConfigPath,
		SessionID:     sessionID,
		Title:         "review acme/repo#1",
	})
	if outcome.IsError {
		t.Fatalf("outcome = %+v", outcome)
	}
	wantPath := claudecfg.TranscriptPath(sessionDir, "assistant-acme-repo", sessionID)
	if outcome.TranscriptPath != wantPath || outcome.Resumed {
		t.Errorf("TranscriptPath = %q Resumed = %v, want %q/false", outcome.TranscriptPath, outcome.Resumed, wantPath)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(raw), "\n")
	if !containsString(args, "--session-id") || containsString(args, "--resume") {
		t.Errorf("首次应 --session-id：%v", args)
	}
	if !containsString(args, "--name") || !containsString(args, "review acme/repo#1") {
		t.Errorf("缺 --name 标题：%v", args)
	}
	if env, err := os.ReadFile(envFile); err != nil || string(env) != "assistant-acme-repo" {
		t.Errorf("CLAUDE_CODE_PROJECT_DIR_NAME = %q, err = %v", string(env), err)
	}

	// 预置文本记录后重试同一待办 → --resume 续接，同一路径
	if err := os.MkdirAll(filepath.Dir(wantPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wantPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outcome = RunSession(SessionOptions{
		Config:        config,
		Prompt:        "review pr #1",
		Cwd:           dir,
		MCPConfigPath: mcpConfigPath,
		SessionID:     sessionID,
		Title:         "review acme/repo#1",
	})
	if outcome.IsError || !outcome.Resumed {
		t.Fatalf("outcome = %+v, want resumed", outcome)
	}
	raw, err = os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args = strings.Split(string(raw), "\n")
	if !containsString(args, "--resume") || containsString(args, "--session-id") {
		t.Errorf("已有记录应 --resume：%v", args)
	}
}
