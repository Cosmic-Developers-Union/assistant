package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"

	"github.com/spf13/cobra"
)

// runLoginAdd 跑 `login add ...` 并返回输出与错误。stdin 换成 strings.Reader：
// promptSession 只要发现输入源不是进程标准输入就按交互处理（见 interactive），
// 这让整条交互流程能在测试里跑通，而不用拉一个真终端。
func runLoginAdd(t *testing.T, configPath, stdin string, args ...string) (string, error) {
	t.Helper()
	out := &bytes.Buffer{}
	command := newLoginCommand(&configPath)
	command.SetOut(out)
	command.SetErr(out)
	command.SetIn(strings.NewReader(stdin))
	command.SetContext(t.Context())
	command.SetArgs(append([]string{"add"}, args...))
	err := command.Execute()
	return out.String(), err
}

// fakeTelegramServer 起一个只回 getMe 成功的假 Telegram 端点。
func fakeTelegramServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "result": map[string]any{"id": 777, "username": "cosmic_bot"},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

// 登记通道凭据的主路径：密钥进 credentials.json，config.json 只留结构。
//
// 这条流程有两处容易做错而后果都很隐蔽——密钥写进了 config.json（等于把刚收好
// 的密钥抄回用户手写的文件），或者自检没过也照样写盘（错的凭据会安静地躺到
// 下次重启才炸）。两条都在这里钉住。
func TestLoginAddTelegramWritesStoreAndConfigOnly(t *testing.T) {
	isolateCredentials(t)
	server := fakeTelegramServer(t)
	configPath := writeChannelConfig(t, instances.Channel{
		Type: instances.ChannelTelegram, Name: "work", APIBaseURL: server.URL,
	})

	out, err := runLoginAdd(t, configPath, "999:FAKE-TOKEN\n123,456\n",
		"--type", "telegram", "--name", "work")
	if err != nil {
		t.Fatalf("登记应成功：%v\n%s", err, out)
	}

	// 凭据库：按通道键索引，带身份
	store, err := credentials.Load(mustCredentialsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	credential, ok, err := store.ChannelCredentialFor("telegram/work", credentials.PurposeTelegram)
	if err != nil || !ok {
		t.Fatalf("凭据库没有 telegram/work：%v %v", ok, err)
	}
	if credential.Token != "999:FAKE-TOKEN" || credential.User != "@cosmic_bot" {
		t.Errorf("凭据 = %+v, want token=999:FAKE-TOKEN user=@cosmic_bot", credential)
	}
	if credential.Source != credentials.SourceLogin {
		t.Errorf("source = %q, want login", credential.Source)
	}

	// config.json：只有结构，密钥一个字都不该有
	file, err := instances.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Channels) != 1 {
		t.Fatalf("channels = %+v, want 1 条", file.Channels)
	}
	entry := file.Channels[0]
	if entry.BotToken != "" {
		t.Errorf("config.json 里留下了 bot_token=%q", entry.BotToken)
	}
	if entry.Key() != "telegram/work" {
		t.Errorf("key = %q, want telegram/work", entry.Key())
	}
	if len(entry.AdminUsers) != 2 || entry.AdminUsers[0] != "123" || entry.AdminUsers[1] != "456" {
		t.Errorf("admin_users = %v, want [123 456]（逗号分隔）", entry.AdminUsers)
	}
	// 端点沿用配置里已写的自建地址：登录自检打的就是它
	if entry.APIBaseURL != server.URL {
		t.Errorf("api_base_url = %q, want %q", entry.APIBaseURL, server.URL)
	}

	// 两个落点都要说出来——「凭据写哪了」不能靠猜
	for _, want := range []string{credentialsName, "telegram/work", "@cosmic_bot"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "999:FAKE-TOKEN") {
		t.Errorf("输出回显了密钥原文：\n%s", out)
	}
}

const credentialsName = "credentials.json"

