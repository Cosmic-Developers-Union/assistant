package claude

import (
	"slices"
	"strings"
	"testing"
)

// argsIndex 返回 flag 在 argv 中的位置（-1 表示不存在）。
func argsIndex(args []string, flag string) int {
	return slices.Index(args, flag)
}

// argsValue 返回 flag 紧随其后的值（用于 `--flag value` 形态）。
func argsValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	index := argsIndex(args, flag)
	if index < 0 {
		t.Fatalf("argv 缺少 %s：%v", flag, args)
	}
	if index+1 >= len(args) {
		t.Fatalf("%s 后无值：%v", flag, args)
	}
	return args[index+1]
}

// BuildArgs 的基线契约：提示词必须紧跟 -p（CLI 的位置参数），输出格式固定
// stream-json（会话进度靠逐行事件解析，换成 json 会丢掉实时性）。
func TestBuildArgsBaseline(t *testing.T) {
	args := BuildArgs(ArgsOptions{Prompt: "评审这个 PR", Verbose: true, PermissionMode: "auto"})

	if len(args) < 2 || args[0] != "-p" {
		t.Fatalf("argv 应以 -p <提示词> 开头：%v", args)
	}
	if args[1] != "评审这个 PR" {
		t.Errorf("提示词 = %q", args[1])
	}
	if got := argsValue(t, args, "--output-format"); got != OutputFormat {
		t.Errorf("--output-format = %q, want %q", got, OutputFormat)
	}
	if !slices.Contains(args, "--verbose") {
		t.Error("Verbose 为真时必须带 --verbose（stream-json 的前提）")
	}
	if got := argsValue(t, args, "--permission-mode"); got != "auto" {
		t.Errorf("--permission-mode = %q", got)
	}
}

// 可选 flag 的缺席：空值不得产出半个 flag（`--settings ""` 会让 CLI 报错，
// 而字段为空本就是「不传」的意思）。
func TestBuildArgsOmitsEmptyOptions(t *testing.T) {
	args := BuildArgs(ArgsOptions{Prompt: "p"})

	for _, flag := range []string{
		"--verbose", "--permission-mode", "--autocompact", "--strict-mcp-config",
		"--mcp-config", "--settings", "--setting-sources", "--append-system-prompt",
		"--max-turns", "--agents", "--bare", "--session-id", "--resume", "--name", "--model",
	} {
		if argsIndex(args, flag) >= 0 {
			t.Errorf("空配置不应产出 %s：%v", flag, args)
		}
	}
	// 固定项恒在：-p 提示词 + --output-format stream-json（输出格式不是可选项）
	if want := []string{"-p", "p", "--output-format", OutputFormat}; !slices.Equal(args, want) {
		t.Errorf("argv = %v, want %v", args, want)
	}
}

