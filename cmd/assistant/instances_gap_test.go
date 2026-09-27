package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
)

// TestSetupConfigWritePathPrefersExplicitOverEnv 断言 setup/login 写配置的落点解析
// 优先级：--config > ASSISTANT_CONFIG > 平台标准缺省。
//
// 这条优先级此前只被测过缺省那一支：显式指定与显式环境变量两支都没人走过。写错
// 落点的后果不是报错，而是静默把配置写到别处——操作者改完 config.json 重启 daemon，
// 配置却从未生效，且没有任何提示；对 setup 而言更糟：它会把机器人账号与令牌的登记
// 写进一份陌生文件。所以三支必须逐条钉死，尤其「显式压过环境变量」这一条：只测缺省
// 值的话，优先级写反（先读环境变量）也照样通过。
func TestSetupConfigWritePathPrefersExplicitOverEnv(t *testing.T) {
	defaultPath, err := instances.DefaultConfigPath()
	if err != nil {
		t.Fatalf("本机应能解析标准配置路径：%v", err)
	}

	cases := []struct {
		name       string
		configPath string
		envPath    string
		want       string
	}{
		{name: "显式 --config 优先", configPath: "/tmp/explicit/config.json", envPath: "/tmp/env/config.json", want: "/tmp/explicit/config.json"},
		{name: "无 --config 时用 ASSISTANT_CONFIG", configPath: "", envPath: "/tmp/env/config.json", want: "/tmp/env/config.json"},
		{name: "两者皆空时用平台缺省", configPath: "", envPath: "", want: defaultPath},
		{name: "只有空白字符视为未指定", configPath: "   ", envPath: "  \t ", want: defaultPath},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("ASSISTANT_CONFIG", testCase.envPath)
			got, err := setupConfigWritePath(testCase.configPath)
			if err != nil {
				t.Fatalf("解析写配置落点失败：%v", err)
			}
			if got != testCase.want {
				t.Errorf("写配置落点应为 %q，got %q", testCase.want, got)
			}
		})
	}
}

// TestNewInstanceManagerReportsMissingReviewToken 断言构造 Manager 时缺 review 用途
// 令牌立即报错。
//
// review 令牌是同步待办的基础：缺它时 Manager 根本构造不出来（不是降级），所以错误
// 必须让操作者去 login add 补凭据，而不是去查网络连通性——错误若被写成「站点不可达」
// 或干脆吞掉，label-sync 会在无令牌的客户端上继续，把所有请求都发成匿名。
func TestNewInstanceManagerReportsMissingReviewToken(t *testing.T) {
	storePath := isolateCredentials(t)
	if err := credentials.Save(storePath, &credentials.File{}); err != nil {
		t.Fatal(err)
	}
	_, err := newInstanceManager(t.Context(), storePath, instances.Instance{
		Host:     "https://plain.example.com",
		Reviewer: instances.Account{Name: "ai"},
	}, instances.Repo{}, func(string, ...any) {})
	if err == nil {
		t.Fatal("缺 review 用途令牌时应报错")
	}
	if !strings.Contains(err.Error(), "缺少") {
		t.Errorf("错误应点明缺少令牌：%v", err)
	}
}

// TestRunLabelSyncFailsWhenStoreIsCorrupt 断言 env 模式（无配置文件）下，动作命令
// 真的跑不动时必须报错，而不是静默成功退出 0。
//
// env 模式是「裸跑 CI」的形态：只有 GITEA_HOST / GITEA_ACCESS_TOKEN，没有配置文件，
// 这里也刻意不放任何凭据——裸跑的操作者拿不到登录态是常态。危险不在于「没令牌」，
// 而在于把「一个仓库都没同步成」降级成绿色退出：站点拒绝可见仓库查询时若被吞掉，
// 操作者拿到的是「跑过了且没问题」的结论，而实际上一次同步根本没发生。
//
// 站点用可达的本地替身（而不是随便一个域名）：本机 DNS 不通的话会先冒出解析错误，
// 测出来的就成了网络分支。断言错误文本必须来自站点拒绝，才能证明这条链真的走到了
// 可见仓库查询。
func TestRunLabelSyncFailsWhenStoreIsCorrupt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	isolateCredentials(t)

	fake := newFakeGitea(t)
	fake.login["base-token"] = "ai"
	// 站点侧拒绝可见仓库查询：这是 Manager 的第一个调用，也是动作命令唯一的入口，
	// 它在 403 下失败意味着一次同步根本没发生。
	fake.reposStatus = http.StatusForbidden

	t.Setenv("GITEA_HOST", fake.server.URL)
	t.Setenv("GITEA_ACCESS_TOKEN", "base-token")
	t.Setenv("GITEA_REPOSITORY", "acme/rocket")

	var stderr strings.Builder
	err := runLabelSync(t.Context(), io.Discard, &stderr, commandOptions{Repository: "acme/rocket"})
	if err == nil {
		t.Fatalf("同步失败时 label-sync 应报错，而不是静默成功：\n%s", stderr.String())
	}
	if !strings.Contains(err.Error(), "repositories rejected") {
		t.Errorf("错误应来自站点拒绝这次查询：%v", err)
	}
}
