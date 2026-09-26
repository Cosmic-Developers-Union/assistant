package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"

	"github.com/spf13/cobra"
)

// initFake 是 init 命令族的最小可编程 Gitea 替身。init 的价值全在「用开发者
// 自己的令牌（purpose=mcp）先自证仓库管理员身份，再决定能不能改协作关系与
// 分支保护」——这条判断链需要站点同时给出自证身份、协作者清单与逐人权限，
// 真实 Gitea 无法在单测里摆出每一种姿态（只读协作者、缺 merge 账号、空仓库），
// 所以用替身把每种拒绝理由单独复现。
type initFake struct {
	server *httptest.Server
	// self 是 /api/v1/user 返回的账号名：init 会拿它跟协作者清单比对。
	self string
	// collaborators 是 GET /collaborators 返回的账号名清单。
	collaborators []string
	// permissions 是逐人权限（GET /collaborators/{user}/permission）；缺省 write。
	permissions map[string]string
	// emptyRepo 决定 GET /repos/{owner}/{name} 是否报 empty=true（无默认分支场景）。
	emptyRepo bool
	// failCollaborators 让协作者清单返回 403，复现「令牌没有仓库管理权限」。
	failCollaborators bool
	// recorded 是收到的全部请求（"METHOD PATH"），写操作是否真发生由它证明。
	recorded []string
}

func newInitFake(t *testing.T) *initFake {
	t.Helper()
	fake := &initFake{self: "dev", permissions: map[string]string{}}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fake.recorded = append(fake.recorded, r.Method+" "+r.URL.Path)
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		if !strings.Contains(r.URL.Path, "/collaborators") && strings.HasPrefix(r.URL.Path, "/api/v1/") {
			if strings.TrimPrefix(r.URL.Path, "/api/v1/") == "user" {
				json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": fake.self, "user_name": fake.self})
				return
			}
		}
		switch {
		case path == "/version":
			json.NewEncoder(w).Encode(map[string]string{"version": "1.22.0"})
		case path == "/user":
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": fake.self, "user_name": fake.self})
		case path == "/user/repos":
			json.NewEncoder(w).Encode([]any{})
		case strings.HasSuffix(path, "/collaborators"):
			fake.writeCollaborators(w)
		case strings.Contains(path, "/collaborators/") && strings.HasSuffix(path, "/permission"):
			fake.writePermission(w, path)
		case strings.Contains(path, "/collaborators/"):
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(path, "/branch_protections"):
			fake.writeBranchProtection(w, r.Method)
		case strings.Contains(path, "/labels"):
			fake.writeLabels(w, r.Method)
		case strings.HasPrefix(path, "/repos/"):
			segments := strings.Split(strings.TrimPrefix(path, "/repos/"), "/")
			fake.writeRepo(w, segments, r.URL.RawQuery)
		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

// newInitServerWithFailures 构造看得到失败开关的 init 替身服务端：fail 提供
// 三个「服务端说不」的开关，路由与载荷沿用 initFake 的那一套，只有被开关拦下
// 的端点会改写状态码。
func newInitServerWithFailures(t *testing.T, fail *initFailFake) *httptest.Server {
	t.Helper()
	fake := fail.initFake
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fake.recorded = append(fake.recorded, r.Method+" "+r.URL.Path)
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		if strings.Contains(path, "/labels") && r.Method != http.MethodGet && fail.labelStatus != 0 {
			w.WriteHeader(fail.labelStatus)
			json.NewEncoder(w).Encode(map[string]string{"message": "labels rejected"})
			return
		}
		if strings.Contains(path, "/collaborators") && r.Method != http.MethodGet && fail.collaboratorStatus != 0 {
			w.WriteHeader(fail.collaboratorStatus)
			json.NewEncoder(w).Encode(map[string]string{"message": "collaborators rejected"})
			return
		}
		if strings.TrimPrefix(r.URL.Path, "/api/v1/") == "user" {
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": fake.self, "user_name": fake.self})
			return
		}
		switch {
		case path == "/version":
			json.NewEncoder(w).Encode(map[string]string{"version": "1.22.0"})
		case path == "/user":
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": fake.self, "user_name": fake.self})
		case path == "/user/repos":
			json.NewEncoder(w).Encode([]any{})
		case strings.HasSuffix(path, "/collaborators"):
			fake.writeCollaborators(w)
		case strings.Contains(path, "/collaborators/") && strings.HasSuffix(path, "/permission"):
			fake.writePermission(w, path)
		case strings.Contains(path, "/collaborators/"):
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(path, "/branch_protections"):
			fake.writeBranchProtection(w, r.Method)
		case strings.Contains(path, "/labels"):
			fake.writeLabels(w, r.Method)
		case strings.HasPrefix(path, "/repos/"):
			segments := strings.Split(strings.TrimPrefix(path, "/repos/"), "/")
			fake.writeRepo(w, segments, r.URL.RawQuery)
		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
}

// writeCollaborators 返回协作者清单；failCollaborators 时用 403 表达「令牌
// 读不到协作者」，这正是 init 要求仓库管理员权限的现实形态。
func (f *initFake) writeCollaborators(w http.ResponseWriter) {
	if f.failCollaborators {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"message": "forbidden"})
		return
	}
	users := []any{}
	for index, name := range f.collaborators {
		users = append(users, map[string]any{"id": index + 1, "login": name, "user_name": name})
	}
	json.NewEncoder(w).Encode(users)
}

// writePermission 按 permissions 表返回权限；缺省 write（协作者名单里没有的人
// 也当作 write，避免替身自身成为拒绝理由）。
func (f *initFake) writePermission(w http.ResponseWriter, path string) {
	segments := strings.Split(strings.TrimPrefix(path, "/repos/"), "/")
	user := segments[len(segments)-2]
	permission := "write"
	if value, ok := f.permissions[user]; ok {
		permission = value
	}
	json.NewEncoder(w).Encode(map[string]any{"permission": permission})
}

// writeLabels 复现标签端点：清单（GET）回已经建好的标签，建标签（POST）必须回
// 单个对象——Gitea 的建标签响应是 Label 而非数组，回数组会让 SDK 在反序列化处
// 直接报错，测试就看不到后面真正要断言的收敛行为。
func (f *initFake) writeLabels(w http.ResponseWriter, method string) {
	if method == http.MethodGet {
		json.NewEncoder(w).Encode([]any{})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"id": 1, "name": "type/bug", "color": "#d73a4a"})
}