// 每个可选 flag 的取值都原样送达：写错键名或漏掉值都不会有报错，只会静默失效。
func TestBuildArgsCarriesEachFlag(t *testing.T) {
	args := BuildArgs(ArgsOptions{
		Prompt:             "提示",
		Verbose:            true,
		PermissionMode:     "auto",
		Autocompact:        "auto",
		StrictMCP:          true,
		MCPConfigPath:      "/tmp/mcp.json",
		SettingsPath:       "/tmp/settings.json",
		SettingSources:     "project",
		AppendSystemPrompt: "附加协议",
		MaxTurns:           300,
		AgentsJSON:         `{"a":{}}`,
		Bare:               true,
		Session:            Session{ID: "sess-1"},
		Name:               "assistant-review-1",
		Model:              "sonnet",
	})

	for flag, want := range map[string]string{
		"--permission-mode":      "auto",
		"--autocompact":          "auto",
		"--mcp-config":           "/tmp/mcp.json",
		"--settings":             "/tmp/settings.json",
		"--setting-sources":      "project",
		"--append-system-prompt": "附加协议",
		"--max-turns":            "300",
		"--agents":               `{"a":{}}`,
		"--session-id":           "sess-1",
		"--name":                 "assistant-review-1",
		"--model":                "sonnet",
	} {
		if got := argsValue(t, args, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	for _, flag := range []string{"--verbose", "--strict-mcp-config", "--bare"} {
		if !slices.Contains(args, flag) {
			t.Errorf("缺少布尔 flag %s：%v", flag, args)
		}
	}
}

// 会话续接的二选一：新鲜会话用 --session-id，续接用 --resume，**不能同时出现**
// （CLI 只认其一，同时给出行为未定义）。
func TestBuildArgsSessionFlagIsExclusive(t *testing.T) {
	fresh := BuildArgs(ArgsOptions{Prompt: "p", Session: Session{ID: "s1"}})
	if !slices.Contains(fresh, "--session-id") {
		t.Errorf("新鲜会话应带 --session-id：%v", fresh)
	}
	if slices.Contains(fresh, "--resume") {
		t.Errorf("新鲜会话不应带 --resume：%v", fresh)
	}

	resumed := BuildArgs(ArgsOptions{Prompt: "p", Session: Session{ID: "s1", Resume: true}})
	if !slices.Contains(resumed, "--resume") {
		t.Errorf("续接应带 --resume：%v", resumed)
	}
	if slices.Contains(resumed, "--session-id") {
		t.Errorf("续接不应带 --session-id：%v", resumed)
	}
}

// 容器形态的 argv：Bin 换 docker、前置 run 与挂载、claude 的 argv 追加在镜像与
// 容器内路径之后。挂载漏一个，容器里的 MCP/记录就会另起一份。
func TestBuildSpecContainer(t *testing.T) {
	spec := BuildSpec(ArgsOptions{
		Prompt:   "评审",
		MaxTurns: 300,
		Container: &Container{
			Name:           "assistant-review-1",
			Cwd:            "/work/checkout",
			Image:          "ghcr.io/example/assistant:latest",
			MountDirs:      []string{"/work/checkout", "/state/claude"},
			AssistantBin:   "/usr/local/bin/assistant",
			ConfigDir:      "/state/claude",
			ProjectDirName: "assistant-acme-video",
			Network:        "host",
			PassthroughEnv: []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
		},
	}, "/usr/local/bin/claude", "/work/checkout", []string{"CLAUDE_CONFIG_DIR=/state/claude"}, 0)

	if spec.Bin != "docker" {
		t.Errorf("容器形态 Bin = %q, want docker", spec.Bin)
	}
	if spec.Container != "assistant-review-1" {
		t.Errorf("Spec.Container = %q（超时兜底要按名 kill）", spec.Container)
	}
	args := spec.Args
	for _, want := range []string{"run", "--rm", "-i"} {
		if !slices.Contains(args, want) {
			t.Errorf("argv 缺少 %s：%v", want, args)
		}
	}
	if got := argsValue(t, args, "--name"); got != "assistant-review-1" {
		t.Errorf("--name = %q", got)
	}
	if got := argsValue(t, args, "-w"); got != "/work/checkout" {
		t.Errorf("-w = %q", got)
	}
	for _, mount := range []string{
		"/work/checkout:/work/checkout",
		"/state/claude:/state/claude",
		"/usr/local/bin/assistant:/usr/local/bin/assistant:ro",
	} {
		if !slices.Contains(args, mount) {
			t.Errorf("argv 缺少挂载 %s：%v", mount, args)
		}
	}
	for _, injected := range []string{
		"CLAUDE_CONFIG_DIR=/state/claude",
		"CLAUDE_CODE_PROJECT_DIR_NAME=assistant-acme-video",
	} {
		if !slices.Contains(args, injected) {
			t.Errorf("argv 缺少容器内注入 %s：%v", injected, args)
		}
	}
	if got := argsValue(t, args, "--network"); got != "host" {
		t.Errorf("--network = %q", got)
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if !slices.Contains(args, key) {
			t.Errorf("argv 缺少透传键 %s：%v", key, args)
		}
	}
	// 镜像与容器内 claude 路径之后才是 claude 的 argv
	imageIndex := slices.Index(args, "ghcr.io/example/assistant:latest")
	if imageIndex < 0 {
		t.Fatalf("argv 缺少镜像：%v", args)
	}
	if imageIndex+1 >= len(args) || args[imageIndex+1] != "/usr/local/bin/claude" {
		t.Errorf("镜像后应为容器内 claude 路径：%v", args[imageIndex:])
	}
	if argsIndex(args, "-p") <= imageIndex {
		t.Errorf("claude 的 argv 应在镜像与路径之后：%v", args)
	}
}

// 非容器形态不引入 docker：Bin 原样、argv 就是 claude 的参数。
func TestBuildSpecNonContainer(t *testing.T) {
	spec := BuildSpec(ArgsOptions{Prompt: "p"}, "/usr/bin/claude", "/work", nil, 0)
	if spec.Bin != "/usr/bin/claude" {
		t.Errorf("Bin = %q", spec.Bin)
	}
	if spec.Container != "" {
		t.Errorf("非容器形态 Container 应为空：%q", spec.Container)
	}
	if slices.Contains(spec.Args, "run") {
		t.Errorf("非容器形态不应出现 docker 子命令：%v", spec.Args)
	}
	if spec.Args[0] != "-p" {
		t.Errorf("argv 应以 -p 开头：%v", spec.Args)
	}
}

// AssistantBin 非绝对路径时忽略只读挂载：`docker -v relative:relative:ro` 会被
// docker 当成命名卷，静默造出一个空卷而不是报错。
func TestBuildSpecContainerIgnoresRelativeAssistantBin(t *testing.T) {
	spec := BuildSpec(ArgsOptions{
		Prompt: "p",
		Container: &Container{
			Name: "c", Cwd: "/w", Image: "img",
			AssistantBin: "assistant",
		},
	}, "claude", "/w", nil, 0)
	for _, arg := range spec.Args {
		if strings.Contains(arg, "assistant:") {
			t.Errorf("相对路径不应产生挂载：%q", arg)
		}
	}
}

// SessionEnv 的顺序与语义：配置根在前、项目目录名仅在 PinProjectDir 时注入。
// 评审会话必须钉项目目录名（记录不能随 worktree 漂移），对话会话反过来必须不钉
// （否则用户 `claude --continue` 接不上）。
func TestSessionEnvPinning(t *testing.T) {
	pinned := SessionEnv(EnvConfig{
		ConfigDir:      "/cfg",
		ProjectDirName: "assistant-acme-video",
		PinProjectDir:  true,
	})
	if !slices.Equal(pinned, []string{
		"CLAUDE_CONFIG_DIR=/cfg",
		"CLAUDE_CODE_PROJECT_DIR_NAME=assistant-acme-video",
	}) {
		t.Errorf("评审会话环境 = %v", pinned)
	}

	unpinned := SessionEnv(EnvConfig{
		ConfigDir:      "/cfg",
		ProjectDirName: "assistant-acme-video",
		PinProjectDir:  false,
	})
	if !slices.Equal(unpinned, []string{"CLAUDE_CONFIG_DIR=/cfg"}) {
		t.Errorf("对话会话不应钉项目目录名：%v", unpinned)
	}
}

// SessionEnv 丢弃空项：注入空值比不注入更糟（CLI 会当作显式配置的空值）。
func TestSessionEnvDropsEmptyEntries(t *testing.T) {
	env := SessionEnv(EnvConfig{
		ConfigDir:      "   ",
		ProjectDirName: "proj",
		PinProjectDir:  true,
		Credentials: []EnvVar{
			{Key: "GITEA_HOST", Value: "https://gitea.example.com"},
			{Key: "GITEA_ACCESS_TOKEN", Value: ""},
			{Key: "  ", Value: "x"},
		},
		Extra: []EnvVar{{Key: "ASSISTANT_CONFIG", Value: "  "}},
	})
	if !slices.Equal(env, []string{"GITEA_HOST=https://gitea.example.com"}) {
		t.Errorf("环境 = %v，空配置根/空值/空键都应被丢弃", env)
	}
}

// 凭据排在额外项之前：凭据是身份（决定评审归属），额外项是补充，顺序让日志更可读。
func TestSessionEnvOrdering(t *testing.T) {
	env := SessionEnv(EnvConfig{
		ConfigDir:      "/cfg",
		PinProjectDir:  true,
		ProjectDirName: "p",
		Credentials:    []EnvVar{{Key: "GITEA_ACCESS_TOKEN", Value: "t"}},
		Extra:          []EnvVar{{Key: "EXTRA", Value: "e"}},
	})
	want := []string{
		"CLAUDE_CONFIG_DIR=/cfg",
		"CLAUDE_CODE_PROJECT_DIR_NAME=p",
		"GITEA_ACCESS_TOKEN=t",
		"EXTRA=e",
	}
	if !slices.Equal(env, want) {
		t.Errorf("环境 = %v, want %v", env, want)
	}
}

// EnvKeys 给容器形态取透传键名（值由 docker 从宿主继承）。
func TestEnvKeys(t *testing.T) {
	keys := EnvKeys([]string{"A=1", "B=2", "C=", "=bad"})
	if !slices.Equal(keys, []string{"A", "B", "C"}) {
		t.Errorf("EnvKeys = %v, want [A B C]（空键丢弃）", keys)
	}
	if got := EnvKeys(nil); len(got) != 0 {
		t.Errorf("EnvKeys(nil) = %v", got)
	}
}

// EnvValue 的取值与缺失判定。
func TestEnvValue(t *testing.T) {
	env := []string{"A=1", "B="}
	if value, ok := EnvValue(env, "A"); !ok || value != "1" {
		t.Errorf("EnvValue(A) = %q/%v", value, ok)
	}
	if value, ok := EnvValue(env, "B"); !ok || value != "" {
		t.Errorf("EnvValue(B) = %q/%v（空值仍存在）", value, ok)
	}
	if _, ok := EnvValue(env, "C"); ok {
		t.Error("EnvValue(C) 应为不存在")
	}
	// 值里含 = 时只切第一个等号
	if value, _ := EnvValue([]string{"URL=https://x?a=b"}, "URL"); value != "https://x?a=b" {
		t.Errorf("含 = 的值被截断：%q", value)
	}
}

// BuildArgs 与旧 dispatcher.sessionCommand 的**标志集**等价（顺序不同，见下）。
//
// 迁移到本包时重新组织了 flag 顺序（--verbose 提到前面、--settings 与其同类相邻），
// 内容一字不差：不多不少。CLI 按名字解析选项、与顺序无关（已用真实 CLI 验证
// `--output-format json --verbose -p …` 可正常出结果），故不把顺序当契约——但把
// 「标志集」钉住：漏一个 flag 是静默失效，写错键名更是不会有报错。
func TestBuildArgsFlagSetMatchesLegacyDispatcher(t *testing.T) {
	got := BuildArgs(ArgsOptions{
		Prompt: "review pr #1", PermissionMode: "auto", Autocompact: "auto",
		Verbose: true, StrictMCP: true, MCPConfigPath: "/tmp/mcp.json",
		SettingSources: "project", MaxTurns: 300, Session: Session{ID: "s-1"},
		Name: "assistant-review-1", SettingsPath: "/tmp/settings.json",
		AppendSystemPrompt: "协议", Model: "sonnet",
	})
	// 旧实现的标志集（撇开顺序与取值）
	wantFlags := []string{
		"-p", "--permission-mode", "--autocompact", "--output-format", "--verbose",
		"--strict-mcp-config", "--mcp-config", "--setting-sources", "--max-turns",
		"--session-id", "--name", "--settings", "--append-system-prompt", "--model",
	}
	seen := map[string]bool{}
	for _, arg := range got {
		if strings.HasPrefix(arg, "-") {
			seen[arg] = true
		}
	}
	for _, flag := range wantFlags {
		if !seen[flag] {
			t.Errorf("缺少 flag %s（漏一个就是静默失效）：%v", flag, got)
		}
	}
	// 反向：不引入旧实现没有的 flag（--bare/--agents 等只在显式要求时出现）
	if len(seen) != len(wantFlags) {
		t.Errorf("flag 数不符：%v", got)
	}
}
