package dispatcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildPromptPull(t *testing.T) {
	prompt := BuildPrompt(KindPull, 58, PromptContext{
		Repository: "owner/repo",
		Title:      "fix: something broke",
		HeadSHA:    "abc1234",
	})
	for _, want := range []string{
		"review pr #58\n",
		"owner/repo 的 #58「fix: something broke」",
		"钉定的 head：abc1234",
		"head 已更新，需以新 head 重新评审",
		"结论正文注明所评审的 head（abc1234）",
		"仅处理该 PR",
		"SKILL.md",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	bare := BuildPrompt(KindPull, 58, PromptContext{})
	if !strings.HasPrefix(bare, "review pr #58\n") {
		t.Errorf("bare prompt should start with review pr #58, got %q", bare)
	}
	if strings.Contains(bare, "钉定的 head") {
		t.Errorf("bare prompt should omit pinned head: %q", bare)
	}
}

func TestBuildPromptIssue(t *testing.T) {
	prompt := BuildPrompt(KindIssue, 62, PromptContext{})
	if !strings.HasPrefix(prompt, "triage issue #62\n") {
		t.Errorf("prompt = %q", prompt)
	}
	if !strings.Contains(prompt, "仅处理该 Issue") {
		t.Errorf("prompt = %q", prompt)
	}
}

func TestFeedStreamLineInit(t *testing.T) {
	outcome := NewSessionOutcome()
	var progress []string
	isResult := FeedStreamLine(
		&outcome,
		`{"type":"system","subtype":"init","session_id":"s-1","model":"claude-x"}`,
		func(line string) { progress = append(progress, line) },
	)
	if isResult {
		t.Error("init line should not be result")
	}
	if outcome.SessionID != "s-1" {
		t.Errorf("SessionID = %q, want s-1", outcome.SessionID)
	}
	if len(progress) != 1 || progress[0] != "session=s-1 model=claude-x" {
		t.Errorf("progress = %v", progress)
	}
}

func TestFeedStreamLineAssistant(t *testing.T) {
	outcome := NewSessionOutcome()
	var progress []string
	FeedStreamLine(
		&outcome,
		`{"type":"assistant","message":{"content":[`+
			`{"type":"thinking","thinking":"…"},`+
			`{"type":"text","text":" 读取 PR diff\n"},`+
			`{"type":"tool_use","name":"mcp__gitea__pull_request_read"}]}}`,
		func(line string) { progress = append(progress, line) },
	)
	if outcome.NumTurns != 1 {
		t.Errorf("NumTurns = %d, want 1", outcome.NumTurns)
	}
	want := []string{"读取 PR diff", "🔧 mcp__gitea__pull_request_read"}
	if len(progress) != len(want) {
		t.Fatalf("progress = %v, want %v", progress, want)
	}
	for i := range want {
		if progress[i] != want[i] {
			t.Errorf("progress[%d] = %q, want %q", i, progress[i], want[i])
		}
	}
}

func TestFeedStreamLineResultSuccess(t *testing.T) {
	outcome := NewSessionOutcome()
	isResult := FeedStreamLine(
		&outcome,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":7,`+
			`"total_cost_usd":0.42,"duration_ms":12345,"session_id":"s-2",`+
			`"result":"已提交 APPROVED","permission_denials":[{},{}]}`,
		nil,
	)
	if !isResult {
		t.Fatal("result line should be recognized")
	}
	if outcome.Subtype != "success" || outcome.IsError {
		t.Errorf("subtype/isError = %q/%v", outcome.Subtype, outcome.IsError)
	}
	if outcome.NumTurns != 7 || outcome.CostUSD != 0.42 || outcome.DurationMS != 12_345 {
		t.Errorf("outcome = %+v", outcome)
	}
	if outcome.SessionID != "s-2" || outcome.Result != "已提交 APPROVED" || outcome.PermissionDenials != 2 {
		t.Errorf("outcome = %+v", outcome)
	}
}

func TestFeedStreamLineResultFallback(t *testing.T) {
	outcome := NewSessionOutcome()
	FeedStreamLine(&outcome, `{"type":"result","subtype":"error_during_execution","errors":["x"]}`, nil)
	if !outcome.IsError || outcome.CostUSD != 0 || outcome.Result != "" {
		t.Errorf("outcome = %+v, want failure fallback", outcome)
	}
	if len(outcome.Errors) != 1 || outcome.Errors[0] != "x" {
		t.Errorf("errors = %v, want [x]", outcome.Errors)
	}
}

func TestFeedStreamLineIgnoresNoise(t *testing.T) {
	outcome := NewSessionOutcome()
	var progress []string
	onProgress := func(line string) { progress = append(progress, line) }
	for _, line := range []string{"not-json", `{"type":"user"}`, ""} {
		if FeedStreamLine(&outcome, line, onProgress) {
			t.Errorf("line %q should be ignored", line)
		}
	}
	if outcome.NumTurns != 0 || len(progress) != 0 {
		t.Errorf("outcome = %+v progress = %v", outcome, progress)
	}
}

func TestRunSessionDrivesClaudeAndCollectsStream(t *testing.T) {
	dir := t.TempDir()
	mcpConfigPath := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcpConfigPath, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "args.txt")
	bin := fakeClaude(t, dir, fmt.Sprintf(
		`printf '%%s\n' "$@" > '%s'
printf '%%s\n' '{"type":"system","subtype":"init","session_id":"s-9","model":"fake"}'
printf '%%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"读取 diff"},{"type":"tool_use","name":"mcp__gitea__pull_request_read"}]}}'
printf '%%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":1.5,"duration_ms":900,"session_id":"s-9","result":"评审完成"}'`,
		argsFile,
	))
	var progress []string
	outcome := RunSession(SessionOptions{
		Config:        testConfig(func(config *Config) { config.ClaudeBin = bin }),
		Prompt:        "review pr #64",
		Cwd:           dir,
		MCPConfigPath: mcpConfigPath,
		OnProgress:    func(line string) { progress = append(progress, line) },
	})
	if outcome.IsError {
		t.Fatalf("outcome = %+v, want success", outcome)
	}
	if outcome.Subtype != "success" || outcome.SessionID != "s-9" || outcome.Result != "评审完成" {
		t.Errorf("outcome = %+v", outcome)
	}
	if !strings.Contains(strings.Join(progress, "\n"), "读取 diff") ||
		!strings.Contains(strings.Join(progress, "\n"), "🔧 mcp__gitea__pull_request_read") {
		t.Errorf("progress = %v", progress)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(raw), "\n")
	for _, flag := range []string{
		"--permission-mode", "auto",
		"--autocompact", "auto",
		"--strict-mcp-config",
		"--mcp-config", mcpConfigPath,
		"--max-turns", "300",
	} {
		if !containsString(args, flag) {
			t.Errorf("missing arg %q in %v", flag, args)
		}
	}
	if !containsString(args, "review pr #64") {
		t.Errorf("prompt missing in %v", args)
	}
}

