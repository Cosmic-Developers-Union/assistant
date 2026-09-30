package cli

import (
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
)

// sameGiteaHost：同站判定只看规范化后的地址（去空白、去尾斜杠），不接受
// 「https://a 与 https://b」或「有 scheme 与无 scheme」这类不同形态。
func TestSameGiteaHost(t *testing.T) {
	for _, test := range []struct {
		name string
		a, b string
		want bool
	}{
		{name: "完全相同", a: "https://gitea.example.com", b: "https://gitea.example.com", want: true},
		{name: "尾斜杠差异", a: "https://gitea.example.com", b: "https://gitea.example.com/", want: true},
		{name: "首尾空白差异", a: "  https://gitea.example.com  ", b: "https://gitea.example.com", want: true},
		{name: "不同站点", a: "https://gitea.example.com", b: "https://other.example.com", want: false},
		{name: "scheme 不同", a: "https://gitea.example.com", b: "http://gitea.example.com", want: false},
		{name: "大小写不同（地址区分大小写，不做归一）", a: "https://Gitea.example.com", b: "https://gitea.example.com", want: false},
		{name: "空与空", a: "", b: "  ", want: true},
		{name: "空与非空", a: "", b: "https://gitea.example.com", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sameGiteaHost(test.a, test.b); got != test.want {
				t.Errorf("sameGiteaHost(%q, %q) = %v, want %v", test.a, test.b, got, test.want)
			}
		})
	}
}

// sortChannelRepos 按仓库名排序：登记命令族的展示要稳定（同样的配置每次
// 输出同一顺序），否则 diff 与人工核对都不可用。
func TestSortChannelRepos(t *testing.T) {
	channel := &instances.Channel{Repos: []instances.Repo{
		{Name: "zeta/repo"},
		{Name: "alpha/repo"},
		{Name: "Mid/Repo"},
	}}
	sortChannelRepos(channel)
	got := make([]string, 0, len(channel.Repos))
	for _, repo := range channel.Repos {
		got = append(got, repo.Name)
	}
	// 字典序（byte 序）：大写在小写之前
	if want := []string{"Mid/Repo", "alpha/repo", "zeta/repo"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("排序结果 = %v, want %v", got, want)
	}
	// 空与单元素：不 panic，且顺序不变
	sortChannelRepos(&instances.Channel{})
	single := &instances.Channel{Repos: []instances.Repo{{Name: "only/repo"}}}
	sortChannelRepos(single)
	if len(single.Repos) != 1 || single.Repos[0].Name != "only/repo" {
		t.Errorf("单元素排序改变了内容：%+v", single.Repos)
	}
}

// selectRepoGitea 的四级优先与歧义报错：--host 显式指定 > 检出 remote 命中的
// 站点 > 已登记该仓库的通道 > 唯一 gitea 通道。选错通道会把仓库登记到别的
// 站点下，后续所有会话都指向错误的实例。
func TestSelectRepoGitea(t *testing.T) {
	t.Run("显式 host 优先", func(t *testing.T) {
		file := &instances.File{Channels: []instances.Channel{
			{Type: instances.ChannelGitea, Host: "https://a.example.com"},
			{Type: instances.ChannelGitea, Host: "https://b.example.com"},
		}}
		channel, err := selectRepoGitea(file, "https://b.example.com", "acme/repo")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if channel.Host != "https://b.example.com" {
			t.Errorf("Host = %q, want b 站点", channel.Host)
		}
	})

	t.Run("显式 host 不在配置中要报可行动的错", func(t *testing.T) {
		file := &instances.File{Channels: []instances.Channel{
			{Type: instances.ChannelGitea, Host: "https://a.example.com"},
		}}
		_, err := selectRepoGitea(file, "https://unknown.example.com", "acme/repo")
		if err == nil {
			t.Fatal("error = nil, want 未登记站点报错")
		}
		if !strings.Contains(err.Error(), "login add") {
			t.Errorf("error = %v, want 指出修复方式（先 assistant login add）", err)
		}
	})

	t.Run("autoHosts 命中配置才生效", func(t *testing.T) {
		file := &instances.File{Channels: []instances.Channel{
			{Type: instances.ChannelGitea, Host: "https://a.example.com"},
			{Type: instances.ChannelGitea, Host: "https://b.example.com"},
		}}
		// 第一个 hint 不在配置里，第二个在：应选第二个
		channel, err := selectRepoGitea(file, "", "acme/repo", "https://git@unconfigured", "https://b.example.com")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if channel.Host != "https://b.example.com" {
			t.Errorf("Host = %q, want 命中的 b 站点", channel.Host)
		}
	})

	t.Run("按已登记仓库定位", func(t *testing.T) {
		file := &instances.File{Channels: []instances.Channel{
			{Type: instances.ChannelGitea, Host: "https://a.example.com", Repos: []instances.Repo{{Name: "acme/repo"}}},
			{Type: instances.ChannelGitea, Host: "https://b.example.com", Repos: []instances.Repo{{Name: "other/repo"}}},
		}}
		channel, err := selectRepoGitea(file, "", "acme/repo")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if channel.Host != "https://a.example.com" {
			t.Errorf("Host = %q, want 登记了该仓库的 a 站点", channel.Host)
		}
	})

	t.Run("多站点且无其它线索时报歧义", func(t *testing.T) {
		file := &instances.File{Channels: []instances.Channel{
			{Type: instances.ChannelGitea, Host: "https://a.example.com"},
			{Type: instances.ChannelGitea, Host: "https://b.example.com"},
		}}
		_, err := selectRepoGitea(file, "", "acme/repo")
		if err == nil {
			t.Fatal("error = nil, want 歧义报错")
		}
		if !strings.Contains(err.Error(), "--host") {
			t.Errorf("error = %v, want 指出用 --host 消歧", err)
		}
	})

	t.Run("唯一 gitea 通道直接选中", func(t *testing.T) {
		file := &instances.File{Channels: []instances.Channel{
			{Type: instances.ChannelGitea, Host: "https://only.example.com"},
			{Type: instances.ChannelTelegram, BotToken: "t"},
		}}
		channel, err := selectRepoGitea(file, "", "acme/repo")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if channel.Host != "https://only.example.com" {
			t.Errorf("Host = %q, want 唯一的 gitea 站点", channel.Host)
		}
	})

	t.Run("没有 gitea 通道时报错", func(t *testing.T) {
		file := &instances.File{Channels: []instances.Channel{
			{Type: instances.ChannelTelegram, BotToken: "t"},
		}}
		_, err := selectRepoGitea(file, "", "acme/repo")
		if err == nil {
			t.Fatal("error = nil, want 没有 gitea 通道报错")
		}
		if !strings.Contains(err.Error(), "login add") {
			t.Errorf("error = %v, want 指出先登录平台", err)
		}
	})
}