// 自检不过就不写：填错的 token 若进了凭据库，症状要到 daemon 启动拉起通道时
// 才出现，那时操作者早就忘了当初登过什么。
func TestLoginAddTelegramKeepsNothingWhenSelfCheckFails(t *testing.T) {
	isolateCredentials(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Unauthorized"})
	}))
	defer server.Close()
	configPath := writeChannelConfig(t, instances.Channel{
		Type: instances.ChannelTelegram, Name: "work", APIBaseURL: server.URL,
	})

	_, err := runLoginAdd(t, configPath, "bad\n123\n", "--type", "telegram", "--name", "work")
	if err == nil || !strings.Contains(err.Error(), "凭据自检失败") {
		t.Fatalf("自检失败应报错：%v", err)
	}
	store, err := credentials.Load(mustCredentialsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.ChannelCredentialFor("telegram/work", credentials.PurposeTelegram); ok {
		t.Error("自检失败却仍写入了凭据")
	}
}

// 空白名单在 telegram 上等于全拒：写出一 nobody 能对话的通道，症状要等到真有人
// 发消息没回音才发现。这里在登记时就拦下。
func TestLoginAddTelegramRejectsEmptyAdminUsers(t *testing.T) {
	isolateCredentials(t)
	server := fakeTelegramServer(t)
	configPath := writeChannelConfig(t, instances.Channel{
		Type: instances.ChannelTelegram, Name: "work", APIBaseURL: server.URL,
	})

	_, err := runLoginAdd(t, configPath, "999:FAKE-TOKEN\n\n", "--type", "telegram", "--name", "work")
	if err == nil || !strings.Contains(err.Error(), "admin_users") {
		t.Fatalf("空白名单应报错并说明后果：%v", err)
	}
}

// 非交互环境下通道凭据无从录入。报错要指向「去终端里跑」，而不是像 Gitea 密码
// 那样推荐一个本命令没有的旗标。
func TestLoginAddChannelRefusesNonInteractiveWithRealHint(t *testing.T) {
	isolateCredentials(t)
	configPath := writeChannelConfig(t, instances.Channel{Type: instances.ChannelTelegram})
	out := &bytes.Buffer{}
	command := newLoginCommand(&configPath)
	command.SetOut(out)
	command.SetErr(out)
	command.SetIn(strings.NewReader("token\n123\n"))
	command.SetContext(t.Context())
	// 让 interactive() 判定为非交互：输入源就是进程标准输入，且不是终端
	command.SetIn(os.Stdin)
	command.SetArgs([]string{"add", "--type", "telegram"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "assistant login add --type telegram") {
		t.Fatalf("err = %v, want 指向终端交互的出路", err)
	}
	if strings.Contains(err.Error(), "--password-stdin") {
		t.Errorf("通道登录不该推荐 Gitea 的旗标：%v", err)
	}
}

// 通道类型没有站点地址：<host> 位置参数必须在解析 host 之前就被拒绝，否则会
// 去探测当前目录的 Gitea remote，把一条 Telegram 登记带进 Gitea 流程。
func TestLoginAddChannelRejectsHostPositional(t *testing.T) {
	isolateCredentials(t)
	configPath := writeChannelConfig(t, instances.Channel{Type: instances.ChannelTelegram})
	out, err := runLoginAdd(t, configPath, "", "--type", "telegram", "https://gitea.example.com")
	if err == nil || !strings.Contains(err.Error(), "不接受位置参数") {
		t.Fatalf("err = %v, want 拒绝 <host> 位置参数", err)
	}
	if strings.Contains(out, "登录成功") {
		t.Errorf("不该走完登录流程：%s", out)
	}
}

// rejectForeignFlags 只看用户真传了的、且属于本命令的旗标。从父命令继承的 --type
// 自己不是「外来旗标」——早期版本用 Flags() 遍历，把 --type 判成了非法，整条
// 通道登录在任何真实命令树下都跑不起来。
func TestRejectForeignFlags(t *testing.T) {
	newAdd := func() *cobra.Command {
		configPath := ""
		group := newLoginCommand(&configPath)
		add, _, err := group.Find([]string{"add"})
		if err != nil {
			t.Fatal(err)
		}
		return add
	}

	t.Run("继承的 --type 不算外来旗标", func(t *testing.T) {
		add := newAdd()
		add.SetContext(t.Context())
		add.Flags().Parse([]string{"--type", "telegram", "--name", "work"})
		if err := rejectForeignFlags(add, "telegram", channelLoginFlags); err != nil {
			t.Fatalf("err = %v, want nil（--type 是继承的，不该被拒）", err)
		}
	})

	t.Run("别类型的旗标显式拒绝", func(t *testing.T) {
		add := newAdd()
		add.SetContext(t.Context())
		add.Flags().Parse([]string{"--password-stdin", "--rotate"})
		err := rejectForeignFlags(add, "weixin", channelLoginFlags)
		if err == nil {
			t.Fatal("Gitea 旗标传给 weixin 应报错")
		}
		for _, want := range []string{"--password-stdin", "--rotate", "--name"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want 含 %q（并说明该用什么）", err, want)
			}
		}
	})

	t.Run("没传的旗标不必解释", func(t *testing.T) {
		add := newAdd()
		add.SetContext(t.Context())
		if err := rejectForeignFlags(add, "weixin", channelLoginFlags); err != nil {
			t.Fatalf("未传任何旗标时不该报错：%v", err)
		}
	})
}

