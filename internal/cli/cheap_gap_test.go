package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"

	"github.com/spf13/cobra"
)

// TestConfigTargetPathPrefersExplicitAndNeverInventsADestination 断言写配置落点的
// 解析口径：显式参数（去空白）压过环境变量，两者都空时退回平台标准位置。
//
// 落点字符串会直接参与「同不同一个文件」的判断，所以带空白的写法（`--config
// " ./config.json "`）必须先规范化：原样使用会与真实文件路径对不上，被当成另一个
// 文件另建一份。而「两者都空」这一支刻意不报错——缺省位置由 DefaultConfigPath
// 决定，首次生成就写在那里；这与 resolveLoginHost 需要「有没有答案」的语义不同。
// 这里把它钉死，是为了让 config new 的幂等（干净目录里跑第一次）有据可依。
func TestConfigTargetPathPrefersExplicitAndNeverInventsADestination(t *testing.T) {
	nativePath, err := instances.DefaultConfigPath()
	if err != nil {
		t.Fatalf("本机应能解析标准配置路径：%v", err)
	}
	cases := []struct {
		name       string
		configPath string
		envPath    string
		want       string
	}{
		{name: "显式参数去空白后优先", configPath: "  /tmp/explicit/config.json  ", envPath: "/tmp/env/config.json", want: "/tmp/explicit/config.json"},
		{name: "无显式参数时用 ASSISTANT_CONFIG", configPath: "", envPath: "  /tmp/env/config.json ", want: "/tmp/env/config.json"},
		{name: "两者皆空时退回平台标准位置", configPath: "   ", envPath: "\t", want: nativePath},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("ASSISTANT_CONFIG", testCase.envPath)
			got, err := configTargetPath(testCase.configPath)
			if err != nil {
				t.Fatalf("解析写配置落点失败：%v", err)
			}
			if got != testCase.want {
				t.Errorf("写配置落点应为 %q，got %q", testCase.want, got)
			}
		})
	}
}

// TestReadAPIKeyFromPipeStopsAtUsageHintOnEmptyInput 断言注入的空输入源会走到「没有
// 读到 api_key」这条带用法示例的错误，而不是别的分支。
//
// readAPIKey 先判断 stdin 是不是终端：是终端才走密码式读取。测试里注入的输入源不是
// *os.File，所以一律走管道分支。空输入必须报错并给出 `echo -n <key>` 的写法——文档
// 里推荐的就是这条命令，错误信息不给出它，操作者就得多翻一次文档。这条断言的价值在于
// 钉住「注入输入源 → 管道分支」这一前提：一旦 stdin 判定改成「看有没有输入源」，
// 本用例就会挂起或误报，从而暴露改动。
func TestReadAPIKeyFromPipeStopsAtUsageHintOnEmptyInput(t *testing.T) {
	for _, stdin := range []string{"", "\n", "   \n"} {
		command := &cobra.Command{}
		command.SetIn(strings.NewReader(stdin))
		_, err := readAPIKey(command)
		if err == nil {
			t.Fatalf("stdin = %q 时应报错", stdin)
		}
		if !strings.Contains(err.Error(), "没有读到 api_key") {
			t.Errorf("stdin = %q：错误应点明没读到 api_key，got %v", stdin, err)
		}
		if !strings.Contains(err.Error(), "echo -n <key>") {
			t.Errorf("stdin = %q：错误应含用法示例，got %v", stdin, err)
		}
	}
}

// TestLoginRemoveRefusesUnremovableSite 断言站点既不在配置里、本地也没有它的凭据时
// remove 明确报错，而不是静默成功。
//
// 静默成功会让操作者以为「已经清干净了」：脚本里跟在 remove 后面的步骤（重新 add、
// 换令牌）会因为旧凭据仍在而表现异常，排查时也找不出线索——退出码 0，输出什么也没说。
// 移除一个不存在的对象必须是一次可见的失败。
func TestLoginRemoveRefusesUnremovableSite(t *testing.T) {
	configPath, _ := isolateSetupRun(t)
	saveSetupConfig(t, configPath, "https://gitea.example.com")

	out, err := runLoginCommand(t, configPath, "remove", "https://nowhere.example.com")
	if err == nil {
		t.Fatalf("移除不在配置里也没有凭据的站点应报错，实际输出：\n%s", out)
	}
	if !strings.Contains(err.Error(), "不在配置中") {
		t.Errorf("错误应说明该站点不在配置中：%v", err)
	}
	// 报错必须发生在写操作之前：配置里的站点条目原封不动。
	file, loadErr := instances.Load(configPath)
	if loadErr != nil {
		t.Fatalf("配置应保持可读：%v", loadErr)
	}
	if len(file.Channels) != 1 {
		t.Errorf("报错时不该改动配置，实际通道：%+v", file.Channels)
	}
}