// writeBranchProtection 复现既有保护（GET）与新建保护（POST）两条路径。
func (f *initFake) writeBranchProtection(w http.ResponseWriter, method string) {
	if method == http.MethodGet {
		json.NewEncoder(w).Encode([]any{})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"id": 1, "rule_name": "main", "branch_name": "main"})
}

// writeRepo 处理原始 do() 走的两条仓库端点：两段路径是单仓库详情（init
// branch-protection 靠它取默认分支），否则是搜索端点（GetRepo 的兜底）。
func (f *initFake) writeRepo(w http.ResponseWriter, segments []string, rawQuery string) {
	if len(segments) < 2 || segments[0] == "" || segments[1] == "" {
		// 搜索形态：?q=owner/name 时回一条命中，否则回空清单。
		if strings.Contains(rawQuery, "q=") {
			json.NewEncoder(w).Encode(map[string]any{"data": []any{f.repoPayload()}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(f.repoPayload())
}

// repoPayload 是仓库详情载荷：空仓库用 empty=true 且没有默认分支表达。
func (f *initFake) repoPayload() map[string]any {
	if f.emptyRepo {
		return map[string]any{"id": 1, "name": "rocket", "empty": true}
	}
	owner := map[string]any{"login": "acme", "user_name": "acme"}
	return map[string]any{
		"id": 1, "name": "rocket", "owner": owner,
		"default_branch": "main", "empty": false,
	}
}

// has 判断某个 "METHOD PATH" 是否出现过：写操作有没有真的发生，靠它断言。
func (f *initFake) has(entry string) bool {
	for _, recorded := range f.recorded {
		if recorded == entry {
			return true
		}
	}
	return false
}

// initLeafFixture 是 init 叶子命令的执行现场：out 收 stdout，leaf 是该叶子
// 命令本身（子测试直接读 leaf.Flags() 断言挂载关系），root 负责执行。
type initLeafFixture struct {
	root *cobra.Command
	leaf *cobra.Command
	out  *bytes.Buffer
	args []string
}

// newInitLeafForTest 是 init 叶子的统一装配。init 的叶子自己不带任何旗标：
// --config 是 root 的持久旗标（init 只持有指向 root options 的指针），
// --repo/--dry-run 是父命令 init 的持久旗标；RunE 里一律走 command.Flags().
// GetString/GetBool 读取。因此这里照 cobra 的做法把叶子挂进 init、init 挂进
// root，再驱动 root 执行。
//
// 参数一律用 --flag=value 的等号形式：--repo 的值形如 owner/name，而 cobra 的
// stripFlags/argsMinusFirstX 对「--flag value」这种空格形式只摘掉旗标名、把值
// 留在原位，于是值会被 Find 当成子命令名再找一遍，最后落到 init 上打印帮助，
// 测试就永远停在「什么也没发生」。等号形式不产生这个歧义。
//
// configPath 为空时不注入 --config（用于「完全没有配置文件」的场景）。
func newInitLeafForTest(t *testing.T, name, configPath string, args ...string) initLeafFixture {
	t.Helper()
	var configFlag string
	options := initTestOptions()
	initCommand := newInitCommand(&configFlag)
	root := newRootCommand(
		&bytes.Buffer{}, &bytes.Buffer{},
		initTestRunner(),
		initTestRunner(),
		initTestRunner(),
	)
	// root 默认用 root options.ConfigPath 构造了 init 子树，但它必然解析不到
	// 本测试的配置；把 root 默认挂上的子命令全部摘掉（含 init），只保留下面这个
	// 绑定到 configFlag 的，让发出的每个请求都落在假站点上。
	root.RemoveCommand(root.Commands()...)
	root.AddCommand(initCommand)
	initCommand.AddCommand(
		newInitReviewerCommand(&configFlag, options),
		newInitMergeCommand(&configFlag, options),
		newInitLabelsCommand(&configFlag, options),
		newInitBranchProtectionCommand(&configFlag, options),
		newInitActionsCommand(options),
	)
	configFlag = configPath
	leaf, _, err := root.Find([]string{"init", name})
	if err != nil || leaf.Name() != name {
		t.Fatalf("在命令树中定位 init %s: %v（得到 %s）", name, err, leaf.Name())
	}

	argv := append([]string{"init", name}, args...)
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(&bytes.Buffer{})
	root.SetContext(t.Context())
	root.SetArgs(argv)
	return initLeafFixture{root: root, leaf: leaf, out: out, args: argv}
}

// run 执行装配好的命令树。每次都重新装一遍 argv：cobra 的 Execute 会把已解析
// 的参数留在原地，同一个 fixture 执行两次时第二次会读到上一轮的残留；重设 argv
// 让每次执行都从同一份原始参数开始。同时显式给出非 nil 的 args——cobra 只在
// args 为 nil 时才回落到 os.Args[1:]，那条路上会读到 go test 自己的旗标，让 Find
// 定位失败并打印帮助。
func (fixture initLeafFixture) run() error {
	fixture.root.SetArgs(fixture.args)
	return fixture.root.Execute()
}

// initTestRunner 返回占位的 root 子命令实现：init 测试只驱动 init 叶子，
// 其余命令的实现不重要，但 newRootCommand 要求提供。
func initTestRunner() managerRunner {
	return func(context.Context, io.Writer, io.Writer, commandOptions) error { return nil }
}

// initTestOptions 返回一份带缺省机器人账号的 init 选项：五个叶子命令都从它
// 取 reviewer/merger 的名字，空名字会让分支保护的白名单落空。
func initTestOptions() *initOptions {
	return &initOptions{Reviewer: instances.DefaultReviewerName, Merger: instances.DefaultMergerName}
}

// newInitFakeFile 写一份指向假站点的配置与一条 mcp 用途令牌：init 全程以
// 开发者自己的身份行动（purpose=mcp），没有它就只能在「缺令牌」处停下。
func newInitFakeFile(t *testing.T, fake *initFake) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(path, &instances.File{Channels: []instances.Channel{{
		Type: instances.ChannelGitea, Host: fake.server.URL, Reviewer: "ai", Merger: "merge",
	}}}); err != nil {
		t.Fatal(err)
	}
	writePurposeCredentials(t, []credentials.Credential{
		{Host: fake.server.URL, User: "dev", Purpose: credentials.PurposeMCP, Token: "mcp-token"},
	})
	return path
}

// TestCommandLoggerWritesToCommandOut 断言命令日志的落点契约：init 全部五个
// 子命令的过程叙述都靠它输出，写错流（落到进程 stdout 而不是命令自己的
// OutOrStdout）会让 CI 里重定向的输出丢字，操作者看不到「做了什么」。
func TestCommandLoggerWritesToCommandOut(t *testing.T) {
	var out bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	commandLogger(command, "init labels")("已处理 %d 个仓库", 3)
	if got, want := out.String(), "[init labels] 已处理 3 个仓库\n"; got != want {
		t.Errorf("日志 = %q, want %q", got, want)
	}
}

// TestSaveInstanceUpsertsGiteaChannel 断言登记写回的字段映射：通道的
// reviewer/merger 是名字字符串而 Instance 上是 Account 结构，映射写错会让
// 配置里存下空账号，之后的机器人与分支保护配置全部落空。新建时补一条通道，
// 已存在时原地更新而不是追加第二条。
func TestSaveInstanceUpsertsGiteaChannel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	const host = "https://gitea.example.com"
	file := &instances.File{
		Providers: map[string]instances.Provider{"primary": {}},
	}
	updated := instances.Instance{
		Host: host, Provider: "primary",
		Reviewer: instances.Account{Name: "ai"}, Merger: instances.Account{Name: "merge"},
		Repos: []instances.Repo{{Name: "acme/rocket"}},
	}
	if err := saveInstance(file, path, host, updated); err != nil {
		t.Fatalf("saveInstance: %v", err)
	}
	if len(file.Channels) != 1 {
		t.Fatalf("通道数 = %d, want 1", len(file.Channels))
	}
	channel := file.Channels[0]
	if channel.Reviewer != "ai" || channel.Merger != "merge" || channel.Provider != "primary" {
		t.Errorf("通道 = %+v", channel)
	}
	// 落盘后再读回来：确认真的是写进文件而不是只改了内存对象。
	reloaded, err := instances.Load(path)
	if err != nil {
		t.Fatalf("重新载入: %v", err)
	}
	if len(reloaded.Channels) != 1 || reloaded.Channels[0].Reviewer != "ai" {
		t.Errorf("重新载入 = %+v", reloaded.Channels)
	}

	// 第二次写回同一站点：必须原地替换，不能堆出第二条通道。
	updated.Reviewer = instances.Account{Name: "ai-2"}
	if err := saveInstance(file, path, host, updated); err != nil {
		t.Fatalf("saveInstance(第二次): %v", err)
	}
	if len(file.Channels) != 1 {
		t.Fatalf("重复登记后通道数 = %d, want 1", len(file.Channels))
	}
	if file.Channels[0].Reviewer != "ai-2" {
		t.Errorf("通道 reviewer = %q, want ai-2", file.Channels[0].Reviewer)
	}
}

// TestInitRejectsRepoNotOnAnyPlatform 断言「站点没登记」时的引导：remote 指向的
// 站点不在配置里时必须点名它并给出 login add，否则操作者只会看到一次连接失败，
// 把「平台没登记」误读成「仓库不存在」。
//
// remote 必须指向一个探测得通的活站点：SelectGiteaRemote 只有在 probe 命中时才回
// origin 作为站点，探测落空会先以「无法从 remote 识别 Gitea 仓库」结束——那是另一
// 条错误路径。所以这里起一个真的 version 端点，让流程走到选通道那一步，再让配置
// 只登记另一个站点，制造出「站点没登记」。
func TestInitRejectsRepoNotOnAnyPlatform(t *testing.T) {
	isolateCredentials(t)
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"version": "1.24.0"})
	}))
	defer live.Close()

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{Channels: []instances.Channel{{
		Type: instances.ChannelGitea, Host: "https://gitea.example.com",
	}}}); err != nil {
		t.Fatal(err)
	}
	// --repo 只接受 owner/name：站点靠 remote 推断，把 URL 塞进来会先撞格式校验。
	work := t.TempDir()
	runGit(t, work, "init")
	runGit(t, work, "remote", "add", "origin", live.URL+"/acme/rocket.git")
	t.Chdir(work)

	fixture := newInitLeafForTest(t, "ai", configPath, "--dry-run")
	err := fixture.run()
	if err == nil || !strings.Contains(err.Error(), "不在配置中") {
		t.Errorf("err = %v, want 含「不在配置中」", err)
	}
}