func TestResolveLoginType(t *testing.T) {
	for _, want := range []string{instances.ChannelGitea, "weixin", "qq", "telegram"} {
		if got, err := resolveLoginType(strings.ToUpper(want)); err != nil || got != want {
			t.Errorf("resolveLoginType(%q) = %q, %v, want %q", want, got, err, want)
		}
	}
	// 非法值必须列出全部合法取值：用户是从文档或旧命令名猜着敲的
	_, err := resolveLoginType("slack")
	if err == nil {
		t.Fatal("未知类型应报错")
	}
	for _, want := range []string{"gitea", "weixin", "qq", "telegram"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want 列出合法值 %q", err, want)
		}
	}
}

// remove / token 是 Gitea 侧的概念（按 host 与账号操作凭据库），对通道键没有意义。
// 放它们过去的后果不是「报错」而是删错东西或对着不存在的站点发请求。
func TestLoginRemoveAndTokenRejectNonGiteaTypes(t *testing.T) {
	isolateCredentials(t)
	configPath := writeChannelConfig(t, instances.Channel{Type: instances.ChannelTelegram, BotToken: "t"})
	for _, testCase := range []struct {
		name string
		args []string
		want string
	}{
		{"remove", []string{"remove", "telegram", "--type", "telegram"}, "不适用于 remove"},
		{"token list", []string{"token", "list", "--type", "telegram"}, "不适用于 token"},
		{"token show", []string{"token", "show", "x", "--type", "weixin"}, "不适用于 token"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			command := newLoginCommand(&configPath)
			command.SetOut(out)
			command.SetErr(out)
			command.SetIn(strings.NewReader(""))
			command.SetContext(t.Context())
			command.SetArgs(testCase.args)
			err := command.Execute()
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("err = %v, want 含 %q", err, testCase.want)
			}
		})
	}
	// 通道用途也不能混进 login token：那里的动作全是站点侧的
	if _, err := resolveTokenPurpose([]string{"host", credentials.PurposeTelegram}); err == nil {
		t.Error("login token 应拒绝对话通道用途")
	}
}

// 通道用途令牌在凭据库里合法，但 login token 整组只管 Gitea：拿一个 Telegram
// token 去 refresh 会对着不存在的站点删旧建新，错误信息还让人去看 Gitea 的页面。
func TestLoginTokenRejectsChannelPurposes(t *testing.T) {
	for _, purpose := range credentials.ChannelPurposes() {
		_, err := resolveTokenPurpose([]string{"host", purpose})
		if err == nil || !strings.Contains(err.Error(), "assistant login add --type "+purpose) {
			t.Errorf("resolveTokenPurpose(%q) = %v, want 指向 login add --type", purpose, err)
		}
	}
	// Gitea 用途不受影响
	if got, err := resolveTokenPurpose([]string{"host", credentials.PurposeMCP}); err != nil || got != credentials.PurposeMCP {
		t.Errorf("resolveTokenPurpose(mcp) = %q, %v", got, err)
	}
}

