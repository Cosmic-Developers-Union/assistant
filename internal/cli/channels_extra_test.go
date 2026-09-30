package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/qq"
)

// runLoginList 跑 `login list [--type <平台>]` 并返回输出与错误。走真实命令树而
// 不是直接调内部函数：这次改动的重点正是「入口收敛到 login」，只测内层函数
// 恰恰会漏掉「命令没接上」这种最容易犯的错。
func runLoginList(t *testing.T, configPath string, args ...string) (string, error) {
	t.Helper()
	out := &bytes.Buffer{}
	command := newLoginCommand(&configPath)
	command.SetOut(out)
	command.SetErr(out)
	command.SetContext(t.Context())
	command.SetArgs(append([]string{"list"}, args...))
	err := command.Execute()
	return out.String(), err
}

// writeChannelConfig 写出只含一个对话通道的配置：通道状态只看 channels，
// 用最小配置能把「通道未配置」与「通道已配置」两条分支分开。
func writeChannelConfig(t *testing.T, channel instances.Channel) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	file := &instances.File{Channels: []instances.Channel{channel}}
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// TestLoginListChannelWithoutChannels 断言没有任何该类型的通道时给出配置引导并
// 返回成功：这是「还没配」而非故障，退出码不该是失败。
func TestLoginListChannelWithoutChannels(t *testing.T) {
	isolateCredentials(t)
	configPath := writeChannelConfig(t, instances.Channel{Type: instances.ChannelGitea, Host: "https://a.example.com"})

	out, err := runLoginList(t, configPath, "--type", "telegram")
	if err != nil {
		t.Fatalf("未配置通道不应报错：%v", err)
	}
	if !strings.Contains(out, "未配置 telegram 通道") {
		t.Errorf("out = %q, want 含「未配置 telegram 通道」", out)
	}
}

// TestLoginListTelegramVerifiesToken 断言会真的打 getMe 自检并报告通过：只看
// 配置里有没有 token 不足以证明凭据可用，实测才是这条命令的价值所在。
func TestLoginListTelegramVerifiesToken(t *testing.T) {
	isolateCredentials(t)
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"id": 42, "username": "assistant_bot"},
		})
	}))
	defer server.Close()

	configPath := writeChannelConfig(t, instances.Channel{
		Type: instances.ChannelTelegram, BotToken: "123:abc", APIBaseURL: server.URL,
		AdminUsers: []string{"7"},
	})
	out, err := runLoginList(t, configPath, "--type", "telegram")
	if err != nil {
		t.Fatalf("自检应通过：%v", err)
	}
	if gotPath != "/bot123:abc/getMe" {
		t.Errorf("请求路径 = %q, want /bot123:abc/getMe（token 是路径的一部分）", gotPath)
	}
	if !strings.Contains(out, "凭据自检：通过（getMe 成功）") {
		t.Errorf("out = %q, want 含自检通过", out)
	}
	// 密钥只显示状态，不能整串打出来
	if strings.Contains(out, "123:abc") {
		t.Errorf("out = %q 泄漏了 bot token 原文", out)
	}
	if !strings.Contains(out, secretSourceConfig) {
		t.Errorf("out = %q, want 标出密钥取自 config.json（内联）", out)
	}
}

// TestLoginListResolvesChannelCredentialFromStore 断言 config.json 里没写密钥时
// 状态来自凭据库，并把身份与落点一起报出来。
//
// 这条是本次迁移的主路径：密钥搬进 credentials.json 之后，状态输出必须说得出
// 「这条通道的密钥在哪」，否则「连上了但不是我以为的那个机器人」无从排查。
func TestLoginListResolvesChannelCredentialFromStore(t *testing.T) {
	isolateCredentials(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "result": map[string]any{"id": 42, "username": "assistant_bot"},
		})
	}))
	defer server.Close()

	store := &credentials.File{}
	store.SetChannelCredential(credentials.Credential{
		Host: "telegram", User: "@assistant_bot", Purpose: credentials.PurposeTelegram, Token: "999:zzz",
	})
	path, err := credentials.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.Save(path, store); err != nil {
		t.Fatal(err)
	}

	configPath := writeChannelConfig(t, instances.Channel{
		Type: instances.ChannelTelegram, APIBaseURL: server.URL, AdminUsers: []string{"7"},
	})
	out, err := runLoginList(t, configPath, "--type", "telegram")
	if err != nil {
		t.Fatalf("自检应通过：%v", err)
	}
	if !strings.Contains(out, "@assistant_bot") || !strings.Contains(out, secretSourceCredentials) {
		t.Errorf("out = %q, want 含凭据库落点与身份", out)
	}
	if strings.Contains(out, "999:zzz") {
		t.Errorf("out = %q 泄漏了 bot token 原文", out)
	}
}