// TestInitRejectsWithoutConfigFile 断言缺少配置时的引导：init 是 dev 专用命令，
// 但没有任何 config.json 时不能只说「找不到文件」，必须给出注册平台的命令，
// 因为这是新机器上第一次跑 init 最常见的状态。
//
// 必须让配置路径解析为「空」：显式给出一个不存在的路径时，resolveInstanceFile 会
// 先撞上读盘错误，操作者看到的变成一句 no such file，永远看不到这句引导。
func TestInitRejectsWithoutConfigFile(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("ASSISTANT_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	work := t.TempDir()
	runGit(t, work, "init")
	runGit(t, work, "remote", "add", "origin", "https://gitea.example.com/acme/rocket.git")
	t.Chdir(work)

	fixture := newInitLeafForTest(t, "ai", "")
	err := fixture.run()
	if err == nil || !strings.Contains(err.Error(), "没有 config.json") {
		t.Errorf("err = %v, want 含「没有 config.json」", err)
	}
}

// TestInitDryRunPrintsPlanWithoutTouchingServer 断言行内最重的语义：--dry-run
// 只打印计划、一个写请求都不发。这条是「显式配置才动手」的护栏——干跑若偷偷
// 加了协作者或改了分支保护，是最难事后发现的一类副作用。
func TestInitDryRunPrintsPlanWithoutTouchingServer(t *testing.T) {
	fake := newInitFake(t)
	fake.collaborators = []string{"dev"}
	fake.permissions["dev"] = "admin"
	configPath := newInitFakeFile(t, fake)

	fixture := newInitLeafForTest(t, "ai", configPath, "--repo=acme/rocket", "--dry-run")
	if err := fixture.run(); err != nil {
		t.Fatalf("init ai --dry-run: %v", err)
	}
	stdout := fixture.out
	// init ai 没有自己的干跑分支：它的计划行与「未触碰服务端」结论由 runInitMerge /
	// runInitLabels 之外的写命令给出，这里只断言它确实只打印计划、没发写请求。
	for _, want := range []string{
		"dry-run：将把 ai 加为 acme/rocket 的协作者（write）",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("输出缺少 %q：\n%s", want, stdout.String())
		}
	}
	for _, method := range []string{"PUT ", "POST ", "DELETE "} {
		for _, recorded := range fake.recorded {
			if strings.HasPrefix(recorded, method) {
				t.Errorf("dry-run 不应发出写请求：%s", recorded)
			}
		}
	}
}

