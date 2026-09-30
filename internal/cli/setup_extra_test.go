package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
)

// runSetupCommand 在隔离环境里执行一次 setup 命令：凭据库与配置都落在临时目录，
// 绝不碰真实用户配置目录。返回 stdout 与命令错误，供各用例断言。
func runSetupCommand(t *testing.T, configPath string, args ...string) (string, error) {
	t.Helper()
	command := newSetupCommand(&configPath)
	out := &bytes.Buffer{}
	command.SetOut(out)
	command.SetErr(out)
	command.SetArgs(args)
	err := command.Execute()
	return out.String(), err
}

// saveSetupConfig 写出一份只含单个站点的配置：runSetup 的「唯一站点自动选中」与
// 「已有仓库清单回退」都依赖它已登记该站点。
func saveSetupConfig(t *testing.T, configPath, host string, repos ...instances.Repo) {
	t.Helper()
	file := &instances.File{
		Providers: map[string]instances.Provider{"anthropic": {}},
		Channels: []instances.Channel{{
			Type:     instances.ChannelGitea,
			Host:     host,
			Provider: "anthropic",
			Repos:    repos,
		}},
	}
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}
}

// grantSetupAdmin 写入一条 admin 用途令牌，让 runSetup 越过「唯一凭据来源」这道
// 门；它是 setup 得以继续走到真正工作逻辑的前提。
func grantSetupAdmin(t *testing.T, host string) {
	t.Helper()
	writePurposeCredentials(t, []credentials.Credential{{
		Host: host, User: "root", Purpose: credentials.PurposeAdmin, Token: "t-admin",
	}})
}

// TestSetupRejectsAmbiguousChannelsWithoutHost 断言多站点且未指定 --host 时报错：
// 配置里有多个平台时「初始化哪一个」没有唯一答案，必须让人显式决定，否则 setup
// 会把令牌与仓库写到操作者没打算改的站点上。
func TestSetupRejectsAmbiguousChannelsWithoutHost(t *testing.T) {
	isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	saveSetupConfig(t, configPath, "https://a.example.com")
	file, err := instances.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	file.AddGiteaChannel(instances.Channel{
		Type: instances.ChannelGitea, Host: "https://b.example.com", Provider: "anthropic",
	})
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}

	_, runErr := runSetupCommand(t, configPath)
	if runErr == nil || !strings.Contains(runErr.Error(), "配置文件有多个站点，请用 --host 指定") {
		t.Errorf("err = %v, want 含「配置文件有多个站点」", runErr)
	}
}

// TestSetupRejectsMissingHost 断言既没有 --host、配置里也没有 gitea 站点时明确
// 报「缺少站点」：空配置下静默挑一个不存在的站点只会让后面的网络错误更难懂。
func TestSetupRejectsMissingHost(t *testing.T) {
	isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")

	_, runErr := runSetupCommand(t, configPath)
	if runErr == nil || !strings.Contains(runErr.Error(), "缺少站点") {
		t.Errorf("err = %v, want 含「缺少站点」", runErr)
	}
}

// TestSetupRejectsUnregisteredHost 断言 --host 指向的站点不在配置里时，runSetup
// 不会因为找不到通道就停下——它继续走凭据门禁，最终以「该站点缺少 admin 用途令牌」
// 收场并点名站点：这正是操作者需要的那条动作指令（先 login add 这个站点），而不是
// 一句让人猜的「平台没登记」。
func TestSetupRejectsUnregisteredHost(t *testing.T) {
	isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	saveSetupConfig(t, configPath, "https://a.example.com")

	_, runErr := runSetupCommand(t, configPath, "--host", "https://b.example.com")
	if runErr == nil || !strings.Contains(runErr.Error(), "https://b.example.com") {
		t.Errorf("err = %v, want 点名未登记的站点 https://b.example.com", runErr)
	}
	if runErr != nil && !strings.Contains(runErr.Error(), "assistant login add") {
		t.Errorf("err = %v, want 含「assistant login add」", runErr)
	}
}