// TestLoginRemoveScopesCredentialsToRequestedUser 断言 --user 只清除指定账号的本地
// 凭据，同站点其它账号的凭据与站点条目都保留。
//
// 「清除该站点全部账号」与「只清除某个账号」是两种不同的运维动作：轮换某个机器人的
// 令牌时用后者。若 --user 被忽略而清掉全部，另一个正在跑的机器人会在无人察觉的
// 情况下失去凭据，直到下一次调度才发现，中间的空窗期没有任何提示。
func TestLoginRemoveScopesCredentialsToRequestedUser(t *testing.T) {
	configPath, credentialPath := isolateSetupRun(t)
	// 配置里放两个站点：只清凭据、不移除站点时，另一个站点要撑住 Validate 的
	// 「至少一项平台」。若只剩一个站点，remove 会在写回配置时以「配置为空」失败，
	// 测出来的就不是 --user 的作用域了。
	saveHostConfig(t, configPath,
		"https://gitea.example.com", "https://gitlab.example.com")
	writePurposeCredentials(t, []credentials.Credential{
		{Host: "https://gitea.example.com", User: "ai", Purpose: credentials.PurposeReview, Token: "t-review"},
		{Host: "https://gitea.example.com", User: "merge", Purpose: credentials.PurposeMerge, Token: "t-merge"},
	})

	out, err := runLoginCommand(t, configPath, "remove", "https://gitea.example.com", "--user", "ai")
	if err != nil {
		t.Fatalf("remove --user 不该失败：%v\n%s", err, out)
	}

	store, err := credentials.Load(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.CredentialForUser("https://gitea.example.com", "merge", credentials.PurposeMerge); !ok {
		t.Errorf("--user ai 不该动到 merge 的凭据：%+v", store.Credentials)
	}
	if _, ok := store.CredentialForUser("https://gitea.example.com", "ai", credentials.PurposeReview); ok {
		t.Errorf("--user ai 应清掉 ai 的凭据：%+v", store.Credentials)
	}
	// 站点条目仍在配置里（只清凭据，不移除平台），输出也不该说「已移除平台」。
	remaining, err := instances.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Channels) != 1 || remaining.Channels[0].Host != "https://gitlab.example.com" {
		t.Errorf("只清除账号凭据时不该移除站点条目：%+v", remaining.Channels)
	}
	// 只清账号凭据，站点本身仍在：结论行应说「移除」的是凭据，而不是整个平台。
	// 判定「平台是否被移除」看的是配置里还剩几个站点——上面已经断言只剩 gitlab，
	// 这里再钉住输出没有谎称「配置无剩余平台，已删除」。
	if strings.Contains(out, "配置无剩余平台") {
		t.Errorf("只清账号凭据不该报平台被移除：\n%s", out)
	}
}

// TestLoginRemoveWritesBackTrimmedHostAndUser 断言 host 参数与 --user 都会被去空白
// 后再匹配：去掉尾斜杠的 host 写法必须与配置里登记的站点视为同一个。
//
// 操作者从 URL 栏复制来的地址常带尾斜杠（`https://gitea.example.com/`），而配置里
// 存的是不带斜杠的形式。若 remove 不做同样的规范化，同一个站点会被当成「不在配置中」，
// 于是要么报错，要么（更糟）在凭据库里留下一份永远匹配不上的孤儿条目。
func TestLoginRemoveWritesBackTrimmedHostAndUser(t *testing.T) {
	configPath, credentialPath := isolateSetupRun(t)
	saveHostConfig(t, configPath,
		"https://gitea.example.com", "https://gitlab.example.com")
	writePurposeCredentials(t, []credentials.Credential{
		{Host: "https://gitea.example.com", User: "ai", Purpose: credentials.PurposeReview, Token: "t-review"},
	})

	out, err := runLoginCommand(t, configPath, "remove", "  https://gitea.example.com/  ", "--user", " ai ")
	if err != nil {
		t.Fatalf("带空白与尾斜杠的写法应被规范化后接受：%v\n%s", err, out)
	}

	store, err := credentials.Load(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.CredentialForUser("https://gitea.example.com", "ai", credentials.PurposeReview); ok {
		t.Errorf("规范化后应匹配到并清除该凭据：%+v", store.Credentials)
	}
	// 规范化后的 host 必须与配置里的条目算作同一个：带尾斜杠的站点条目被移除，
	// 另一个站点留下（本次只清账号凭据，不移除平台本身）。
	remaining, err := instances.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Channels) != 1 || remaining.Channels[0].Host != "https://gitlab.example.com" {
		t.Errorf("带空白/尾斜杠的 host 应匹配到配置里的条目：%+v", remaining.Channels)
	}
	if !strings.Contains(out, "已移除") {
		t.Errorf("应报告移除结果：\n%s", out)
	}
}

