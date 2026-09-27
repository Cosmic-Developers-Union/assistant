package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/dispatcher"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"

	"github.com/spf13/cobra"
)

// fakeGitea 是 Gitea 的最小可编程替身：只实现 checkTargetsHealth / ListWork
// 真正会打到的端点。它存在的理由是这些函数的价值全在「按站点口径
// 校验令牌、把待办清单渲染成人能读的文本」，用假实现才能把每种失败姿态
// （版本不可达、reviewer 令牌过期、admin 降级）单独复现；真实 Gitea 无法
// 在单测里构造这些姿态。
type fakeGitea struct {
	server *httptest.Server
	// login 是 /api/v1/user 按 Authorization 头返回的账号名；未登记的头一律 401。
	login map[string]string
	// version 是 /api/v1/version 返回的版本号；空串表示返回 500（实例不可用）。
	version string
	// issues / pulls 是仓库列表端点返回的条目（按仓库全名索引）。
	issues map[string][]map[string]any
	pulls  map[string][]map[string]any
	// comments 是条目评论（GET /repos/{o}/{r}/issues/{n}/comments），键为
	// "<owner>/<repo>#<编号>"：FollowUpMessages 的过滤契约（跳过 reviewer 自己的
	// 评论、跳过空正文）只能靠喂进真实形态的评论清单来钉。
	comments map[string][]map[string]any
	// reviews 是 PR 的 review 清单（GET /repos/{o}/{r}/pulls/{n}/reviews），
	// 同样按 "<owner>/<repo>#<编号>" 索引：完成判定看的就是它的 user 与 submitted。
	reviews map[string][]map[string]any
	// repositories 是 /api/v1/user/repos 返回的仓库全名清单：Manager 会拿它
	// 校验目标仓库对当前令牌可见，清单里没有目标仓库就直接报错。
	repositories []string
	// reposStatus 非零时 /api/v1/user/repos 返回它（而不是仓库清单）。
	//
	// 「可见仓库」是 Manager.Check 的第一个调用，也是 label-sync / check 这类
	// 动作唯一的入口：它在 401/403/5xx 下失败意味着一次巡检根本没发生。这份失败
	// 必须能被构造出来，否则「动作失败时错误是否冒到最外层」的分支只能靠真实站点
	// 才能走到。
	reposStatus int
}