// TestSetupRejectsNonAdminIdentity 断言本地身份记录显示不是管理员时立即拒绝：
// setup 要建号、发令牌、下发密钥，权威判定虽在服务端，但在这里就挡住能让操作者
// 早一步知道该换个令牌。
func TestSetupRejectsNonAdminIdentity(t *testing.T) {
	isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	saveSetupConfig(t, configPath, "https://a.example.com")

	path, err := credentials.Path()
	if err != nil {
		t.Fatal(err)
	}
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: "https://a.example.com", User: "bob", IsAdmin: false})
	if err := credentials.Save(path, store); err != nil {
		t.Fatal(err)
	}

	_, runErr := runSetupCommand(t, configPath, "--host", "https://a.example.com")
	if runErr == nil || !strings.Contains(runErr.Error(), "不是 https://a.example.com 的实例管理员") {
		t.Errorf("err = %v, want 含「不是 ... 的实例管理员」", runErr)
	}
}

// TestSetupRequiresAdminCredential 断言通过了身份门禁但没有 admin 用途令牌时
// 明确要求先 assistant login add：admin 令牌是 setup 唯一允许的凭据来源，用别的
// 用途令牌去建号会以更晦涩的服务端 403 收场。
func TestSetupRequiresAdminCredential(t *testing.T) {
	isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	saveSetupConfig(t, configPath, "https://a.example.com")

	_, runErr := runSetupCommand(t, configPath, "--host", "https://a.example.com")
	if runErr == nil {
		t.Fatal("期望缺少 admin 令牌时报错，实际没有")
	}
	if !strings.Contains(runErr.Error(), "admin") {
		t.Errorf("err = %v, want 点名缺少 admin 用途令牌", runErr)
	}
}

// TestCleanStringsTrimsAndDropsBlanks 断言 --repos 的清洗口径：逗号列表里常见的
// 空白与空项必须被丢掉，否则空字符串会被当成仓库名传给服务端，得到一次无法定位
// 的 404。
func TestCleanStringsTrimsAndDropsBlanks(t *testing.T) {
	cases := []struct {
		name  string
		input []string
		want  []string
	}{
		{"去空白", []string{"  acme/rocket  "}, []string{"acme/rocket"}},
		{"丢空项", []string{"acme/rocket", "", "   ", "other/repo"}, []string{"acme/rocket", "other/repo"}},
		{"全空得空", []string{"", "  "}, []string{}},
		{"空入空出", nil, []string{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := cleanStrings(testCase.input)
			if len(got) != len(testCase.want) {
				t.Fatalf("cleanStrings(%q) = %q, want %q", testCase.input, got, testCase.want)
			}
			for index := range testCase.want {
				if got[index] != testCase.want[index] {
					t.Errorf("cleanStrings(%q)[%d] = %q, want %q", testCase.input, index, got[index], testCase.want[index])
				}
			}
		})
	}
}

// TestLoadInstanceFileForSetupAllowsExplicitNewPath 断言显式路径不存在是允许的
// （首次创建配置），而不是报错：setup 的常用姿势就是指向一个还没建出来的文件。
func TestLoadInstanceFileForSetupAllowsExplicitNewPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.json")
	gotPath, file, err := loadInstanceFileForSetup(path)
	if err != nil {
		t.Fatalf("显式新路径不应报错：%v", err)
	}
	if gotPath != path {
		t.Errorf("path = %q, want %q", gotPath, path)
	}
	if file != nil {
		t.Errorf("file = %+v, want nil（文件尚不存在）", file)
	}
}

// TestLoadInstanceFileForSetupReadsExistingFile 断言已存在的配置会被真正读进来：
// setup 的增量更新（复用已有仓库、已有凭据）全靠它，读成 nil 会让每次 setup 都
// 表现得像第一次运行。
func TestLoadInstanceFileForSetupReadsExistingFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	saveSetupConfig(t, configPath, "https://a.example.com")

	_, file, err := loadInstanceFileForSetup(configPath)
	if err != nil {
		t.Fatalf("读取已有配置: %v", err)
	}
	if file == nil {
		t.Fatal("file = nil, want 已载入的配置")
	}
	if found, ok := findGiteaChannel(file, "https://a.example.com"); !ok || found == nil {
		t.Errorf("载入的配置里没有 https://a.example.com 通道：%+v", file.Channels)
	}
}
