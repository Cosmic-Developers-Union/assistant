package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/qq"
)

// writeChannelConfig 写出只含一个对话通道的配置：频道状态命令只看 channels，
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

// TestTelegramStatusWithoutChannels 断言没有任何 telegram 通道时给出配置引导并
// 返回成功：这是「还没配」而非故障，退出码不该是失败。
func TestTelegramStatusWithoutChannels(t *testing.T) {
	isolateCredentials(t)
	configPath := writeChannelConfig(t, instances.Channel{Type: instances.ChannelGitea, Host: "https://a.example.com"})

	command := newTelegramStatusCommand(&configPath)
	out := &bytes.Buffer{}
	command.SetOut(out)
	command.SetErr(out)
	command.SetArgs(nil)
	if err := command.Execute(); err != nil {
		t.Fatalf("未配置通道不应报错：%v", err)
	}
	if !strings.Contains(out.String(), "未配置 Telegram 通道") {
		t.Errorf("out = %q, want 含「未配置 Telegram 通道」", out.String())
	}
}

// TestTelegramStatusVerifiesToken 断言 status 会真的打 getMe 自检并报告通过：
// 只看配置有没有填 token 不足以证明凭据可用，实测才是这个子命令的价值所在。
func TestTelegramStatusVerifiesToken(t *testing.T) {
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
	command := newTelegramStatusCommand(&configPath)
	out := &bytes.Buffer{}
	command.SetOut(out)
	command.SetErr(out)
	command.SetArgs(nil)
	if err := command.Execute(); err != nil {
		t.Fatalf("自检应通过：%v", err)
	}
	if gotPath != "/bot123:abc/getMe" {
		t.Errorf("请求路径 = %q, want /bot123:abc/getMe（token 是路径的一部分）", gotPath)
	}
	if !strings.Contains(out.String(), "凭据自检：通过（getMe 成功）") {
		t.Errorf("out = %q, want 含自检通过", out.String())
	}
	// 密钥只显示前缀，不能整串打出来
	if strings.Contains(out.String(), "123:abc") {
		t.Errorf("out = %q 泄漏了 bot token 原文", out.String())
	}
}

// TestTelegramStatusReportsVerifyFailure 断言 getMe 失败时命令返回错误并点名通道
// 键：凭据自检失败必须让退出码非零，否则脚本化的巡检会把坏凭据当成健康。
func TestTelegramStatusReportsVerifyFailure(t *testing.T) {
	isolateCredentials(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Unauthorized"})
	}))
	defer server.Close()

	configPath := writeChannelConfig(t, instances.Channel{
		Type: instances.ChannelTelegram, Name: "prod", BotToken: "bad", APIBaseURL: server.URL,
	})
	command := newTelegramStatusCommand(&configPath)
	out := &bytes.Buffer{}
	command.SetOut(out)
	command.SetErr(out)
	command.SetArgs(nil)
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "telegram/prod 凭据自检失败") {
		t.Errorf("err = %v, want 含「telegram/prod 凭据自检失败」", err)
	}
}

// TestQQStatusWithoutChannels 断言没有 qq 通道（含配置为空）时给出引导且不报错。
func TestQQStatusWithoutChannels(t *testing.T) {
	isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{}); err != nil {
		t.Fatal(err)
	}

	command := newQQStatusCommand(&configPath)
	out := &bytes.Buffer{}
	command.SetOut(out)
	command.SetErr(out)
	command.SetArgs(nil)
	if err := command.Execute(); err != nil {
		t.Fatalf("未配置通道不应报错：%v", err)
	}
	if !strings.Contains(out.String(), "未配置 QQ 通道") {
		t.Errorf("out = %q, want 含「未配置 QQ 通道」", out.String())
	}
}

