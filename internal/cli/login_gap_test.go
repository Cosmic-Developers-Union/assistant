package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/spf13/cobra"
)

// minimalConfig 是一份「有内容但没有平台」的合法配置：Validate 只要求
// runtimes/channels/instances 至少有一项非空，因此一个 runtime 就够。
//
// 它专门用来构造「配置在、平台条目不在」这一形态——这正是登录态只剩凭据库时
// 的真实样子，list 必须能退回凭据库视角回答。
func minimalConfig() *instances.File {
	return &instances.File{Runtimes: map[string]instances.Runtime{"main": {MainAgent: "main"}}}
}

// seedLoginGapStore 把一份凭据库写到 ASSISTANT_CREDENTIALS 指向的位置。
// list/describe 的行为完全由这份文件决定，因此各用例只描述内容、不关心落盘细节。
func seedLoginGapStore(t *testing.T, store *credentials.File) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	if err := credentials.Save(path, store); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDescribeIdentityAndPurposesValueSpace 覆盖身份与用途描述的全部取值：
// none（从未登录）、@user（普通账号）、@user(admin)（实例管理员）、以及凭据为
// 空时用途描述同样是 none。
//
// 这两串文本是 assistant login list 的对外输出，操作者据此判断「这台机器上
// 到底登过哪些站点、有没有管理员权限」。把 none 写成空串、或漏掉 admin 标记，
// 会让人误以为站点已登录/已有管理能力，进而跳过 login add 直接跑 setup。
func TestDescribeIdentityAndPurposesValueSpace(t *testing.T) {
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: "https://admin.example.com", User: "ge", IsAdmin: true})
	store.SetIdentity(credentials.Identity{Host: "https://plain.example.com", User: "dev"})
	store.SetCredential(credentials.Credential{
		Host: "https://plain.example.com", User: "dev", Purpose: credentials.PurposeMCP, Token: "t",
	})
	store.SetCredential(credentials.Credential{
		Host: "https://plain.example.com", User: "dev", Purpose: credentials.PurposeAdmin, Token: "t2",
	})

	cases := []struct {
		name       string
		host       string
		identity   string
		purposes   string
		whyNotNone bool
	}{
		{name: "从未登录", host: "https://never.example.com", identity: "none", purposes: "none"},
		{name: "普通账号", host: "https://plain.example.com", identity: "@dev", purposes: "mcp@dev,admin@dev", whyNotNone: true},
		{name: "管理员", host: "https://admin.example.com", identity: "@ge(admin)", purposes: "none"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := describeIdentity(store, testCase.host); got != testCase.identity {
				t.Errorf("describeIdentity(%s) = %q, want %q", testCase.host, got, testCase.identity)
			}
			if got := describePurposes(store, testCase.host); got != testCase.purposes {
				t.Errorf("describePurposes(%s) = %q, want %q", testCase.host, got, testCase.purposes)
			}
		})
	}

	// 「none」必须是显式字符串而不是空串：空串在 list 输出里就是一个空字段，
	// 操作者分不清「没有这个站点」和「有这个站点但没有凭据」。
	if got := describeIdentity(&credentials.File{}, "https://x.example.com"); got == "" {
		t.Error("无身份时应输出 none 而不是空串")
	}
	if got := describePurposes(&credentials.File{}, "https://x.example.com"); got == "" {
		t.Error("无凭据时应输出 none 而不是空串")
	}
}

// TestLoginListWithoutConfigReportsCredentialStore 断言「配置里没有平台但凭据库
// 里有」这一形态的 list 输出：逐站点打印 repos=0、身份与用途，并给出凭据库路径。
//
// 这是「换机器/删了 config.json 但凭据还在」的真实场景，也是操作者找回登录态
// 的唯一线索；缺了凭据库路径就无从知道该备份或清理哪个文件。
func TestLoginListWithoutConfigReportsCredentialStore(t *testing.T) {
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: "https://only.example.com", User: "dev", IsAdmin: true})
	store.SetCredential(credentials.Credential{
		Host: "https://only.example.com", User: "dev", Purpose: credentials.PurposeMCP, Token: "t",
	})
	storePath := seedLoginGapStore(t, store)

	// 配置里没有 channels、但有 runtimes：这就是「配置还在、平台条目被删了」
	// 的形态，list 该退回凭据库视角而不是报「配置为空」。
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, minimalConfig()); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	list := newLoginListCommand(&configPath, nil)
	list.SetOut(&out)
	list.SetErr(&out)
	if err := list.Execute(); err != nil {
		t.Fatalf("login list error = %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"https://only.example.com", "repos=0", "identity=@dev(admin)", "mcp@dev", "凭据库：" + storePath,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("输出缺少 %q：\n%s", want, text)
		}
	}
}