// TestInitRejectsSelfAsNonAdminCollaborator 断言开发者自证这道门：自己的令牌
// 只能读到 write/read 权限时立刻停下并说明该用什么命令。放过去会让 init 后续
// 的写操作以一次权限不足的 403 结束，把「身份不够」误报成「站点故障」。
func TestInitRejectsSelfAsNonAdminCollaborator(t *testing.T) {
	fake := newInitFake(t)
	fake.collaborators = []string{"dev"}
	fake.permissions["dev"] = "write"
	configPath := newInitFakeFile(t, fake)

	fixture := newInitLeafForTest(t, "ai", configPath, "--repo=acme/rocket")
	err := fixture.run()
	if err == nil {
		t.Fatal("期望报错，实际没有")
	}
	if !strings.Contains(err.Error(), "init 需要仓库管理员权限") {
		t.Errorf("err = %v, want 含「init 需要仓库管理员权限」", err)
	}
	if !strings.Contains(err.Error(), "assistant setup") {
		t.Errorf("err = %v, 缺少替代命令指引", err)
	}
}

// TestInitRejectsUnreadableCollaborators 断言读不到协作者清单时的引导：403
// 说明令牌没有仓库管理面权限，错误要说清 init 需要什么、站点级配置该走哪条路，
// 而不是把 403 原样抛给操作者。
func TestInitRejectsUnreadableCollaborators(t *testing.T) {
	fake := newInitFake(t)
	fake.failCollaborators = true
	configPath := newInitFakeFile(t, fake)

	fixture := newInitLeafForTest(t, "ai", configPath, "--repo=acme/rocket")
	err := fixture.run()
	if err == nil || !strings.Contains(err.Error(), "读取协作者失败") {
		t.Fatalf("err = %v, want 含「读取协作者失败」", err)
	}
	if !strings.Contains(err.Error(), "站点级配置用 assistant setup") {
		t.Errorf("err = %v, 缺少站点级替代路径", err)
	}
}