// mergeChannelEntry 只更新这次登录确定的字段，保留其余既有配置；密钥一律清空。
// qq 的 app_id 也要清：解析时内联优先，留着旧的会让新登记的机器人连不上。
func TestMergeChannelEntry(t *testing.T) {
	existing := instances.Channel{
		Type: instances.ChannelQQ, Name: "q1", APIBaseURL: "https://proxy.example",
		AppID: "old", AppSecret: "old-secret", AdminUsers: []string{"u1"}, SplitLimit: 500,
	}
	merged := mergeChannelEntry(existing, instances.Channel{
		Type: instances.ChannelQQ, Name: "q1", AdminUsers: []string{"u2"},
	})
	if merged.AppID != "" || merged.AppSecret != "" {
		t.Errorf("密钥没清干净：%+v", merged)
	}
	if merged.APIBaseURL != "https://proxy.example" {
		t.Errorf("api_base_url 被覆盖：%q", merged.APIBaseURL)
	}
	if merged.SplitLimit != 500 {
		t.Errorf("split_limit 被覆盖：%d", merged.SplitLimit)
	}
	if len(merged.AdminUsers) != 1 || merged.AdminUsers[0] != "u2" {
		t.Errorf("admin_users = %v, want [u2]", merged.AdminUsers)
	}
}

// 端点解析：旗标 > 既有配置 > 缺省。反代用户在 config.json 里写 api_base_url，
// 登录自检必须打同一个端点，否则永远验的是官方地址。
func TestExistingAPIBaseURL(t *testing.T) {
	file := &instances.File{Channels: []instances.Channel{
		{Type: instances.ChannelTelegram, Name: "work", APIBaseURL: "https://proxy.example"},
		{Type: instances.ChannelTelegram, APIBaseURL: "https://anon.example"},
	}}
	if got := existingAPIBaseURL(file, instances.ChannelTelegram, "work", ""); got != "https://proxy.example" {
		t.Errorf("= %q, want 既有配置的端点", got)
	}
	if got := existingAPIBaseURL(file, instances.ChannelTelegram, "", ""); got != "https://anon.example" {
		t.Errorf("= %q, want 匿名实例的端点", got)
	}
	if got := existingAPIBaseURL(file, instances.ChannelTelegram, "work", "https://flag.example"); got != "https://flag.example" {
		t.Errorf("= %q, want 旗标优先", got)
	}
	if got := existingAPIBaseURL(file, instances.ChannelWeixin, "absent", ""); got != "" {
		t.Errorf("= %q, want 空", got)
	}
}

func TestSplitUserList(t *testing.T) {
	want := []string{"a", "b", "c"}
	for _, raw := range []string{"a,b,c", "a, b, c", "a，b，c", "a b c", "a;b;c", "  a , b ,c  "} {
		if got := splitUserList(raw); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("splitUserList(%q) = %v, want %v", raw, got, want)
		}
	}
	if got := splitUserList(""); len(got) != 0 {
		t.Errorf("splitUserList(空) = %v, want 空", got)
	}
	// 重复的用户 id 只留一次
	if got := splitUserList("a,a,b"); strings.Join(got, ",") != "a,b" {
		t.Errorf("splitUserList(a,a,b) = %v, want [a b]", got)
	}
}

func TestCredentialSummaryNeverLeaksToken(t *testing.T) {
	cases := map[string]resolvedChannel{
		"没有":   {Value: ""},
		"内联":   {Value: "s3cret", Source: secretSourceConfig},
		"凭据库":  {Value: "s3cret", Source: secretSourceCredentials, Identity: "@bot"},
		"凭据库无": {Value: "s3cret", Source: secretSourceCredentials},
	}
	for name, resolved := range cases {
		summary := credentialSummary(resolved)
		if strings.Contains(summary, "s3cret") {
			t.Errorf("%s：凭据摘要泄漏了密钥：%q", name, summary)
		}
	}
	if got := credentialSummary(cases["没有"]); got != "none" {
		t.Errorf("= %q, want none", got)
	}
	if got := credentialSummary(cases["凭据库"]); !strings.Contains(got, "@bot") || !strings.Contains(got, secretSourceCredentials) {
		t.Errorf("= %q, want 含身份与落点", got)
	}
}