// TestLoginRemoveDeletesConfigWhenNoPlatformLeft 断言站点是配置里最后一个平台时，
// remove 会把配置文件本身删掉，并如实说明。
//
// 留着一份 `{"version":1,"channels":[]}` 的空配置文件比删掉它更有害：它让启动流程
// 认为「已配置」（文件存在），却一个通道都没有，于是既不去读环境变量单实例模式，
// 也调度不出任何实例——系统安静地什么都不做。所以「清空后没有平台」必须是删除 +
// 明说，而不是留下空壳。
func TestLoginRemoveDeletesConfigWhenNoPlatformLeft(t *testing.T) {
	configPath, _ := isolateSetupRun(t)
	// 用 instances 形态而不是 saveSetupConfig：后者带 Providers，而 providers 也算
	// 「平台」，configHasNoPlatform 会为 false，配置文件被回写而不是删除，就测不到
	// 「最后一个平台」这一支了。
	saveHostConfig(t, configPath, "https://gitea.example.com")
	writePurposeCredentials(t, []credentials.Credential{
		{Host: "https://gitea.example.com", User: "ai", Purpose: credentials.PurposeReview, Token: "t-review"},
	})

	out, err := runLoginCommand(t, configPath, "remove", "https://gitea.example.com")
	if err != nil {
		t.Fatalf("移除唯一平台应成功：%v\n%s", err, out)
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Errorf("配置已无剩余平台时应删除配置文件，实际 stat=%v", statErr)
	}
	if !strings.Contains(out, "已删除") {
		t.Errorf("应说明配置文件已删除：\n%s", out)
	}
}

// TestLoginRemoveDeletesCredentialFileWhenStoreEmpties 断言凭据库清空后文件被删除
// （而不是留下一份空 JSON）。
//
// 凭据文件的存在本身会被后续命令当作「本机有登录态」：留一份空库，login list 会说
// 「未登记任何平台」看着没问题，但任何依赖「有没有凭据文件」来判断是否需要引导登录的
// 流程都会被误导。清空即删除，落盘状态才与实际一致。
func TestLoginRemoveDeletesCredentialFileWhenStoreEmpties(t *testing.T) {
	configPath, credentialPath := isolateSetupRun(t)
	saveHostConfig(t, configPath, "https://gitea.example.com")
	writePurposeCredentials(t, []credentials.Credential{
		{Host: "https://gitea.example.com", User: "ai", Purpose: credentials.PurposeReview, Token: "t-review"},
	})

	if _, err := runLoginCommand(t, configPath, "remove", "https://gitea.example.com"); err != nil {
		t.Fatalf("移除唯一站点应成功：%v", err)
	}
	if _, statErr := os.Stat(credentialPath); !os.IsNotExist(statErr) {
		t.Errorf("凭据库清空后应删除文件，实际 stat=%v", statErr)
	}
}

// saveHostConfig 写一份只有 instances（host 为实例键）的配置，并为每个 host 填上
// reviewer/merger——Validate 要求两者非空。instances 是遗留形态，载入时会被迁移成
// gitea 通道：它没有 providers / runtimes / agents，所以「清空后已无平台」的分支
// 只有用这种配置才走得到。
func saveHostConfig(t *testing.T, configPath string, hosts ...string) {
	t.Helper()
	file := &instances.File{}
	for _, host := range hosts {
		file.Instances = append(file.Instances, instances.Instance{
			Host:     host,
			Reviewer: instances.Account{Name: "ai"},
			Merger:   instances.Account{Name: "merge"},
		})
	}
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}
}

// runLoginCommand 在隔离环境里执行一条 login 子命令，返回合并后的输出与命令错误。
// login 组里 list/remove 都不需要网络，所以这里不做站点替身。
func runLoginCommand(t *testing.T, configPath string, args ...string) (string, error) {
	t.Helper()
	command := newLoginCommand(&configPath)
	out := &strings.Builder{}
	command.SetOut(out)
	command.SetErr(out)
	command.SetArgs(args)
	err := command.Execute()
	return out.String(), err
}

// isolateLoginRemove 隔离一条 login remove 用例所需的全部落点：凭据库与配置目录都
// 指向临时目录。绝不触碰真实用户配置目录。
func isolateLoginRemove(t *testing.T) (configPath, credentialPath string) {
	t.Helper()
	return isolateSetupRun(t)
}