// TestInitRejectsMissingMCPCredential 断言凭据缺失的报错口径：init 固定用
// purpose=mcp 的开发者令牌，缺这条令牌时必须点名用途与补齐方式，否则会拿空
// 令牌去请求，把「凭据没配」误报成「站点拒绝」。
func TestInitRejectsMissingMCPCredential(t *testing.T) {
	fake := newInitFake(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{Channels: []instances.Channel{{
		Type: instances.ChannelGitea, Host: fake.server.URL,
	}}}); err != nil {
		t.Fatal(err)
	}
	isolateCredentials(t)

	fixture := newInitLeafForTest(t, "ai", configPath, "--repo=acme/rocket")
	err := fixture.run()
	if err == nil || !strings.Contains(err.Error(), "purpose=mcp") {
		t.Fatalf("err = %v, want 点名 purpose=mcp", err)
	}
	if !strings.Contains(err.Error(), "缺少 mcp 用途令牌") {
		t.Errorf("err = %v, 缺少用途缺失说明", err)
	}
}

// TestInitReviewerAddsWriteCollaborator 断言内容评审机器人的授权姿态：把 ai
// 以 write 加为协作者（评审要能提交 review，但不需要管理面），并留下「重跑
// branch-protection 可升到 2 票」的下一步提示——双批准是系统的硬约定。
func TestInitReviewerAddsWriteCollaborator(t *testing.T) {
	fake := newInitFake(t)
	fake.collaborators = []string{"dev"}
	fake.permissions["dev"] = "admin"
	configPath := newInitFakeFile(t, fake)

	fixture := newInitLeafForTest(t, "ai", configPath, "--repo=acme/rocket")
	if err := fixture.run(); err != nil {
		t.Fatalf("init ai: %v", err)
	}
	stdout := fixture.out
	if !fake.has("PUT /api/v1/repos/acme/rocket/collaborators/ai") {
		t.Errorf("缺少添加 ai 协作者的请求：%v", fake.recorded)
	}
	for _, want := range []string{
		"已把 ai 加为 acme/rocket 的协作者（write）",
		"重跑 assistant init branch-protection 可把 required approvals 升为 2",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("输出缺少 %q：\n%s", want, stdout.String())
		}
	}
}

