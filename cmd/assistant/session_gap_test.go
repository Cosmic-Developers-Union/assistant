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

	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/sessionstore"

	"github.com/spf13/cobra"
)

// isolateSessionConfig 把配置目录钉在临时目录里，并返回该目录。
//
// session push 的地址解析横跨「旗标 → config.json → 环境/sidecar 文件」三层，
// 每层都从配置目录出发去读文件；不隔离就会读到操作者本机的真实
// sessions-remote.json，用例的成败取决于跑测机器上有没有在跑 serve。
func isolateSessionConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	t.Setenv("ASSISTANT_SESSIONS_URL", "")
	t.Setenv("ASSISTANT_SESSIONS_TOKEN", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	if content != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// seedSessionRecord 在给定记录根下造一条待推送的会话记录，并返回项目名。
//
// Push 是对 batch.Sessions 逐条发请求：记录根为空时它一个请求都不发，于是
// 「推送失败要报错」「请求打到旗标地址」这类断言会静默通过。要真的验证推送，
// 必须先有一条记录。
func seedSessionRecord(t *testing.T, root string) string {
	t.Helper()
	project := "proj"
	projectDir := filepath.Join(root, "projects", project)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := []byte("{\"type\":\"user\",\"message\":{\"content\":\"hello\"}}\n")
	if err := os.WriteFile(filepath.Join(projectDir, "sess-1.jsonl"), line, 0o600); err != nil {
		t.Fatal(err)
	}
	return project
}

// sessionPushCommand 造一条只带运行期的 session push 命令：输出与错误都收进
// 调用方给的缓冲，输入用空 reader（这条路径不该读 stdin）。
func sessionPushCommand(t *testing.T, stdout, stderr *bytes.Buffer) *cobra.Command {
	t.Helper()
	command := &cobra.Command{}
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetIn(strings.NewReader(""))
	command.SetContext(t.Context())
	return command
}

// TestSessionPushReportsUnresolvableConfigDir 断言配置目录都定不下来时 session
// push 立刻报错退出，而不是退回当前目录去猜。
//
// 地址解析的第一层是配置目录（config.json 里的 sessions.remote），第二层才是
// 环境变量。配置目录解析失败意味着这台机器既没有 HOME 也没有 XDG——此时任何
// 「用 cwd 兜底」的写法会让推送记录落到一个与实际配置无关的目录里，日后再也
// 找不到。错误必须当场冒出来，而不是等到记录库拒绝连接。
func TestSessionPushReportsUnresolvableConfigDir(t *testing.T) {
	t.Setenv("ASSISTANT_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("ASSISTANT_SESSIONS_URL", "")
	t.Setenv("ASSISTANT_SESSIONS_TOKEN", "")

	if _, err := resolveConfigDir(""); err == nil {
		t.Skip("本机在 HOME/XDG 皆空时仍能定位配置目录，构造不出该分支")
	}

	var stdout, stderr bytes.Buffer
	err := runSessionPush(t.Context(), sessionPushCommand(t, &stdout, &stderr), "", &sessionPushOptions{})
	if err == nil {
		t.Fatalf("配置目录未知时应报错：\n%s", stdout.String())
	}
}

// TestSessionPushDryRunListsSessionsWithoutContactingSite 断言 --dry-run 只列
// 出将推送的会话、一个请求都不发。
//
// dry-run 是「先看清会推什么再决定」的安全阀。如果它在没有服务端时也报错，
// 操作者就无法在断网或服务端未起时先核对清单；如果它悄悄发了请求，就违背了
// 只读承诺。两种情况都必须被钉住，所以这里既不给地址、也不起假服务端。
func TestSessionPushDryRunListsSessionsWithoutContactingSite(t *testing.T) {
	dir := isolateSessionConfig(t, "")
	root := filepath.Join(dir, "claude")
	projectDir := filepath.Join(root, "projects", "-home-ge-rocket")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := []byte("{\"type\":\"user\",\"message\":{\"content\":\"你好\"}}\n")
	if err := os.WriteFile(filepath.Join(projectDir, "sess-1.jsonl"), line, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	options := &sessionPushOptions{DryRun: true, Root: root}
	if err := runSessionPush(t.Context(), sessionPushCommand(t, &stdout, &stderr), "", options); err != nil {
		t.Fatalf("dry-run 应能在没有服务端时跑通：%v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "dry-run：1 条待推送") {
		t.Errorf("dry-run 应点明待推送条数：\n%s", output)
	}
	if !strings.Contains(output, "-home-ge-rocket") {
		t.Errorf("dry-run 应逐条列出会话（项目名）：\n%s", output)
	}
}

// TestSessionPushDryRunQuietSuppressesLog 断言 --quiet 下 dry-run 的清单照旧
// 输出（清单是命令的主要结论），而 --quiet 影响的是「记录库＝…」这类过程日志。
//
// 这里的取舍是：结论不可被 --quiet 吞掉，否则命令成功却没有可读输出；过程日志
// 可以被吞掉。两条都要有用例，否则日后把 logf 换成直接 Fprintf 不会有人发现。
func TestSessionPushDryRunQuietSuppressesLog(t *testing.T) {
	dir := isolateSessionConfig(t, "")
	root := filepath.Join(dir, "claude")

	var stdout, stderr bytes.Buffer
	options := &sessionPushOptions{DryRun: true, Quiet: true, Root: root}
	if err := runSessionPush(t.Context(), sessionPushCommand(t, &stdout, &stderr), "", options); err != nil {
		t.Fatalf("空记录根在 dry-run + quiet 下也应是空清单而非错误：%v", err)
	}
	if !strings.Contains(stdout.String(), "dry-run：0 条待推送") {
		t.Errorf("--quiet 不该吞掉结论行：\n%s", stdout.String())
	}
}

// TestSessionPushReportsMissingRemoteAddress 断言三层地址都解析不出时，错误把
// 四条可走的路一次讲清（config.json / assistant serve / --url / 文件）。
//
// 这是操作者最常撞上的失败：本地有记录、但没告诉它往哪推。错误信息若只说
// 「缺少 URL」，操作者得去翻源码才知道有四种配置方式；把四条路写进错误是
// 这个函数存在的意义，必须有用例钉住。
func TestSessionPushReportsMissingRemoteAddress(t *testing.T) {
	dir := isolateSessionConfig(t, "")
	root := filepath.Join(dir, "claude")

	var stdout, stderr bytes.Buffer
	options := &sessionPushOptions{Root: root}
	err := runSessionPush(t.Context(), sessionPushCommand(t, &stdout, &stderr), "", options)
	if err == nil {
		t.Fatal("没有记录库地址时应报错")
	}
	for _, want := range []string{"sessions.remote", "assistant serve", "--url", sessionstore.RemoteFile} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应点明配置路径 %q：%v", want, err)
		}
	}
}

// TestSessionPushUsesFlagURLOverEverything 断言 --url/--token 旗标压过其余三层。
//
// 旗标是操作者在命令行上刚敲下的意图，也是唯一能临时指向另一台记录库的开关。
// 若它被 config.json 或环境变量盖住，--url 就成了摆设；而且这一点很难从行为上
// 侧写——只能靠「请求确实打到旗标给的那个地址」来证明。
func TestSessionPushUsesFlagURLOverEverything(t *testing.T) {
	dir := isolateSessionConfig(t, "")
	root := filepath.Join(dir, "claude")
	seedSessionRecord(t, root)

	var received []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"stored": 0})
	}))
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer
	options := &sessionPushOptions{Root: root, URL: server.URL, Token: "flag-token"}
	if err := runSessionPush(t.Context(), sessionPushCommand(t, &stdout, &stderr), "", options); err != nil {
		t.Fatalf("旗标地址下应能跑通：%v\n%s", err, stdout.String())
	}
	if len(received) == 0 {
		t.Errorf("应至少发过一个请求到旗标地址")
	}
	if !strings.Contains(stdout.String(), "来源 flag") {
		t.Errorf("过程日志应说明地址来源是旗标：\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "已推送 1 条会话记录") {
		t.Errorf("一条记录推送后应给出汇总行：\n%s", stdout.String())
	}
}

