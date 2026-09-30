package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
)

// TestQQStatusGuidesWhenConfigHasNoQQChannel 断言配置能读、但里面一条 qq 通道都没
// 有时，qq status 打印加通道的引导并以成功退出。
//
// 这是「配置已经跑起来了（有 runtime），只是还没接 QQ」的正常形态：命令必须把
// 「该往 channels 里加什么」写清楚，而不是报错或者静默什么都不输出。渠道与
// 「配置文件不存在」是两种不同的处境，命令必须分别对待。
func TestQQStatusGuidesWhenConfigHasNoQQChannel(t *testing.T) {
	isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{
		Runtimes: map[string]instances.Runtime{"main": {MainAgent: "main"}},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := runLoginList(t, configPath, "--type", "qq")
	if err != nil {
		t.Fatalf("没有 qq 通道不该报错：%v\n%s", err, out)
	}
	if !strings.Contains(out, "未配置 qq 通道：运行 assistant login add --type qq 登记凭据") {
		t.Errorf("应给出加通道的引导：\n%s", out)
	}
}

// TestQQStatusReportsMissingConfigFile 断言配置目录里没有 config.json 时 qq status
// 报错退出，而不是把它当成「没有 qq 通道」打印引导。
//
// 二者对操作者指向相反：引导的意思是「配置能读，只是还没加 qq 条目」（去加条目），
// 而配置文件整个不存在意味着后面每一步都会在「读不到配置」上打转（先去生成配置）。
// 把读失败说成「未配置通道」，会让人在一个根本不存在的文件上反复加通道。
func TestQQStatusReportsMissingConfigFile(t *testing.T) {
	isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")

	out, err := runLoginList(t, configPath, "--type", "qq")
	if err == nil {
		t.Fatalf("配置文件不存在时应报错：\n%s", out)
	}
	if strings.Contains(out, "未配置 qq 通道") {
		t.Errorf("不该把读不到配置说成未配置通道：\n%s", out)
	}
}

// TestQQStatusSkipsIncompleteCredentialFromStoredReferences 断言 app_id/app_secret
// 都写成 $VAR 引用、而变量都没定义时，qq status 给出「凭据不完整、跳过自检」的
// 结论并以成功退出，同时把两个字段呈现为空（app_id=- / secret=none）。
//
// 这是 q.qq.com 密钥最推荐的写法（不落盘），因此它必然经过「展开失败保留原值」
// 这条路径。命令必须能让操作者看出这两个字段其实是空的——如果这里变成「自检
// 失败」，人会去 q.qq.com 查凭据，而真正的问题是环境变量没设。
func TestQQStatusSkipsIncompleteCredentialFromStoredReferences(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("QQ_GAP_ID_UNSET", "")
	t.Setenv("QQ_GAP_SECRET_UNSET", "")
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{Channels: []instances.Channel{{
		Type: instances.ChannelQQ,
		// 通过 .env 引用写进配置：文件本身必须是合法配置（校验要求两个字段非空），
		// 展开失败后保留的正是这些引用字面量。
		AppID: "$QQ_GAP_ID_UNSET", AppSecret: "$QQ_GAP_SECRET_UNSET",
	}}}); err != nil {
		t.Fatal(err)
	}

	out, err := runLoginList(t, configPath, "--type", "qq")
	if err != nil {
		t.Fatalf("展开失败保留原值后应走跳过自检，不该失败：%v\n%s", err, out)
	}
	text := out
	// 展开失败保留原值，但列表按「有效值」呈现：空引用落到 orDash 的 - 与
	// tokenState 的 none。
	if !strings.Contains(text, "app_id=-") {
		t.Errorf("未展开的 app_id 应呈现为空：\n%s", text)
	}
	if !strings.Contains(text, "secret=none") {
		t.Errorf("未展开的 app_secret 应呈现为空：\n%s", text)
	}
	if !strings.Contains(text, "凭据自检：跳过") {
		t.Errorf("应说明跳过自检的原因：\n%s", text)
	}
	if strings.Contains(text, "存在凭据自检失败的 qq 通道") {
		t.Errorf("跳过自检不是失败：\n%s", text)
	}
}

// TestQQStatusMasksSecretAsSetNotEmpty 断言列表里 app_secret 只呈现为
// set/none，绝不回显密钥本身。
//
// qq status 的输出经常被贴进 issue 或聊天里求助；回显密钥（哪怕只显示片段）就等于
// 把它泄露到那些地方。这条钉住的是「有值就说 set、空值就说 none」这个不泄露口径。
func TestQQStatusMasksSecretAsSetNotEmpty(t *testing.T) {
	isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{Channels: []instances.Channel{{
		Type: instances.ChannelQQ, AppID: "102000", AppSecret: "s3cret-should-never-appear",
	}}}); err != nil {
		t.Fatal(err)
	}

	// 这里只断言列表行的脱敏口径；命令随后会去真实 token 端点自检，成败与本用例
	// 无关（网络不可用时它只是一句自检失败）。
	out, _ := runLoginList(t, configPath, "--type", "qq")

	text := out
	if strings.Contains(text, "s3cret-should-never-appear") {
		t.Errorf("列表不该回显 app_secret：\n%s", text)
	}
	if !strings.Contains(text, "secret=set（config.json）") {
		t.Errorf("有密钥时应显示 secret=set（config.json）：\n%s", text)
	}
}