// TestInitMergeAddsAdminCollaborator 断言状态评审/合并机器人的授权姿态：merge
// 拿的是 admin（要能改分支保护与合并），且必须提示 MERGE_TOKEN 由管理员分发
// ——令牌不出现在开发者手上的约定不能让操作者自己猜。
func TestInitMergeAddsAdminCollaborator(t *testing.T) {
	fake := newInitFake(t)
	fake.collaborators = []string{"dev"}
	fake.permissions["dev"] = "admin"
	configPath := newInitFakeFile(t, fake)

	fixture := newInitLeafForTest(t, "merge", configPath, "--repo=acme/rocket")
	if err := fixture.run(); err != nil {
		t.Fatalf("init merge: %v", err)
	}
	stdout := fixture.out
	if !fake.has("PUT /api/v1/repos/acme/rocket/collaborators/merge") {
		t.Errorf("缺少添加 merge 协作者的请求：%v", fake.recorded)
	}
	for _, want := range []string{
		"已把 merge 加为 acme/rocket 的协作者（admin）",
		"MERGE_TOKEN secret 由站点管理员运行 assistant setup 分发",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("输出缺少 %q：\n%s", want, stdout.String())
		}
	}
}

// TestInitBranchProtectionRequiresMergerCollaborator 断言分支保护前的账号前置
// 校验：协作者里没有 merge 就必须停下并列出当前协作者及其权限。少了 merge 就
// 没有人能会签，写进去的保护规则会变成永远合不了的门——必须先补账号。
func TestInitBranchProtectionRequiresMergerCollaborator(t *testing.T) {
	fake := newInitFake(t)
	fake.collaborators = []string{"dev", "ai"}
	fake.permissions["dev"] = "admin"
	configPath := newInitFakeFile(t, fake)

	fixture := newInitLeafForTest(t, "branch-protection", configPath, "--repo=acme/rocket")
	err := fixture.run()
	if err == nil || !strings.Contains(err.Error(), "协作者中没有 merge 账号") {
		t.Fatalf("err = %v, want 含「协作者中没有 merge 账号」", err)
	}
	// 错误必须带上现有协作者与权限，操作者才知道要补谁、跟谁比。
	if !strings.Contains(err.Error(), "dev(admin)") || !strings.Contains(err.Error(), "ai(write)") {
		t.Errorf("err = %v, 缺少协作者清单", err)
	}
	if !strings.Contains(err.Error(), "assistant init merge") {
		t.Errorf("err = %v, 缺少补救命令", err)
	}
}