// TestSessionPushReadsAddressFromConfigFile 断言旗标缺失时从 config.json 的
// sessions.remote 读到地址与令牌，并把来源记成 config。
//
// 这是服务端在别的机器上时的推荐用法：写进配置而不是每条命令都敲 --url。
// 「来源 config」这一行是操作者判断自己改的文件有没有生效的唯一线索。
func TestSessionPushReadsAddressFromConfigFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"stored": 0})
	}))
	t.Cleanup(server.Close)

	// config.json 必须通过 instances.Load 的语义校验：它要求至少有一个
	// runtime 或 channel（否则会被当成「空骨架」当作没有配置文件），所以这里
	// 除了 sessions 之外还得给一个最小 channel。
	config := `{"channels":[{"type":"gitea","host":"https://example.com"}],"sessions":{"remote":{"url":` +
		strconv.Quote(server.URL) + `,"token":"cfg-token"}}}`
	dir := isolateSessionConfig(t, config)
	root := filepath.Join(dir, "claude")
	seedSessionRecord(t, root)

	var stdout, stderr bytes.Buffer
	options := &sessionPushOptions{Root: root}
	// configPath 必须显式给：resolveInstanceFile 只看旗标/环境变量这一侧，
	// 传 "" 会让它回落到平台标准配置目录，看不到用例刚写的 config.json。
	configPath := filepath.Join(dir, "config.json")
	if err := runSessionPush(t.Context(), sessionPushCommand(t, &stdout, &stderr), configPath, options); err != nil {
		t.Fatalf("应从 config.json 读到地址与令牌：%v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "来源 config") {
		t.Errorf("过程日志应说明地址来源是配置文件：\n%s", stdout.String())
	}
}