// TestQQStatusVerifiesCredential 断言 qq status 会实测换取 access token 并把
// AppSecret 展开后的值送到 token 端点：AppSecret 支持 $VAR 引用，送错值会让自检在
// 一片「配置看起来没问题」里静默失败。
//
// 端点是固定常量（不在 APIBaseURL 下），无法经配置指向假站点，所以这里不走子命令，
// 直接对着 qqChannelViews + verifyQQ 断言协议：展开后的 AppID/AppSecret 必须原样
// 进入换取 access token 的请求体。
func TestQQStatusVerifiesCredential(t *testing.T) {
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

	channel := instances.Channel{
		Type: instances.ChannelQQ, AppID: "$QQ_APP_ID", AppSecret: "$QQ_APP_SECRET",
	}
	views := qqChannelViews(&instances.File{Channels: []instances.Channel{channel}})
	if len(views) != 1 {
		t.Fatalf("qqChannelViews = %d 条, want 1", len(views))
	}
	if views[0].AppSecret != "s3cret" || views[0].AppID != "102000" {
		t.Fatalf("view = %+v, want app_id/app_secret 均展开", views[0])
	}

	// verifyQQ 用生产默认 token 端点；这里直接构造等价 client，验证同一份字段被
	// 送成官方要求的 JSON body（appId/clientSecret）。
	client := qq.NewClient(qq.Config{
		AppID: views[0].AppID, AppSecret: views[0].AppSecret, TokenURL: tokenServer.URL,
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

// TestVerifyQQSkipsIncompleteCredential 断言凭据不全时跳过自检并说明原因：跳过而
// 不是失败，是因为「还没填」与「填了但不被接受」需要操作者做不同的事。
func TestVerifyQQSkipsIncompleteCredential(t *testing.T) {
	out := &bytes.Buffer{}
	if err := verifyQQ(qqChannelView{Key: "qq", AppID: "102000"}, out); err != nil {
		t.Fatalf("凭据不全应跳过而非报错：%v", err)
	}
	if !strings.Contains(out.String(), "凭据不完整：跳过自检") {
		t.Errorf("out = %q, want 含「凭据不完整：跳过自检」", out.String())
	}
}

// TestVerifyQQAbortsWhenTokenRejected 断言 token 端点拒绝凭据时 verifyQQ 上抛
// 错误：把「被平台拒绝」和「网络不通」混成静默通过，等于凭据自检什么都没检。
func TestVerifyQQAbortsWhenTokenRejected(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"code": "100007", "message": "appid invalid"})
	}))
	defer tokenServer.Close()

	// verifyQQ 自身用的是内置 token 端点，这里直接驱动同一份逻辑所依赖的客户端，
	// 断言拒绝确实变成错误。
	client := qq.NewClient(qq.Config{AppID: "102000", AppSecret: "bad", TokenURL: tokenServer.URL})
	err := client.VerifyCredential(t.Context())
	if err == nil {
		t.Fatal("被拒绝的凭据不应返回 nil")
	}
	if !strings.Contains(err.Error(), "被平台拒绝") {
		t.Errorf("err = %v, want 含「被平台拒绝」（FatalError 语义）", err)
	}
}

// TestQQChannelViewsExpandsAndFallsBack 断言 qqChannelViews 的两个关键口径：
// app_id / app_secret 的 $VAR 引用要展开；展开失败时保留原值而不是清空——自检会
// 拿原值去请求并给出失败原因，清空只会让操作者看到一句「凭据不完整」，查不出是
// 哪个环境变量没设。
func TestQQChannelViewsExpandsAndFallsBack(t *testing.T) {
	t.Setenv("QQ_ID_OK", "102000")

	file := &instances.File{Channels: []instances.Channel{
		{Type: instances.ChannelGitea, Host: "https://a.example.com"},
		{
			Type: instances.ChannelQQ, AppID: "$QQ_ID_OK", AppSecret: "$QQ_SECRET_UNSET",
			APIBaseURL: "https://api.sgroup.qq.com", AdminUsers: []string{"u1"}, SplitLimit: 1000,
		},
	}}
	views := qqChannelViews(file)
	if len(views) != 1 {
		t.Fatalf("qqChannelViews 只应含 qq 通道，实际 %d 条", len(views))
	}
	view := views[0]
	if view.AppID != "102000" {
		t.Errorf("AppID = %q, want 102000（$QQ_ID_OK 应展开）", view.AppID)
	}
	if view.AppSecret != "$QQ_SECRET_UNSET" {
		t.Errorf("AppSecret = %q, want 保留原值 $QQ_SECRET_UNSET", view.AppSecret)
	}
	if view.Key != "qq" {
		t.Errorf("Key = %q, want qq", view.Key)
	}
	if view.SplitLimit != 1000 || len(view.AdminUsers) != 1 {
		t.Errorf("view = %+v, want SplitLimit=1000 / 1 个 admin", view)
	}
}