func newFakeGitea(t *testing.T) *fakeGitea {
	t.Helper()
	fake := &fakeGitea{
		login:        map[string]string{},
		version:      "1.22.0",
		issues:       map[string][]map[string]any{},
		pulls:        map[string][]map[string]any{},
		comments:     map[string][]map[string]any{},
		reviews:      map[string][]map[string]any{},
		repositories: []string{},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		switch {
		case path == "/version":
			if fake.version == "" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"version": fake.version})
		case path == "/user":
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
			name, ok := fake.login[token]
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"message": "unauthorized"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": name, "user_name": name})
		case path == "/user/repos":
			// 同步令牌可见的仓库清单（check --wait 与不限仓库的 Manager 都靠它）：
			// 必须是列表形态，否则调用方拿到 "{}" 会当成反序列化错误；内容取自
			// repos 登记表，好让 Manager 的「目标仓库对令牌可见」校验通过。
			if fake.reposStatus != 0 {
				w.WriteHeader(fake.reposStatus)
				json.NewEncoder(w).Encode(map[string]string{"message": "repositories rejected"})
				return
			}
			repositories := []any{}
			for _, full := range fake.repositories {
				owner, name, ok := strings.Cut(full, "/")
				if !ok {
					continue
				}
				repositories = append(repositories, map[string]any{
					"id": len(repositories) + 1, "name": name,
					"owner": map[string]any{"login": owner, "user_name": owner},
				})
			}
			json.NewEncoder(w).Encode(repositories)
		case strings.HasPrefix(path, "/repos/"):
			segments := strings.Split(strings.TrimPrefix(path, "/repos/"), "/")
			if len(segments) < 3 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			full := segments[0] + "/" + segments[1]
			resource := segments[2]
			// 列表端点统一按页提供：第一页给条目，之后给空页（nextPage 靠
			// 条目数判断是否续页）。
			if r.URL.Query().Get("page") != "" && r.URL.Query().Get("page") != "1" {
				json.NewEncoder(w).Encode([]any{})
				return
			}
			switch resource {
			case "issues":
				// /issues/{n}/comments 是评论清单（FollowUpMessages 的输入），
				// 不能落到下面的条目列表里——那会让调用方把 Issue 当评论解析。
				if len(segments) >= 5 && segments[4] == "comments" {
					json.NewEncoder(w).Encode(payloadOrEmpty(fake.comments[full+"#"+segments[3]]))
					return
				}
				json.NewEncoder(w).Encode(payloadOrEmpty(fake.issues[full]))
			case "pulls":
				// /pulls/{n} 与 /pulls/{n}/reviews 是单条目端点：前者回单个 PR
				// 对象（回数组会让 SDK 报 "cannot unmarshal array into ...
				// PullRequest"），后者回该 PR 的 review 清单。只有 /pulls
				// （列表端点）才是数组。
				if len(segments) >= 5 && segments[4] == "reviews" {
					json.NewEncoder(w).Encode(payloadOrEmpty(fake.reviews[full+"#"+segments[3]]))
					return
				}
				if len(segments) >= 4 {
					if pull, ok := fake.singlePull(full, segments[3]); ok {
						json.NewEncoder(w).Encode(pull)
						return
					}
					w.WriteHeader(http.StatusNotFound)
					return
				}
				json.NewEncoder(w).Encode(payloadOrEmpty(fake.pulls[full]))
			case "reviews":
				json.NewEncoder(w).Encode([]any{})
			case "labels":
				// 标签端点有两种方法：POST 建标签要回一个 gitea.Label 对象
				// （回数组会让 SDK 报 "cannot unmarshal array into ... gitea.Label"），
				// GET 列表才是数组。名字从请求体里回读，好让调用方接到的标签名
				// 确实是它刚创建的那一个。
				if r.Method == http.MethodPost || r.Method == http.MethodPatch {
					var body struct {
						Name string `json:"name"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					json.NewEncoder(w).Encode(map[string]any{
						"id": 1, "name": body.Name, "color": "#000000",
					})
					return
				}
				json.NewEncoder(w).Encode([]any{})
			default:
				json.NewEncoder(w).Encode([]any{})
			}
		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func payloadOrEmpty(items []map[string]any) []map[string]any {
	if items == nil {
		return []map[string]any{}
	}
	return items
}

// singlePull 从登记表里按编号取单个 PR：/pulls/{n} 是单条目端点，
// GetPullRequest（完成判定的第一步）靠它拿 head.sha。
func (f *fakeGitea) singlePull(full, rawNumber string) (map[string]any, bool) {
	number, err := strconv.ParseInt(rawNumber, 10, 64)
	if err != nil {
		return nil, false
	}
	for _, pull := range f.pulls[full] {
		if existing, ok := pull["number"].(int64); ok && existing == number {
			return pull, true
		}
	}
	return nil, false
}

// issuePayload 构造一个 Gitea issue/PR 载荷：IsPull 决定它被折叠成 PR 还是 Issue。
func issuePayload(index int64, title string, isPull bool, labels ...string) map[string]any {
	labelPayloads := make([]map[string]any, 0, len(labels))
	for _, name := range labels {
		labelPayloads = append(labelPayloads, map[string]any{"id": 1, "name": name})
	}
	return map[string]any{
		"id": index, "number": index, "title": title, "state": "open",
		"labels": labelPayloads,
		"pull_request": func() any {
			if isPull {
				return map[string]any{"merged": false}
			}
			return nil
		}(),
	}
}

// fakeGiteaCommand 返回一个绑定了假 Gitea 的目标：instance.Host 故意留空让
// checkTargetsHealth 走 config.Host 兜底分支（这正是一个要钉住的行为——通道名
// 与展示名不一致时日志仍要点出站点的真实身份）。
func fakeGiteaCommand(t *testing.T, fake *fakeGitea, configPath string) (dispatchTarget, *bytes.Buffer) {
	t.Helper()
	client, err := status.NewClient(fake.server.URL, "reviewer-token")
	if err != nil {
		t.Fatal(err)
	}
	host := fake.server.URL
	stderr := &bytes.Buffer{}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(stderr)
	target := dispatchTarget{
		repo: instances.Repo{Name: "acme/rocket"},
		config: dispatcher.Config{
			Host:        host,
			ConfigPath:  configPath,
			Repository:  status.Repository{Owner: "acme", Name: "rocket"},
			AccessToken: "reviewer-token",
			Reviewer:    "ai",
			BaseBranch:  "main",
		},
		client: client,
	}
	return target, stderr
}

// TestCheckTargetsHealthReportsEachTokenRole 断言健康检查把三条身份事实分别落
// 到日志：实例可达 + reviewer 是谁、merge/admin 用途令牌各自以哪个账号可用。
// 三者任一沉默都会让操作者在评审卡住时无从判断是「实例挂了」还是「令牌过期」。
func TestCheckTargetsHealthReportsEachTokenRole(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := newFakeGitea(t)
	fake.login["reviewer-token"] = "ai"
	fake.login["merge-token"] = "merge"
	fake.login["admin-token"] = "root"
	configPath := filepath.Join(t.TempDir(), "config.json")
	target, _ := fakeGiteaCommand(t, fake, configPath)
	writePurposeCredentials(t, []credentials.Credential{
		{Host: target.config.Host, User: "ai", Purpose: credentials.PurposeReview, Token: "reviewer-token"},
		{Host: target.config.Host, User: "merge", Purpose: credentials.PurposeMerge, Token: "merge-token"},
		{Host: target.config.Host, User: "root", Purpose: credentials.PurposeAdmin, Token: "admin-token"},
	})

	var logs []string
	if err := checkTargetsHealth(t.Context(), []dispatchTarget{target}, func(line string) {
		logs = append(logs, line)
	}); err != nil {
		t.Fatalf("checkTargetsHealth: %v", err)
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{
		"instance " + target.config.Host + " 可用（Gitea 1.22.0，reviewer @ai）",
		"instance " + target.config.Host + " merge @merge 可用",
		"instance " + target.config.Host + " admin @root 可用",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("日志缺少 %q：\n%s", want, joined)
		}
	}
}

// TestCheckTargetsHealthSurfacesFailuresAndMissingTokens 断言三类异常各有明确
// 出口：实例不可达直接失败（不能带着聋掉的调度器常驻）、reviewer 令牌无效
// 直接失败（后续所有评审都会以错身份落库）、merge 令牌缺失只提示（合并不
// 参与内容评审，缺它不该拦住评审启动）。
func TestCheckTargetsHealthSurfacesFailuresAndMissingTokens(t *testing.T) {
	t.Run("实例不可达", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		fake := newFakeGitea(t)
		fake.version = ""
		target, _ := fakeGiteaCommand(t, fake, filepath.Join(t.TempDir(), "config.json"))
		err := checkTargetsHealth(t.Context(), []dispatchTarget{target}, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "不可用") {
			t.Fatalf("err = %v, want 不可用", err)
		}
		if !strings.Contains(err.Error(), "健康检查失败 (HTTP 500)") {
			t.Errorf("err 未点出 HTTP 状态：%v", err)
		}
	})

	t.Run("reviewer 令牌无效", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		fake := newFakeGitea(t)
		target, _ := fakeGiteaCommand(t, fake, filepath.Join(t.TempDir(), "config.json"))
		err := checkTargetsHealth(t.Context(), []dispatchTarget{target}, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "reviewer 令牌校验失败") {
			t.Fatalf("err = %v, want reviewer 令牌校验失败", err)
		}
	})

	t.Run("缺 merge 令牌只提示", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		fake := newFakeGitea(t)
		fake.login["reviewer-token"] = "ai"
		configPath := filepath.Join(t.TempDir(), "config.json")
		target, _ := fakeGiteaCommand(t, fake, configPath)
		var logs []string
		if err := checkTargetsHealth(t.Context(), []dispatchTarget{target}, func(line string) {
			logs = append(logs, line)
		}); err != nil {
			t.Fatalf("缺 merge 令牌不应报错: %v", err)
		}
		joined := strings.Join(logs, "\n")
		if !strings.Contains(joined, "缺少 merge 用途令牌") {
			t.Errorf("缺少 merge 提示：\n%s", joined)
		}
		if !strings.Contains(joined, "assistant setup --host "+target.config.Host) {
			t.Errorf("提示未给出补凭据的命令：\n%s", joined)
		}
	})

	t.Run("admin 令牌校验失败降级为警告", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		fake := newFakeGitea(t)
		fake.login["reviewer-token"] = "ai"
		fake.login["merge-token"] = "merge"
		configPath := filepath.Join(t.TempDir(), "config.json")
		target, _ := fakeGiteaCommand(t, fake, configPath)
		writePurposeCredentials(t, []credentials.Credential{
			{Host: target.config.Host, User: "ai", Purpose: credentials.PurposeReview, Token: "reviewer-token"},
			{Host: target.config.Host, User: "merge", Purpose: credentials.PurposeMerge, Token: "merge-token"},
			// admin 令牌错误：只警告，不能拦住启动。
			{Host: target.config.Host, User: "root", Purpose: credentials.PurposeAdmin, Token: "stale-token"},
		})
		var logs []string
		if err := checkTargetsHealth(t.Context(), []dispatchTarget{target}, func(line string) {
			logs = append(logs, line)
		}); err != nil {
			t.Fatalf("admin 令牌失败不应报错: %v", err)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "admin 令牌校验失败") {
			t.Errorf("缺少 admin 降级警告：\n%s", strings.Join(logs, "\n"))
		}
	})

	t.Run("merge 令牌校验失败直接报错", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		fake := newFakeGitea(t)
		fake.login["reviewer-token"] = "ai"
		configPath := filepath.Join(t.TempDir(), "config.json")
		target, _ := fakeGiteaCommand(t, fake, configPath)
		writePurposeCredentials(t, []credentials.Credential{
			{Host: target.config.Host, User: "ai", Purpose: credentials.PurposeReview, Token: "reviewer-token"},
			{Host: target.config.Host, User: "merge", Purpose: credentials.PurposeMerge, Token: "stale-merge"},
		})
		err := checkTargetsHealth(t.Context(), []dispatchTarget{target}, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "merge 令牌校验失败") {
			t.Fatalf("err = %v, want merge 令牌校验失败", err)
		}
	})
}

// TestCheckTargetsHealthChecksEachHostOnce 断言同站点只探一次：多仓库配置下
// 逐仓库重探会把启动延迟乘上仓库数，也让日志重复到看不出重点。
func TestCheckTargetsHealthChecksEachHostOnce(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fake := newFakeGitea(t)
	fake.login["reviewer-token"] = "ai"
	configPath := filepath.Join(t.TempDir(), "config.json")
	first, _ := fakeGiteaCommand(t, fake, configPath)
	second := first
	second.repo = instances.Repo{Name: "acme/lab"}
	second.config.Repository = status.Repository{Owner: "acme", Name: "lab"}

	var logs []string
	if err := checkTargetsHealth(t.Context(), []dispatchTarget{first, second}, func(line string) {
		logs = append(logs, line)
	}); err != nil {
		t.Fatalf("checkTargetsHealth: %v", err)
	}
	available := 0
	for _, line := range logs {
		if strings.Contains(line, "可用（Gitea") {
			available++
		}
	}
	if available != 1 {
		t.Errorf("同站点应只探一次，实际 %d 次：\n%s", available, strings.Join(logs, "\n"))
	}
}

// TestNewDispatchClientDebugLogsRequests 断言 --debug 时每一次 Gitea 往返都留
// 一条站点相对路径的日志：这是「系统为什么这样做」在外部调用面的唯一证据，
// 路径必须剥掉主机与 /api/v1（否则日志里混进令牌宿主与噪声前缀）。
func TestNewDispatchClientDebugLogsRequests(t *testing.T) {
	fake := newFakeGitea(t)
	fake.login["reviewer-token"] = "ai"
	host := fake.server.URL

	stderr := os.Stderr
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = write
	t.Cleanup(func() { os.Stderr = stderr })
	defer func() { os.Stderr = stderr; write.Close() }()

	client, err := newDispatchClient(dispatcher.Config{Host: host, AccessToken: "reviewer-token"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AuthenticatedUser(t.Context()); err != nil {
		t.Fatal(err)
	}
	write.Close()
	captured, _ := os.ReadFile("/dev/stdin")
	_ = captured
	buffer := &bytes.Buffer{}
	if _, err := buffer.ReadFrom(read); err != nil {
		t.Fatal(err)
	}
	line := buffer.String()
	if !strings.Contains(line, "[dispatch:d ") {
		t.Fatalf("未输出 debug 请求日志：%q", line)
	}
	if !strings.Contains(line, "Gitea GET /user → 200 ") {
		t.Errorf("日志未按「method + 站点相对路径 + 状态码」渲染：%q", line)
	}
	if strings.Contains(line, host) {
		t.Errorf("日志不应含主机名：%q", line)
	}
	if strings.Contains(line, "reviewer-token") {
		t.Errorf("日志不应含令牌：%q", line)
	}
}

// TestNewDispatchClientWithoutDebugStaysSilent 断言非 --debug 时不装请求日志
// 钩子：默认级日志只讲「发生了什么」，把每次往返都倒出来会让真正重要的结果
// 被淹没（这也是 --debug 与 --verbose 的分界）。
func TestNewDispatchClientWithoutDebugStaysSilent(t *testing.T) {
	fake := newFakeGitea(t)
	fake.login["reviewer-token"] = "ai"
	host := fake.server.URL

	stderr := os.Stderr
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = write
	defer func() { os.Stderr = stderr; write.Close() }()

	client, err := newDispatchClient(dispatcher.Config{Host: host, AccessToken: "reviewer-token"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AuthenticatedUser(t.Context()); err != nil {
		t.Fatal(err)
	}
	write.Close()
	os.Stderr = stderr
	buffer := &bytes.Buffer{}
	if _, err := buffer.ReadFrom(read); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buffer.String(), "Gitea GET") {
		t.Errorf("未开 debug 时不应输出请求日志：%q", buffer.String())
	}
}

// TestNewDispatchClientCarriesTokenAndHost 断言 debug 与非 debug 两条分支都
// 把配置里的 host/token 原样带进出站请求：令牌错带到别处会让评审以错身份落
// 库，host 错则连不上站点，两者都必须在网络层之前就与配置一致。
func TestNewDispatchClientCarriesTokenAndHost(t *testing.T) {
	fake := newFakeGitea(t)
	fake.login["reviewer-token"] = "ai"

	// 非 debug 分支：令牌正确时能认到身份。
	client, err := newDispatchClient(dispatcher.Config{Host: fake.server.URL, AccessToken: "reviewer-token"}, false)
	if err != nil {
		t.Fatalf("newDispatchClient: %v", err)
	}
	login, err := client.AuthenticatedUser(t.Context())
	if err != nil {
		t.Fatalf("AuthenticatedUser: %v", err)
	}
	if login != "ai" {
		t.Errorf("login = %q, want ai", login)
	}

	// 令牌错误时假站点返回 401：错误必须冒泡（否则「令牌错」会被当成功）。
	bad, err := newDispatchClient(dispatcher.Config{Host: fake.server.URL, AccessToken: "stale"}, false)
	if err != nil {
		t.Fatalf("newDispatchClient: %v", err)
	}
	if _, err := bad.AuthenticatedUser(t.Context()); err == nil {
		t.Error("错误令牌应报错")
	}
}

// TestPreviewProvidersDistinguishesPresetAndGlobalOptimization 断言 dry-run 的
// provider 预告把三种情况分开说：用户自定义 provider、内置预设（要加标注，
// 否则和自定义同名时无法分辨）、以及叠加全局优化时的项数清单。dry-run 的
// 全部价值就是让人在不产生副作用的前提下看到将发生什么。
func TestPreviewProvidersDistinguishesPresetAndGlobalOptimization(t *testing.T) {
	stderr := &bytes.Buffer{}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(stderr)

	targets := []dispatchTarget{
		// 无 provider 也无全局优化：不输出任何行（沉默即「内置缺省」）。
		{repo: instances.Repo{Name: "acme/silent"}},
		{
			repo: instances.Repo{Name: "acme/custom"},
			config: dispatcher.Config{
				ProviderName: "my-provider",
				Provider:     claudecfg.Overrides{Env: map[string]string{"MY_KEY": "v"}},
			},
		},
		{
			repo: instances.Repo{Name: "acme/preset"},
			config: dispatcher.Config{
				ProviderName:  "anthropic",
				Optimizations: claudecfg.Overrides{Env: map[string]string{"ZED": "1", "ABC": "2"}},
			},
		},
	}
	previewProviders(command, targets)
	got := stderr.String()
	if strings.Contains(got, "acme/silent") {
		t.Errorf("无 provider 的目标不应出现在预告里：%q", got)
	}
	if !strings.Contains(got, "acme/custom 使用 provider my-provider（env 1 项：MY_KEY") {
		t.Errorf("自定义 provider 行缺失：%q", got)
	}
	if strings.Contains(got, "my-provider 内置预设") {
		t.Errorf("自定义 provider 不应被标注成内置预设：%q", got)
	}
	if !strings.Contains(got, "anthropic 内置预设") {
		t.Errorf("内置预设缺少标注：%q", got)
	}
	if !strings.Contains(got, "全局优化（env 2 项：ABC、ZED") {
		t.Errorf("全局优化项名未按字典序列出：%q", got)
	}
	if !strings.Contains(got, "不写仓库文件") {
		t.Errorf("预告应说明零副作用：%q", got)
	}
}

// TestPreviewManagedTargetsSaysCloneOrAlign 断言 dry-run 对受管克隆的两种动作
// 分别预告：没有 .git 的是「将克隆」，已有克隆的是「将对齐基线」。二者对操作
// 者的意义完全不同（一个要网络，一个只 fetch），混作一谈会让人误判耗时。
// 显式检出（非受管）不预告——它不受 assistant 管辖。
func TestPreviewManagedTargetsSaysCloneOrAlign(t *testing.T) {
	stderr := &bytes.Buffer{}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(stderr)

	empty := t.TempDir()
	cloned := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cloned, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	previewManagedTargets(command, []dispatchTarget{
		{repo: instances.Repo{Name: "acme/fresh"}, repoDir: empty, managed: true, config: dispatcher.Config{BaseBranch: "main"}},
		{repo: instances.Repo{Name: "acme/existing"}, repoDir: cloned, managed: true, config: dispatcher.Config{BaseBranch: "release"}},
		{repo: instances.Repo{Name: "acme/explicit"}, repoDir: t.TempDir(), managed: false},
	})
	got := stderr.String()
	if !strings.Contains(got, "dry-run：将克隆 acme/fresh → "+empty) {
		t.Errorf("缺克隆预告：%q", got)
	}
	if !strings.Contains(got, "dry-run：检测轮前将把 acme/existing 对齐 origin/release") {
		t.Errorf("缺对齐预告：%q", got)
	}
	if strings.Contains(got, "acme/explicit") {
		t.Errorf("显式检出不应被预告：%q", got)
	}
}

// TestResolveDispatchTargetsRejectsRetiredAndForeignConfigs 断言配置入口把两种
// 「看起来像配置但不是」的输入当场挡下：已退役的 --run 与旧 run.yaml。二者
// 都曾是可运行的输入，静默接受会让操作者以为在跑新逻辑。
func TestResolveDispatchTargetsRejectsRetiredAndForeignConfigs(t *testing.T) {
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})

	if _, err := resolveDispatchTargets(command, "", "", &dispatcherOptions{Run: "run.yaml"}); err == nil ||
		!strings.Contains(err.Error(), "已退役") {
		t.Fatalf("--run err = %v, want 已退役", err)
	}

	yamlPath := filepath.Join(t.TempDir(), "run.yaml")
	if err := os.WriteFile(yamlPath, []byte("instances: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := resolveDispatchTargets(command, "", yamlPath, &dispatcherOptions{})
	if err == nil || !strings.Contains(err.Error(), "不是 config.json") {
		t.Fatalf("run.yaml err = %v, want 不是 config.json", err)
	}
}

// TestResolveDispatchTargetsRejectsUnregisteredRepo 断言 --repo 选了配置里不存在
// 的仓库时要点名报错：静默产出空目标会让 run 起来后什么都不做，操作者却以为
// 仓库在跑。
func TestResolveDispatchTargetsRejectsUnregisteredRepo(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{
		Channels: []instances.Channel{{
			Type: instances.ChannelGitea, Host: "https://gitea.example.com",
			Repos: []instances.Repo{{Name: "acme/rocket"}},
		}},
		Runtimes: map[string]instances.Runtime{"main": {Root: data}},
	}); err != nil {
		t.Fatal(err)
	}
	withReviewCredential(t, "https://gitea.example.com")
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})

	_, err := resolveDispatchTargets(command, "acme/nope", configPath, &dispatcherOptions{})
	if err == nil || !strings.Contains(err.Error(), "不在 gitea 通道的 repos 中") {
		t.Fatalf("err = %v, want 不在 gitea 通道的 repos 中", err)
	}
}

// TestResolveDispatchTargetsChatOnlyConfigIsAllowed 断言「只配了对话通道、没有
// 可运行仓库」的配置不是错误：对话服务与状态查询照常可用，真正判空由调用方
// 在装配完 daemon 之后决定。这里把该分支锁死以免有人把判空提前成硬错误。
func TestResolveDispatchTargetsChatOnlyConfigIsAllowed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{
		Channels: []instances.Channel{{Type: instances.ChannelTelegram, Name: "tg", BotToken: "t"}},
		Runtimes: map[string]instances.Runtime{"main": {Root: data}},
	}); err != nil {
		t.Fatal(err)
	}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})

	targets, err := resolveDispatchTargets(command, "", configPath, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("只有对话通道时应放行（判空交给调用方）: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("targets = %+v, want 空", targets)
	}
}

// TestResolveDispatchTargetsNamesChannelWithoutRepos 断言配了 gitea 但没有 repos
// 的通道要在 stderr 点名并给出 setup 指引：这是最常见的半配置状态，不点名就
// 只剩一个「共处理 0 个」的哑谜。
func TestResolveDispatchTargetsNamesChannelWithoutRepos(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{
		Channels: []instances.Channel{{
			Type: instances.ChannelGitea, Host: "https://gitea.example.com", Reviewer: "ai",
			Repos: []instances.Repo{{Name: "acme/rocket"}},
		}},
		Runtimes: map[string]instances.Runtime{"main": {Root: data}},
	}); err != nil {
		t.Fatal(err)
	}
	withReviewCredential(t, "https://gitea.example.com")
	// 追加一个没有 repos 的 gitea 通道（不同名，避免与上面合并）。
	file, err := instances.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	file.Channels = append(file.Channels, instances.Channel{Type: instances.ChannelGitea, Name: "empty", Host: "https://gitea2.example.com"})
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}
	command := &cobra.Command{}
	targetsOut := &bytes.Buffer{}
	command.SetOut(targetsOut)
	stderr := &bytes.Buffer{}
	command.SetErr(stderr)

	if _, err := resolveDispatchTargets(command, "", configPath, &dispatcherOptions{}); err != nil {
		t.Fatalf("resolveDispatchTargets: %v", err)
	}
	if !strings.Contains(stderr.String(), "跳过 gitea 通道 gitea/empty：未配置仓库") {
		t.Errorf("未点名空通道：%q", stderr.String())
	}
}

// TestRunDispatchLoopDryRunPreviewsAndReturns 断言 run --dry-run 的完整出口：
// 预告受管克隆与 provider、登记目标（含状态库）、跑一次 DryRunPass，然后正常
// 返回而不进入常驻等待。这是唯一能在单测里走完 run 主干的路径，也是「dry-run
// 零副作用」这句话的可执行定义。
func TestRunDispatchLoopDryRunPreviewsAndReturns(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	fake := newFakeGitea(t)
	fake.login["reviewer-token"] = "ai"
	fake.issues["acme/rocket"] = []map[string]any{issuePayload(11, "add widget", false, "review")}
	configPath := writeFakeGiteaConfig(t, fake, "acme/rocket")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	command := &cobra.Command{}
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetContext(t.Context())

	err := runDispatchLoop(command, "", configPath, &dispatcherOptions{DryRun: true})
	if err != nil {
		t.Fatalf("runDispatchLoop(dry-run): %v", err)
	}
	if !strings.Contains(stderr.String(), "dry-run：将克隆 acme/rocket") {
		t.Errorf("dry-run 未预告克隆：%q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "dispatch") {
		t.Errorf("dry-run 未在 stdout 留下调度日志：%q", stdout.String())
	}
}

// TestResolveEnvDispatcherFallsBackToEnvironment 断言没有配置文件时的环境变量
// 单实例模式：GITEA_HOST/GITEA_ACCESS_TOKEN/GITEA_REPOSITORY 就能跑，repoDir
// 落到当前目录（或 git 仓库根）。这是「配置文件可选」这条承诺的实现——没有仓库
// 时仍应硬失败，所以仓库身份也必须能纯靠环境变量给全。
func TestResolveEnvDispatcherFallsBackToEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GITEA_HOST", "gitea.example.com")
	t.Setenv("GITEA_ACCESS_TOKEN", "env-token")
	t.Setenv("GITEA_REPOSITORY", "acme/rocket")

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	config, repoDir, err := resolveEnvDispatcher(command, "", &dispatcherOptions{})
	if err != nil {
		t.Fatalf("resolveEnvDispatcher: %v", err)
	}
	if config.Host != "gitea.example.com" || config.AccessToken != "env-token" {
		t.Errorf("config = %+v, want 环境变量解析出的 host/token", config)
	}
	if repoDir == "" {
		t.Error("repoDir 应落到当前目录或其 git 根")
	}
}

// TestDispatcherFlagsSyncMirrorTriState 断言 --sync-mirror 的三态：未指定时
// 留给配置裁决（nil），显式 true/false 时把选择钉死成指针。三态是「配置显式
// 化」的体现——命令行没说的话配置说了算，命令行说了就不许配置翻案。
func TestDispatcherFlagsSyncMirrorTriState(t *testing.T) {
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})

	flags := dispatcherFlags(command, "", &dispatcherOptions{})
	if flags.SyncMirror != nil {
		t.Errorf("未指定时 SyncMirror = %v, want nil（交给配置）", *flags.SyncMirror)
	}

	// 模拟 cobra 解析出 --sync-mirror=false。
	command.Flags().Bool("sync-mirror", true, "test")
	if err := command.Flags().Set("sync-mirror", "false"); err != nil {
		t.Fatal(err)
	}
	flags = dispatcherFlags(command, "", &dispatcherOptions{})
	if flags.SyncMirror == nil || *flags.SyncMirror {
		t.Errorf("显式 false 应钉成 false，got %v", flags.SyncMirror)
	}
}

// writePurposeCredentials 写入带用途的令牌：checkTargetsHealth 的每个用途都从这里解析。
func writePurposeCredentials(t *testing.T, items []credentials.Credential) {
	t.Helper()
	path, err := credentials.Path()
	if err != nil {
		t.Fatal(err)
	}
	store, err := credentials.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		item.TokenName = "assistant-test"
		store.SetCredential(item)
	}
	if err := credentials.Save(path, store); err != nil {
		t.Fatal(err)
	}
}

// writeFakeGiteaConfig 写出一份 host 指向假 Gitea 的配置（含 review 用途凭据），
// 让 resolveDispatchTargets 产出的 target.client 真正打到假站点上。
func writeFakeGiteaConfig(t *testing.T, fake *fakeGitea, repos ...string) string {
	t.Helper()
	host := strings.TrimPrefix(fake.server.URL, "http://")
	configPath := t.TempDir() + "/config.json"
	entries := make([]instances.Repo, 0, len(repos))
	for _, name := range repos {
		entries = append(entries, instances.Repo{Name: name})
	}
	// 同一批仓库也登记到 /user/repos 响应里：Manager 只处理「目标仓库对令牌
	// 可见」的实例，漏登记会让每个走 Manager 的测试都栽在可见性校验上。
	fake.repositories = append(fake.repositories, repos...)
	if err := instances.Save(configPath, &instances.File{
		Channels: []instances.Channel{{
			Type: instances.ChannelGitea, Host: fake.server.URL, Reviewer: "ai",
			Token: "reviewer-token", Repos: entries,
		}},
		Runtimes: map[string]instances.Runtime{"main": {Root: t.TempDir(), IntervalMS: 60000, SessionTimeoutMS: 60000}},
	}); err != nil {
		t.Fatal(err)
	}
	withReviewCredential(t, host)
	return configPath
}