// TestInitBranchProtectionWritesApprovalsAndWhitelist 断言分支保护写入的核心
// 数值：required approvals = 1（merge 恒 1 票）+ 1（ai 是协作者，内容批准另算
// 一票）= 2，合并白名单只含 merge，分支取仓库默认分支。这几个数字就是双批准
// 约定在站点上的落地形态，错一个都会让门禁形同虚设或永久卡死。
func TestInitBranchProtectionWritesApprovalsAndWhitelist(t *testing.T) {
	fake := newInitFake(t)
	fake.collaborators = []string{"dev", "ai", "merge"}
	fake.permissions["dev"] = "admin"
	configPath := newInitFakeFile(t, fake)

	fixture := newInitLeafForTest(t, "branch-protection", configPath,
		"--repo=acme/rocket", "--extra-approvals=1")
	if err := fixture.run(); err != nil {
		t.Fatalf("init branch-protection: %v", err)
	}
	stdout := fixture.out
	if !fake.has("POST /api/v1/repos/acme/rocket/branch_protections") {
		t.Errorf("缺少写分支保护的请求：%v", fake.recorded)
	}
	for _, want := range []string{
		"required approvals=3（merge 恒 1 票，ai 协作者在否=true，附加 1）",
		"合并白名单=merge，分支=main",
		"分支保护已写入 acme/rocket（main）",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("输出缺少 %q：\n%s", want, stdout.String())
		}
	}
}

// TestInitBranchProtectionRejectsEmptyRepository 断言空仓库的拒绝理由：没有
// 默认分支就没有可保护的分支名，硬写会创建一条挂不到任何分支的规则。错误必须
// 直说这一点，而不是把站点的 422 抛出来。
func TestInitBranchProtectionRejectsEmptyRepository(t *testing.T) {
	fake := newInitFake(t)
	fake.collaborators = []string{"dev", "ai", "merge"}
	fake.permissions["dev"] = "admin"
	fake.emptyRepo = true
	configPath := newInitFakeFile(t, fake)

	fixture := newInitLeafForTest(t, "branch-protection", configPath, "--repo=acme/rocket")
	err := fixture.run()
	if err == nil || !strings.Contains(err.Error(), "仓库为空或无默认分支") {
		t.Errorf("err = %v, want 含「仓库为空或无默认分支」", err)
	}
	if fake.has("POST /api/v1/repos/acme/rocket/branch_protections") {
		t.Error("空仓库不应写入分支保护")
	}
}

// TestInitLabelsReconcilesTagSystem 断言标签收敛的调用与结果叙述：假站点没有
// 任何标签，收敛必须把内置标签体系逐个建出来（而不是只报一句「已收敛」），
// 操作者才能在日志里看到标签真的补齐了。
func TestInitLabelsReconcilesTagSystem(t *testing.T) {
	fake := newInitFake(t)
	fake.collaborators = []string{"dev"}
	fake.permissions["dev"] = "admin"
	configPath := newInitFakeFile(t, fake)

	fixture := newInitLeafForTest(t, "labels", configPath, "--repo=acme/rocket")
	if err := fixture.run(); err != nil {
		t.Fatalf("init labels: %v", err)
	}
	stdout := fixture.out
	created := 0
	for _, recorded := range fake.recorded {
		if recorded == "POST /api/v1/repos/acme/rocket/labels" {
			created++
		}
	}
	if want := len(status.LabelDefinitions()); created != want {
		t.Errorf("创建标签 %d 条, want %d（内置标签体系每条都要补齐）", created, want)
	}
	if want := len(status.LabelDefinitions()); want < 5 {
		t.Fatalf("内置标签体系只有 %d 条，测试前提不成立", want)
	}
	if !strings.Contains(stdout.String(), "标签体系已收敛（acme/rocket）") {
		t.Errorf("输出缺少收敛结论：\n%s", stdout.String())
	}
}

