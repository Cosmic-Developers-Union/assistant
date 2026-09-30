package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/dispatcher"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// TestMaskTokenForLogKeepsBothEnds 断言日志掩码只在两端留痕：凭据库里的令牌
// 有各种长度，短令牌若被截成 "ab***yz" 会泄露大部分字节，所以 <=8 一律全遮。
func TestMaskTokenForLogKeepsBothEnds(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		token string
		want  string
	}{
		{"空令牌显式标注而不是空白", "", "(空)"},
		{"单字节令牌全遮", "a", "***"},
		{"恰好八字节全遮", "12345678", "***"},
		{"九字节起两端各留四位", "123456789", "1234***6789"},
		{"长令牌只露前四后四", "abcdefghijklmnop", "abcd***mnop"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := maskTokenForLog(testCase.token); got != testCase.want {
				t.Errorf("maskTokenForLog(%q) = %q, want %q", testCase.token, got, testCase.want)
			}
		})
	}
}

// TestMaskedEnvKeysSortedAndExact 断言展开环境变量的键按字典序输出：日志里键
// 顺序漂移会让两次诊断输出无法比对，且有值的键绝不能漏报。
func TestMaskedEnvKeysSortedAndExact(t *testing.T) {
	keys := maskedEnvKeys(map[string]string{"ZED": "1", "ABC": "2", "M": "3"})
	if !sort.StringsAreSorted(keys) {
		t.Fatalf("keys = %v, 未排序", keys)
	}
	if strings.Join(keys, ",") != "ABC,M,ZED" {
		t.Errorf("keys = %v", keys)
	}
	if got := maskedEnvKeys(nil); len(got) != 0 {
		t.Errorf("maskedEnvKeys(nil) = %v, want 空", got)
	}
}

// TestShortGiteaPathStripsAPIPrefix 断言日志里的 Gitea 路径是操作者能对着
// 界面复现的相对路径：带前缀的要剥掉，能解析出查询串的要保留，不可解析时
// 原样返回（宁可长也不丢信息）。
func TestShortGiteaPathStripsAPIPrefix(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		host   string
		rawURL string
		want   string
	}{
		{"带主机的完整 URL 剥前缀", "gitea.example.com", "https://gitea.example.com/api/v1/repos/acme/video/issues", "/repos/acme/video/issues"},
		{"主机末尾斜杠不影响", "gitea.example.com/", "https://gitea.example.com/api/v1/repos/acme/video", "/repos/acme/video"},
		{"无法剥前缀时回退解析并保留查询串", "other.host", "https://gitea.example.com/api/v1/repos/acme/video?state=open", "/repos/acme/video?state=open"},
		{"不可解析的输入原样返回", "gitea.example.com", "://bad", "://bad"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := shortGiteaPath(testCase.host, testCase.rawURL); got != testCase.want {
				t.Errorf("shortGiteaPath(%q, %q) = %q, want %q", testCase.host, testCase.rawURL, got, testCase.want)
			}
		})
	}
}

// TestFirstNonEmptyPrefersTrimmedValue 断言回退链取的是去掉空白后的值：配置里
// 的空串与全空格都应视为未设置，否则会把 "--repo ”" 这种空值当成有效仓库名。
func TestFirstNonEmptyPrefersTrimmedValue(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		values []string
		want   string
	}{
		{"首个非空", []string{"a", "b"}, "a"},
		{"跳过空串与空白", []string{"", "   ", "c"}, "c"},
		{"结果去两端空白", []string{"  d  "}, "d"},
		{"全部为空返回空串", []string{"", " "}, ""},
		{"无参数返回空串", nil, ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := firstNonEmpty(testCase.values...); got != testCase.want {
				t.Errorf("firstNonEmpty(%v) = %q, want %q", testCase.values, got, testCase.want)
			}
		})
	}
}