func mustCredentialsPath(t *testing.T) string {
	t.Helper()
	path, err := credentials.Path()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// saveChannelEntry 按 (type, name) 更新而不是每次追加：重新登录改的是同一条通道
// 的凭据，多出一条会让「哪条在生效」变成猜谜。
func TestSaveChannelEntryMergesInsteadOfAppending(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := instances.Save(path, &instances.File{Channels: []instances.Channel{
		{Type: instances.ChannelTelegram, Name: "work", BotToken: "old", AdminUsers: []string{"1"}},
	}}); err != nil {
		t.Fatal(err)
	}
	file, err := instances.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveChannelEntry(file, path, instances.Channel{
		Type: instances.ChannelTelegram, Name: "work", AdminUsers: []string{"2"},
	}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := instances.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Channels) != 1 {
		t.Fatalf("channels = %+v, want 1 条", reloaded.Channels)
	}
	entry := reloaded.Channels[0]
	if entry.BotToken != "" {
		t.Errorf("旧的内联 bot_token 应被清掉：%q", entry.BotToken)
	}
	if entry.AdminUsers[0] != "2" {
		t.Errorf("admin_users = %v, want [2]", entry.AdminUsers)
	}

	// 不同实例名是另一条通道
	if err := saveChannelEntry(file, path, instances.Channel{Type: instances.ChannelTelegram, Name: "personal"}); err != nil {
		t.Fatal(err)
	}
	reloaded, _ = instances.Load(path)
	if len(reloaded.Channels) != 2 {
		t.Errorf("channels = %+v, want 2 条", reloaded.Channels)
	}
}

// fakeQQTokenServer 起一个接受任意 AppID/AppSecret 的假 token 端点。
// 生产端点是包内常量、不经配置，测试只能从这里注入（见 qqTokenURL）。
func fakeQQTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at-1", "expires_in": "7200"})
	}))
	t.Cleanup(server.Close)
	qqTokenURL = server.URL
	t.Cleanup(func() { qqTokenURL = "" })
	return server
}

// QQ 登记：凭据进凭据库（user 维度就是 AppID），config.json 只留结构。
// AppID 不是密钥，但仍只进凭据库——它是 app_secret 的身份维度，分开放会让
// 「这条凭据属于哪个机器人」在两个文件之间对不上。
func TestLoginAddQQWritesStoreAndConfigOnly(t *testing.T) {
	isolateCredentials(t)
	fakeQQTokenServer(t)
	configPath := writeChannelConfig(t, instances.Channel{Type: instances.ChannelQQ, Name: "support"})

	out, err := runLoginAdd(t, configPath, "102000\ns3cret\nopenid-1\n",
		"--type", "qq", "--name", "support")
	if err != nil {
		t.Fatalf("登记应成功：%v\n%s", err, out)
	}
	store, err := credentials.Load(mustCredentialsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	credential, ok, err := store.ChannelCredentialFor("qq/support", credentials.PurposeQQ)
	if err != nil || !ok {
		t.Fatalf("凭据库没有 qq/support：%v %v", ok, err)
	}
	if credential.Token != "s3cret" || credential.User != "102000" {
		t.Errorf("凭据 = %+v, want token=s3cret user=102000", credential)
	}
	file, err := instances.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	entry := file.Channels[0]
	if entry.AppSecret != "" || entry.AppID != "" {
		t.Errorf("config.json 里留下了 app_id/app_secret：%+v", entry)
	}
	if len(entry.AdminUsers) != 1 || entry.AdminUsers[0] != "openid-1" {
		t.Errorf("admin_users = %v, want [openid-1]", entry.AdminUsers)
	}
	if strings.Contains(out, "s3cret") {
		t.Errorf("输出回显了 AppSecret：\n%s", out)
	}
}

// QQ 自检失败同样不写盘：app_secret 填错的现象是「通道起来了但发不出消息」，
// 写进库之后更难联想到是哪次登录填错的。
func TestLoginAddQQKeepsNothingWhenSelfCheckFails(t *testing.T) {
	isolateCredentials(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"code": "100007", "message": "appid invalid"})
	}))
	defer server.Close()
	qqTokenURL = server.URL
	defer func() { qqTokenURL = "" }()

	configPath := writeChannelConfig(t, instances.Channel{Type: instances.ChannelQQ, Name: "support"})
	_, err := runLoginAdd(t, configPath, "102000\nbad\nopenid\n", "--type", "qq", "--name", "support")
	if err == nil || !strings.Contains(err.Error(), "凭据自检失败") {
		t.Fatalf("自检失败应报错：%v", err)
	}
	store, err := credentials.Load(mustCredentialsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.ChannelCredentialFor("qq/support", credentials.PurposeQQ); ok {
		t.Error("自检失败却仍写入了凭据")
	}
}

