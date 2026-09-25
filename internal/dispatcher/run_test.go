package dispatcher

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
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
		"附加 system 提示词",
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
	sessionMCPCopy := filepath.Join(dir, "session-mcp.json")
	bin := fakeClaude(t, dir, fmt.Sprintf(
		`printf '%%s\n' "$@" > '%s'
prev=""
for arg in "$@"; do
  if [ "$prev" = "--mcp-config" ]; then cp "$arg" '%s'; fi
  prev="$arg"
done
printf '%%s\n' '{"type":"system","subtype":"init","session_id":"s-9","model":"fake"}'
printf '%%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"读取 diff"},{"type":"tool_use","name":"mcp__gitea__pull_request_read"}]}}'
printf '%%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":1.5,"duration_ms":900,"session_id":"s-9","result":"评审完成"}'`,
		argsFile, sessionMCPCopy,
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
		"--max-turns", "300",
	} {
		if !containsString(args, flag) {
			t.Errorf("missing arg %q in %v", flag, args)
		}
	}
	// -mcp-config 指向生成的会话配置（assistant 注入的 gitea server），不再是仓库文件
	index := slices.Index(args, "--mcp-config")
	if index < 0 || index+1 >= len(args) {
		t.Fatalf("missing --mcp-config in %v", args)
	}
	if sessionMCP := args[index+1]; sessionMCP == mcpConfigPath {
		t.Errorf("--mcp-config 应指向生成的会话配置，而不是仓库 .mcp.json：%v", args)
	} else if _, ok := readMCPServers(t, sessionMCPCopy)[claudecfg.MCPServerGitea]; !ok {
		t.Errorf("会话 MCP 配置缺少 gitea server：%s", sessionMCPCopy)
	}
	// --append-system-prompt 带上 assistant 内置的评审协议（不依赖仓库里的 skill 文件；
	// 多行提示词在参数文件里被拆成多行，所以按整体内容断言）
	if !strings.Contains(string(raw), "--append-system-prompt") ||
		!strings.Contains(string(raw), "PR 审查协议") ||
		!strings.Contains(string(raw), "标签体系") {
		t.Errorf("附加 system 提示词缺少内置评审协议：%s", raw)
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
		SessionID:     "11111111-2222-4333-8444-555555555555",
		Title:         "review acme/repo#1",
	}
	bin, args, container := sessionCommand(options)
	if bin != "claude" || container != "" {
		t.Fatalf("bin=%q container=%q, want claude/bare", bin, container)
	}
	for _, want := range []string{
		"-p", "review pr #1", "--model", "sonnet", "--mcp-config", "/repo/.mcp.json",
		"--setting-sources", "project",
		"--session-id", "11111111-2222-4333-8444-555555555555",
		"--name", "review acme/repo#1",
	} {
		if !containsString(args, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	if containsString(args, "--no-session-persistence") {
		t.Errorf("会话记录应持久化（不再禁用）：%v", args)
	}
	if containsString(args, "--settings") {
		t.Errorf("未生成会话配置时不应传 --settings: %v", args)
	}

	// 已存在文本记录时改为 --resume 续接
	resumed := options
	resumed.SessionResume = true
	_, args, _ = sessionCommand(resumed)
	if !containsString(args, "--resume") || containsString(args, "--session-id") {
		t.Errorf("resume 形态参数错误：%v", args)
	}
}

// 独立会话配置：env 与权限放行走 claudecfg，未显式预置时由 RunSession 写临时
// 文件；Docker 形态按相同路径挂载 settings 所在目录。
func TestWriteClaudeSessionSettings(t *testing.T) {
	dir, cleanup, err := createSessionConfigDir()
	if err != nil {
		t.Fatalf("createSessionConfigDir: %v", err)
	}
	defer cleanup()
	path, err := writeClaudeSessionSettings(dir, claudecfg.Overrides{
		Env:      map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.example.com"},
		Settings: map[string]any{"model": "gateway-model"},
	})
	if err != nil {
		t.Fatalf("writeClaudeSessionSettings: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	env, _ := document["env"].(map[string]any)
	if env["BASH_DEFAULT_TIMEOUT_MS"] != claudecfg.Env["BASH_DEFAULT_TIMEOUT_MS"] ||
		env["MAX_MCP_OUTPUT_TOKENS"] != claudecfg.Env["MAX_MCP_OUTPUT_TOKENS"] {
		t.Errorf("env = %+v", env)
	}
	if env["ANTHROPIC_BASE_URL"] != "https://gateway.example.com" {
		t.Errorf("provider env 未注入：%+v", env)
	}
	if document["model"] != "gateway-model" {
		t.Errorf("provider settings 未合并：%v", document["model"])
	}
	if document["enableAllProjectMcpServers"] != false {
		t.Errorf("enableAllProjectMcpServers = %v, want false", document["enableAllProjectMcpServers"])
	}
	permissions, _ := document["permissions"].(map[string]any)
	allow, _ := permissions["allow"].([]any)
	if !containsString(toStrings(allow), "mcp__gitea__*") || !containsString(toStrings(allow), "Bash(git diff:*)") {
		t.Errorf("allow = %v", allow)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup 后文件仍在：%v", err)
	}
}

func toStrings(values []any) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func TestSessionMountDirsDedupes(t *testing.T) {
	dirs := sessionMountDirs(SessionOptions{
		MCPConfigPath: "/srv/repo/.mcp.json",
		SettingsPath:  "/srv/repo/session.json",
	})
	if !slices.Equal(dirs, []string{"/srv/repo"}) {
		t.Errorf("dirs = %v, want [/srv/repo]", dirs)
	}
	dirs = sessionMountDirs(SessionOptions{
		MCPConfigPath: "/srv/repo/.mcp.json",
		SettingsPath:  "/tmp/settings/settings.json",
	})
	if !slices.Equal(dirs, []string{"/srv/repo", "/tmp/settings"}) {
		t.Errorf("dirs = %v", dirs)
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
		SettingsPath:  "/tmp/settings/settings.json",
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
		"-v /tmp/settings:/tmp/settings",
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

// 会话必须显式拿到 reviewer 身份与配置来源：config.json 模式下 daemon 进程环境
// 通常没有 Gitea 变量，会话 MCP 会退回开发者个人的 mcp 令牌，review 不再以 ai 落库。
func TestSessionInjectsGiteaIdentityEnv(t *testing.T) {
	// 宿主环境里的同名变量必须被会话注入值覆盖（而不是反过来）：否则 daemon 的
	// 环境会决定会话的 Gitea 身份。
	t.Setenv("GITEA_HOST", "https://ambient.example.com")
	t.Setenv("GITEA_ACCESS_TOKEN", "ambient-token")
	t.Setenv("ASSISTANT_CONFIG", "/ambient/config.json")
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env.txt")
	bin := fakeClaude(t, dir, fmt.Sprintf(
		`printf '%%s|%%s|%%s\n' "$GITEA_HOST" "$GITEA_ACCESS_TOKEN" "$ASSISTANT_CONFIG" > '%s'
printf '%%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"s-1","result":"ok"}'`,
		envFile,
	))
	config := testConfig(func(config *Config) {
		config.ClaudeBin = bin
		config.Host = "https://gitea.example.com"
		config.AccessToken = "reviewer-token"
		config.ConfigPath = "/etc/assistant/config.json"
	})
	outcome := RunSession(SessionOptions{
		Config:        config,
		Prompt:        "review pr #1",
		Cwd:           dir,
		MCPConfigPath: filepath.Join(dir, ".mcp.json"),
	})
	if outcome.IsError {
		t.Fatalf("outcome = %+v, want success", outcome)
	}
	raw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(raw))
	if got != "https://gitea.example.com|reviewer-token|/etc/assistant/config.json" {
		t.Fatalf("会话环境 = %q（应钉定 reviewer 身份与 config 路径）", got)
	}
}

// 会话凭据环境：配置项齐全时三件都注入；环境变量单实例模式不产生 ASSISTANT_CONFIG；
// docker 形态按 -e 键透传（值由宿主进程环境继承）。
func TestSessionCredentialEnv(t *testing.T) {
	full := Config{
		Host:        "https://gitea.example.com",
		AccessToken: "reviewer-token",
		ConfigPath:  "/etc/assistant/config.json",
	}
	if got, want := sessionCredentialEnv(full), []string{
		"GITEA_HOST=https://gitea.example.com",
		"GITEA_ACCESS_TOKEN=reviewer-token",
		"ASSISTANT_CONFIG=/etc/assistant/config.json",
	}; !slices.Equal(got, want) {
		t.Errorf("sessionCredentialEnv = %v, want %v", got, want)
	}
	if got, want := sessionCredentialKeys(full), []string{
		"GITEA_HOST", "GITEA_ACCESS_TOKEN", "ASSISTANT_CONFIG",
	}; !slices.Equal(got, want) {
		t.Errorf("sessionCredentialKeys = %v, want %v", got, want)
	}
	if got, want := sessionCredentialEnv(Config{Host: "h", AccessToken: "t"}),
		[]string{"GITEA_HOST=h", "GITEA_ACCESS_TOKEN=t"}; !slices.Equal(got, want) {
		t.Errorf("单实例模式 sessionCredentialEnv = %v, want %v", got, want)
	}
	if got := sessionCredentialEnv(Config{}); len(got) != 0 {
		t.Errorf("空配置不应注入环境变量：%v", got)
	}

	docker := full
	docker.DockerImage = "assistant-review:dev"
	docker.ClaudeBin = "claude"
	t.Setenv("GITEA_ACCESS_TOKEN", "reviewer-token")
	_, args, _ := sessionCommand(SessionOptions{
		Config: docker, Prompt: "review pr #1", Cwd: "/w", MCPConfigPath: "/w/.mcp.json",
	})
	joined := strings.Join(args, " ")
	for _, key := range []string{"-e GITEA_HOST", "-e GITEA_ACCESS_TOKEN", "-e ASSISTANT_CONFIG"} {
		if !strings.Contains(joined, key) {
			t.Errorf("docker args missing %q: %v", key, args)
		}
	}
}

// 项目评审约定：ProjectDir 下的 .assistant/review.md 作为附加 system 提示词注入。
func TestRunSessionInjectsReviewConventions(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".assistant"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, ".assistant", "review.md"),
		[]byte("# 项目约定\n必须检查目录边界\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "args.txt")
	bin := fakeClaude(t, dir, fmt.Sprintf(
		`printf '%%s\n' "$@" > '%s'
printf '%%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"s-1","result":"ok"}'`,
		argsFile,
	))
	outcome := RunSession(SessionOptions{
		Config:        testConfig(func(config *Config) { config.ClaudeBin = bin }),
		Prompt:        "review pr #9",
		Cwd:           dir,
		ProjectDir:    dir,
		MCPConfigPath: filepath.Join(dir, ".mcp.json"),
	})
	if outcome.IsError {
		t.Fatalf("outcome = %+v, want success", outcome)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	// 约定内容含换行，按整段原文匹配（printf 每参数一行会把它拆开）
	if !strings.Contains(string(raw), "--append-system-prompt") ||
		!strings.Contains(string(raw), "必须检查目录边界") {
		t.Errorf("args = %q, 缺少附加 system 提示词", raw)
	}
}

// 约定文件缺失时不传 --append-system-prompt；超长内容截断。
func TestReadReviewConventions(t *testing.T) {
	dir := t.TempDir()
	if got := ReadReviewConventions(dir); got != "" {
		t.Errorf("缺文件应返回空串，got %q", got)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".assistant"), 0o755); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("约", ReviewConventionsLimit+100)
	if err := os.WriteFile(filepath.Join(dir, ".assistant", "review.md"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ReadReviewConventions(dir)
	if !strings.Contains(got, "已截断") {
		t.Errorf("超长约定应截断，len=%d", len(got))
	}
}

// 评审/分诊协议来自 assistant 内置的 review 技能（仓库没 install 也生效），
// 项目自有约定（.assistant/review.md）附在其后。
func TestReviewProtocolPrompt(t *testing.T) {
	dir := t.TempDir()
	prompt := ReviewProtocolPrompt(dir)
	for _, want := range []string{"PR 审查协议", "Issue 分诊协议", "标签体系", "type/bug"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("内置协议缺少 %q", want)
		}
	}
	if strings.Contains(prompt, "项目评审约定") {
		t.Errorf("没有 .assistant/review.md 时不该有项目约定段：%s", prompt)
	}

	if err := os.MkdirAll(filepath.Join(dir, ".assistant"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".assistant", "review.md"), []byte("# 项目约定\n\n必须跑 go test ./...\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt = ReviewProtocolPrompt(dir)
	if !strings.Contains(prompt, "必须跑 go test ./...") || !strings.Contains(prompt, "## 项目评审约定") {
		t.Errorf("项目约定未附上：%s", prompt)
	}
	if !strings.Contains(prompt, "PR 审查协议") {
		t.Errorf("追加项目约定后内置协议不该丢：%s", prompt)
	}
}

func int64Ptr(v int64) *int64 { return &v }

// thinking 帧不再逐条上日志：一段思考只报一条汇总（tokens / 时长 / t/s / 帧数），
// 其余事件的时间线保持原样。
func TestStreamWriterAggregatesThinkingTokenFrames(t *testing.T) {
	var progress []string
	outcome := NewSessionOutcome()
	collect := func(line string) { progress = append(progress, line) }
	stream := &streamWriter{outcome: &outcome, onProgress: collect, debug: true,
		thinking: thinkingTracker{onProgress: collect, debug: true}}
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s-1"}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":10,"estimated_tokens_delta":10}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":25,"estimated_tokens_delta":15}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":40,"estimated_tokens_delta":15}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"结论"}]}}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":5,"estimated_tokens_delta":5}`,
		`{"type":"result","subtype":"success"}`,
	}
	for _, line := range lines {
		if _, err := stream.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	stream.thinking.finish()

	var summaries []string
	for _, line := range progress {
		switch {
		case strings.Contains(line, "thinking_tokens"):
			t.Errorf("thinking 帧被逐条上报：%q", line)
		case strings.Contains(line, "thinking"):
			summaries = append(summaries, line)
		}
	}
	if len(summaries) != 3 {
		t.Fatalf("thinking 汇总 = %v, want 段汇总×2 + 合计×1", summaries)
	}
	if !strings.Contains(summaries[0], "40 tokens") || !strings.Contains(summaries[0], "3 帧") {
		t.Errorf("第一段汇总 = %q, want 40 tokens / 3 帧", summaries[0])
	}
	if !strings.Contains(summaries[0], "t/s") {
		t.Errorf("段汇总 = %q, want 含 t/s", summaries[0])
	}
	if !strings.Contains(summaries[2], "45 tokens") || !strings.Contains(summaries[2], "2 段") {
		t.Errorf("合计 = %q, want 45 tokens / 2 段", summaries[2])
	}
}

// 流以 thinking 帧结尾（如超时截断）时，收尾冲刷仍会报出段汇总与合计。
func TestThinkingTrackerFlushesTrailingBurst(t *testing.T) {
	var progress []string
	outcome := NewSessionOutcome()
	collect := func(line string) { progress = append(progress, line) }
	stream := &streamWriter{outcome: &outcome, onProgress: collect, debug: true,
		thinking: thinkingTracker{onProgress: collect, debug: true}}
	for i := 0; i < 3; i++ {
		line := `{"type":"system","subtype":"thinking_tokens","estimated_tokens":7,"estimated_tokens_delta":7}`
		if _, err := stream.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	stream.thinking.finish()

	for _, line := range progress {
		if strings.Contains(line, "事件") {
			t.Errorf("thinking 帧被逐条上报：%q", line)
		}
	}
	if len(progress) != 2 {
		t.Fatalf("progress = %v, want 段汇总 + 合计", progress)
	}
	if !strings.Contains(progress[0], "21 tokens") || !strings.Contains(progress[1], "21 tokens") {
		t.Errorf("汇总 = %v, want 各含 21 tokens", progress)
	}
}

// 增量缺失的帧按累计值差值补算（兼容只报 estimated_tokens 的生产者）：段内
// 首帧作基线不计数，防止跨思考段的重复累计。
func TestThinkingTrackerFallsBackToCumulative(t *testing.T) {
	var progress []string
	tracker := thinkingTracker{onProgress: func(line string) { progress = append(progress, line) }, debug: true}
	tracker.observe(streamEvent{EstimatedTokens: int64Ptr(10)})
	tracker.observe(streamEvent{EstimatedTokens: int64Ptr(25)})
	tracker.flush()
	if len(progress) != 1 || !strings.Contains(progress[0], "15 tokens") {
		t.Fatalf("progress = %v, want 汇总含 15 tokens（25-10 基线差值）", progress)
	}
}