// TestAppendCallbacksChainWhenFirstPresent 断言回调组合的短路语义：first 为
// nil 时必须原样返回 second（不能包一层，否则 dispatcher 的 nil 判定失效），
// 两者都在时必须先 first 再 second，且 second 的返回值决定互斥取舍。
func TestAppendCallbacksChainWhenFirstPresent(t *testing.T) {
	t.Run("first 为 nil 直接返回 second", func(t *testing.T) {
		want := func([]dispatcher.WorkItem) {}
		if got := appendOnQueue(nil, want); &got == nil {
			t.Fatal("appendOnQueue 返回 nil")
		}
		var called []string
		second := func([]dispatcher.WorkItem) { called = append(called, "second") }
		appendOnQueue(nil, second)(nil)
		if strings.Join(called, ",") != "second" {
			t.Errorf("called = %v", called)
		}
	})

	t.Run("两个回调按序执行", func(t *testing.T) {
		var called []string
		combined := appendOnQueue(
			func([]dispatcher.WorkItem) { called = append(called, "first") },
			func([]dispatcher.WorkItem) { called = append(called, "second") },
		)
		combined([]dispatcher.WorkItem{{Number: 1}})
		if strings.Join(called, ",") != "first,second" {
			t.Errorf("called = %v", called)
		}
	})

	t.Run("start 回调返回 second 的互斥结论", func(t *testing.T) {
		var order []string
		allow := appendOnStartBool(
			func(dispatcher.WorkItem) bool { order = append(order, "first"); return false },
			func(dispatcher.WorkItem) bool { order = append(order, "second"); return true },
		)
		if !allow(dispatcher.WorkItem{}) {
			t.Error("互斥结论应以 second 为准（true）")
		}
		if strings.Join(order, ",") != "first,second" {
			t.Errorf("order = %v", order)
		}
		if nilFirst := appendOnStartBool(nil, func(dispatcher.WorkItem) bool { return false }); nilFirst(dispatcher.WorkItem{}) {
			t.Error("first 为 nil 时应直接采用 second 的 false")
		}
	})

	t.Run("finish 回调两个都收到待办与结论", func(t *testing.T) {
		var seen []string
		finish := appendOnFinish(
			func(item dispatcher.WorkItem, outcome dispatcher.SessionOutcome) {
				seen = append(seen, "first:"+outcome.Subtype)
			},
			func(item dispatcher.WorkItem, outcome dispatcher.SessionOutcome) {
				seen = append(seen, "second:"+outcome.Subtype)
			},
		)
		finish(dispatcher.WorkItem{Number: 9}, dispatcher.SessionOutcome{Subtype: "success"})
		if strings.Join(seen, ",") != "first:success,second:success" {
			t.Errorf("seen = %v", seen)
		}
	})
}

// TestReadyTargetsAndSkipSummarySplitTargets 断言就绪筛选与跳过摘要互补：
// 被跳过的仓库只要没进 readyTargets，就必须在摘要里点名并给出原因，否则
// 操作者会看到「共处理 0 个」却不知为何。
func TestReadyTargetsAndSkipSummarySplitTargets(t *testing.T) {
	targets := []dispatchTarget{
		{config: dispatcher.Config{Repository: status.Repository{Owner: "acme", Name: "video"}}},
		{config: dispatcher.Config{Repository: status.Repository{Owner: "acme", Name: "audio"}}, repo: instances.Repo{Name: "acme/audio"}, skipReason: "缺少脚手架"},
		{config: dispatcher.Config{Repository: status.Repository{Owner: "acme", Name: "docs"}}, repo: instances.Repo{Name: "acme/docs"}, skipReason: "缺少凭据"},
	}
	ready := readyTargets(targets)
	if len(ready) != 1 || ready[0].config.Repository.Name != "video" {
		t.Fatalf("readyTargets = %+v", ready)
	}
	summary := skipSummary(targets)
	if !strings.Contains(summary, "acme/audio（缺少脚手架）") || !strings.Contains(summary, "acme/docs（缺少凭据）") {
		t.Errorf("skipSummary = %q", summary)
	}
	if strings.Contains(summary, "video") {
		t.Errorf("就绪的仓库不应出现在跳过摘要里：%q", summary)
	}
	if got := skipSummary(nil); got != "" {
		t.Errorf("skipSummary(nil) = %q, want 空", got)
	}
	// 无跳过项时摘要为空，调用方据此跳过整行输出。
	if got := skipSummary(ready); got != "" {
		t.Errorf("全部就绪时摘要应为空，got %q", got)
	}
}

