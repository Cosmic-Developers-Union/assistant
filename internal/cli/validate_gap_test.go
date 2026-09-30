package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	builtinagents "github.com/Cosmic-Developers-Union/assistant/internal/agents"
	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
)

// emptyCredentialStore 把凭据库指向一个不存在的临时文件。credentials.Path() 把
// 空串与未设置一视同仁，都会回落到平台标准配置目录，而开发机的真实
// ~/.config/.../credentials.json 里通常四类令牌俱全——不隔离的话，「缺令牌」
// 分支会被真实凭据库静默填满，断言看着通过其实什么都没测。
func emptyCredentialStore(t *testing.T) {
	t.Helper()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(t.TempDir(), "credentials.json"))
}

// writeValidateConfig 把一份调好的 map 落成 config.json（与 validateFixture 不同，
// 它只管配置本体，不碰凭据库，便于精确控制凭据态）。
func writeValidateConfig(t *testing.T, config map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	document, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(document, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// findingsFor 把 findings 摊平成 "STATUS subject — detail" 行，便于按状态筛选断言。
func findingsFor(findings []validateFinding, status string) []string {
	lines := make([]string, 0, len(findings))
	for _, finding := range findings {
		if finding.status == status {
			lines = append(lines, finding.subject+" — "+finding.detail)
		}
	}
	return lines
}

// TestValidateConfigFileReportsDisabledChannel 断言 enabled=false 的通道报 WARN
// 而不是 ERROR：保留定义但不启动是合法状态（多站点分批上线时常见），把它当错误
// 会让 validate 在健康的配置上以退出码 1 结束，操作者于是学会忽略它。
func TestValidateConfigFileReportsDisabledChannel(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	config := map[string]any{
		"providers": map[string]any{"minimax": map[string]any{"api_key": "sk-cp-test"}},
		"channels": []map[string]any{{
			"type": "telegram", "host": "telegram", "enabled": false, "bot_token": "tg-token",
		}},
	}
	findings := validateConfigFile(writeValidateConfig(t, config), mustLoadInstance(t, config))

	warns := findingsFor(findings, "WARN")
	found := false
	for _, line := range warns {
		if strings.Contains(line, "channels.telegram") && strings.Contains(line, "enabled=false") {
			found = true
		}
	}
	if !found {
		t.Errorf("停用通道应报 WARN 且说明保留定义：%v", warns)
	}
	for _, line := range findingsFor(findings, "ERROR") {
		if strings.Contains(line, "channels.telegram") {
			t.Errorf("停用通道不该计入 ERROR：%s", line)
		}
	}
}

// TestValidateConfigFileChannelCredentialProblems 逐平台钉住「缺什么就报什么」：
// 每种通道的必填字段不同（weixin/telegram 要 bot_token、qq 要 app_id+app_secret），
// 报错文案里带上获取途径，操作者才知道去哪儿补。这条断言覆盖 validate.go 里
// 通道凭据的缺失出口：密钥缺省在凭据库（按通道键索引），所以 config.json 里
// 没写不算配置错误。validate 真正要报的是「两处都没有」以及补凭据的命令。
//
// 夹具用 telegram 通道承载 provider 引用，子测试才能只改被测的那一个通道而
// 不触发无关的「provider 未被引用」提示。
func TestValidateConfigFileChannelCredentialProblems(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	cases := []struct {
		name    string
		channel map[string]any
		want    string
	}{
		{"weixin 缺密钥", map[string]any{"type": "weixin", "name": "wx"}, "通道 weixin/wx 缺凭据"},
		{"qq 缺 app_id", map[string]any{"type": "qq", "name": "q1", "app_secret": "s"}, "通道 qq/q1 缺 app_id 或 app_secret"},
		{"qq 缺 app_secret", map[string]any{"type": "qq", "name": "q2", "app_id": "102"}, "通道 qq/q2 缺 app_id 或 app_secret"},
		{"telegram 缺密钥", map[string]any{"type": "telegram", "name": "tg"}, "通道 telegram/tg 缺凭据"},
		{"未知 type", map[string]any{"type": "signal", "name": "sig"}, "未知 type（应为 weixin/qq/telegram/gitea）"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// 这些通道都通不过 instances.File.Validate（就是被测的那条缺失），所以
			// 只能手工构造 File：validate 是配置的最后一道拦截，它必须比加载器更宽
			// ——加载器放行的文件 validate 一定放行，被加载器拦下的文件也必须给出
			// 同一句人话解释，否则「配置无效」之外操作者读不到任何定位信息。
			config := map[string]any{
				"default_provider": "minimax",
				"providers":        map[string]any{"minimax": map[string]any{"api_key": "sk-cp-test"}},
				"channels": []map[string]any{
					// 合法 telegram 通道承担 provider 引用，让 findings 里不出现
					// 「provider 未被引用」这类会干扰计数的行。
					{"type": "telegram", "name": "tg-ok", "bot_token": "tg-token"},
					testCase.channel,
				},
			}
			file := &instances.File{
				DefaultProvider: "minimax",
				Providers:       map[string]instances.Provider{"minimax": {APIKey: "sk-cp-test"}},
				Channels: []instances.Channel{
					{Type: instances.ChannelTelegram, Name: "tg-ok", BotToken: "tg-token"},
					channelFixture(testCase.channel),
				},
			}
			findings := validateConfigFile(writeValidateConfig(t, config), file)
			errors := findingsFor(findings, "ERROR")
			matches := 0
			for _, line := range errors {
				if strings.Contains(line, testCase.want) {
					matches++
				}
			}
			if matches != 1 {
				t.Errorf("want 恰好一条含 %q 的 ERROR，got %v", testCase.want, errors)
			}
		})
	}
}

// TestValidateConfigFileGiteaChannelTokenFallback 断言 gitea 通道没写 token 时不是
// 错误、而是说明回退到凭据库 purpose=review：token 缺省是设计好的退化路径
// （setup 把令牌写进凭据库），把它当缺失会让每个正常站点都报错。
func TestValidateConfigFileGiteaChannelTokenFallback(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	config := map[string]any{
		"providers": map[string]any{"minimax": map[string]any{"api_key": "sk-cp-test"}},
		"channels": []map[string]any{{
			"type": "gitea", "host": "https://gitea.example.com",
			"repos": []map[string]any{{"name": "acme/video"}},
		}},
	}
	findings := validateConfigFile(writeValidateConfig(t, config), mustLoadInstance(t, config))
	oks := findingsFor(findings, "OK")
	found := false
	for _, line := range oks {
		if strings.Contains(line, "channels.gitea") && strings.Contains(line, "token 缺省回退凭据库") &&
			strings.Contains(line, "reviewer=ai merger=merge") {
			found = true
		}
	}
	if !found {
		t.Errorf("gitea 通道应说明 token 回退且报 reviewer/merger：%v", oks)
	}
}

// TestValidateConfigFileWarnsRepoDirProblems 断言 repo.dir 的两级告警：不是目录、
// 是目录但不是 git 检出分别报不同的 WARN。dir 是评审 fetch/worktree 的前提，
// 路径挂错（容器部署最常见的坑）必须被看见，且不该阻断启动——所以是 WARN。
func TestValidateConfigFileWarnsRepoDirProblems(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	notDirectory := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(notDirectory, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	noGit := t.TempDir()
	config := map[string]any{
		"providers": map[string]any{"minimax": map[string]any{"api_key": "sk-cp-test"}},
		"channels": []map[string]any{{
			"type": "gitea", "host": "https://gitea.example.com",
			"repos": []map[string]any{
				{"name": "acme/notdir", "dir": notDirectory},
				{"name": "acme/nogit", "dir": noGit},
			},
		}},
	}
	findings := validateConfigFile(writeValidateConfig(t, config), mustLoadInstance(t, config))
	warns := strings.Join(findingsFor(findings, "WARN"), "\n")
	if !strings.Contains(warns, "在本地不是目录") {
		t.Errorf("指向普通文件应报「不是目录」：\n%s", warns)
	}
	if !strings.Contains(warns, "不是 git 检出") {
		t.Errorf("目录里没有 .git 应报「不是 git 检出」：\n%s", warns)
	}
}

// TestValidateConfigFileSchemaResolution 断言 $schema 的相对/绝对/URL 三种解析：
// 相对路径按 config.json 自身位置解析（编辑器也这么解析），存在报 OK、不存在报
// WARN 并提示 config init 会写一份；URL 一律 OK（validate 不联网）。
func TestValidateConfigFileSchemaResolution(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	base := map[string]any{
		"providers": map[string]any{"minimax": map[string]any{"api_key": "sk-cp-test"}},
		"instances": []map[string]any{{
			"host": "https://gitea.example.com", "repos": []map[string]any{{"name": "acme/video"}},
		}},
	}

	t.Run("URL 直接通过", func(t *testing.T) {
		config := cloneConfig(base)
		config["$schema"] = "https://example.com/config.schema.json"
		findings := validateConfigFile(writeValidateConfig(t, config), mustLoadInstance(t, config))
		if !strings.Contains(strings.Join(findingsFor(findings, "OK"), "\n"), "$schema — https://example.com/config.schema.json") {
			t.Errorf("远程 schema 应报 OK：%v", findingsFor(findings, "OK"))
		}
	})

	t.Run("相对路径存在则通过", func(t *testing.T) {
		config := cloneConfig(base)
		config["$schema"] = "schema.json"
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		document, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, document, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "schema.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		findings := validateConfigFile(path, mustLoadInstanceFile(t, path))
		oks := strings.Join(findingsFor(findings, "OK"), "\n")
		if !strings.Contains(oks, "$schema — schema.json（编辑器补全与悬停文档）") {
			t.Errorf("存在的相对 schema 应报 OK：\n%s", oks)
		}
	})

	t.Run("相对路径缺失则告警", func(t *testing.T) {
		config := cloneConfig(base)
		config["$schema"] = "missing.json"
		findings := validateConfigFile(writeValidateConfig(t, config), mustLoadInstance(t, config))
		warns := strings.Join(findingsFor(findings, "WARN"), "\n")
		if !strings.Contains(warns, "$schema — missing.json 指向的 schema 文件不存在") ||
			!strings.Contains(warns, "assistant config init 会写一份") {
			t.Errorf("缺失的相对 schema 应报 WARN 并给出补救：\n%s", warns)
		}
	})
}

// TestValidateConfigFileNormalizationNotesBecomeWarns 断言 File.Notes() 的规范化
// 提示被转成 WARN。notes 是「配置能跑但被静默改写」的唯一出口（如 instances 迁移
// 成 channels），不报出来等于悄悄改了操作者的配置——与「配置显式化」红线冲突。
func TestValidateConfigFileNormalizationNotesBecomeWarns(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	config := map[string]any{
		"providers": map[string]any{"minimax": map[string]any{"api_key": "sk-cp-test"}},
		"instances": []map[string]any{{
			"host": "https://gitea.example.com", "repos": []map[string]any{{"name": "acme/video"}},
		}},
	}
	file := mustLoadInstance(t, config)
	notes := file.Notes()
	if len(notes) == 0 {
		t.Fatal("夹具失效：instances 应产生规范化 note")
	}
	findings := validateConfigFile(writeValidateConfig(t, config), file)
	warns := strings.Join(findingsFor(findings, "WARN"), "\n")
	if !strings.Contains(warns, "配置规范化") {
		t.Errorf("规范化提示应转成 WARN：\n%s", warns)
	}
	for _, note := range notes {
		if !strings.Contains(warns, note) {
			t.Errorf("输出缺少 note %q：\n%s", note, warns)
		}
	}
}

// TestValidateConfigFileNoRuntimesIsSkipNotError 断言没有 runtime 时报 SKIP：
// 纯通道部署（只接消息不跑评审）是合法形态，报 ERROR 会让这类部署无法通过 validate。
func TestValidateConfigFileNoRuntimesIsSkipNotError(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	config := map[string]any{
		"providers": map[string]any{"minimax": map[string]any{"api_key": "sk-cp-test"}},
		"channels": []map[string]any{{
			"type": "telegram", "host": "telegram", "bot_token": "tg-token",
		}},
	}
	findings := validateConfigFile(writeValidateConfig(t, config), mustLoadInstance(t, config))
	if lines := findingsFor(findings, "SKIP"); len(lines) != 1 || !strings.Contains(lines[0], "runtimes") {
		t.Errorf("无 runtime 应恰好报一条 SKIP runtimes，got %v", lines)
	}
	if strings.Contains(strings.Join(findingsFor(findings, "ERROR"), "\n"), "runtimes") {
		t.Error("无 runtime 不该计入 ERROR")
	}
}

// TestValidateConfigFileRuntimeDefaultsToBuiltinSubagents 断言 runtime 缺省填充：
// main_agent 空则用缺省名、subagents 为 nil 则展开内置子代理清单。validate 打印的
// 是「实际会跑什么」，缺省若不展开，操作者看到的详情与实际行为不符。
func TestValidateConfigFileRuntimeDefaultsToBuiltinSubagents(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	config := map[string]any{
		"providers": map[string]any{"minimax": map[string]any{"api_key": "sk-cp-test"}},
		"channels": []map[string]any{{
			// 合法 telegram 通道承担 provider 引用（引用链：default_provider →
			// minimax），runtime 才能把它列进 channels 而不同时报错。
			"type": "telegram", "name": "tg", "bot_token": "tg-token",
		}},
		"runtimes": map[string]any{
			"bare": map[string]any{"root": t.TempDir(), "channels": []string{"telegram/tg"}},
		},
	}
	file := mustLoadInstance(t, config)
	findings := validateConfigFile(writeValidateConfig(t, config), file)
	oks := strings.Join(findingsFor(findings, "OK"), "\n")
	if !strings.Contains(oks, "runtimes.bare") || !strings.Contains(oks, "main agent="+instances.DefaultMainAgent) {
		t.Errorf("缺省 main agent 应展开成 %s：\n%s", instances.DefaultMainAgent, oks)
	}
	if !strings.Contains(oks, "子代理=") || strings.Contains(oks, "子代理=；") {
		t.Errorf("缺省 subagents 应展开成内置清单：\n%s", oks)
	}
	for _, name := range builtinagents.SubagentNames() {
		if !strings.Contains(oks, name) {
			t.Errorf("内置子代理 %s 应出现在 runtime 详情里：\n%s", name, oks)
		}
	}
}

// TestValidateCredentialsAllPurposesIsClean 断言凭据齐备（mcp/admin/review/merge
// 四类用途都在）时结论是 OK 且把令牌名逐一列出：凭据检查是操作者判断「还要不要
// 跑 setup」的唯一依据，把齐备误报成缺令牌会让人反复重跑派生流程，把缺令牌误报
// 成齐备又会让 daemon 在第一次评审时才暴露问题。
func TestValidateCredentialsAllPurposesIsClean(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	dir := t.TempDir()
	storePath := filepath.Join(dir, "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", storePath)
	host := "https://gitea.example.com"
	credentialsFile := map[string]any{
		"version":  1,
		"identity": []map[string]any{{"host": host, "user": "developer", "is_admin": true}},
		"credentials": []map[string]any{
			{"host": host, "user": "bot-mcp", "purpose": "mcp", "token": "t-mcp"},
			{"host": host, "user": "bot-admin", "purpose": "admin", "token": "t-admin"},
			{"host": host, "user": "bot-review", "purpose": "review", "token": "t-review"},
			{"host": host, "user": "bot-merge", "purpose": "merge", "token": "t-merge"},
		},
	}
	encoded, err := json.Marshal(credentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	// 用显式 gitea 通道（不用遗留 instances 节）：只有通道自带 repos 才要求
	// merge 令牌，而遗留 instances 会被迁移成通道并留下一条规范化备注，既干扰
	// WARN 断言也偏离「操作者按当前格式写的配置」这个真实场景。
	config := map[string]any{
		"default_provider": "minimax",
		"providers":        map[string]any{"minimax": map[string]any{"api_key": "sk-cp-test"}},
		"channels": []map[string]any{{
			"type": "gitea", "host": host,
			"repos": []map[string]any{{"name": "acme/video"}},
		}},
	}
	path := writeValidateConfig(t, config)
	findings := validateConfigFile(path, mustLoadInstanceFile(t, path))

	// 四类用途都在：一条 ERROR 都不该有，结论是 OK 且列出全部令牌名。凭据齐备
	// 却仍报缺令牌（或反过来把齐备当缺）都会让操作者白跑 setup。
	ok := false
	for _, line := range findingsFor(findings, "OK") {
		if strings.Contains(line, "credentials "+host) &&
			strings.Contains(line, "身份 developer（管理员）") &&
			strings.Contains(line, "令牌：mcp/admin/review/merge") {
			ok = true
		}
	}
	if !ok {
		t.Errorf("四类令牌齐备应报 OK 并列出令牌：%v", findingsFor(findings, "OK"))
	}
	for _, line := range findingsFor(findings, "ERROR") {
		if strings.Contains(line, "credentials "+host) {
			t.Errorf("令牌齐备时不该报 ERROR：%s", line)
		}
	}
	if got := findingsFor(findings, "WARN"); len(got) != 0 {
		t.Errorf("令牌齐备时不该有 WARN：%v", got)
	}
}

// TestValidateCredentialsReportsMissingIdentityAndStoreErrors 断言凭据库的两种
// 硬失败：没有登录身份（只登录过别处）、凭据文件解析不了。两者都必须是 ERROR——
// 凭据库坏了却不报，daemon 起来后会在第一次评审时才暴露。
func TestValidateCredentialsReportsMissingIdentityAndStoreErrors(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	config := map[string]any{
		"providers": map[string]any{"minimax": map[string]any{"api_key": "sk-cp-test"}},
		"instances": []map[string]any{{
			"host": "https://gitea.example.com", "repos": []map[string]any{{"name": "acme/video"}},
		}},
	}
	path := writeValidateConfig(t, config)

	t.Run("没有登录身份", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
		findings := validateConfigFile(path, mustLoadInstanceFile(t, path))
		errors := strings.Join(findingsFor(findings, "ERROR"), "\n")
		if !strings.Contains(errors, "没有登录身份") || !strings.Contains(errors, "assistant login add https://gitea.example.com") {
			t.Errorf("缺身份应报 ERROR 并给出 login add 提示：\n%s", errors)
		}
	})

	t.Run("凭据文件解析失败", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("{ not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		findings := validateConfigFile(path, mustLoadInstanceFile(t, path))
		errors := strings.Join(findingsFor(findings, "ERROR"), "\n")
		if !strings.Contains(errors, "credentials.json — 读取失败") {
			t.Errorf("坏凭据库应报 ERROR：\n%s", errors)
		}
	})
}

// TestValidateProviderDefinitionAndMissingCredentialHints 断言 provider 三项：
// 定义但被引用→OK（带凭据来源与引用方）、被引用但解析不了→ERROR（带获罪于谁）、
// 引用了未定义的 provider→ERROR。resolve 的失败原因必须归到正确的引用方，否则
// 操作者会在几十个仓库里猜是哪一个写错了。
func TestValidateProviderDefinitionAndMissingCredentialHints(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	emptyCredentialStore(t)
	t.Run("provider 解析不了并点名引用方", func(t *testing.T) {
		// 被引用的 provider 解析失败时，报错必须同时给出「解析不了的原因」与「谁
		// 引用的」：配置里可能有几十个仓库各带 provider，只说「有个 provider 坏了」
		// 等于什么都没说。这里用 openai 预设故意不给 ANTHROPIC_BASE_URL——该类供应商
		// 没有 Anthropic 兼容端点，缺少翻译代理地址时 Resolve 必须报错（见
		// provider/openai.go 的 RequiresBaseURL），这是最常见的真实误配。
		config := map[string]any{
			"default_provider": "openai",
			"providers": map[string]any{
				"openai": map[string]any{"api_key": "sk-proj-test"},
			},
			"channels": []map[string]any{{
				"type": "telegram", "name": "tg", "bot_token": "tg-token",
			}},
		}
		// 手工构造 File：File.Validate 的 reference 闭包会在加载期先报「default_provider:
		// 需要 ANTHROPIC_BASE_URL」并让 Load 失败，用加载路径拿不到 *File*；而
		// validate.go 要的正是「已装载」的文件，故这里直接构造运行期形态。
		file := &instances.File{
			DefaultProvider: "openai",
			Providers:       map[string]instances.Provider{"openai": {APIKey: "sk-proj-test"}},
			Channels: []instances.Channel{
				{Type: instances.ChannelTelegram, Name: "tg", BotToken: "tg-token"},
			},
		}
		findings := validateConfigFile(writeValidateConfig(t, config), file)
		errors := strings.Join(findingsFor(findings, "ERROR"), "\n")
		if !strings.Contains(errors, "providers.openai") {
			t.Errorf("解析失败应以 providers.openai 起行：\n%s", errors)
		}
		if !strings.Contains(errors, "被 default_provider 引用") {
			t.Errorf("解析失败必须点名引用方 default_provider：\n%s", errors)
		}
		if !strings.Contains(errors, "需要 ANTHROPIC_BASE_URL") {
			t.Errorf("应转述 Resolve 的原始原因（缺 base_url）：\n%s", errors)
		}
	})

	t.Run("provider 无凭据", func(t *testing.T) {
		config := map[string]any{
			"default_provider": "minimax",
			"providers":        map[string]any{"minimax": map[string]any{}},
			"channels": []map[string]any{{
				"type": "telegram", "name": "tg", "bot_token": "tg-token",
			}},
		}
		// 手工构造 File：File.Validate 的 provider 校验要求有凭据（api_key/
		// auth_token 至少一项），「定义了但没凭据」的配置同样进不了加载路径。
		file := &instances.File{
			DefaultProvider: "minimax",
			Providers:       map[string]instances.Provider{"minimax": {}},
			Channels: []instances.Channel{
				{Type: instances.ChannelTelegram, Name: "tg", BotToken: "tg-token"},
			},
		}
		findings := validateConfigFile(writeValidateConfig(t, config), file)
		errors := strings.Join(findingsFor(findings, "ERROR"), "\n")
		if !strings.Contains(errors, "providers.minimax") || !strings.Contains(errors, "没有 api_key/auth_token") {
			t.Errorf("无凭据的 provider 应报 ERROR：\n%s", errors)
		}
	})

	t.Run("无 provider 但进程环境有凭据", func(t *testing.T) {
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "env-token")
		config := map[string]any{
			"channels": []map[string]any{{
				"type": "telegram", "name": "tg", "bot_token": "tg-token",
			}},
		}
		findings := validateConfigFile(writeValidateConfig(t, config), mustLoadInstance(t, config))
		oks := strings.Join(findingsFor(findings, "OK"), "\n")
		if !strings.Contains(oks, "providers — 没有选中 provider，使用进程环境凭据") {
			t.Errorf("进程环境有凭据应报 OK：\n%s", oks)
		}
	})
}

// TestValidateProviderDirectorySkipsNoiseAndSorts 断言遗留 providers/ 目录的读取
// 规则：只认 .json 普通文件，跳过子目录与点文件，且名字排序后输出。目录里常混着
// 编辑器备份与 .DS_Store，报成「provider 定义」会让操作者去找不存在的定义。
func TestValidateProviderDirectorySkipsNoiseAndSorts(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	config := map[string]any{
		"providers": map[string]any{"minimax": map[string]any{"api_key": "sk-cp-test"}},
		"channels": []map[string]any{{
			"type": "telegram", "host": "telegram", "bot_token": "tg-token",
		}},
	}
	path := writeValidateConfig(t, config)
	directory := filepath.Join(filepath.Dir(path), "providers")
	if err := os.MkdirAll(filepath.Join(directory, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"zhipu.json", "minimax.json", ".hidden.json", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	findings := validateProviderDirectory(path)
	if len(findings) != 1 {
		t.Fatalf("want 一条结论，got %+v", findings)
	}
	if findings[0].status != "ERROR" {
		t.Errorf("status = %s, want ERROR", findings[0].status)
	}
	// 只报 zhipu、minimax，且按名字排序；点文件、非 .json、子目录都不算。
	if !strings.HasPrefix(findings[0].detail, "minimax、zhipu 已不再被读取") {
		t.Errorf("detail = %q, want 排序后的 minimax、zhipu", findings[0].detail)
	}
	if strings.Contains(findings[0].detail, "hidden") || strings.Contains(findings[0].detail, "notes") ||
		strings.Contains(findings[0].detail, "nested") {
		t.Errorf("噪声文件不该被当成 provider 定义：%q", findings[0].detail)
	}

	t.Run("目录不存在时静默", func(t *testing.T) {
		if findings := validateProviderDirectory(filepath.Join(t.TempDir(), "config.json")); findings != nil {
			t.Errorf("没有 providers/ 目录时应返回 nil，got %+v", findings)
		}
	})
}

// TestReportValidateWrittenWording 断言 reportValidate 在「刚写入」语境下的措辞：
// written 非空表示配置是本命令写下的，报错要说清「已写入但有 N 处问题」——否则
// 操作者会以为写入失败而重跑，把配置覆盖回去。
func TestReportValidateWrittenWording(t *testing.T) {
	stdout := &bytes.Buffer{}
	err := reportValidate(stdout, []validateFinding{
		{"ERROR", "providers.x", "没有 api_key"},
		{"WARN", "repo acme/video", "不是 git 检出"},
	}, "/etc/assistant/config.json", "/etc/assistant/config.json")
	if err == nil {
		t.Fatal("有 ERROR 时应返回错误")
	}
	if !strings.Contains(err.Error(), "配置已写入 /etc/assistant/config.json，但校验发现 1 处问题") {
		t.Errorf("err = %v, want 说明已写入且报了问题数", err)
	}

	// WARN 不阻断：有提示时措辞里要带上提示条数。
	stdout.Reset()
	if err := reportValidate(stdout, []validateFinding{{"WARN", "providers.x", "未引用"}}, "", "/etc/assistant/config.json"); err != nil {
		t.Fatalf("只有 WARN 不该报错：%v", err)
	}
	if !strings.Contains(stdout.String(), "（另有 1 条提示，不阻断启动）") {
		t.Errorf("WARN 条数应写进汇总：\n%s", stdout.String())
	}
}

// TestPrintValidateFindingOmitsEmptyDetail 断言无详情时不打印分隔符「—」：输出里
// 悬着一个破折号会让操作者以为后面本该有话。
func TestPrintValidateFindingOmitsEmptyDetail(t *testing.T) {
	stdout := &bytes.Buffer{}
	printValidateFinding(stdout, validateFinding{status: "SKIP", subject: "runtimes"})
	if got := stdout.String(); got != "SKIP      runtimes\n" {
		t.Errorf("无详情输出 = %q, want 无 — 分隔符", got)
	}

	stdout.Reset()
	printValidateFinding(stdout, validateFinding{status: "OK", subject: "providers.x", detail: "内置预设"})
	if got := stdout.String(); got != "OK        providers.x — 内置预设\n" {
		t.Errorf("有详情输出 = %q, want 含 — 分隔符", got)
	}
}

// TestProviderSummaryIncludesPresetEndpointModelCounts 断言 provider 摘要把预设、
// 端点、模型与三类覆盖计数都写出来。摘要是操作者确认「跑的到底是哪个端点/模型」
// 的唯一地方——不打印值本身（密钥不能进日志），但端点与模型必须可见。
func TestProviderSummaryIncludesPresetEndpointModelCounts(t *testing.T) {
	overrides := providerOverridesForTest(t)
	summary := providerSummary("minimax", overrides)
	for _, want := range []string{"内置预设", "端点 https://api.example.com", "模型 test-model", "env ", "/ settings ", "/ mcp "} {
		if !strings.Contains(summary, want) {
			t.Errorf("摘要缺少 %q：%s", want, summary)
		}
	}
}

// TestSortedKeysAndRuntimeKeysSortDeterministically 断言两个排序辅助函数的确定性：
// 打印顺序不稳会让 diff 抖动、也让「同一份配置两次输出不一致」看起来像出了问题。
func TestSortedKeysAndRuntimeKeysSortDeterministically(t *testing.T) {
	if got := sortedKeys(map[string]string{"zeta": "z", "alpha": "a", "mid": "m"}); strings.Join(got, ",") != "alpha,mid,zeta" {
		t.Errorf("sortedKeys = %v, want alpha,mid,zeta", got)
	}
	if got := sortedKeys(nil); got == nil || len(got) != 0 {
		t.Errorf("sortedKeys(nil) = %v, want 空切片", got)
	}
	names := sortedRuntimeKeys(map[string]instances.Runtime{
		"zeta":  {Root: "/tmp/zeta"},
		"alpha": {Root: "/tmp/alpha"},
	})
	if strings.Join(names, ",") != "alpha,zeta" {
		t.Errorf("sortedRuntimeKeys = %v, want alpha,zeta", names)
	}
}

// TestValidateReportsNoConfigFile 断言找不到 config.json 时的错误给出定位顺序与
// 下一步命令：validate 的第一件事是「改哪个文件」，报一句「配置不存在」帮不上忙。
//
// 只在「没有任何定位线索」时才走这条分支：给了 --config / ASSISTANT_CONFIG 而
// 文件不存在是一种更具体的错误（由加载器报出并带上绝对路径），这里断言的是
// 三处都空、连相对路径都想不出来的那种茫然——它必须告诉操作者去看哪三个位置。
func TestValidateReportsNoConfigFile(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	// 空串（而非某个路径）：--config 与 ASSISTANT_CONFIG 都等于没给。
	t.Setenv("ASSISTANT_CONFIG", "")
	// TestMain 已把 cwd 切到临时目录，那里没有 config.json，于是 file == nil。
	stdout := &bytes.Buffer{}
	err := runValidate(stdout, "")
	if err == nil {
		t.Fatal("配置不存在时应报错")
	}
	for _, want := range []string{"没有 config.json", "--config / ASSISTANT_CONFIG / 当前目录", "assistant login add"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, 缺少 %q", err, want)
		}
	}
}

// TestNewValidateCommandRunsAndReportsNoArgs 驱动 cobra 层：validate 不接受位置
// 参数，且 RunE 把 stdout 接到命令输出上（reportValidate 的汇总必须出现在这里）。
func TestNewValidateCommandRunsAndReportsNoArgs(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	config := map[string]any{
		"default_provider": "minimax",
		"providers":        map[string]any{"minimax": map[string]any{"api_key": "sk-cp-test"}},
		"instances": []map[string]any{{
			"host": "https://gitea.example.com", "repos": []map[string]any{{"name": "acme/video"}},
		}},
	}
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	path := writeValidateConfig(t, config)
	credentialsFile := map[string]any{
		"version":  1,
		"identity": []map[string]any{{"host": "https://gitea.example.com", "user": "developer"}},
		"credentials": []map[string]any{
			{"host": "https://gitea.example.com", "user": "bot", "purpose": "review", "token": "t"},
			{"host": "https://gitea.example.com", "user": "bot", "purpose": "merge", "token": "t"},
			{"host": "https://gitea.example.com", "user": "bot", "purpose": "admin", "token": "t"},
			{"host": "https://gitea.example.com", "user": "bot", "purpose": "mcp", "token": "t"},
		},
	}
	encoded, err := json.Marshal(credentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	configFlag := path
	command := newValidateCommand(&configFlag)
	stdout := &bytes.Buffer{}
	command.SetOut(stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"extra-arg"})
	if err := command.Execute(); err == nil {
		t.Error("多余的位置参数应被拒绝")
	}

	command = newValidateCommand(&configFlag)
	stdout.Reset()
	command.SetOut(stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs(nil)
	if err := command.Execute(); err != nil {
		t.Fatalf("execute: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout.String(), "校验通过") {
		t.Errorf("汇总应写到命令输出：\n%s", stdout.String())
	}
}

// cloneConfig 深拷贝一份 map（子测试会往里塞 $schema，别互相串味）。
func cloneConfig(source map[string]any) map[string]any {
	encoded, err := json.Marshal(source)
	if err != nil {
		panic(err)
	}
	clone := map[string]any{}
	if err := json.Unmarshal(encoded, &clone); err != nil {
		panic(err)
	}
	return clone
}

// mustLoadInstance 把一份 map 落盘后按真实加载路径读回来：validateConfigFile 拿到
// 的 File 必须与 daemon 启动时读到的一致（含规范化 notes），手工构造 File 会漏掉
// 迁移逻辑。
func mustLoadInstance(t *testing.T, config map[string]any) *instances.File {
	t.Helper()
	return mustLoadInstanceFile(t, writeValidateConfig(t, config))
}

// mustLoadInstanceFile 从指定路径读回配置，读不进来即终止测试——夹具坏了继续跑
// 只会得到误导性的结论。
func mustLoadInstanceFile(t *testing.T, path string) *instances.File {
	t.Helper()
	file, err := instances.Load(path)
	if err != nil {
		t.Fatalf("加载夹具配置 %s：%v", path, err)
	}
	if file == nil {
		t.Fatalf("夹具配置 %s 加载为 nil", path)
	}
	return file
}

// channelFixture 把测试用的 map 字面量转成 instances.Channel：需要 channels 字段
// 通不过 instances.File.Validate 的用例（缺字段的正是被测分支）没法走真实加载
// 路径，只能手工构造，这里集中一处免得每个子测试各写一份。
func channelFixture(config map[string]any) instances.Channel {
	text := func(key string) string {
		value, _ := config[key].(string)
		return value
	}
	return instances.Channel{
		Type:      text("type"),
		Name:      text("name"),
		BotToken:  text("bot_token"),
		AppID:     text("app_id"),
		AppSecret: text("app_secret"),
	}
}

// providerOverridesForTest 造一份三项都非空的覆盖，用于断言 providerSummary 的
// 端点/模型/计数三项都被打印。
func providerOverridesForTest(t *testing.T) claudecfg.Overrides {
	t.Helper()
	return claudecfg.Overrides{
		Env: map[string]string{
			"ANTHROPIC_BASE_URL": "https://api.example.com",
			"ANTHROPIC_MODEL":    "test-model",
		},
		Settings: map[string]any{"permissions": map[string]any{}},
		MCP:      map[string]any{"gitea": map[string]any{}},
	}
}
