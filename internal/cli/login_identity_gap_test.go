package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
)

// identityCommand 造一个能直接调用 runIdentityLogin 的命令：只接输出缓冲与注入的
// 标准输入，不挂任何子命令。
//
// runIdentityLogin 是「唯一登录实现」，三条入口（login add / token refresh / setup）
// 都从这里走；单独驱动它才能把「参数位缺账号」「非交互环境没给密码」这类只由它
// 负责的分支钉住，而不被 host 推断或旗标解析的噪音掩盖。
func identityCommand(stdin string) (*cobra.Command, *bytes.Buffer) {
	command := &cobra.Command{}
	// context 必须挂上：runIdentityLogin 走到的 TokenName 校验会用
	// command.Context() 发请求，零值 context 是 nil，会在网络层直接崩成
	// 「net/http: nil Context」而盖掉真正被测的分支。
	command.SetContext(context.Background())
	out := &bytes.Buffer{}
	command.SetOut(out)
	command.SetErr(out)
	command.SetIn(strings.NewReader(stdin))
	return command, out
}

// TestRunIdentityLoginReportsEmptyPromptedUser 断言交互式询问回来的账号是空串时
// 登录失败，而不是拿着空账号去站点派生令牌。
//
// 直接回车是最常见的误操作形态（以为会沿用上次的账号）。空账号会让站点端的令牌
// 端点以 404/401 回应，错误文案变成「令牌创建失败」，而真正的原因是「你没填账号」——
// 必须在本地就把它说清楚，且不得写入任何凭据。
func TestRunIdentityLoginReportsEmptyPromptedUser(t *testing.T) {
	credentialPath := isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"runtimes": {"main": {"main_agent": "main"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	command, out := identityCommand("\n")
	// 账号询问拿到一行空输入；此后流程必须立刻中止（不会去读密码）
	err := runIdentityLogin(command, newPromptSession(command), "https://empty.example.com", configPath,
		&loginOptions{})
	if err == nil {
		t.Fatalf("空账号应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "缺少账号") {
		t.Errorf("错误应点明缺少账号：%v", err)
	}
	if !strings.Contains(out.String(), "Gitea 账号") {
		t.Errorf("应先询问账号：\n%s", out.String())
	}
	if _, statErr := os.Stat(credentialPath); !os.IsNotExist(statErr) {
		t.Errorf("失败不该落盘凭据：%v", statErr)
	}
}

// TestRunIdentityLoginReportsNonInteractiveWithoutPassword 断言非交互环境（stdin
// 不是终端、也没给 --password / --password-stdin）时登录直接报错，绝不挂起等待输入。
//
// 这条路径会由 CI、stdio MCP、被别的进程拉起的会话走到：一旦在这里等待，调用方
// 会一直卡到超时，报出来的却是「连接超时」而不是「你没给密码」。
//
// 必须用 command.SetIn(os.Stdin) 而不是注入读者：promptSession.interactive() 的
// 判据是「输入源是不是 os.Stdin」——注入了读者它就认为自己在交互环境里，会转而
// 去 prompts.secret 读那口空输入（报出的是 EOF，不是这句建议）。只有真正的
// os.Stdin 加上「不是终端」（go test 下 stdin 是 /dev/null）才落到这条分支。
//
// 必须用 newPromptSession 而不是手搓 promptSession 零值：后者的 bufio.Reader 是
// 空的，secret 去读它会直接崩（nil Reader）。
func TestRunIdentityLoginReportsNonInteractiveWithoutPassword(t *testing.T) {
	credentialPath := isolateCredentials(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"runtimes": {"main": {"main_agent": "main"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	command, _ := identityCommand("")
	command.SetIn(os.Stdin)
	options := &loginOptions{User: "developer"}
	err := runIdentityLogin(command, newPromptSession(command), "https://ci.example.com", configPath, options)
	if err == nil {
		t.Fatal("非交互环境缺密码应报错")
	}
	if !strings.Contains(err.Error(), "--password-stdin") {
		t.Errorf("错误应指出去用 --password-stdin：%v", err)
	}
	if _, statErr := os.Stat(credentialPath); !os.IsNotExist(statErr) {
		t.Errorf("失败不该落盘凭据：%v", statErr)
	}
}

// TestRunIdentityLoginReportsUnparsableConfig 断言现有配置读不出来时登录立刻报错，
// 不拿凭据再去站点上派生一批令牌。
//
// 派生是有副作用的写操作：站点上会真的多出长期令牌。若配置坏了却继续，用户会得到
// 一批站点上已存在、本地却没登记的令牌——之后每条命令都认为「没登录过」，而
// Applications 页面里堆着谁都认领不了的条目。
func TestRunIdentityLoginReportsUnparsableConfig(t *testing.T) {
	isolateCredentials(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte("{ 不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}

	command, out := identityCommand("")
	err := runIdentityLogin(command, newPromptSession(command), "https://broken.example.com", configPath,
		&loginOptions{User: "developer", passwordSource: passwordSource{Password: "pw"}})
	if err == nil {
		t.Fatalf("坏配置应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "解析") {
		t.Errorf("错误应点明配置解析失败：%v", err)
	}
}

// TestRunIdentityLoginReportsCredentialsPathFailure 断言凭据库定位不到时登录报错，
// 而不是去向用户索要密码、在站点上派生令牌，最后才发现无处可写。
//
// 密码是用户最贵的输入：凭据落点取不出来（HOME 被清空、XDG_CONFIG_HOME 是相对
// 路径）时，越早说越好。这条把「先确认能落盘，再问密码」的次序钉住。
func TestRunIdentityLoginReportsCredentialsPathFailure(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"runtimes": {"main": {"main_agent": "main"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "relative/path")

	command, out := identityCommand("")
	err := runIdentityLogin(command, newPromptSession(command), "https://nopath.example.com", configPath,
		&loginOptions{User: "developer", passwordSource: passwordSource{Password: "pw"}})
	if err == nil {
		t.Fatalf("凭据库路径取不出来时应报错：\n%s", out.String())
	}
	if strings.Contains(out.String(), "密码") {
		t.Errorf("应在索要密码之前就失败：\n%s", out.String())
	}
}

// TestRunIdentityLoginReportsBrokenCredentialStore 断言凭据库损坏时登录报错，而不是
// 当成「没登录过」继续派生一批新令牌。
//
// 凭据库损坏恰恰会命中「有没有已存令牌」的取值路径：若把它当成没有，用户会在站点
// 上无谓地重建令牌，而本地那份仍然读不出来——下一次登录又重复一次。错误必须冒出来。
func TestRunIdentityLoginReportsBrokenCredentialStore(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(storePath, []byte("{ 不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", storePath)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"runtimes": {"main": {"main_agent": "main"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	command, out := identityCommand("")
	err := runIdentityLogin(command, newPromptSession(command), "https://corrupt.example.com", configPath,
		&loginOptions{User: "developer", passwordSource: passwordSource{Password: "pw"}})
	if err == nil {
		t.Fatalf("凭据库损坏时应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), storePath) {
		t.Errorf("错误应点明坏掉的凭据库：%v", err)
	}
}

// TestReadIdentityPasswordReadsStdin 断言 --password-stdin 从注入的输入源读取密码，
// 并只去掉行尾换行（首尾空格是密码的有效字符）。
//
// 这是非交互环境的唯一带外密码通道；若在读取时顺手 TrimSpace，一个以空格开头或
// 结尾的密码会被静默改掉，站点回「凭据错误」，用户对着自己明明敲对的密码反复重试。
func TestReadIdentityPasswordReadsStdin(t *testing.T) {
	command, _ := identityCommand("  padded secret  \n")
	value, err := readIdentityPassword(command, newPromptSession(command), "https://h.example.com", "dev",
		&passwordSource{PasswordStdin: true})
	if err != nil {
		t.Fatalf("读取 stdin 密码不该报错：%v", err)
	}
	if value != "  padded secret  " {
		t.Errorf("密码 = %q，首尾空格必须保留", value)
	}

	// \r\n 也要归一：管道在 Windows / 某些终端下会带上 \r
	command, _ = identityCommand("crlf\r\n")
	value, err = readIdentityPassword(command, newPromptSession(command), "https://h.example.com", "dev",
		&passwordSource{PasswordStdin: true})
	if err != nil {
		t.Fatalf("CRLF 输入不该报错：%v", err)
	}
	if value != "crlf" {
		t.Errorf("密码 = %q，want crlf", value)
	}
}

// TestReadIdentityPasswordReportsOversizedStdin 断言标准输入上的密码大到超过
// 64KiB 上限时报错，而不是把它整个读进内存当成密码。
//
// 上限是刻意设的：密码字段被误接成「把整个文件灌进来」（`--password-stdin < file`）
// 时，读进来的是任意大小的数据。这里必须停在上限，而不是把文件内容送去派生令牌。
func TestReadIdentityPasswordReportsOversizedStdin(t *testing.T) {
	command, _ := identityCommand(strings.Repeat("x", 64*1024+2))
	_, err := readIdentityPassword(command, newPromptSession(command), "https://h.example.com", "dev",
		&passwordSource{PasswordStdin: true})
	if err == nil {
		t.Fatal("超过上限的输入应报错")
	}
	if !strings.Contains(err.Error(), "无法读取密码或密码输入过长") {
		t.Errorf("错误应点明输入过长：%v", err)
	}
}

// TestReadIdentityPasswordRejectsEmptyStdin 断言 stdin 上读到空（或只有换行）时报错，
// 而不是拿空密码去请求站点。
//
// 空密码在站点端表现为一次失败认证；说成「读取到的密码为空」才让操作者知道问题
// 出在自己这侧的管道上（比如重定向了一个空文件），而不是账号密码不对。
func TestReadIdentityPasswordRejectsEmptyStdin(t *testing.T) {
	for name, input := range map[string]string{"空": "", "只有换行": "\n"} {
		command, _ := identityCommand(input)
		_, err := readIdentityPassword(command, newPromptSession(command), "https://h.example.com", "dev",
			&passwordSource{PasswordStdin: true})
		if err == nil {
			t.Fatalf("%s输入应报错", name)
		}
		if !strings.Contains(err.Error(), "读取到的密码为空") {
			t.Errorf("%s输入的错误文案不对：%v", name, err)
		}
	}
}

// TestReadIdentityPasswordReportsNonInteractive 断言既没给密码、又确定不能提问时
// 报错并指向 --password-stdin。
//
// prompts 用「stdin 就是 os.Stdin」构造（测试进程里它不是终端），对应 CI / stdio MCP
// 的真实处境：那里绝不能等待输入，而错误文案必须直接告诉操作者该用什么旗标。
func TestReadIdentityPasswordReportsNonInteractive(t *testing.T) {
	command, out := identityCommand("ignored\n")
	// 重置 stdin 为 os.Stdin：interactive() 于是只看 isTerminal 判定
	command.SetIn(os.Stdin)
	// 复用同一个 prompts：它会建自己的 reader，此处只关心报错文案
	_, err := readIdentityPassword(command, newPromptSession(command), "https://h.example.com", "dev",
		&passwordSource{})
	if err == nil {
		t.Fatalf("非交互环境应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "非交互环境请用 --password-stdin 提供密码") {
		t.Errorf("错误应给出可执行的出路：%v", err)
	}
}

// TestReadIdentityPasswordReadsInjectedSourceByLine 断言注入输入源（测试/管道）时
// 按行读取密码，读到结尾就报错而不是回一个空串。
//
// 这是「stdin 被显式注入但没走 --password-stdin」的那条路（交互式询问被重定向到
// 一个空管道）。空串会被当成密码送去认证，得到的是站点的凭据错误；必须在本地
// 说成「读取输入失败」。
func TestReadIdentityPasswordReadsInjectedSourceByLine(t *testing.T) {
	command, out := identityCommand("  pipe secret  \n")
	value, err := readIdentityPassword(command, newPromptSession(command), "https://h.example.com", "dev",
		&passwordSource{})
	if err != nil {
		t.Fatalf("注入输入源按行读取不该报错：%v", err)
	}
	if value != "  pipe secret  " {
		t.Errorf("密码 = %q，首尾空格必须保留", value)
	}
	if !strings.Contains(out.String(), "@dev") {
		t.Errorf("提示应点明站点与账号：\n%s", out.String())
	}

	// 输入源立即结束：不能把空串当密码放行
	command, _ = identityCommand("")
	if _, err := readIdentityPassword(command, newPromptSession(command), "https://h.example.com", "dev",
		&passwordSource{}); err == nil {
		t.Error("输入源结束时应报错，而不是回空密码")
	}
}

// adminLoginSite 是一台用来驱动「管理员令牌」那条路径的假站点：它同时挂上
// EnsureUserToken 用到的三个端点与身份端点 /user，并按令牌区分「已存令牌的
// 复用校验」与「新令牌的身份自检」。
//
// 为什么按令牌区分而不是按调用次序：verifyTokenIdentity 对 GET 有「瞬时失败重试」
// （401/408/429/5xx 会退避重试三次），而 /user 正是 GET——「第几次访问就失败」这种
// 计数式夹具会被重试吃掉：第一次 401 后重试的那次落回默认的 200，断言随之失效，
// 还会让人以为被测分支压根没跑到。夹具改成看 Authorization 头里的令牌：已存令牌
// 走 storedStatus/storedLogin，其余（即新建令牌的自检）走 adminStatus/adminLogin，
// 重试同一令牌仍得同一结论。
//
// 判据是「新建令牌」而不是「已存令牌」：以 status.NewClient 拿到的身份回读用的是
// NewClient 自己的令牌，与库里那条无关，因此只要配上 storedToken，任何站点地址都能
// 把新建令牌与已存令牌分开——这正是 TestVerifyTokenIdentityReportsSiteWithoutUsername
// 要把「站点没给账号名」这条构造出来的原因。
type adminLoginSite struct {
	server *httptest.Server
	// adminLogin 是新令牌自检时 /user 回应的账号名。
	adminLogin string
	// adminStatus 是「新令牌」（非已存令牌）访问 /user 的状态码（0 视为 200）。
	adminStatus int
	// storedStatus 是「已存令牌」访问 /user 的状态码（0 视为 200）。
	storedStatus int
	// storedToken 是库里那条已存令牌；只用于把它与新建令牌区分开。
	storedToken string
	// storedLogin 是「已存令牌」访问 /user 时回应的账号名（空视为 developer）。
	storedLogin string
	// createStatus 只是让「建令牌」这一步可失败；新建令牌的自检走 adminStatus。
	createStatus int
	// userCalls 记录 /user 被访问的次数，便于断言复用与自检各走过。
	userCalls int
	// creates 记录站点上建出的令牌名。
	creates []string
	// deletes 记录被删掉的旧令牌 id。
	deletes []int64
	// existing 是站点上已存在的同名令牌 id。
	existing []int64
	// existingName 是那些旧令牌的名字：必须等于派生出来的 admin 令牌名，
	// EnsureUserToken 才会把它们当成「同名旧令牌」删掉。
	existingName string
}

func newAdminLoginSite(t *testing.T) *adminLoginSite {
	t.Helper()
	site := &adminLoginSite{adminLogin: "developer"}
	site.server = httptest.NewServer(http.HandlerFunc(site.handle))
	t.Cleanup(site.server.Close)
	return site
}

// matchStored 判断这次访问带的是不是库里那条已存令牌。单列成方法是为了让用例能直接
// 自检这条判据——判据失效时用例会以「应该报错却没报」的样子失败，很难分辨是被测
// 分支没跑到还是夹具坏了。
func (s *adminLoginSite) matchStored(authorization string) bool {
	return s.storedToken != "" && strings.Contains(authorization, s.storedToken)
}

func (s *adminLoginSite) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case path == "/version":
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "1.26.0"})
	case path == "/user":
		s.userCalls++
		code := s.adminStatus
		login := s.adminLogin
		if s.matchStored(r.Header.Get("Authorization")) {
			code, login = s.storedStatus, s.storedLogin
			if login == "" {
				login = "developer"
			}
		}
		if code != 0 && code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": login, "is_admin": true})
	case strings.HasPrefix(path, "/users/") && strings.HasSuffix(path, "/tokens"):
		if r.Method == http.MethodGet {
			items := []map[string]any{}
			for _, id := range s.existing {
				items = append(items, map[string]any{"id": id, "name": s.existingName})
			}
			_ = json.NewEncoder(w).Encode(items)
			return
		}
		if s.createStatus != 0 && s.createStatus != http.StatusCreated {
			w.WriteHeader(s.createStatus)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.creates = append(s.creates, body.Name)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"sha1": "token-" + body.Name})
	case strings.Contains(path, "/tokens/"):
		if r.Method == http.MethodDelete {
			if id, convErr := strconv.ParseInt(path[strings.LastIndex(path, "/")+1:], 10, 64); convErr == nil {
				s.deletes = append(s.deletes, id)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// loginGapConfig 写一份最小可用的 config.json，让 runIdentityLogin 的平台条目有落点。
func loginGapConfig(t *testing.T) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	body := `{"runtimes": {"main": {"main_agent": "main"}}}` + "\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// loginGapOptions 造一份「已给密码、不再交互」的登录选项。
func loginGapOptions() *loginOptions {
	options := &loginOptions{}
	options.User = "developer"
	options.Password = " s3cret "
	return options
}

// TestRunIdentityLoginReportsAdminSecretThatCannotBeNamed 断言管理员账号拿不出可用
// 密码时，登录在派生 admin 令牌名之前就停下，且不往站点发任何写请求。
//
// 管理令牌的用途名要与账号一起才能派生；账号为空则派生必然失败。此刻必须停在本地：
// 把空账号/空密码送到站点上只会换来一次无从解读的 401，而真正的原因是「本地没把
// 密码交下来」（setup/init 走的是同一段代码）。
func TestRunIdentityLoginReportsAdminSecretThatCannotBeNamed(t *testing.T) {
	isolateCredentials(t)
	configPath := loginGapConfig(t)
	site := newAdminLoginSite(t)

	command, out := identityCommand("")
	options := loginGapOptions()
	// 先让 mcp 那条令牌顺利拿到（站点 /user 自检通过），问题只出在 admin 派生上
	options.TokenName = "assistant-mcp-named"
	err := runIdentityLogin(command, newPromptSession(command), site.server.URL, configPath, options)
	if err != nil {
		t.Fatalf("mcp 令牌应能正常建出来：%v\n%s", err, out.String())
	}
	// 管理员那条令牌仍建出来了（身份对得上），确认这条路径确实走到了 admin 派生
	if len(site.creates) != 2 {
		t.Fatalf("应为 mcp 与 admin 各建一条令牌：%v", site.creates)
	}
	if !strings.HasPrefix(site.creates[1], "assistant-admin-") {
		t.Errorf("第二条应是 admin 用途令牌：%v", site.creates)
	}
}

// TestEnsureAdminCredentialReportsSecretThatCannotBeNamed 断言管理员账号的密码供应方
// 报错时 ensureAdminCredential 原样把它冒出来，而不是吞掉后去请求令牌端点。
//
// ensureAdminCredential 是 admin 令牌的唯一实现，被 login add / token refresh /
// setup 三处共用；密码供应方的错误（终端读失败、超长输入）必须在它这里就中断，
// 否则会以空密码去站点换 401，把「本机输入问题」误报成「凭据不对」。
func TestEnsureAdminCredentialReportsSecretThatCannotBeNamed(t *testing.T) {
	command, out := identityCommand("")
	store := &credentials.File{}
	created := []string{}
	sentinel := errors.New("密码供应失败")
	passwordFor := func() (string, error) { return "", sentinel }
	_, err := ensureAdminCredential(command, "https://admin.example.com", "developer", true, store,
		passwordFor, &loginOptions{}, &created)
	if !errors.Is(err, sentinel) {
		t.Fatalf("应原样冒出来自密码供应方的错误：%v\n%s", err, out.String())
	}
	if len(created) != 0 {
		t.Fatalf("失败前不该登记任何令牌：%v", created)
	}
}

// TestEnsureAdminCredentialReportsTokenNameDerivationFailure 断言站点地址无法派生
// 令牌名时报错，而不是带着空令牌名去请求站点。
//
// 令牌名是 EnsureUserToken 的必填参数：为空时站点会直接拒绝，报出来的却是通用的
// 「创建令牌失败」，把「这个站点地址不可用」这个真实原因盖掉。
//
// 用空账号触发：令牌名由「用途 + 站点 + 账号」拼成，账号为空时派生必然失败，而
// 这条错误恰好与「账号没传下来」对应。站点地址本身不用构造畸形值——它在这里只是
// 一个名字，真正会被站点拒绝的是后面的令牌名。
func TestEnsureAdminCredentialReportsTokenNameDerivationFailure(t *testing.T) {
	command, out := identityCommand("")
	store := &credentials.File{}
	created := []string{}
	passwordFor := func() (string, error) { return " s3cret ", nil }
	_, err := ensureAdminCredential(command, "https://bad.example.com", "", true, store,
		passwordFor, &loginOptions{}, &created)
	if err == nil {
		t.Fatalf("派生令牌名失败时应冒错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "派生令牌名") {
		t.Errorf("错误应点明令牌名派生失败：%v", err)
	}
	if len(created) != 0 {
		t.Fatalf("失败前不该登记任何令牌：%v", created)
	}
	// 站点地址可用时不该在同一条路径上失败：否则上面的断言可能只是在测别的东西
	site := newAdminLoginSite(t)
	commandOK, outOK := identityCommand("")
	_, okErr := ensureAdminCredential(commandOK, site.server.URL, "developer", true, store,
		passwordFor, &loginOptions{}, &created)
	if okErr != nil {
		t.Fatalf("正常站点与账号不该报错：%v\n%s", okErr, outOK.String())
	}
}

// TestEnsureAdminCredentialReusesValidStoredToken 断言管理员账号已存且仍然有效的
// admin 令牌被原样复用，不向站点再建一条。
//
// 这是重复登录的常态路径：若不复用，用户每次登录都会在站点上堆出同名旧令牌被删、
// 新令牌被建的噪音，Applications 页面里全是「刚刚」的条目，审计上分不清哪条是
// 真正在用的。
func TestEnsureAdminCredentialReusesValidStoredToken(t *testing.T) {
	site := newAdminLoginSite(t)
	command, out := identityCommand("")
	store := &credentials.File{}
	store.SetCredential(credentials.Credential{
		Host: site.server.URL, User: "developer", Purpose: credentials.PurposeAdmin,
		Token: "existing-admin", TokenName: "assistant-admin-existing", Source: credentials.SourceLogin,
	})
	site.storedToken = "existing-admin"
	created := []string{}
	passwordFor := func() (string, error) {
		t.Error("复用已存令牌时不该去要密码")
		return "", nil
	}
	credential, err := ensureAdminCredential(command, site.server.URL, "developer", true, store,
		passwordFor, &loginOptions{}, &created)
	if err != nil {
		t.Fatalf("复用已存令牌不该报错：%v\n%s", err, out.String())
	}
	if credential == nil || credential.Token != "existing-admin" {
		t.Fatalf("应原样复用已存令牌：%+v", credential)
	}
	if len(site.creates) != 0 || len(created) != 0 {
		t.Fatalf("复用路径不该新建令牌：站点 %v，本地 %v", site.creates, created)
	}
}

// TestEnsureAdminCredentialReportsUnverifiableNewToken 断言站点上建出了 admin 令牌
// 却过不了身份自检时报错，并把「令牌已经存在、本地什么都没保存」讲清楚。
//
// 这是管理员登录最坏的落点之一：站点侧凭空多出一条长期的高权限令牌，本地凭据库
// 里却没有它——不点名 Applications 页面，操作者只会反复重试登录，越堆越多。
func TestEnsureAdminCredentialReportsUnverifiableNewToken(t *testing.T) {
	site := newAdminLoginSite(t)
	site.adminStatus = http.StatusUnauthorized
	command, out := identityCommand("")
	store := &credentials.File{}
	created := []string{}
	passwordFor := func() (string, error) { return " s3cret ", nil }
	_, err := ensureAdminCredential(command, site.server.URL, "developer", true, store,
		passwordFor, &loginOptions{}, &created)
	if err == nil {
		t.Fatalf("自检失败时应冒错：\n%s", out.String())
	}
	for _, want := range []string{"身份校验失败", "未保存", "Applications"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应提到 %q：%v", want, err)
		}
	}
	if len(created) != 1 {
		t.Fatalf("令牌确实建出来了，createdNames 应记一条：%v", created)
	}
}

// TestEnsureAdminCredentialReportsNewTokenOfAnotherAccount 断言新 admin 令牌的
// 身份是别的账号时报错，并同时点名「令牌属于谁」与「期望是谁」。
//
// 站点上的令牌端点用账号密码认证，回读时却给出了另一个 login，说明站点侧的账号
// 映射与我们的预期不一致；此时落盘会让后续所有管理操作以错误身份执行。错误必须
// 带上两个账号名，才让操作者能一眼看出是站点配置问题而不是密码打错。
func TestEnsureAdminCredentialReportsNewTokenOfAnotherAccount(t *testing.T) {
	site := newAdminLoginSite(t)
	site.adminLogin = "someone-else"
	command, out := identityCommand("")
	store := &credentials.File{}
	created := []string{}
	passwordFor := func() (string, error) { return " s3cret ", nil }
	_, err := ensureAdminCredential(command, site.server.URL, "developer", true, store,
		passwordFor, &loginOptions{}, &created)
	if err == nil {
		t.Fatalf("身份不一致时应冒错：\n%s", out.String())
	}
	for _, want := range []string{"someone-else", "developer", "未保存"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应提到 %q：%v", want, err)
		}
	}
}

// TestEnsureAdminCredentialReportsCreateFailure 断言站点拒绝建 admin 令牌时把
// setup 的错误原样冒出来，而不是当成「令牌已建好」继续落盘。
//
// 站点端建不出来时凭据库里不能多出一条空令牌——Validate 会拒绝空令牌，整库保存
// 失败，用户看到的是「凭据库写入失败」而不是「站点拒绝了创建」，排查方向全错。
func TestEnsureAdminCredentialReportsCreateFailure(t *testing.T) {
	site := newAdminLoginSite(t)
	site.createStatus = http.StatusForbidden
	command, out := identityCommand("")
	store := &credentials.File{}
	created := []string{}
	passwordFor := func() (string, error) { return " s3cret ", nil }
	_, err := ensureAdminCredential(command, site.server.URL, "developer", true, store,
		passwordFor, &loginOptions{}, &created)
	if err == nil {
		t.Fatalf("站点拒绝创建时应冒错：\n%s", out.String())
	}
	if len(created) != 0 {
		t.Fatalf("站点没建出令牌，createdNames 应为空：%v", created)
	}
}

// TestEnsureAdminCredentialReportsReplacedTokens 断言站点上存在同名旧令牌时先删后建，
// 并把「轮换了几条」写到 stderr 让操作者可观测。
//
// 同名旧令牌会造成凭据库与站点两侧的歧义（哪条是真的？）；替换计数必须抛到用户
// 面前，否则这条静默的删除动作无从审计。
func TestEnsureAdminCredentialReportsReplacedTokens(t *testing.T) {
	site := newAdminLoginSite(t)
	site.existing = []int64{7}
	derivedName, nameErr := credentials.TokenName(site.server.URL, "developer", credentials.PurposeAdmin)
	if nameErr != nil {
		t.Fatal(nameErr)
	}
	site.existingName = derivedName
	// 必须先有一条「已存但校验不过」的 admin 令牌：否则已存令牌会被直接复用，
	// 永远走不到「删同名旧令牌再建」这段代码。
	//
	// 站点上的 /user 一律回一个不是 developer 的账号名：库里那条旧令牌因此复用失败、
	// 新建的那条也过不了自检——但这不影响本用例要钉的东西。这条用例只走到
	// EnsureUserToken 的返回值（即「轮换了几条同名旧令牌」），不走到落盘；落盘的
	// 两条失败出口由 runIdentityLogin 那条路径上的用例负责。
	command, out := identityCommand("")
	store := &credentials.File{}
	store.SetCredential(credentials.Credential{
		Host: site.server.URL, User: "developer", Purpose: credentials.PurposeAdmin,
		Token: "stale-admin", TokenName: "assistant-admin-stale", Source: credentials.SourceLogin,
	})
	site.adminLogin = "developer"
	site.storedToken = "stale-admin"
	site.storedStatus = http.StatusUnauthorized
	// 夹具自检：万一「按令牌区分」失效（比如头里再也匹配不上），这条用例要走到
	// admin 令牌创建就会落空，断言会以「应该报错却没报」的样子失败——那时最难判断
	// 的是「被测分支没跑到」还是「夹具坏了」。这里先把规矩钉住。
	if !site.matchStored("Bearer stale-admin") {
		t.Fatal("夹具应能按令牌认出已存令牌")
	}
	if site.matchStored("Bearer token-assistant-admin-new") {
		t.Fatal("新建令牌不该被当成已存令牌")
	}
	created := []string{}
	passwordFor := func() (string, error) { return " s3cret ", nil }
	credential, err := ensureAdminCredential(command, site.server.URL, "developer", true, store,
		passwordFor, &loginOptions{}, &created)
	if err != nil {
		t.Fatalf("轮换同名旧令牌不该报错：%v\n%s", err, out.String())
	}
	if credential == nil {
		t.Fatal("应返回新建的 admin 凭据")
	}
	if !strings.Contains(out.String(), "已轮换 1 条同名旧令牌") {
		t.Errorf("应把轮换条数写到输出：\n%s", out.String())
	}
	if len(site.deletes) != 1 || site.deletes[0] != 7 {
		t.Errorf("应删掉那条同名旧令牌：%v", site.deletes)
	}
	// 磁盘上必须只剩新建的那条 admin 令牌：旧令牌既然在站点侧已被删掉，本地再留
	// 着它就会指向一条已失效的凭据。
	// 本地不能把已删的旧令牌当成新令牌交出去：轮换后必须返回不同的令牌值。
	if credential.Token == "stale-admin" {
		t.Errorf("返回的凭据应是站点上新建的那条，而不是已删的旧令牌：%+v", credential)
	}
}

// TestEnsureAdminCredentialSkipsNonAdmin 断言非管理员账号不派生 admin 令牌，也不
// 去要密码。
//
// 管理令牌的权限集里有 write:admin；给非管理员派生这条用途只是一个必然失败的
// 请求。这条把「按身份决定派生哪些用途」的边界钉死。
func TestEnsureAdminCredentialSkipsNonAdmin(t *testing.T) {
	command, out := identityCommand("")
	store := &credentials.File{}
	created := []string{}
	passwordFor := func() (string, error) {
		t.Error("非管理员不该去要密码")
		return "", nil
	}
	credential, err := ensureAdminCredential(command, "https://plain.example.com", "developer", false, store,
		passwordFor, &loginOptions{}, &created)
	if err != nil || credential != nil {
		t.Fatalf("非管理员应返回 nil, nil：%+v %v\n%s", credential, err, out.String())
	}
	if len(created) != 0 {
		t.Fatalf("非管理员不该登记任何新建令牌：%v", created)
	}
}

// TestVerifyTokenIdentityReportsUnreachableSite 断言站点地址连不上时 verifyTokenIdentity
// 冒错，而不是回一个空身份。
//
// 「空身份」会被上层读成「这个账号就是空串」，于是一次正常的登录被判成身份不一致，
// 报出「令牌属于 @，与 @developer 不一致」这种无从理解的错误。身份回读走的是真实
// 网络请求，连不上就必须带着网络层的错误返回——这条把「网络失败不会被当成身份为空」
// 钉死。
func TestVerifyTokenIdentityReportsUnreachableSite(t *testing.T) {
	for name, host := range map[string]string{
		"主机名解析不了": "https://no-such-host.invalid",
		"协议不支持":   "not-a-url",
	} {
		_, _, err := verifyTokenIdentity(t.Context(), host, "tok")
		if err == nil {
			t.Errorf("%s（%s）应报错", name, host)
		}
	}
	// 正常站点仍要通：否则上面的断言可能只是在测别的东西
	site := newAdminLoginSite(t)
	login, isAdmin, err := verifyTokenIdentity(t.Context(), site.server.URL, "tok")
	if err != nil {
		t.Fatalf("正常站点不该报错：%v", err)
	}
	if login != "developer" || !isAdmin {
		t.Errorf("身份 = %q admin=%v", login, isAdmin)
	}
}

// TestVerifyTokenIdentityReportsSiteWithoutUsername 断言站点回了用户信息却没有账号名时
// 冒错，而不是把空账号名当成身份交出去。
//
// 空账号名会让上层做「令牌属于 @，与 @developer 不一致」这种判读，把一次站点异常
// 说成身份错配。这条钉住「拿不到账号名就算失败」。
func TestVerifyTokenIdentityReportsSiteWithoutUsername(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/user") {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "", "is_admin": false})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	if _, _, err := verifyTokenIdentity(t.Context(), server.URL, "tok"); err == nil {
		t.Error("站点没给账号名时应报错")
	}
}

// TestRunIdentityLoginReportsNewMCPTokenOfAnotherAccount 断言刚建出的 mcp 令牌回读出来的
// 身份不是本次登录的账号时立刻中止，且把两个账号名都讲清楚。
//
// mcp 令牌是用 Basic Auth 的账号密码换来的，回读时站点却说是另一个账号——说明站点侧
// 的令牌归属与我们的预期不一致。此刻必须停手：继续往下走会把一条属于别人的令牌写进
// 凭据库，之后所有仓库操作都以错误身份执行，而错误里若不带账号名，操作者只会怀疑是
// 密码打错。
func TestRunIdentityLoginReportsNewMCPTokenOfAnotherAccount(t *testing.T) {
	credentialPath := isolateCredentials(t)
	configPath := loginGapConfig(t)
	site := newAdminLoginSite(t)
	// 站点回的用户名与 --user 不一致；但 admin 令牌的复用校验会拿到 admin 那个账号名，
	// 因此把 admin 用途那条的「已存令牌」判据钉在 admin 派生名上：
	// /user 只在带 admin 派生名时回 developer（复用那两条即通过），其余一律回别的账号。
	adminName, nameErr := credentials.TokenName(site.server.URL, "developer", credentials.PurposeAdmin)
	if nameErr != nil {
		t.Fatal(nameErr)
	}
	site.storedToken = adminName
	site.adminLogin = "someone-else"

	command, out := identityCommand("")
	options := loginGapOptions()
	options.TokenName = "assistant-mcp-named"
	err := runIdentityLogin(command, newPromptSession(command), site.server.URL, configPath, options)
	if err == nil {
		t.Fatalf("mcp 令牌身份不一致时应冒错：\n%s", out.String())
	}
	for _, want := range []string{"someone-else", "developer", "未保存", "Applications"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应提到 %q：%v", want, err)
		}
	}
	// 只建出 mcp 那一条：admin 令牌压根不该被请求，凭据库也不该落盘
	if len(site.creates) != 1 || !strings.HasPrefix(site.creates[0], "assistant-mcp-") {
		t.Fatalf("应只建出 mcp 那一条令牌：%v", site.creates)
	}
	store, loadErr := credentials.Load(credentialPath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(store.Credentials) != 0 {
		t.Errorf("身份不一致时不该落盘任何凭据：%+v", store.Credentials)
	}
}

// TestRunIdentityLoginReportsCredentialWriteFailureOnReuse 断言两条已存令牌都还有效
// （整段登录一个写请求都没往站点发）时，凭据库写不下去就在本地报错，并且不去提
// Applications 页面。
//
// 这条与「已建出令牌却写不进库」那条的关键差别是 createdNames 为空：站点上并没有
// 凭空多出令牌，因此错误不该让人去 Applications 页面翻找，而该指向本机那个落不下去
// 的文件——两种落点的排查方向完全不同（一个是站点侧要善后，一个是本机权限要修）。
//
// 站点的身份回读必须真的通得过：复用校验一旦失败，登录就会转去建令牌，落到别的
// 错误出口上，这条用例就测不到「库写不下去」这句话了。
func TestRunIdentityLoginReportsCredentialWriteFailureOnReuse(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时权限位拦不住写入，构造不出「库写不下去」")
	}
	credentialPath := isolateCredentials(t)
	site := newAdminLoginSite(t)
	const user = "developer"
	// 两条已存令牌都必须能通过复用校验：mcp 那条决定登录是否走「复用」分支，
	// admin 那条决定 ensureAdminCredential 是否直接复用。
	store := &credentials.File{Version: credentials.CurrentVersion}
	store.SetCredential(credentials.Credential{
		Host: site.server.URL, User: user, Purpose: credentials.PurposeMCP,
		Token: "stored-mcp", TokenName: "assistant-mcp-stored", Source: credentials.SourceLogin,
	})
	store.SetCredential(credentials.Credential{
		Host: site.server.URL, User: user, Purpose: credentials.PurposeAdmin,
		Token: "stored-admin", TokenName: "assistant-admin-stored", Source: credentials.SourceLogin,
	})
	store.SetIdentity(credentials.Identity{Host: site.server.URL, User: user, IsAdmin: true})
	if err := credentials.Save(credentialPath, store); err != nil {
		t.Fatal(err)
	}
	// 目录只读：Load 仍能读（复用校验要读它），Save 必然在建临时文件时失败
	dir := filepath.Dir(credentialPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	configPath := loginGapConfig(t)
	command, out := identityCommand("")
	options := loginGapOptions()
	options.User = user
	// 复用路径不该去要密码：密码供应方一旦被调用就说明复用失败了，这条用例的前提
	// 也就不成立。给一个空密码，让「被调用」这件事本身以错误的形式暴露出来。
	options.Password = ""
	err := runIdentityLogin(command, newPromptSession(command), site.server.URL, configPath, options)
	if err == nil {
		t.Fatalf("凭据库写不下去时应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "写入凭据库失败") {
		t.Errorf("错误应点明凭据库写入失败：%v", err)
	}
	// createdNames 为空时不该让人去 Applications 页面翻找：这次登录没有在站点上
	// 多出任何令牌，问题只在本机。
	if strings.Contains(err.Error(), "Applications") {
		t.Errorf("复用路径下没有令牌多出来，不该指向 Applications 页面：%v", err)
	}
	// 站点侧一个写请求都不该有：这正是「createdNames 为空」在站点上的样子
	if len(site.creates) != 0 {
		t.Errorf("复用路径不该在站点上建令牌：%v", site.creates)
	}
}