// TestLoginListEmptyStoreSaysNothingRegistered 断言凭据库为空且配置里没有平台时，
// list 给出「未登记任何平台」这句可行动提示，而不是打印一片空白。
//
// 空输出会被当成命令没跑成功；而这句话直接告诉操作者下一步该敲什么。
func TestLoginListEmptyStoreSaysNothingRegistered(t *testing.T) {
	seedLoginGapStore(t, &credentials.File{})

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, minimalConfig()); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	list := newLoginListCommand(&configPath, nil)
	list.SetOut(&out)
	list.SetErr(&out)
	if err := list.Execute(); err != nil {
		t.Fatalf("login list error = %v", err)
	}
	if !strings.Contains(out.String(), "未登记任何平台") {
		t.Errorf("空凭据库应提示未登记平台：\n%s", out.String())
	}
}

// TestLoginListReportsCorruptCredentialStore 断言凭据库损坏时 list 不中断：向
// stderr 告警，并把配置里已登记的平台原样列出来（身份与用途全为 none）。
//
// 只看凭据库的只读命令在凭据库损坏时也必须能回答「配置里登记了哪些站点」——
// 这正是要修它的时候最需要的信息；直接报错退出会让操作者连该修哪个站点都看不到。
func TestLoginListReportsCorruptCredentialStore(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", storePath)
	if err := os.WriteFile(storePath, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(dir, "config.json")
	if err := instances.Save(configPath, &instances.File{Instances: []instances.Instance{{
		Host:     "https://corrupt.example.com",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
	}}}); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	list := newLoginListCommand(&configPath, nil)
	list.SetOut(&out)
	list.SetErr(&errOut)
	if err := list.Execute(); err != nil {
		t.Fatalf("凭据库损坏不该让 list 失败：%v", err)
	}
	if !strings.Contains(errOut.String(), "警告：读取凭据库失败") {
		t.Errorf("应在 stderr 告警：\n%s", errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, "https://corrupt.example.com") {
		t.Errorf("配置里的平台仍应列出：\n%s", text)
	}
	if !strings.Contains(text, "identity=none") || !strings.Contains(text, "tokens=none") {
		t.Errorf("凭据不可用时身份与用途应为 none：\n%s", text)
	}
}

// TestResolveLoginHostInfersSoleConfiguredInstance 断言不给 --host/位置参数时，
// 唯一的已登记站点被自动采用，且去掉结尾斜杠。
//
// 这是「一台机器只登一个站点」这个最常见形态下的免参数体验；推断不出来的话
// 操作者会被要求手敲地址，而结尾斜杠不去掉会让凭据库的 (host) 唯一键错位。
func TestResolveLoginHostInfersSoleConfiguredInstance(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := instances.Save(configPath, &instances.File{Instances: []instances.Instance{{
		Host:     "https://solo.example.com/",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
	}}}); err != nil {
		t.Fatal(err)
	}
	// 配置经 ASSISTANT_CONFIG 定位：真实调用方不会记着传 --config，单站点推断
	// 必须在这条默认入口上生效。
	t.Setenv("ASSISTANT_CONFIG", configPath)

	host, err := resolveLoginHost(t.Context(), "", "", "")
	if err != nil {
		t.Fatalf("唯一站点应被推断：%v", err)
	}
	if host != "https://solo.example.com" {
		t.Errorf("host = %q, want 去掉结尾斜杠的 https://solo.example.com", host)
	}

	// 显式地址优先于配置：argHost 同样要去掉结尾斜杠
	host, err = resolveLoginHost(t.Context(), "", "https://explicit.example.com/", "")
	if err != nil {
		t.Fatal(err)
	}
	if host != "https://explicit.example.com" {
		t.Errorf("显式地址 = %q, want 去斜杠后原样返回", host)
	}

	// 位置参数同样走 argHost：两个入口（位置参数 / --host）都必须能定地址
	host, err = resolveLoginHost(t.Context(), "", "https://positional.example.com", "--host")
	if err != nil {
		t.Fatal(err)
	}
	if host != "https://positional.example.com" {
		t.Errorf("位置参数 = %q, want 优先于 --host", host)
	}

	// 两个站点：不允许猜，且当前目录不是 git 检出、没有 Gitea remote 可探测
	ambiguous := filepath.Join(dir, "ambiguous.json")
	if err := instances.Save(ambiguous, &instances.File{Instances: []instances.Instance{
		{Host: "https://a.example.com", Reviewer: instances.Account{Name: "ai"}, Merger: instances.Account{Name: "merge"}},
		{Host: "https://b.example.com", Reviewer: instances.Account{Name: "ai"}, Merger: instances.Account{Name: "merge"}},
	}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CONFIG", ambiguous)
	t.Chdir(dir)
	if _, err := resolveLoginHost(t.Context(), ambiguous, "", ""); err == nil {
		t.Error("多站点且无法探测时应报「请指定平台地址」而不是猜一个")
	}
}

// TestRunLoginPromptsForMissingHostAndRejectsEmpty 断言交互式补齐站点地址：
// 配置推断不出来时走一次询问，回答为空则报「缺少站点地址」而不是拿空串继续。
//
// 空 host 继续往下走会往凭据库写一条 host="" 的记录，之后所有按 host 的查询
// 都命中不了它——必须在入口拦住。
func TestRunLoginPromptsForMissingHostAndRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	configPath := filepath.Join(dir, "config.json")

	var out bytes.Buffer
	command := newLoginAddCommand(&configPath, nil)
	command.SetOut(&out)
	command.SetErr(&out)
	// 显式注入 stdin：interactive() 为真，询问走 prompts.line 而不是拒绝等待
	command.SetIn(strings.NewReader("\n"))
	command.SetArgs([]string{"--user", "dev"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "缺少站点地址") {
		t.Fatalf("空回答应报缺少站点地址：%v", err)
	}
	if !strings.Contains(out.String(), "Gitea 站点地址") {
		t.Errorf("应先询问站点地址：\n%s", out.String())
	}
}

// TestLoginAddRequiresPasswordFlagInNonInteractive 断言非交互环境（stdin 就是
// 终端的替身，调用方没有注入输入源，也没有 --password/--password-stdin）时，
// 登录不挂起等待，而是报出可行动的「请用 --password-stdin」。
//
// CI / stdio MCP 里 stdin 不会有人喂数据，一旦在这里阻塞，整个进程就永久卡死；
// 报错是唯一安全的行为。
//
// 站点地址走 --host 而不是位置参数：ConfigPath 非空时它同时是「配置落点」，
// 平台地址另有 --host 一途，两条入口必须都通。
func TestLoginAddRequiresPasswordFlagInNonInteractive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	configPath := filepath.Join(dir, "config.json")

	_, server := newLoginServer(t, "developer", false)

	var out bytes.Buffer
	command := newLoginAddCommand(&configPath, nil)
	command.SetOut(&out)
	command.SetErr(&out)
	// 不调用 SetIn：InOrStdin() 回落到 os.Stdin，非终端 → 非交互
	command.SetArgs([]string{"--host", server.URL, "--user", "developer"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "--password-stdin") {
		t.Fatalf("非交互环境应提示用 --password-stdin：%v", err)
	}
}

// TestReadIdentityPasswordSources 覆盖密码来源的优先级与三条错误路径：
// --password 直用、空 --password-stdin 源报「读取到的密码为空」、超过 64KiB 的
// 输入源报「无法读取密码或密码输入过长」、注入输入源下按行读取并拒绝空值。
//
// 密码是派生令牌的唯一输入；来源判定错了要么把密码当空串发给站点、要么在
// 非交互环境里读一个永不结束的流。64KiB 上限与空值检查是防「误把文件/dev/stdin
// 整段当密码」的两道闸。
//
// --password-stdin 的分支读的是「替换掉 stdin 的那个源」而非 os.Stdin——测试
// 进程自己跑在某个 stdin 下，去读真 stdin 会让用例行为随执行环境变化（本地
// 终端 vs CI 管道）。这里统一用 SetIn 注入确定性的源；真 stdin 的空源形态由
// 登录端到端用例覆盖。
func TestReadIdentityPasswordSources(t *testing.T) {
	// 每次用例用同一条命令对象承载输入源与提示：--password-stdin 分支读的是
	// command.InOrStdin()，注入的源必须挂在被传入的那条命令上。
	newSession := func(t *testing.T, input string) (*cobra.Command, *promptSession, *bytes.Buffer) {
		t.Helper()
		command := newLoginAddCommand(new(string), nil)
		var errOut bytes.Buffer
		command.SetErr(&errOut)
		command.SetIn(strings.NewReader(input))
		return command, newPromptSession(command), &errOut
	}

	t.Run("--password 优先", func(t *testing.T) {
		// 同时给了 --password 与 --password-stdin：前者优先，不该去读输入流
		command, prompts, _ := newSession(t, "from-stdin\n")
		got, err := readIdentityPassword(command, prompts, "h", "u",
			&passwordSource{Password: " from-flag ", PasswordStdin: true})
		if err != nil || got != " from-flag " {
			t.Fatalf("(got, err) = (%q, %v)，--password 应原样采用", got, err)
		}
	})

	t.Run("空 stdin 源报密码为空", func(t *testing.T) {
		// --password-stdin 的输入源为空：必须在入口拦住，而不是把空串当密码发出去
		command, prompts, _ := newSession(t, "")
		_, err := readIdentityPassword(command, prompts, "h", "u",
			&passwordSource{PasswordStdin: true})
		if err == nil || !strings.Contains(err.Error(), "读取到的密码为空") {
			t.Fatalf("err = %v, want 读取到的密码为空", err)
		}
	})

	t.Run("超长 stdin 源被拒", func(t *testing.T) {
		// 误把 /dev/zero、整段文件接进 stdin 时，读取量会越过 64KiB 闸门；
		// 不拦住就会把一大段垃圾当密码去派生令牌。
		command, prompts, _ := newSession(t, strings.Repeat("x", 64*1024+16))
		_, err := readIdentityPassword(command, prompts, "h", "u",
			&passwordSource{PasswordStdin: true})
		if err == nil || !strings.Contains(err.Error(), "无法读取密码或密码输入过长") {
			t.Fatalf("err = %v, want 无法读取密码或密码输入过长", err)
		}
	})

	t.Run("注入输入源按行读取", func(t *testing.T) {
		command, prompts, errOut := newSession(t, "typed-secret\n")
		got, err := readIdentityPassword(command, prompts, "h", "u", &passwordSource{})
		if err != nil || got != "typed-secret" {
			t.Fatalf("(got, err) = (%q, %v)", got, err)
		}
		if !strings.Contains(errOut.String(), "请输入") {
			t.Errorf("应向用户显示提示：\n%s", errOut.String())
		}
	})

	t.Run("按行读到空串报密码为空", func(t *testing.T) {
		command, prompts, _ := newSession(t, "\n")
		_, err := readIdentityPassword(command, prompts, "h", "u", &passwordSource{})
		if err == nil || !strings.Contains(err.Error(), "密码为空") {
			t.Fatalf("err = %v, want 密码为空", err)
		}
	})
}

// TestLoginAddRejectsEmptyPasswordFromStdinSource 断言 --password-stdin 的输入源
// 读不到任何字节时报出可行动的「读取到的密码为空」，而不是把空密码拿去做认证。
//
// 这是 CI 里最常见的误用：忘了把密码接到 stdin，命令会静默地向站点发一次空
// 密码认证；明确报错才能让人发现是管道漏了。
func TestLoginAddRejectsEmptyPasswordFromStdinSource(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	configPath := filepath.Join(dir, "config.json")

	_, server := newLoginServer(t, "developer", false)

	command := newLoginAddCommand(&configPath, nil)
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader(""))
	command.SetArgs([]string{"--host", server.URL, "--user", "developer", "--password-stdin"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "读取到的密码为空") {
		t.Fatalf("空输入源应报密码为空：%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "credentials.json")); statErr == nil {
		t.Error("密码读不到时不该写入凭据库")
	}
}

// TestPromptSessionLineAndSecretTrimRules 断言两处输入读取的换行/空格处理差异：
// line 去首尾空格（地址、账号里的空白是误输入），secret 只去行尾换行（密码里的
// 空格可能是有效字符）。
//
// 把密码按 line 处理会静默改掉用户密码导致认证失败；把地址按 secret 处理则会把
// 尾随空格带进 host，让凭据库的 (host) 唯一键错位。
func TestPromptSessionLineAndSecretTrimRules(t *testing.T) {
	command := newLoginAddCommand(new(string), nil)
	command.SetIn(strings.NewReader("  padded value  \nraw secret  \n"))
	var errOut bytes.Buffer
	command.SetErr(&errOut)
	prompts := newPromptSession(command)

	line, err := prompts.line("账号：")
	if err != nil {
		t.Fatal(err)
	}
	if line != "padded value" {
		t.Errorf("line = %q, want 去掉首尾空格", line)
	}
	secret, err := prompts.secret("密码：", "请用 --password-stdin 提供密码")
	if err != nil {
		t.Fatal(err)
	}
	if secret != "raw secret  " {
		t.Errorf("secret = %q, want 只去行尾换行", secret)
	}
	for _, want := range []string{"账号：", "密码："} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr 缺少提示 %q：\n%s", want, errOut.String())
		}
	}
}

// TestPromptSessionLineReportsReadFailure 断言读取失败（输入源已耗尽且没有任何
// 内容）时如实报错，而不是把空串当作用户的回答继续往下走。
//
// 空串会让后续所有「缺参数」判定都误以为用户已经给了值。
func TestPromptSessionLineReportsReadFailure(t *testing.T) {
	command := newLoginAddCommand(new(string), nil)
	command.SetIn(strings.NewReader(""))
	command.SetErr(&bytes.Buffer{})
	if _, err := newPromptSession(command).line("账号："); err == nil {
		t.Error("输入源耗尽应报读取输入失败")
	}
	if _, err := newPromptSession(command).secret("密码：", "请用 --password-stdin 提供密码"); err == nil {
		t.Error("输入源耗尽应报读取输入失败")
	}
}

// TestLoginAddWithoutUserInNonInteractive 断言非交互环境缺 --user 时先报「缺少
// 账号」而不是先要密码：账号是后续所有查询的键，缺它连该向谁认证都不知道。
//
// 错误顺序错了会让操作者在补了密码之后才发现还要补账号，白跑一轮站点请求。
func TestLoginAddWithoutUserInNonInteractive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	configPath := filepath.Join(dir, "config.json")

	_, server := newLoginServer(t, "developer", false)

	command := newLoginAddCommand(&configPath, nil)
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"--host", server.URL, "--password", "p"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "缺少账号") {
		t.Fatalf("缺账号应报缺少账号：%v", err)
	}
}

// TestLoginAddRejectsTokenOfAnotherAccount 断言站点上新令牌校验出的身份与请求的
// 账号不一致时，本地不落盘任何凭据，并在错误里点名双方账号与令牌名。
//
// 这是「站点上 token 被别处复用/校验接口串了」的侦测点：一旦把别人的令牌写进
// 本账号条目，之后所有以该账号名义的评审/合并都会以错误的身份执行。
func TestLoginAddRejectsTokenOfAnotherAccount(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	configPath := filepath.Join(dir, "config.json")

	// 站点用账号 developer 的密码发令牌，但 /user 回的是另一个 login
	state, server := newLoginServer(t, "developer", false)
	state.login = "someone-else"

	command := newLoginAddCommand(&configPath, nil)
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader(" s3cret \n"))
	command.SetArgs([]string{"--host", server.URL, "--user", "developer", "--password-stdin"})
	err := command.Execute()
	if err == nil {
		t.Fatal("身份不一致应报错")
	}
	for _, want := range []string{"someone-else", "未保存"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误缺少 %q：%v", want, err)
		}
	}
	store, loadErr := credentials.Load(filepath.Join(dir, "credentials.json"))
	if loadErr != nil && !os.IsNotExist(loadErr) {
		t.Fatalf("凭据库应未被写入或为空：%v", loadErr)
	}
	if store != nil {
		if _, ok := store.CredentialForUser(server.URL, "developer", credentials.PurposeMCP); ok {
			t.Error("身份不一致时不该写入 mcp 凭据")
		}
	}
	if _, statErr := os.Stat(configPath); statErr == nil {
		t.Error("身份不一致时不该写入 config.json")
	}
}

// TestRunTokenRefreshRejectsEmptyAccountStore 断言凭据库里该站点一个账号都没有时，
// refresh 报出「先用 login add」的可行动错误，且不发任何站点请求。
//
// 没有记录就无从知道该用哪个账号的密码，静默向站点发一次无凭据请求只会暴露
// 一个无从解读的 401。
func TestRunTokenRefreshRejectsEmptyAccountStore(t *testing.T) {
	seedLoginGapStore(t, &credentials.File{})

	command := newLoginTokenRefreshCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader("pw\n"))
	command.SetArgs([]string{"https://gitea.example.com", "mcp", "--password-stdin"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "先 assistant login add") {
		t.Fatalf("空凭据库应提示先登录：%v", err)
	}
}