// TestMissingScaffoldingListsManagedFiles 断言托管文件的缺失清单：目标仓库
// 必须完全装载才能被调度，少一个都算未就绪，清单是给出「去跑 install」的依据。
func TestMissingScaffoldingListsManagedFiles(t *testing.T) {
	dir := t.TempDir()
	missing := missingScaffolding(dir)
	wantAll := []string{".mcp.json", ".claude/settings.json", ".claude/skills/review/SKILL.md", "AGENTS.md"}
	if len(missing) != len(wantAll) {
		t.Fatalf("missingScaffolding(空目录) = %v, want %v", missing, wantAll)
	}
	for _, name := range wantAll {
		if !strings.Contains(strings.Join(missing, "\n"), name) {
			t.Errorf("缺失清单没有 %s：%v", name, missing)
		}
	}

	for _, name := range []string{".mcp.json", "AGENTS.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	missing = missingScaffolding(dir)
	if len(missing) != 2 {
		t.Fatalf("补齐后 missingScaffolding = %v, want 2 项", missing)
	}
	for _, name := range []string{".claude/settings.json", ".claude/skills/review/SKILL.md"} {
		if !strings.Contains(strings.Join(missing, "\n"), name) {
			t.Errorf("缺失清单没有 %s：%v", name, missing)
		}
	}
}

// TestCountReposCountsDistinctReferencedRepos 断言仓库数按通道键筛选后的清单计：
// referenced 非空时只算被本次选中的通道（否则会把没在处理的仓库算进去），
// 为空时算全部 gitea 通道。
func TestCountReposCountsDistinctReferencedRepos(t *testing.T) {
	file := &instances.File{
		Channels: []instances.Channel{
			{Type: instances.ChannelGitea, Host: "gitea.example.com", Repos: []instances.Repo{{Name: "acme/video"}, {Name: "acme/audio"}}},
			{Type: instances.ChannelGitea, Name: "backup", Host: "gitea.example.com", Repos: []instances.Repo{{Name: "acme/docs"}}},
		},
	}
	if got := countRepos(file, map[string]bool{"gitea": true}); got != 2 {
		t.Errorf("选中主干通道时 countRepos = %d, want 2", got)
	}
	if got := countRepos(file, map[string]bool{}); got != 3 {
		t.Errorf("无筛选时 countRepos = %d, want 3", got)
	}
	if got := countRepos(file, map[string]bool{"gitea/backup": true}); got != 1 {
		t.Errorf("选中命名通道时 countRepos = %d, want 1", got)
	}
	if got := countRepos(file, map[string]bool{"gitea/nope": true}); got != 0 {
		t.Errorf("选中不存在的通道时 countRepos = %d, want 0", got)
	}
}

// TestTargetTokenAndHasChatService 断言两个调度前置判定：令牌取自
// config.AccessToken（凭据库解析后的落点），对话服务只看启用的非 gitea 通道
// （gitea 通道是机器人身份，不是对话）。
func TestTargetTokenAndHasChatService(t *testing.T) {
	target := dispatchTarget{config: dispatcher.Config{AccessToken: "abc"}}
	if got := targetToken(target); got != "abc" {
		t.Errorf("targetToken = %q, want abc", got)
	}
	if got := targetToken(dispatchTarget{}); got != "" {
		t.Errorf("空配置 targetToken = %q, want 空", got)
	}

	disabled := false
	file := &instances.File{Channels: []instances.Channel{
		{Type: instances.ChannelGitea, Host: "gitea.example.com"},
		{Type: instances.ChannelQQ, AppID: "1", Enabled: &disabled},
	}}
	if hasChatService(file) {
		t.Error("只有 gitea 与停用通道时不应认为有对话服务")
	}
	file.Channels = append(file.Channels, instances.Channel{Type: instances.ChannelTelegram, BotToken: "t"})
	if !hasChatService(file) {
		t.Error("启用的 telegram 通道应视为有对话服务")
	}
	empty := &instances.File{}
	if hasChatService(empty) {
		t.Error("无通道时不应认为有对话服务")
	}
}