// TestInitLabelsDryRunSkipsServer 断言行内最重的语义：init labels 的干跑只打印
// 计划、一个标签都不建；标签是门禁的状态载体，干跑偷偷改动会让评审队列的
// 「待评审/已批准」在所有 PR 上同时错位。
func TestInitLabelsDryRunSkipsServer(t *testing.T) {
	fake := newInitFake(t)
	fake.collaborators = []string{"dev"}
	fake.permissions["dev"] = "admin"
	configPath := newInitFakeFile(t, fake)

	fixture := newInitLeafForTest(t, "labels", configPath, "--repo=acme/rocket", "--dry-run")
	if err := fixture.run(); err != nil {
		t.Fatalf("init labels --dry-run: %v", err)
	}
	stdout := fixture.out
	if !strings.Contains(stdout.String(), "dry-run：将收敛 acme/rocket 的标签体系") {
		t.Errorf("输出缺少干跑计划：\n%s", stdout.String())
	}
	if fake.has("POST /api/v1/repos/acme/rocket/labels") {
		t.Error("dry-run 不应创建标签")
	}
}

// TestInitActionsInstallsWorkflow 断言 init actions 的落点：workflow 写进目标
// 仓库的 .gitea/workflows/ 且内容带托管标记；这与「本地 workflow 就绪、服务端
// secret 由 setup 分发」的分工一致，写错目录会让 Actions 完全不被触发。
func TestInitActionsInstallsWorkflow(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := instances.Save(configPath, &instances.File{Channels: []instances.Channel{{
		Type: instances.ChannelGitea, Host: "https://gitea.example.com",
	}}}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	fixture := newInitLeafForTest(t, "actions", "")
	if err := fixture.run(); err != nil {
		t.Fatalf("init actions: %v", err)
	}
	stdout := fixture.out
	workflow := filepath.Join(dir, ".gitea", "workflows", "assistant.yml")
	data, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatalf("读取 workflow: %v", err)
	}
	if !strings.Contains(string(data), "managed-by: assistant") {
		t.Errorf("workflow 缺少托管标记：\n%s", data)
	}
	for _, want := range []string{
		"本地 workflow 就绪；服务端 secret 由 assistant setup 分发",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("输出缺少 %q：\n%s", want, stdout.String())
		}
	}
}

// TestInitCommandsRejectUnknownFlagsEarly 断言干跑旗标挂在父命令上：--dry-run
// 与 --repo 是 init 父命令的持久旗标，五个叶子都要能继承到。漏挂会让
// 「assistant init review --dry-run」变成未定义旗标错误——干跑正是最该能用的
// 安全出口。
func TestInitCommandsRejectUnknownFlagsEarly(t *testing.T) {
	isolateCredentials(t)
	var configFlag string
	parent := newInitCommand(&configFlag)
	for _, name := range []string{"actions", "merge", "reviewer", "labels", "branch-protection"} {
		leaf, _, err := parent.Find([]string{name})
		if err != nil || leaf == nil {
			t.Fatalf("init %s 缺失: %v", name, err)
		}
	}
	leaf, _, err := parent.Find([]string{"reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"repo", "dry-run"} {
		if leaf.Flags().Lookup(name) == nil {
			t.Errorf("init 叶子缺少继承旗标 --%s", name)
		}
	}
	// 父命令自己必须注册这两个持久旗标（叶子的继承来源）。
	for _, name := range []string{"repo", "dry-run"} {
		if parent.PersistentFlags().Lookup(name) == nil {
			t.Errorf("init 父命令缺少持久旗标 --%s", name)
		}
	}
}

// TestContextCancellationReachesInitThroughCommand 断言命令上下文能传到做网络
// 请求的那一层：ctx 取消后 init 必须立刻以取消错误结束，而不是继续对着站点
// 发请求。Ctrl+C / SIGTERM 停止 CI 时靠的就是这条链路。
func TestContextCancellationReachesInitThroughCommand(t *testing.T) {
	fake := newInitFake(t)
	fake.collaborators = []string{"dev"}
	fake.permissions["dev"] = "admin"
	configPath := newInitFakeFile(t, fake)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fixture := newInitLeafForTest(t, "ai", configPath, "--repo=acme/rocket")
	fixture.root.SetContext(ctx)
	err := fixture.run()
	if err == nil {
		t.Fatal("已取消的 ctx 应报错")
	}
	if !strings.Contains(err.Error(), "context canceled") && !strings.Contains(err.Error(), "取消") {
		t.Errorf("err = %v, want 取消相关", err)
	}
	if fake.has("PUT /api/v1/repos/acme/rocket/collaborators/ai") {
		t.Error("ctx 已取消时不应发出写请求")
	}
}