// TestSessionPushFallsBackToSidecarFile 断言 config.json 没有 remote 时退回
// <配置目录>/sessions-remote.json，并把来源记成 env/sidecar。
//
// 同机 serve 会写下端点文件（serve.json），手工配置走 sessions-remote.json；
// 两者都是「不写 config.json 也能用」的路径。退回顺序本身是契约，来源标签让
// 操作者能分辨究竟读到了哪一个。
func TestSessionPushFallsBackToSidecarFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"stored": 0})
	}))
	t.Cleanup(server.Close)

	dir := isolateSessionConfig(t, "")
	sidecar, err := json.Marshal(sessionstore.RemoteConfig{URL: server.URL, Token: "sidecar-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionstore.RemoteFile), sidecar, 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "claude")
	seedSessionRecord(t, root)

	var stdout, stderr bytes.Buffer
	options := &sessionPushOptions{Root: root}
	if err := runSessionPush(t.Context(), sessionPushCommand(t, &stdout, &stderr), "", options); err != nil {
		t.Fatalf("应从 sidecar 文件读到地址：%v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "来源 env/sidecar") {
		t.Errorf("过程日志应说明地址来源是 sidecar：\n%s", stdout.String())
	}
}

// TestSessionPushFallsBackToClaudeDir 断言 --root 与 CLAUDE_CONFIG_DIR 都缺失时
// 用 instances.ClaudeDir() 兜底，而不是报「缺少记录根目录」。
//
// 会话记录的真实落点由运行时的 runtime.claude_dir 决定，但 CLI 单跑时没有
// runtime 可读，此时 CLAUDE_CONFIG_DIR 就是唯一权威。若这条兜底没接线，所有
// 不带 --root 的调用都会失败——而带上 --root 的用例永远发现不了。
func TestSessionPushFallsBackToClaudeDir(t *testing.T) {
	dir := isolateSessionConfig(t, "")
	claudeRoot := filepath.Join(dir, "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeRoot)
	projectDir := filepath.Join(claudeRoot, "projects", "proj")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "s1.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	// 不给 Root：能否看到会话完全取决于 CLAUDE_CONFIG_DIR 有没有被读。
	options := &sessionPushOptions{DryRun: true}
	if err := runSessionPush(t.Context(), sessionPushCommand(t, &stdout, &stderr), "", options); err != nil {
		t.Fatalf("应回退到 CLAUDE_CONFIG_DIR：%v", err)
	}
	if !strings.Contains(stdout.String(), "dry-run：1 条待推送（记录根 "+claudeRoot+"）") {
		t.Errorf("记录根应是 CLAUDE_CONFIG_DIR：\n%s", stdout.String())
	}
}

// TestSessionPushReportsServerFailure 断言推送请求失败时错误穿透到调用方，
// 而不是被当成「零条存储」。
//
// 服务端 500 与「服务端说没有新记录」在行为上截然不同：前者要操作者去查服务端
// 日志，后者是正常结束。把两者混起来（例如忽略 Push 的错误后看 result.Stored）
// 会让真实的推送故障静默通过。
func TestSessionPushReportsServerFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	dir := isolateSessionConfig(t, "")
	root := filepath.Join(dir, "claude")
	seedSessionRecord(t, root)

	var stdout, stderr bytes.Buffer
	options := &sessionPushOptions{Root: root, URL: server.URL, Token: "t"}
	err := runSessionPush(t.Context(), sessionPushCommand(t, &stdout, &stderr), "", options)
	if err == nil {
		t.Fatalf("服务端 500 时应报错：\n%s", stdout.String())
	}
}

// TestSessionPushReportsUnreadableProjectDir 断言记录根存在但 projects/ 读不动
// （权限不足）时报错，而不是当成「没有记录」静默成功。
//
// Collect 对「projects/ 不存在」专门放行（首次使用是正常状态），但对其它读取
// 失败必须报错——否则一个权限有问题的记录根会被当成空记录根，推送永远无事发生，
// 操作者还以为已经同步完了。
func TestSessionPushReportsUnreadableProjectDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("以 root 跑时权限位不生效，构造不出读失败")
	}
	dir := isolateSessionConfig(t, "")
	root := filepath.Join(dir, "claude")
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(projects, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(projects, 0o700) })

	var stdout, stderr bytes.Buffer
	options := &sessionPushOptions{DryRun: true, Root: root}
	err := runSessionPush(t.Context(), sessionPushCommand(t, &stdout, &stderr), "", options)
	if err == nil {
		t.Fatalf("projects/ 读不动时应报错：\n%s", stdout.String())
	}
}

// TestSessionPushReportsUnresolvableClaudeDir 断言 --root 缺失且 CLAUDE_CONFIG_DIR
// 与配置目录都定不下来时，兜底解析的失败原样上报。
//
// 这条兜底（instances.ClaudeDir）只在没有运行时配置的裸跑里生效；它失败说明机器
// 连配置目录都定不下来（HOME/XDG 皆空）。若错误被吞掉，root 会变成空串，随后
// 变成「缺少记录根目录」——把「机器环境不完整」误报成「参数没给」。
func TestSessionPushReportsUnresolvableClaudeDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ASSISTANT_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if _, err := instances.ClaudeDir(); err == nil {
		t.Skip("本机在 HOME/XDG 皆空时仍能定位配置目录，构造不出该分支")
	}

	var stdout, stderr bytes.Buffer
	options := &sessionPushOptions{DryRun: true}
	err := runSessionPush(t.Context(), sessionPushCommand(t, &stdout, &stderr), "", options)
	if err == nil {
		t.Fatalf("兜底记录根解析失败时应报错：\n%s", stdout.String())
	}
}