// TestLoginListTelegramReportsVerifyFailure 断言 getMe 失败时命令返回错误并点名
// 通道键：凭据自检失败必须让退出码非零，否则脚本化的巡检会把坏凭据当成健康。
func TestLoginListTelegramReportsVerifyFailure(t *testing.T) {
	isolateCredentials(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Unauthorized"})
	}))
	defer server.Close()

	configPath := writeChannelConfig(t, instances.Channel{
		Type: instances.ChannelTelegram, Name: "prod", BotToken: "bad", APIBaseURL: server.URL,
	})
	_, err := runLoginList(t, configPath, "--type", "telegram")
	if err == nil || !strings.Contains(err.Error(), "存在凭据自检失败的 telegram 通道") {
		t.Errorf("err = %v, want 含自检失败", err)
	}
	if !strings.Contains(err.Error(), "telegram") {
		t.Errorf("err = %v, want 点名平台", err)
	}
}

// TestLoginListQQSkipsIncompleteCredential 断言凭据不全时跳过自检并说明原因：
// 跳过而不是失败，是因为「还没填」与「填了但不被接受」需要操作者做不同的事。
func TestLoginListQQSkipsIncompleteCredential(t *testing.T) {
	isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{}); err != nil {
		t.Fatal(err)
	}
	out, err := runLoginList(t, configPath, "--type", "qq")
	if err != nil {
		t.Fatalf("未配置通道不应报错：%v", err)
	}
	if !strings.Contains(out, "未配置 qq 通道") {
		t.Errorf("out = %q, want 含「未配置 qq 通道」", out)
	}
}

// TestQQCredentialVerificationSendsOfficialFields 断言 QQ 凭据实测把展开后的
// AppID/AppSecret 原样送成官方要求的 JSON body（appId/clientSecret）。
//
// 端点是固定常量（不在 APIBaseURL 下），无法经配置指向假站点，所以这里不走子
// 命令，直接驱动 verifyQQCredential 依赖的同一个客户端来断言协议。
func TestQQCredentialVerificationSendsOfficialFields(t *testing.T) {
	t.Setenv("QQ_APP_ID", "102000")
	t.Setenv("QQ_APP_SECRET", "s3cret")
	var received struct {
		AppID        string `json:"appId"`
		ClientSecret string `json:"clientSecret"`
	}
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-1", "expires_in": "7200"})
	}))
	defer tokenServer.Close()

	// 先过一遍通道密钥的解析，确认 $VAR 引用被展开，再拿展开后的值去实测
	resolved, err := resolveChannelSecret(instances.Channel{
		Type: instances.ChannelQQ, AppID: "$QQ_APP_ID", AppSecret: "$QQ_APP_SECRET",
	}, &credentials.File{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.AppID != "102000" || resolved.Value != "s3cret" {
		t.Fatalf("resolved = %+v, want app_id/app_secret 均展开", resolved)
	}

	client := qq.NewClient(qq.Config{
		AppID: resolved.AppID, AppSecret: resolved.Value, TokenURL: tokenServer.URL,
	})
	if err := client.VerifyCredential(t.Context()); err != nil {
		t.Fatalf("VerifyCredential: %v", err)
	}
	if received.AppID != "102000" {
		t.Errorf("token 端点收到 appId = %q, want 102000", received.AppID)
	}
	if received.ClientSecret != "s3cret" {
		t.Errorf("token 端点收到 clientSecret = %q, want s3cret", received.ClientSecret)
	}
}

// TestQQVerifyAbortsWhenTokenRejected 断言 token 端点拒绝凭据时自检上抛错误：
// 把「被平台拒绝」和「网络不通」混成静默通过，等于凭据自检什么都没检。
func TestQQVerifyAbortsWhenTokenRejected(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"code": "100007", "message": "appid invalid"})
	}))
	defer tokenServer.Close()

	client := qq.NewClient(qq.Config{AppID: "102000", AppSecret: "bad", TokenURL: tokenServer.URL})
	err := client.VerifyCredential(t.Context())
	if err == nil {
		t.Fatal("被拒绝的凭据不应返回 nil")
	}
	if !strings.Contains(err.Error(), "被平台拒绝") {
		t.Errorf("err = %v, want 含「被平台拒绝」（FatalError 语义）", err)
	}
}