// fakeWeixinLoginServer 起一个走完 ilink 扫码握手的假端点：先发二维码，随即确认。
func fakeWeixinLoginServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get_bot_qrcode"):
			json.NewEncoder(w).Encode(map[string]any{
				"qrcode": "QR-1", "qrcode_img_content": "https://ilink.example.com/qr/1",
			})
		case strings.HasSuffix(r.URL.Path, "/get_qrcode_status"):
			json.NewEncoder(w).Encode(map[string]any{
				"status": "confirmed", "bot_token": "wx-tok", "ilink_bot_id": "bot-42",
				"ilink_user_id": "wxid_7",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// 微信扫码登记：凭据进凭据库，config.json 留通道条目与身份字段
// （login_user_id 决定空白名单时只允许谁对话，必须留在配置里）。
func TestLoginAddWeixinWritesStoreAndKeepsIdentityFields(t *testing.T) {
	isolateCredentials(t)
	server := fakeWeixinLoginServer(t)
	configPath := writeChannelConfig(t, instances.Channel{
		Type: instances.ChannelWeixin, Name: "work", BaseURL: server.URL, SplitLimit: 1500,
	})

	out, err := runLoginAdd(t, configPath, "", "--type", "weixin", "--name", "work")
	if err != nil {
		t.Fatalf("扫码登记应成功：%v\n%s", err, out)
	}
	store, err := credentials.Load(mustCredentialsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	credential, ok, err := store.ChannelCredentialFor("weixin/work", credentials.PurposeWeixin)
	if err != nil || !ok {
		t.Fatalf("凭据库没有 weixin/work：%v %v", ok, err)
	}
	if credential.Token != "wx-tok" || credential.User != "wxid_7" {
		t.Errorf("凭据 = %+v, want token=wx-tok user=wxid_7", credential)
	}
	file, err := instances.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	entry := file.Channels[0]
	if entry.BotToken != "" {
		t.Errorf("config.json 里留下了 bot_token=%q", entry.BotToken)
	}
	if entry.LoginUserID != "wxid_7" || entry.BotID != "bot-42" {
		t.Errorf("身份字段没写进配置：%+v", entry)
	}
	// 端点与 split_limit 是既有配置，不该被登录流程抹掉
	if entry.BaseURL != server.URL || entry.SplitLimit != 1500 {
		t.Errorf("既有配置被覆盖：%+v", entry)
	}
	if !strings.Contains(out, "wxid_7") {
		t.Errorf("输出应报出身份：\n%s", out)
	}
}

// 扫码拿不到凭据时不该留下半条通道：写盘顺序是「先凭据库后 config.json」，
// 反过来的话 config 校验失败会留下一条指向空密钥的悬空通道条目。
func TestLoginAddWeixinReportsScanFailure(t *testing.T) {
	isolateCredentials(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	configPath := writeChannelConfig(t, instances.Channel{
		Type: instances.ChannelWeixin, Name: "work", BaseURL: server.URL,
	})

	_, err := runLoginAdd(t, configPath, "", "--type", "weixin", "--name", "work")
	if err == nil {
		t.Fatal("取二维码失败应报错")
	}
	store, loadErr := credentials.Load(mustCredentialsPath(t))
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, ok, _ := store.ChannelCredentialFor("weixin/work", credentials.PurposeWeixin); ok {
		t.Error("扫码失败却仍写入了凭据")
	}
}