func TestRunSessionFlushesTrailingLineWithoutNewline(t *testing.T) {
	dir := t.TempDir()
	bin := fakeClaude(t, dir,
		`printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-7","model":"fake"}'
printf '%s' '{"type":"result","subtype":"success","is_error":false,"num_turns":2,"total_cost_usd":0.3,"duration_ms":400,"session_id":"s-7","result":"残行结果"}'`,
	)
	outcome := RunSession(SessionOptions{
		Config:        testConfig(func(config *Config) { config.ClaudeBin = bin }),
		Prompt:        "review pr #64",
		Cwd:           dir,
		MCPConfigPath: filepath.Join(dir, ".mcp.json"),
	})
	if outcome.SessionID != "s-7" || outcome.Result != "残行结果" {
		t.Errorf("outcome = %+v, want trailing line parsed", outcome)
	}
}

func TestRunSessionNonZeroExitCollectsErrors(t *testing.T) {
	dir := t.TempDir()
	bin := fakeClaude(t, dir, "echo 'mcp handshake failed' >&2\nexit 3")
	outcome := RunSession(SessionOptions{
		Config:        testConfig(func(config *Config) { config.ClaudeBin = bin }),
		Prompt:        "p",
		Cwd:           dir,
		MCPConfigPath: filepath.Join(dir, ".mcp.json"),
	})
	if !outcome.IsError {
		t.Error("outcome.IsError = false, want true")
	}
	joined := strings.Join(outcome.Errors, "\n")
	for _, want := range []string{"退出码 3", "mcp handshake failed", "未收到 result"} {
		if !strings.Contains(joined, want) {
			t.Errorf("errors = %v, want %q", outcome.Errors, want)
		}
	}
}

func TestRunSessionTimeout(t *testing.T) {
	dir := t.TempDir()
	// exec 让 sh 把自己替换成 sleep，SIGTERM 直接命中
	bin := fakeClaude(t, dir, "exec sleep 30")
	startedAt := time.Now()
	outcome := RunSession(SessionOptions{
		Config: testConfig(func(config *Config) {
			config.ClaudeBin = bin
			config.SessionTimeout = 200 * time.Millisecond
		}),
		Prompt:        "p",
		Cwd:           dir,
		MCPConfigPath: filepath.Join(dir, ".mcp.json"),
	})
	if !outcome.IsError {
		t.Error("outcome.IsError = false, want true")
	}
	if !strings.Contains(strings.Join(outcome.Errors, "\n"), "超时") {
		t.Errorf("errors = %v, want 超时", outcome.Errors)
	}
	if elapsed := time.Since(startedAt); elapsed > 15*time.Second {
		t.Errorf("elapsed = %v, want < 15s", elapsed)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestSessionCommandBare(t *testing.T) {
	options := SessionOptions{
		Config:        Config{ClaudeBin: "claude", Model: "sonnet"},
		Prompt:        "review pr #1",
		Cwd:           "/work",
		MCPConfigPath: "/repo/.mcp.json",
	}
	bin, args, container := sessionCommand(options)
	if bin != "claude" || container != "" {
		t.Fatalf("bin=%q container=%q, want claude/bare", bin, container)
	}
	for _, want := range []string{"-p", "review pr #1", "--model", "sonnet", "--mcp-config", "/repo/.mcp.json"} {
		if !containsString(args, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
}

func TestSessionCommandDocker(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "secret")
	t.Setenv("UNRELATED_VAR", "x")
	options := SessionOptions{
		Config: Config{
			ClaudeBin:     "claude",
			DockerImage:   "assistant-review:dev",
			DockerNetwork: "host",
		},
		Prompt:        "review pr #2",
		Cwd:           "/tmp/worktrees/pr-2",
		MCPConfigPath: "/srv/repo/.mcp.json",
	}
	bin, args, container := sessionCommand(options)
	if bin != "docker" || container == "" {
		t.Fatalf("bin=%q container=%q, want docker/named", bin, container)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run --rm -i",
		"--name " + container,
		"-v /tmp/worktrees/pr-2:/tmp/worktrees/pr-2",
		"-w /tmp/worktrees/pr-2",
		"-v /srv/repo:/srv/repo",
		"--network host",
		"-e ANTHROPIC_API_KEY",
		"assistant-review:dev claude -p review pr #2",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker args missing %q: %v", want, args)
		}
	}
	if strings.Contains(joined, "UNRELATED_VAR") {
		t.Errorf("docker args leaked unrelated env: %v", args)
	}
}
