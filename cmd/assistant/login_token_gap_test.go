package main

import (
	"bytes"
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

// tokenRefreshServer 是一台只为轮换失败面服务的假 Gitea：令牌端点（列出同名旧
// 令牌 / 删除 / 新建）正常，但新令牌的身份自检可由用例指定失败方式。
//
// 为什么不复用 login_identity_test.go 的 newLoginServer：那台夹具的 /api/v1/user
// 只检查 Authorization 头是否存在，任何请求都回 200，轮换里「令牌建出来了但身份
// 校验不过」这一整片分支永远走不到。而它恰恰是轮换最危险的落点——站点上新令牌
// 已经存在、本地却什么都没保存，操作者若只看「命令报了错」而不知道凭据库仍指向
// 已删掉的旧令牌，会以为重跑一次就好。
//
//   - identityStatus/mode = ok 且 login 一致：自检通过（正常路径）。
//   - mode = unauthorized：/user 回 401 → verifyTokenIdentity 报错。
//   - login = 别的账号：自检成功但身份对不上。
type tokenRefreshServer struct {
	server *httptest.Server
	// oldTokens 是站点上同名的旧令牌 ID 列表，轮换时会逐条删除。
	oldTokens []int64
	// createStatus 新建令牌的 HTTP 状态码（0 视为 201）。
	createStatus int
	// identityStatus 自检端点 /user 的状态码（0 视为 200）。
	identityStatus int
	// login 自检端点回应的账号名。
	login string
	// creates / deletes 记录写侧调用，便于断言「删除已发生但未保存」。
	creates []string
	deletes []int64
	// scopes 记录每条新建令牌收到的权限集，便于断言「权限集回落/沿用」。
	scopes map[string][]string
	// tokenName 覆盖同名旧令牌的名字（缺省用 assistant-mcp-gitea-example-com-ge）。
	tokenName string
}

func newTokenRefreshServer(t *testing.T, oldTokens []int64) *tokenRefreshServer {
	t.Helper()
	fake := &tokenRefreshServer{oldTokens: oldTokens, login: "ge", scopes: map[string][]string{}}
	fake.tokenName = "assistant-mcp-gitea-example-com-ge"
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *tokenRefreshServer) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case path == "/version":
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "1.26.0"})
	case path == "/user":
		if f.identityStatus != 0 && f.identityStatus != http.StatusOK {
			w.WriteHeader(f.identityStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": f.login, "is_admin": false})
	case strings.HasPrefix(path, "/users/") && strings.HasSuffix(path, "/tokens"):
		// 令牌端点必须带目标账号的 Basic Auth（密码由用例通过 --password /
		// --password-stdin 送到）：只有核对过凭据，用例才能证明「密码真的送到了
		// 站点」而不是被静默丢弃。这也让「密码没送到」以 401 的形式显式暴露。
		//
		// 比较用 TrimSpace：--password 旗标的值原样进 Basic Auth（不去空白），
		// 而用例写的 " s3cret " 是为了证明「旗标值就是站点收到的那串字节」；
		// 按空格敏感比较会把每个用该写法的用例误判成「密码没送到」。
		if authUser, authPassword, ok := r.BasicAuth(); !ok || authUser != "ge" || strings.TrimSpace(authPassword) != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			list := make([]map[string]any, 0, len(f.oldTokens)+len(f.creates))
			for _, id := range f.oldTokens {
				list = append(list, map[string]any{"id": id, "name": f.oldTokenName()})
			}
			// 真实 Gitea 在建出令牌后会让 GetAccessTokens 立刻看到它；这里照搬，
			// 否则 EnsureUserToken 的「建 → 再列一次确认」在夹具上会看到一份不含
			// 新令牌的列表，走到本站点并不存在的分支上去。
			for index, name := range f.creates {
				list = append(list, map[string]any{"id": int64(1000 + index), "name": name})
			}
			_ = json.NewEncoder(w).Encode(list)
			return
		}
		var body struct {
			Name   string   `json:"name"`
			Scopes []string `json:"scopes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.createStatus != 0 && f.createStatus != http.StatusCreated {
			w.WriteHeader(f.createStatus)
			return
		}
		f.creates = append(f.creates, body.Name)
		f.scopes[body.Name] = body.Scopes
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"sha1": "rotated-token-" + body.Name})
	case strings.Contains(path, "/tokens/"):
		id := path[strings.LastIndex(path, "/")+1:]
		f.deletes = append(f.deletes, parseInt(id))
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// oldTokenName 是站点上同名旧令牌的名字，与凭据记录里的令牌名一致——轮换靠它
// 找到并删除旧令牌，名字对不上就一条都删不到。
func (f *tokenRefreshServer) oldTokenName() string {
	if f.tokenName != "" {
		return f.tokenName
	}
	return "assistant-mcp-gitea-example-com-ge"
}

func parseInt(value string) int64 {
	var result int64
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0
		}
		result = result*10 + int64(digit-'0')
	}
	return result
}

// seedRefreshStore 写一份只有 mcp 令牌的凭据库，令牌名与假站点上的同名旧令牌
// 一致（轮换必须保持这个名字，否则站点上删不掉任何东西）。
//
// host 必须与随后传给 runRefresh 的站点地址完全一致：凭据按 (host, user, purpose)
// 唯一，host 对不上时轮换会先在「没有该用途令牌」处停下，站点侧一条请求都不会发。
func seedRefreshStore(t *testing.T, host string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: host, User: "ge", IsAdmin: true})
	store.SetCredential(credentials.Credential{
		Host: host, User: "ge", Purpose: credentials.PurposeMCP,
		Token: "old-mcp-token", TokenName: "assistant-mcp-gitea-example-com-ge",
		Scopes: credentials.MCPScopes(), Source: credentials.SourceLogin,
	})
	if err := credentials.Save(path, store); err != nil {
		t.Fatal(err)
	}
	return path
}

// runRefresh 用固定密码跑一次轮换命令；返回值是错误与 stdout。
func runRefresh(t *testing.T, host string) (error, string) {
	t.Helper()
	var out bytes.Buffer
	command := newLoginTokenRefreshCommand()
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{host, credentials.PurposeMCP, "--user", "ge", "--password", " s3cret "})
	err := command.Execute()
	return err, out.String()
}

// TestRunTokenRefreshRejectsIdentityCheckFailureWithoutReplacedOld 断言站点上
// 没有同名旧令牌（replaced==0）而新令牌身份自检失败时，报错说明令牌已建出、
// 未保存，并提醒去 Applications 页面处理。
//
// 这条路径没有「旧令牌已被删」的连带后果，因此错误里不该出现「本地凭据库仍指向
// 失效令牌」这类危言——把两种情况混成一句话会让操作者去修一个并没有坏的凭据。
func TestRunTokenRefreshRejectsIdentityCheckFailureWithoutReplacedOld(t *testing.T) {
	fake := newTokenRefreshServer(t, nil)
	fake.identityStatus = http.StatusUnauthorized
	storePath := seedRefreshStore(t, fake.server.URL)

	runErr, _ := runRefresh(t, fake.server.URL)
	if runErr == nil {
		t.Fatal("自检失败应报错")
	}
	if !strings.Contains(runErr.Error(), "身份校验失败，未保存") {
		t.Errorf("err = %v, want 身份校验失败且未保存", runErr)
	}
	if strings.Contains(runErr.Error(), "本地凭据库仍指向失效令牌") {
		t.Errorf("没有删除过旧令牌，不该声称凭据库失效：%v", runErr)
	}

	stored, err := credentials.Load(storePath)
	if err != nil {
		t.Fatal(err)
	}
	credential, ok := stored.CredentialForUser(fake.server.URL, "ge", credentials.PurposeMCP)
	if !ok || credential.Token != "old-mcp-token" {
		t.Errorf("自检失败时不该改写凭据库，got %+v", credential)
	}
}

// TestRunTokenRefreshRejectsIdentityCheckFailureAfterReplacedOld 断言同名旧令牌
// 已被删除、新令牌身份自检又失败时，错误必须点明「本地凭据库仍指向失效令牌」
// 并给出重新登录的恢复路径。
//
// 这是轮换唯一会把用户锁在站点外的形态：旧令牌在站点上没了，本地还拿着它。
// 只报一句「身份校验失败」会让操作者重试，而重试用的正是那条已失效的密码之外
// 的唯一凭据——必须在错误里就把后果和下一步写清楚。
func TestRunTokenRefreshRejectsIdentityCheckFailureAfterReplacedOld(t *testing.T) {
	fake := newTokenRefreshServer(t, []int64{7})
	fake.identityStatus = http.StatusUnauthorized
	storePath := seedRefreshStore(t, fake.server.URL)

	runErr, _ := runRefresh(t, fake.server.URL)
	if runErr == nil {
		t.Fatal("自检失败应报错")
	}
	for _, want := range []string{
		"身份校验失败，未保存", "本地凭据库仍指向失效令牌", "请重新登录",
		"1 条同名旧令牌已被删除", "assistant login add " + fake.server.URL + " --user ge",
	} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("错误缺少 %q：%v", want, runErr)
		}
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != 7 {
		t.Errorf("轮换应删掉同名旧令牌 7，got %v", fake.deletes)
	}

	stored, err := credentials.Load(storePath)
	if err != nil {
		t.Fatal(err)
	}
	credential, ok := stored.CredentialForUser(fake.server.URL, "ge", credentials.PurposeMCP)
	if ok && credential.Token != "old-mcp-token" {
		t.Errorf("自检失败时不该保存新令牌，got %q", credential.Token)
	}
}

// TestRunTokenRefreshRejectsTokenOfAnotherAccount 断言令牌自检成功但站点回的身份
// 与目标账号不一致时：不落盘，并在错误里点名双方账号（外面还得说清凭据库是否
// 已失效）。
//
// 站点上 token 被别处复用、或鉴权接口串了账号，都表现为这里。若把别人的令牌写进
// 本账号的条目，之后以该账号名义做的所有评审/合并都会用错误身份执行。
func TestRunTokenRefreshRejectsTokenOfAnotherAccount(t *testing.T) {
	cases := []struct {
		name         string
		oldTokens    []int64
		wantLocked   bool
		wantExtraOps []string
	}{
		{name: "无同名旧令牌", oldTokens: nil, wantLocked: false},
		{name: "已删同名旧令牌", oldTokens: []int64{7}, wantLocked: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newTokenRefreshServer(t, testCase.oldTokens)
			fake.login = "someone-else"
			storePath := seedRefreshStore(t, fake.server.URL)

			runErr, _ := runRefresh(t, fake.server.URL)
			if runErr == nil {
				t.Fatal("身份不一致应报错")
			}
			for _, want := range []string{
				"someone-else", "@ge 不一致", "未保存", "撤销该令牌",
			} {
				if !strings.Contains(runErr.Error(), want) {
					t.Errorf("错误缺少 %q：%v", want, runErr)
				}
			}
			gotLocked := strings.Contains(runErr.Error(), "本地凭据库已失效")
			if gotLocked != testCase.wantLocked {
				t.Errorf("本地凭据库已失效 = %v, want %v（err=%v）", gotLocked, testCase.wantLocked, runErr)
			}
			if testCase.wantLocked && !strings.Contains(runErr.Error(), "请重新登录") {
				t.Errorf("删除过旧令牌时必须给出重新登录的恢复路径：%v", runErr)
			}

			stored, err := credentials.Load(storePath)
			if err != nil {
				t.Fatal(err)
			}
			credential, ok := stored.CredentialForUser(fake.server.URL, "ge", credentials.PurposeMCP)
			if ok && credential.Token != "old-mcp-token" {
				t.Errorf("身份不一致时不该改写凭据库，got %q", credential.Token)
			}
		})
	}
}

// TestRunTokenRefreshReportsCreateFailureWithoutReplacedOld 断言站点上无同名旧
// 令牌、新建直接失败时，错误只讲重建失败本身（加令牌名），不编造「凭据库仍指向
// 失效令牌」——根本没删过东西。
//
// replaced>0 的同形错误已由 TestLoginTokenRefreshPartialFailureExplainsLockout
// 覆盖；两条分支的文案差别就是操作者判断「本地还能不能用」的唯一依据。
func TestRunTokenRefreshReportsCreateFailureWithoutReplacedOld(t *testing.T) {
	fake := newTokenRefreshServer(t, nil)
	fake.createStatus = http.StatusInternalServerError
	seedRefreshStore(t, fake.server.URL)

	runErr, _ := runRefresh(t, fake.server.URL)
	if runErr == nil {
		t.Fatal("新建失败应报错")
	}
	if !strings.Contains(runErr.Error(), "重建令牌 assistant-mcp-gitea-example-com-ge 失败") {
		t.Errorf("err = %v, want 点名令牌名的重建失败", runErr)
	}
	if strings.Contains(runErr.Error(), "本地凭据库仍指向失效令牌") {
		t.Errorf("没有删除过旧令牌，不该声称凭据库失效：%v", runErr)
	}
}

// TestRunTokenRefreshKeepsStoredScopesAndSource 断言轮换沿用记录里的权限集与来源，
// 且成功后站点上确实删掉了同名旧令牌、凭据库换成新令牌。
//
// 令牌名与权限集必须原样保持：站点侧同名替换是唯一可行的续期方式，权限集变了会
// 让既有的 CI / MCP 在下次调用时才发现被降权。来源字段（login/setup）决定其它
// 命令怎么提示恢复路径，轮换不能把它抹成默认值。
func TestRunTokenRefreshKeepsStoredScopesAndSource(t *testing.T) {
	fake := newTokenRefreshServer(t, []int64{7, 8})
	storePath := seedRefreshStore(t, fake.server.URL)
	// 来源标成 setup：轮换必须保留它（凭据来自 setup 还是 login 决定提示语）
	stored, err := credentials.Load(storePath)
	if err != nil {
		t.Fatal(err)
	}
	credential, ok := stored.CredentialForUser(fake.server.URL, "ge", credentials.PurposeMCP)
	if !ok {
		t.Fatal("种子凭据应存在")
	}
	credential.Source = credentials.SourceSetup
	credential.Scopes = credentials.MCPScopes()
	stored.SetCredential(credential)
	if err := credentials.Save(storePath, stored); err != nil {
		t.Fatal(err)
	}

	runErr, out := runRefresh(t, fake.server.URL)
	if runErr != nil {
		t.Fatalf("轮换应成功：%v", runErr)
	}
	if !strings.Contains(out, "已轮换 2 条同名旧令牌") {
		t.Errorf("输出应报告轮换条数：\n%s", out)
	}
	if len(fake.creates) != 1 {
		t.Fatalf("应新建 1 条令牌，got %v", fake.creates)
	}
	if len(fake.deletes) != 2 {
		t.Errorf("应删掉 2 条同名旧令牌，got %v", fake.deletes)
	}

	after, err := credentials.Load(storePath)
	if err != nil {
		t.Fatal(err)
	}
	rotated, ok := after.CredentialForUser(fake.server.URL, "ge", credentials.PurposeMCP)
	if !ok {
		t.Fatal("轮换后凭据应仍在")
	}
	if rotated.Token != "rotated-token-"+credential.TokenName {
		t.Errorf("凭据未换成新令牌：%q", rotated.Token)
	}
	if rotated.Source != credentials.SourceSetup {
		t.Errorf("来源应保持 setup，got %q", rotated.Source)
	}
	if rotated.TokenName != credential.TokenName {
		t.Errorf("令牌名不该变：%q", rotated.TokenName)
	}
}

// runTokenCommand 跑一次 login token 子命令（list / show），把 stdout 与 stderr 收在一起
// 返回；不传 args 时走命令自身的 Usage（用于断言「只出帮助」这类退化入口）。
func runTokenCommand(t *testing.T, newCommand func() *cobra.Command, args ...string) (error, string) {
	t.Helper()
	var out bytes.Buffer
	command := newCommand()
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs(args)
	err := command.Execute()
	return err, out.String()
}

// TestLoginTokenCommandWithoutArgsPrintsHelp 断言 `login token` 裸调（既没有位置参数
// 也没有子命令）时只打印自己的用法并成功返回，而不是把 Args 校验交给某个子命令去误报。
//
// 这是操作者最常见的探索入口：不知道该用 list 还是 show 时先敲 `login token`。
// 若这里返回错误或什么都不打印，帮助信息就只能靠猜子命令名才看得到。
func TestLoginTokenCommandWithoutArgsPrintsHelp(t *testing.T) {
	runErr, out := runTokenCommand(t, func() *cobra.Command { return newLoginTokenCommand(nil) })
	if runErr != nil {
		t.Fatalf("裸调 login token 不该报错：%v", runErr)
	}
	if !strings.Contains(out, "list") || !strings.Contains(out, "refresh") {
		t.Errorf("应列出可用子命令：\n%s", out)
	}
}

// TestLoginTokenShowReportsUnknownPurpose 断言 show 的 purpose 位置参数不认识时报错，
// 且错误里把已登记用途都列出来。
//
// 凭据库只认登记过的用途；不认识时不报错、静默回落到 mcp 会把「我要看 admin 令牌」
// 变成「给你看 mcp 令牌」——操作者看到的是真令牌，却属于另一个用途，据此排查会跑偏。
func TestLoginTokenShowReportsUnknownPurpose(t *testing.T) {
	host, _ := seedTokenShowStore(t)
	runErr, _ := runTokenCommand(t, newLoginTokenShowCommand, host, "not-a-purpose")
	if runErr == nil {
		t.Fatal("未知用途应报错")
	}
	if !strings.Contains(runErr.Error(), "未知用途") {
		t.Errorf("错误应点明用途未知：%v", runErr)
	}
	for _, want := range credentials.Purposes() {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("错误应列出已登记用途 %q：%v", want, runErr)
		}
	}
}

// TestLoginTokenRefreshReportsUnknownPurpose 断言 refresh 的 purpose 参数不认识时
// 在派生令牌名之前就停下，不向站点发任何写请求。
//
// 轮换是写操作（删旧建新）；带着未知用途往下走会先删掉一份站点上的旧令牌，再在
// 「用途不合法」处失败——磁盘上的令牌就这么没了，而报错只提用途，操作者根本不会
// 把它和「站点上少了条令牌」联系起来。
func TestLoginTokenRefreshReportsUnknownPurpose(t *testing.T) {
	fake := newTokenRefreshServer(t, []int64{7})
	seedRefreshStore(t, fake.server.URL)

	runErr, out := runTokenCommand(t, newLoginTokenRefreshCommand,
		fake.server.URL, "not-a-purpose", "--user", "ge", "--password", " s3cret ")
	if runErr == nil {
		t.Fatalf("未知用途应报错：\n%s", out)
	}
	if !strings.Contains(runErr.Error(), "未知用途") {
		t.Errorf("错误应点明用途未知：%v", runErr)
	}
	if len(fake.deletes) != 0 || len(fake.creates) != 0 {
		t.Errorf("用途不合法时不该碰站点：删 %v，建 %v", fake.deletes, fake.creates)
	}
}

// TestLoginTokenListWithoutMatchingCredentials 断言库非空但按 host / user 过滤后没有
// 命中时明确说「没有匹配的令牌」，而不是只打一行空表头让人以为命令坏了。
//
// 过滤条件写错（大小写、带不带 scheme）是高频失误；静默的空输出会被读成「这份凭据
// 库是空的」，于是操作者转去除错凭据库本身，而真正的问题只是一个拼错的 host。
func TestLoginTokenListWithoutMatchingCredentials(t *testing.T) {
	seedTokenShowStore(t)
	runErr, out := runTokenCommand(t, newLoginTokenListCommand, "--user", "nobody")
	if runErr != nil {
		t.Fatalf("无命中不该报错：%v", runErr)
	}
	if !strings.Contains(out, "没有匹配的令牌") {
		t.Errorf("应明确说明没有命中：\n%s", out)
	}
}

// TestLoginTokenListReportsEmptyStore 断言库为空时 list 给出的是「怎么把第一条令牌登记
// 进来」，而不是一张空表。
//
// 空库是新装环境的第一现场；这时操作者需要的是下一步动作（login add <host>），
// 而不是需要自己去猜为什么没有任何输出。
func TestLoginTokenListReportsEmptyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	if err := credentials.Save(path, &credentials.File{}); err != nil {
		t.Fatal(err)
	}
	runErr, out := runTokenCommand(t, newLoginTokenListCommand)
	if runErr != nil {
		t.Fatalf("空库不该报错：%v", runErr)
	}
	if !strings.Contains(out, "凭据库为空") || !strings.Contains(out, "login add") {
		t.Errorf("空库应给出下一步动作：\n%s", out)
	}
}

// seedTokenShowStore 写一份含 mcp 与 admin 两条用途令牌的凭据库（host 由调用方决定
// 还是固定域名？固定域名：show / list 只读本地，不需要站点可达——不依赖网络是本条
// 夹具的重点，否则用例会被 DNS / 代理拖成偶发失败）。
func seedTokenShowStore(t *testing.T) (string, string) {
	t.Helper()
	const host = "https://tokens.example.com"
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: host, User: "ge", IsAdmin: true})
	store.SetCredential(credentials.Credential{
		Host: host, User: "ge", Purpose: credentials.PurposeMCP,
		Token: "mcp-token-value", TokenName: "assistant-mcp-tokens-example-com-ge",
		Scopes: credentials.MCPScopes(), Source: credentials.SourceLogin,
	})
	store.SetCredential(credentials.Credential{
		Host: host, User: "ge", Purpose: credentials.PurposeAdmin,
		Token: "admin-token-value", TokenName: "assistant-admin-tokens-example-com-ge",
		Scopes: credentials.AdminScopes(), Source: credentials.SourceLogin,
	})
	if err := credentials.Save(path, store); err != nil {
		t.Fatal(err)
	}
	return host, path
}

// TestLoginTokenShowReportsCorruptStore 断言凭据文件损坏时 show 原样冒出解析错误，
// 而不是把它当成「这条用途没登记过」。
//
// show 的下一句是「用 login add 派生」；库坏了却照着这句话重登，只会再写一份同样
// 读不出来的文件——错误必须让人看到「文件解析失败」，才可能去修那份文件。
func TestLoginTokenShowReportsCorruptStore(t *testing.T) {
	corruptCredentialStore(t)
	runErr, _ := runTokenCommand(t, newLoginTokenShowCommand, "https://tokens.example.com")
	if runErr == nil {
		t.Fatal("凭据库损坏应报错")
	}
	if strings.Contains(runErr.Error(), "没有") && strings.Contains(runErr.Error(), "用途令牌") {
		t.Errorf("不该把解析失败说成「未登记」：%v", runErr)
	}
	if !strings.Contains(runErr.Error(), "credentials.json") {
		t.Errorf("错误应点出坏掉的文件：%v", runErr)
	}
}

// TestLoginTokenRefreshReportsCorruptStore 断言凭据文件损坏时 refresh 在向站点发出
// 任何写请求之前就停下。
//
// 轮换会先删站点上的同名旧令牌。若读本地失败被当成「没有该用途令牌」而继续，
// 就会出现「站点上删了、本地又没记录」的双丢局面——比单纯报错糟得多，因为那份
// 令牌从此谁也用不上，且没有任何本地痕迹能提醒操作者。
func TestLoginTokenRefreshReportsCorruptStore(t *testing.T) {
	path := corruptCredentialStore(t)
	_ = path
	runErr, out := runTokenCommand(t, newLoginTokenRefreshCommand,
		"https://tokens.example.com", credentials.PurposeMCP, "--user", "ge", "--password", " s3cret ")
	if runErr == nil {
		t.Fatalf("凭据库损坏应报错：\n%s", out)
	}
	if !strings.Contains(runErr.Error(), "credentials.json") {
		t.Errorf("错误应点出坏掉的文件：%v", runErr)
	}
}

// TestLoginTokenListReportsCorruptStore 断言凭据文件损坏时 list 同样冒错，不打印
// 「凭据库为空」。
//
// 「为空」与「坏了」在操作者眼里是天差地别的两件事：前者意味着还没登录，后者意味着
// 已经登录过但文件被谁改坏了（可能连令牌都还在站点上）。把后者说成前者，会让人以
// 为无凭据可用而重复登录并新建一堆令牌。
func TestLoginTokenListReportsCorruptStore(t *testing.T) {
	corruptCredentialStore(t)
	runErr, out := runTokenCommand(t, newLoginTokenListCommand)
	if runErr == nil {
		t.Fatalf("凭据库损坏应报错：\n%s", out)
	}
	if strings.Contains(out, "凭据库为空") {
		t.Errorf("不该把损坏的库说成空库：\n%s", out)
	}
}

// readOnlyCredentialStore 写一份按指定内容落地的凭据库，并把所在目录改成不可写，
// 返回凭据路径。
//
// 「库写不下去」是轮换里仅次于「旧令牌已删」的坏消息出口：站点上已经建出新令牌、
// 本地却留不住。要单独打到这个出口，必须让 Load 能读到旧记录（复用路径要读它）
// 而 Save 必失败——只有目录权限能同时满足这两条，故用 0500 目录 + 清理时复原。
func readOnlyCredentialStore(t *testing.T, content string) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时权限位拦不住写入，构造不出「库写不下去」")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return path
}

// loginTokenCredentialsContent 造一份只含一条 mcp 令牌的凭据库内容（JSON 文本）。
//
// 直接写 JSON 而不是走 credentials.Save：用例要控制的正是文件里那些「正常写库
// 造不出来」的字段组合（空令牌名、空权限集、语义非法的用途），Save 会把它们
// 规范化掉或直接拒绝落盘。
func loginTokenCredentialsContent(t *testing.T, host, tokenName, purpose string, scopes []string) string {
	t.Helper()
	content, err := json.Marshal(map[string]any{
		"version": credentials.CurrentVersion,
		"identities": []map[string]any{
			{"host": host, "user": "ge", "is_admin": true},
		},
		"credentials": []map[string]any{{
			"host": host, "user": "ge", "purpose": purpose,
			"token": "stored-token", "token_name": tokenName, "scopes": scopes,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// TestLoginTokenShowCheckReportsInvalidToken 断言 show --check 在站点否认该令牌时
// 报错，并把两个出路一起说出来：重新派生（login add）与直接轮换（token refresh）。
//
// --check 是「这条令牌还能用吗」的唯一本地判据，失败时操作者正处在需要立刻换一条
// 的状态。只报一句「令牌无效」而不给命令，等于让人自己回忆该用 add 还是 refresh；
// 而这两条路的代价完全不同（add 会新建、refresh 会删站点上的同名旧令牌）。
func TestLoginTokenShowCheckReportsInvalidToken(t *testing.T) {
	host, _ := seedTokenShowStore(t)
	fake := newTokenRefreshServer(t, nil)
	fake.server.Config.Close()
	// 让凭据里的 host 指向一台已经关闭的站点：--check 必然拿不到身份。
	if err := reseedCredentialHost(t, host, fake.server.URL); err != nil {
		t.Fatal(err)
	}

	runErr, out := runTokenCommand(t, newLoginTokenShowCommand, fake.server.URL, credentials.PurposeMCP, "--check")
	if runErr == nil {
		t.Fatalf("站点不可达时 --check 应报错：\n%s", out)
	}
	for _, want := range []string{"令牌无效或站点不可达", "assistant login token refresh"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("错误应提到 %q：%v", want, runErr)
		}
	}
	// 记录字段在 --check 之前就打印了：这是有意的（--check 只是附加动作），
	// 断言它仍在，避免将来把校验前移而让操作者在失败时看不到任何记录。
	if !strings.Contains(out, "令牌：mcp-token-value") {
		t.Errorf("--check 失败也不该吞掉记录本身：\n%s", out)
	}
}

// reseedCredentialHost 把凭据库里 host 下所有用途的令牌改挂到 newHost 上。
//
// show --check 会用记录里的 host 去验证令牌，而夹具 seedTokenShowStore 固定用
// 一个不存在的域名（只读路径不依赖网络）。要测网络失败路径，必须把记录指到一台
// 真实起过又关掉的站点上。
func reseedCredentialHost(t *testing.T, oldHost, newHost string) error {
	t.Helper()
	path, err := credentials.Path()
	if err != nil {
		return err
	}
	store, err := credentials.Load(path)
	if err != nil {
		return err
	}
	for _, credential := range store.Credentials {
		if credential.Host != oldHost {
			continue
		}
		credential.Host = newHost
		store.SetCredential(credential)
	}
	return credentials.Save(path, store)
}

// TestLoginTokenRefreshReportsUnreadableStore 断言轮换在凭据库读不出来时报错，
// 报错点明坏在哪个文件，并且没有向站点发出任何写请求。
//
// 轮换的第一步是删站点上的同名旧令牌。若本地读失败被当成「没有该用途令牌」而继续，
// 就会删掉站点上那条谁都再用不上的令牌，而本地没有任何痕迹能提醒操作者——比单纯
// 报错糟得多。错误文案由命令框架原样带出解析失败，因此这里钉的是「文件路径 +
// 站点零改动」这两条可观察事实。
func TestLoginTokenRefreshReportsUnreadableStore(t *testing.T) {
	path := corruptCredentialStore(t)
	fake := newTokenRefreshServer(t, []int64{7})

	runErr, out := runTokenCommand(t, newLoginTokenRefreshCommand,
		fake.server.URL, credentials.PurposeMCP, "--user", "ge", "--password", " s3cret ")
	if runErr == nil {
		t.Fatalf("凭据库读不出来时应报错：\n%s", out)
	}
	if !strings.Contains(runErr.Error(), path) {
		t.Errorf("错误应点明坏掉的凭据库 %s：%v", path, runErr)
	}
	if len(fake.deletes) != 0 || len(fake.creates) != 0 {
		t.Errorf("库读不出来时不该碰站点：删 %v，建 %v", fake.deletes, fake.creates)
	}
}

// TestLoginTokenShowReportsUnreadableStore 断言 show 在凭据库读不出来时报错，
// 且不打印任何记录字段。
//
// show 输出的是令牌明文。库读不出来时若继续走「查不到这条用途」的分支，打印出来
// 的将是一句「用 login add 派生」——操作者据此重登会覆盖掉一份只是被改坏、令牌
// 可能还在站点上有效的记录。错误必须停在解析这一步。
func TestLoginTokenShowReportsUnreadableStore(t *testing.T) {
	path := corruptCredentialStore(t)
	runErr, out := runTokenCommand(t, newLoginTokenShowCommand, "https://tokens.example.com")
	if runErr == nil {
		t.Fatalf("凭据库读不出来时应报错：\n%s", out)
	}
	if !strings.Contains(runErr.Error(), path) {
		t.Errorf("错误应点明坏掉的凭据库 %s：%v", path, runErr)
	}
	if strings.Contains(out, "令牌：") {
		t.Errorf("读不出来时不该输出任何记录：\n%s", out)
	}
	if strings.Contains(runErr.Error(), "用途令牌（assistant login add") {
		t.Errorf("不该把读失败说成「没有该用途令牌」：%v", runErr)
	}
}

// TestLoginTokenListReportsUnreadableStore 断言 list 在凭据库读不出来时报错，
// 且不复述「凭据库为空」之外的空表语义。
//
// 「为空」与「坏了」对操作者是两个世界：前者说明还没登录，后者说明登录过但文件
// 被改坏（令牌多半还在站点上）。list 是排错时第一个敲的命令，它必须把两者分清。
func TestLoginTokenListReportsUnreadableStore(t *testing.T) {
	path := corruptCredentialStore(t)
	runErr, out := runTokenCommand(t, newLoginTokenListCommand)
	if runErr == nil {
		t.Fatalf("凭据库读不出来时应报错：\n%s", out)
	}
	if !strings.Contains(runErr.Error(), path) {
		t.Errorf("错误应点明坏掉的凭据库 %s：%v", path, runErr)
	}
	if strings.Contains(out, "凭据库为空") || strings.Contains(out, "凭据库：") {
		t.Errorf("读不出来时不该打印空表语义：\n%s", out)
	}
}

// TestLoginTokenRefreshRoundTripsThroughStdinPassword 断言 --password-stdin 的密码
// 真的送到了站点（Basic Auth 通过）、新令牌建出并写回凭据库。
//
// 轮换的密码来源与 add 共用一条链（--password > --password-stdin > 终端隐藏输入）。
// 这条用例专门走 stdin：它是最容易被接线忽略的一环（管道里的密码读没读、读到的
// 是哪个变量），而站点侧用 Basic Auth 校验，密码不对会以 401 的形式暴露出来。
func TestLoginTokenRefreshRoundTripsThroughStdinPassword(t *testing.T) {
	fake := newTokenRefreshServer(t, []int64{7})
	// 凭据里的 host 必须与站点地址逐字一致（凭据按 host 索引），否则轮换会在
	// 「没有该用途令牌」处停下，站点一条请求都收不到。
	storePath := seedRefreshStore(t, fake.server.URL)

	command := newLoginTokenRefreshCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader(" s3cret \n"))
	command.SetContext(t.Context())
	command.SetArgs([]string{fake.server.URL, credentials.PurposeMCP, "--user", "ge", "--password-stdin"})

	if err := command.Execute(); err != nil {
		t.Fatalf("轮换应成功：%v\n%s", err, out.String())
	}
	// 站点侧用 Basic Auth 核对：建出了令牌就说明管道里的密码确实被读到了
	if len(fake.creates) != 1 {
		t.Fatalf("应从 stdin 取到密码并建出令牌：%v", fake.creates)
	}
	stored, err := credentials.Load(storePath)
	if err != nil {
		t.Fatal(err)
	}
	rotated, ok := stored.CredentialForUser(fake.server.URL, "ge", credentials.PurposeMCP)
	if !ok || rotated.Token != "rotated-token-"+fake.creates[0] {
		t.Errorf("新令牌应写回凭据库，got %+v", rotated)
	}
}

// TestLoginTokenRefreshReportsPasswordSourceFailure 断言注入的输入源读到 EOF 时
// 轮换立刻停下，且站点上一条新令牌都没建出来。
//
// 密码是轮换的前置条件（站点不接受无密码的令牌重建）。若把它排在「建令牌」之后，
// 失败时站点上会先多出一条令牌再报错，本地却什么都没记——正是最该避免的落空态。
//
// 构造的是「输入流当场 EOF」：既没有 --password 也没有 --password-stdin，注入的空
// 输入源在第一行就返回 EOF。此时 readIdentityPassword 走注入输入源那一路，返回
// 「读取输入失败: EOF」——它必须原样冒到最外层，不能被替换成一句「缺令牌」之类的
// 文案，否则操作者会去查凭据库而不是去补密码来源。
//
// 密码在读输入这一步就失败，所以在站点被碰之前就停下：creates / deletes 都必须为空。
// 这正证明密码获取排在所有站点写操作之前——若它被挪到 EnsureUserToken 之后，站点上
// 会先多出一条令牌再报错，本地却什么都没记，是最难善后的落空态。
func TestLoginTokenRefreshReportsPasswordSourceFailure(t *testing.T) {
	fake := newTokenRefreshServer(t, []int64{7})
	seedRefreshStore(t, fake.server.URL)

	command := newLoginTokenRefreshCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	// 关键：不给 --password / --password-stdin，注入一个读到 EOF 的空源。
	// 不注入的话 cobra 会绑到 os.Stdin，测试进程里它可能是终端——那样这条用例就
	// 变成在等真人输入，而不是在构造「拿不到密码」。
	command.SetIn(strings.NewReader(""))
	command.SetContext(t.Context())
	command.SetArgs([]string{fake.server.URL, credentials.PurposeMCP, "--user", "ge"})

	err := command.Execute()
	if err == nil {
		t.Fatalf("输入源为空时应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "读取输入失败") {
		t.Errorf("错误应点明是从输入源读密码失败：%v", err)
	}
	if len(fake.creates) != 0 || len(fake.deletes) != 0 {
		t.Errorf("密码拿不到时不该碰站点：建 %v，删 %v", fake.creates, fake.deletes)
	}
}

// TestLoginTokenRefreshReportsCredentialSaveFailure 断言站点上已建出新令牌、
// 凭据库却写不下去时，报错把「新令牌已在站点上」讲清楚。
//
// 这是轮换里唯一「两边都对不上」的落点：站点多出一条新令牌，本地还记着旧的那条
// （旧令牌已被同名替换删除）。只报一句「写入凭据库失败」会让操作者以为轮换没发生，
// 于是站点上越堆越多。
func TestLoginTokenRefreshReportsCredentialSaveFailure(t *testing.T) {
	fake := newTokenRefreshServer(t, []int64{7})
	fake.tokenName = "assistant-mcp-refresh-save"
	content := loginTokenCredentialsContent(t, fake.server.URL, "assistant-mcp-refresh-save", credentials.PurposeMCP, credentials.MCPScopes())
	readOnlyCredentialStore(t, content)

	runErr, out := runTokenCommand(t, newLoginTokenRefreshCommand,
		fake.server.URL, credentials.PurposeMCP, "--user", "ge", "--password", " s3cret ")
	if runErr == nil {
		t.Fatalf("凭据库写不下去时应报错：\n%s", out)
	}
	if !strings.Contains(runErr.Error(), "写入凭据库失败") {
		t.Errorf("错误应点明凭据库写入失败：%v", runErr)
	}
	if len(fake.creates) != 1 {
		t.Errorf("站点上应已建出新令牌：建 %v", fake.creates)
	}
	if len(fake.deletes) != 1 {
		t.Errorf("站点上的同名旧令牌应已被删掉：删 %v", fake.deletes)
	}
}

// TestLoginTokenRefreshKeepsStoredTokenNameAndScopes 断言 refresh 原样沿用记录里
// 的令牌名与权限集，即使站点侧旧令牌有两条同名记录也全部删掉。
//
// 令牌名是站点侧同名替换的唯一句柄：名字变了就删不掉旧的，站上会长期留一条谁也
// 不知道的令牌。权限集同理——降权要到下一次调用才暴露，而那时已经来不及回溯是
// 哪次轮换改的。
func TestLoginTokenRefreshKeepsStoredTokenNameAndScopes(t *testing.T) {
	fake := newTokenRefreshServer(t, []int64{3, 4})
	fake.tokenName = "assistant-mcp-refresh-keep"
	content := loginTokenCredentialsContent(t, fake.server.URL, "assistant-mcp-refresh-keep", credentials.PurposeMCP, credentials.MCPScopes())
	path := corruptCredentialStoreBytes(t, content)

	runErr, out := runTokenCommand(t, newLoginTokenRefreshCommand,
		fake.server.URL, credentials.PurposeMCP, "--user", "ge", "--password", " s3cret ")
	if runErr != nil {
		t.Fatalf("轮换应成功：%v\n%s", runErr, out)
	}
	if len(fake.creates) != 1 || fake.creates[0] != "assistant-mcp-refresh-keep" {
		t.Fatalf("应沿用记录里的令牌名：%v", fake.creates)
	}
	if !strings.Contains(out, "已轮换 2 条同名旧令牌") {
		t.Errorf("应报告删掉的同名旧令牌条数：\n%s", out)
	}
	if len(fake.deletes) != 2 {
		t.Errorf("应删掉站点上两条同名旧令牌：%v", fake.deletes)
	}
	stored, err := credentials.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	rotated, ok := stored.CredentialForUser(fake.server.URL, "ge", credentials.PurposeMCP)
	if !ok {
		t.Fatal("轮换后凭据应仍在")
	}
	if rotated.TokenName != "assistant-mcp-refresh-keep" {
		t.Errorf("令牌名不该变：%q", rotated.TokenName)
	}
	if len(rotated.Scopes) != len(credentials.MCPScopes()) {
		t.Errorf("权限集不该变：%v", rotated.Scopes)
	}
}

// TestLoginTokenDerivesNameWhenStoredTokenNameMissing 断言记录里的令牌名缺失时
// refresh 现场按 (host, user, purpose) 派生出规范名，并用它去删站点上的同名旧令牌。
//
// 早期版本的凭据库可能没写 token_name（或手工改库时删掉了）。名字缺失若被当成
// 空名直接拿去建令牌，站点会收下一条无名令牌，而同名替换就永远删不掉它——所以
// 这里必须派生。断言派生结果命中站点上那条同名旧令牌。
func TestLoginTokenDerivesNameWhenStoredTokenNameMissing(t *testing.T) {
	fake := newTokenRefreshServer(t, []int64{9})
	derived, err := credentials.TokenName(fake.server.URL, "ge", credentials.PurposeMCP)
	if err != nil {
		t.Fatal(err)
	}
	// 站点上那条旧令牌必须正好叫派生名，否则「命中了它」就无从谈起
	fake.tokenName = derived
	content := loginTokenCredentialsContent(t, fake.server.URL, "", credentials.PurposeMCP, credentials.MCPScopes())
	corruptCredentialStoreBytes(t, content)

	runErr, out := runTokenCommand(t, newLoginTokenRefreshCommand,
		fake.server.URL, credentials.PurposeMCP, "--user", "ge", "--password", " s3cret ")
	if runErr != nil {
		t.Fatalf("轮换应成功：%v\n%s", runErr, out)
	}
	if len(fake.creates) != 1 || fake.creates[0] != derived {
		t.Errorf("应按 (host,user,purpose) 派生令牌名 %q，got %v", derived, fake.creates)
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != 9 {
		t.Errorf("派生的名字应能命中站点上的同名旧令牌：%v", fake.deletes)
	}
}

// TestLoginTokenRefreshFallsBackToDefaultScopes 断言记录里权限集为空时 refresh
// 回落到该用途的默认权限集，而不是拿空集去建令牌。
//
// Gitea 建令牌时不传 scopes 会得到站点默认（通常是全权），比该用途需要的权限大
// 得多——一条只用来读仓库的 mcp 令牌会变成能改设置的管理令牌。回落到
// credentials.DefaultScopes 是唯一安全的做法，必须钉住。
func TestLoginTokenRefreshFallsBackToDefaultScopes(t *testing.T) {
	fake := newTokenRefreshServer(t, []int64{11})
	fake.tokenName = "assistant-mcp-refresh-scopes"
	content := loginTokenCredentialsContent(t, fake.server.URL, "assistant-mcp-refresh-scopes", credentials.PurposeMCP, nil)
	corruptCredentialStoreBytes(t, content)

	runErr, out := runTokenCommand(t, newLoginTokenRefreshCommand,
		fake.server.URL, credentials.PurposeMCP, "--user", "ge", "--password", " s3cret ")
	if runErr != nil {
		t.Fatalf("轮换应成功：%v\n%s", runErr, out)
	}
	if len(fake.scopes) != 1 {
		t.Fatalf("站点应收到一次建令牌请求：%v", fake.scopes)
	}
	got := fake.scopes["assistant-mcp-refresh-scopes"]
	if len(got) != len(credentials.MCPScopes()) {
		t.Errorf("权限集应回落到该用途默认值：%v", got)
	}
}

// stubTokenCredentialPath 临时把凭据库定位换成给定的实现，返回还原函数。
//
// 定位失败在 HOME 与 XDG_CONFIG_HOME 皆空时真实发生（os.UserConfigDir 会返回
// "neither $XDG_CONFIG_HOME nor $HOME are defined"，已实测）——容器里以空环境跑
// 就是这种情形。本机默认构造不出来（两者都有值），故用注入缝换掉它，把
// list/show/refresh 三条命令里「连凭据文件落点都定不下来」的 return 分支打到。
func stubTokenCredentialPath(t *testing.T, fn func() (string, error)) {
	t.Helper()
	original := loginTokenCredentialPath
	loginTokenCredentialPath = fn
	t.Cleanup(func() { loginTokenCredentialPath = original })
}

// stubTokenCredentials 临时把凭据库读取换成给定的实现，返回还原函数。
//
// 有了它，「落点解析成功、读取失败」这一条能在不弄坏真实文件的前提下打到：注入缝
// 直接返回一个包装错误，命令必须把它原样带出来，而不是当成「没有该用途令牌」。
func stubTokenCredentials(t *testing.T, fn func(string) (*credentials.File, error)) {
	t.Helper()
	original := loginTokenCredentials
	loginTokenCredentials = fn
	t.Cleanup(func() { loginTokenCredentials = original })
}

// TestLoginTokenCommandsReportUnresolvableCredentialPath 断言三条令牌命令在凭据库
// 落点定不下来时都立刻报错，而不是拿一个拼出来的路径继续往下读。
//
// 落点解析失败意味着这台机器既没有 XDG_CONFIG_HOME 也没有 HOME（容器里以空环境跑、
// 或 HOME 被清掉）。此时任何「拼个相对路径去读」的兜底都会让命令在错误的目录里找到
// 一份陌生凭据、或者悄悄把自己当成未登录。三条命令共享同一条前置链，必须都挡住——
// 只有 list 挡住而 show/refresh 放行的话，最危险的那条（会删站点令牌的 refresh）
// 恰恰能在无法确认本地状态的前提下动手。
func TestLoginTokenCommandsReportUnresolvableCredentialPath(t *testing.T) {
	stubTokenCredentialPath(t, func() (string, error) {
		return "", errors.New("定位凭据目录失败：HOME 与 XDG_CONFIG_HOME 皆为空")
	})
	cases := []struct {
		name    string
		command func() *cobra.Command
		args    []string
	}{
		{"list", newLoginTokenListCommand, nil},
		{"show", newLoginTokenShowCommand, []string{"https://tokens.example.com"}},
		{"refresh", newLoginTokenRefreshCommand, []string{"https://tokens.example.com", credentials.PurposeMCP, "--user", "ge", "--password", "pw"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runErr, out := runTokenCommand(t, testCase.command, testCase.args...)
			if runErr == nil {
				t.Fatalf("落点定不下来时应报错：\n%s", out)
			}
			if !strings.Contains(runErr.Error(), "定位凭据目录失败") {
				t.Errorf("错误应保留底层定位失败的原因：%v", runErr)
			}
		})
	}
}

// TestLoginTokenShowReportsCredentialLoadFailure 断言 show 在凭据库读取失败时把
// 底层错误原样带出来，而不是把它说成「没有该用途令牌」。
//
// 两者对操作者是完全不同的动作：前者要去修本机那个读不出来的文件，后者会让人去
// Gitea 重新派生一条令牌——而本地文件根本没修好，重派一次还是读不出来。
func TestLoginTokenShowReportsCredentialLoadFailure(t *testing.T) {
	stubTokenCredentialPath(t, func() (string, error) { return "/tmp/whatever.json", nil })
	stubTokenCredentials(t, func(string) (*credentials.File, error) {
		return nil, errors.New("读取 /tmp/whatever.json 失败：权限不足")
	})

	runErr, out := runTokenCommand(t, newLoginTokenShowCommand, "https://tokens.example.com")
	if runErr == nil {
		t.Fatalf("读取失败时应报错：\n%s", out)
	}
	if !strings.Contains(runErr.Error(), "权限不足") {
		t.Errorf("错误应保留底层读取失败的原因：%v", runErr)
	}
	if strings.Contains(runErr.Error(), "没有") && strings.Contains(runErr.Error(), "令牌") {
		t.Errorf("不该把它说成没有该用途令牌：%v", runErr)
	}
}

// TestLoginTokenRefreshReportsCredentialLoadFailure 断言 refresh 在凭据库读取失败
// 时停下：既不在站点上动任何令牌，也不把错误伪装成「没登录过」。
//
// refresh 会删站点上的同名旧令牌，前提是本地那份记录可信。读取失败时这条前提不成立，
// 必须就地停下——否则一次误判就会把一条仍然有效的令牌从站点删掉，而本地记录还留着它。
func TestLoginTokenRefreshReportsCredentialLoadFailure(t *testing.T) {
	fake := newTokenRefreshServer(t, []int64{7})
	stubTokenCredentialPath(t, func() (string, error) { return "/tmp/whatever.json", nil })
	stubTokenCredentials(t, func(string) (*credentials.File, error) {
		return nil, errors.New("读取 /tmp/whatever.json 失败：权限不足")
	})

	runErr, out := runTokenCommand(t, newLoginTokenRefreshCommand,
		fake.server.URL, credentials.PurposeMCP, "--user", "ge", "--password", "pw")
	if runErr == nil {
		t.Fatalf("读取失败时应报错：\n%s", out)
	}
	if !strings.Contains(runErr.Error(), "权限不足") {
		t.Errorf("错误应保留底层读取失败的原因：%v", runErr)
	}
	if len(fake.creates) != 0 || len(fake.deletes) != 0 {
		t.Errorf("凭据不可信时不该碰站点：建 %v，删 %v", fake.creates, fake.deletes)
	}
}

// TestLoginTokenListReportsCredentialLoadFailure 断言 list 在凭据库读取失败时报错，
// 而不是打印一句「凭据库为空」。
//
// 「空库」与「读不出来」对操作者是相反的结论：前者说明还没登录过，后者说明已经登录过
// 但本机状态坏了。把后者显示成前者，会让人重新 login add，把一份读不出来的文件再覆盖
// 一次，原本还能救的状态就此丢失。
func TestLoginTokenListReportsCredentialLoadFailure(t *testing.T) {
	stubTokenCredentialPath(t, func() (string, error) { return "/tmp/whatever.json", nil })
	stubTokenCredentials(t, func(string) (*credentials.File, error) {
		return nil, errors.New("读取 /tmp/whatever.json 失败：权限不足")
	})

	runErr, out := runTokenCommand(t, newLoginTokenListCommand)
	if runErr == nil {
		t.Fatalf("读取失败时应报错：\n%s", out)
	}
	if !strings.Contains(runErr.Error(), "权限不足") {
		t.Errorf("错误应保留底层读取失败的原因：%v", runErr)
	}
	if strings.Contains(out, "凭据库为空") {
		t.Errorf("读不出来时不该打印空库语义：\n%s", out)
	}
}

// TestResolveTokenUserPrefersRecordedIdentity 断言站点有两个账号时，身份记录决定
// 操作哪一个，而不是报「请用 --user 指定」。
//
// 身份记录就是「本站点当前登录的是谁」这一事实，是 assistant login add 写下的。
// 忽略它、把「存在多个账号」当成歧义，会让最常见的一次操作（派生 mcp 令牌）在只
// 登录过其中一个账号的机器上莫名要求 --user，操作者只能靠猜。
func TestResolveTokenUserPrefersRecordedIdentity(t *testing.T) {
	host := "https://identity.example.com"
	store := &credentials.File{Version: credentials.CurrentVersion}
	store.SetIdentity(credentials.Identity{Host: host, User: "ge"})
	store.SetCredential(credentials.Credential{
		Host: host, User: "someone-else", Purpose: credentials.PurposeMCP, Token: "other-token",
	})

	user, err := resolveTokenUser(store, host, "")
	if err != nil {
		t.Fatalf("有身份记录时不该报错：%v", err)
	}
	if user != "ge" {
		t.Errorf("应以身份记录的账号为准，got %q", user)
	}
}

// seedAmbiguousTokenStore 写一份「站点上有两个账号、但没有身份记录」的凭据库，
// 用来构造 resolveTokenUser 的两个拒绝分支：完全没人（0 个账号）与多账号无定论。
//
// 这两种机器状态都真实存在：多账号是同一台机登过两个 Gitea 账号，没身份记录是
// 老版本凭据库或手工编辑过。此时命令若「猜一个账号」，轮换就可能删掉另一个账号
// 在站点上同名的令牌——令牌名按 (host,user,purpose) 生成，不会撞，但操作者拿到
// 的结果会指向他没打算动的那个人。
func seedAmbiguousTokenStore(t *testing.T, host string, users ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	store := &credentials.File{}
	for _, user := range users {
		store.SetCredential(credentials.Credential{
			Host: host, User: user, Purpose: credentials.PurposeMCP,
			Token: "token-" + user, TokenName: "assistant-mcp-" + user,
			Scopes: credentials.MCPScopes(), Source: credentials.SourceLogin,
		})
	}
	if err := credentials.Save(path, store); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoginTokenShowRejectsAmbiguousAccount 断言没有身份记录且站点有多个账号时
// show 直接拒绝，并把可选账号列出来让人用 --user 点名。
//
// 不猜是刻意的：show 会打印令牌明文，猜错账号等于把另一个账号的令牌送到操作者
// 眼前（可能贴进某个外部配置里）。宁可让人多敲一个 --user。
func TestLoginTokenShowRejectsAmbiguousAccount(t *testing.T) {
	host := "https://multi.example.com"
	seedAmbiguousTokenStore(t, host, "ge", "bot")

	runErr, out := runTokenCommand(t, newLoginTokenShowCommand, host)
	if runErr == nil {
		t.Fatalf("多账号且无身份记录时应拒绝并索要 --user：\n%s", out)
	}
	for _, want := range []string{"多个账号", "@ge", "@bot", "--user"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("错误应提到 %q：%v", want, runErr)
		}
	}
	// 显式点名后必须走得通：证明拒绝的只是「不猜」，不是「用不了」。
	runErr, out = runTokenCommand(t, newLoginTokenShowCommand, host, "--user", "bot")
	if runErr != nil {
		t.Fatalf("点名账号应能查看：%v\n%s", runErr, out)
	}
	if !strings.Contains(out, "token-bot") {
		t.Errorf("应输出被点名账号的令牌明文：\n%s", out)
	}
}

// TestLoginTokenShowReportsNoAccounts 断言凭据库里该站点一个账号都没有时，错误
// 指路 login add，而不是笼统地说「找不到令牌」。
//
// 两者对操作者是不同动作：没账号 = 这台机器从没登过该站点（去 add）；有账号没这条
// 用途 = 派生时的 purpose 不对（去 list 看已有用途）。混为一谈会让人白跑一次登录。
func TestLoginTokenShowReportsNoAccounts(t *testing.T) {
	host := "https://never.example.com"
	seedAmbiguousTokenStore(t, host)

	runErr, out := runTokenCommand(t, newLoginTokenShowCommand, host)
	if runErr == nil {
		t.Fatalf("无账号时应报错：\n%s", out)
	}
	if !strings.Contains(runErr.Error(), "assistant login add") {
		t.Errorf("错误应给出登录命令：%v", runErr)
	}
	if strings.Contains(runErr.Error(), "--user") {
		t.Errorf("一个账号都没有时不该索要 --user：%v", runErr)
	}
}

// TestLoginTokenRefreshRejectsAmbiguousAccount 断言轮换同样拒绝在多账号无身份记录
// 时猜测账号。
//
// 轮换比 show 更重：它会在站点上删掉同名旧令牌再重建。猜错账号会改动另一个账号的
// 站点令牌，而操作者本地凭据库指向的那份纹丝不动——他以为续了期，实际什么都没变。
func TestLoginTokenRefreshRejectsAmbiguousAccount(t *testing.T) {
	host := "https://multi.example.com"
	seedAmbiguousTokenStore(t, host, "ge", "bot")

	runErr, out := runTokenCommand(t, newLoginTokenRefreshCommand, host, "--password-stdin")
	if runErr == nil {
		t.Fatalf("多账号且无身份记录时轮换应拒绝：\n%s", out)
	}
	if !strings.Contains(runErr.Error(), "--user") {
		t.Errorf("错误应索要 --user：%v", runErr)
	}
}

// TestLoginTokenRefreshReportsNoAccounts 断言轮换在无账号时给出的错误指向 login add。
func TestLoginTokenRefreshReportsNoAccounts(t *testing.T) {
	host := "https://never.example.com"
	seedAmbiguousTokenStore(t, host)

	runErr, out := runTokenCommand(t, newLoginTokenRefreshCommand, host, "--password-stdin")
	if runErr == nil {
		t.Fatalf("无账号时应报错：\n%s", out)
	}
	if !strings.Contains(runErr.Error(), "assistant login add") {
		t.Errorf("错误应给出登录命令：%v", runErr)
	}
}

// TestLoginTokenRefreshSynthesizesMissingTokenNameAndScopes 断言凭据记录里缺
// 令牌名/权限集时，轮换按 (host,user,purpose) 与用途缺省值补齐后再去站点重建。
//
// 缺这两项的记录可能来自手工编辑的凭据库或更早版本的派生流程。站点对空令牌名会
// 拒绝（或建出一条事后在 Applications 页面无法辨认的令牌）；空权限集则意味着新
// 令牌的授权面跟旧的不一致。此处钉的是「补齐后站点收到的确实是这两样」——只断言
// 命令成功是不够的，命令也可以在发空值时恰好被假站点容忍。
func TestLoginTokenRefreshSynthesizesMissingTokenNameAndScopes(t *testing.T) {
	host := "https://tokens.example.com"
	// 记录里刻意不写 token_name / scopes：只留令牌值本身。
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: host, User: "ge"})
	store.SetCredential(credentials.Credential{
		Host: host, User: "ge", Purpose: credentials.PurposeMCP,
		Token: "old-token", Source: credentials.SourceLogin,
	})
	if err := credentials.Save(path, store); err != nil {
		t.Fatal(err)
	}

	fake := newTokenRefreshServer(t, nil)
	// 把记录改挂到真实站点地址上（凭据按 host 索引，必须逐字一致）。
	if err := reseedCredentialHost(t, host, fake.server.URL); err != nil {
		t.Fatal(err)
	}

	command := newLoginTokenRefreshCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader("s3cret\n"))
	command.SetContext(t.Context())
	command.SetArgs([]string{fake.server.URL, credentials.PurposeMCP, "--password-stdin"})
	if err := command.Execute(); err != nil {
		t.Fatalf("缺令牌名/权限集时应按缺省补齐完成轮换：%v\n%s", err, out.String())
	}

	// 站点收到的令牌名必须是按 (host,user,purpose) 生成的那个，不是空串。
	wantName, err := credentials.TokenName(fake.server.URL, "ge", credentials.PurposeMCP)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.creates) != 1 {
		t.Fatalf("站点应收到一次新建：%v", fake.creates)
	}
	if fake.creates[0] != wantName {
		t.Errorf("站点收到的令牌名应为合成的 %q，实际 %q", wantName, fake.creates[0])
	}
	// 权限集必须退回该用途的登记缺省值，而不是空集。
	wantScopes, err := credentials.DefaultScopes(credentials.PurposeMCP)
	if err != nil {
		t.Fatal(err)
	}
	got := fake.scopes[wantName]
	if strings.Join(got, ",") != strings.Join(wantScopes, ",") {
		t.Errorf("权限集应退回用途缺省值 %v，实际 %v", wantScopes, got)
	}
}

// TestLoginTokenRefreshReportsMismatchedIdentity 断言新令牌自检出来属于另一个账号时
// 轮换拒绝保存，并且站点上没有任何同名旧令牌被删（replaced==0）。
//
// 这是最像「成功」的失败：站点建出了令牌、自检也通过，只是令牌属于别人。若照常写回
// 本地，凭据库里就会存下一条不属于当前账号的令牌——之后所有以该账号身份做的事
// （评审、合并）都会以别人的名义发生。必须按身份不符处理。
func TestLoginTokenRefreshReportsMismatchedIdentity(t *testing.T) {
	fake := newTokenRefreshServer(t, nil)
	fake.login = "someone-else"
	seedRefreshStore(t, fake.server.URL)

	runErr, out := runRefresh(t, fake.server.URL)
	if runErr == nil {
		t.Fatalf("令牌属于别的账号时应拒绝保存：\n%s", out)
	}
	for _, want := range []string{"someone-else", "不一致", "未保存"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("错误应提到 %q：%v", want, runErr)
		}
	}
	// 站点上没有同名旧令牌被删：文案不该吓唬操作者说凭据库已失效。
	if strings.Contains(runErr.Error(), "已被删除") {
		t.Errorf("没有旧令牌被删时不该声称凭据库已失效：%v", runErr)
	}
}

// TestLoginTokenRefreshReportsMismatchedIdentityAfterDelete 断言删掉同名旧令牌之后
// 才发现身份不符时，错误必须把「本地凭据库已失效、要重新登录」这段后果说全。
//
// replaced>0 意味着站点上那条被本地凭据库指向的旧令牌已经没了。此时只报一句
// 「属于别的账号」会让操作者以为本地凭据照常可用，直到下一次推送/评审才发现
// 被锁在站点外——后果与恢复路径必须跟错误一起出现。
func TestLoginTokenRefreshReportsMismatchedIdentityAfterDelete(t *testing.T) {
	fake := newTokenRefreshServer(t, []int64{7})
	fake.login = "someone-else"
	seedRefreshStore(t, fake.server.URL)

	runErr, out := runRefresh(t, fake.server.URL)
	if runErr == nil {
		t.Fatalf("删旧令牌后身份不符也应报错：\n%s", out)
	}
	for _, want := range []string{"someone-else", "1 条同名旧令牌已被删除", "assistant login add"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("错误应提到 %q：%v", want, runErr)
		}
	}
}

// TestLoginTokenRefreshReportsTokenNameDerivationFailure 断言记录里缺令牌名、而
// 按 (host,user,purpose) 又派生不出名字时当场停下，不向站点发任何请求。
//
// TokenName 在账号为空或用途未登记时报错。这条分支存在的意义是：绝不用空串或凭空
// 拼的名字去站点建令牌——空名的令牌在 Applications 页面无法辨认，事后想撤销都找不到
// 它。这里同时钉住「报错」与「站点零改动」，因为「先建了再报错」正是最坏的落空态。
//
// 构造的是「未登记用途」这一半：purpose 位置参数先经 resolveTokenPurpose 拦截，
// 所以真正的入口是记录里存了已登记用途、而 TokenName 因别的原因失败。用空账号记录
// 无法落盘（凭据校验要求 user 非空），因此走「站点的 host 归一化失败」——这是
// TokenName 内部唯一不依赖参数的失败点。
func TestLoginTokenRefreshReportsTokenNameDerivationFailure(t *testing.T) {
	fake := newTokenRefreshServer(t, nil)
	// 记录里的 host 归一化不出 slug（HostSlug 只接受 http/https 主机名），
	// 但凭据校验只要求非空，所以能落盘。
	storePath := seedRefreshStore(t, fake.server.URL)
	raw := `{"version":` + strconv.Itoa(credentials.CurrentVersion) + `,"credentials":[{"host":"   ","user":"ge","purpose":"mcp","token":"t","scopes":["a"]}]}`
	if err := os.WriteFile(storePath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	command := newLoginTokenRefreshCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader("s3cret\n"))
	command.SetContext(t.Context())
	command.SetArgs([]string{"   ", credentials.PurposeMCP, "--user", "ge", "--password-stdin"})
	err := command.Execute()
	if err == nil {
		t.Fatalf("派生不出令牌名时应报错：\n%s", out.String())
	}
	if len(fake.creates) != 0 || len(fake.deletes) != 0 {
		t.Errorf("派生令牌名失败时不该碰站点：建 %v，删 %v", fake.creates, fake.deletes)
	}
}

// TestLoginTokenRefreshReportsIdentityCheckFailureWithoutReplacedOld 断言站点上
// 没有同名旧令牌（replaced==0）、新令牌建出后身份自检失败时，错误说明令牌已建出但
// 未保存，且不声称凭据库已失效。
//
// replaced==0 意味着本地那条旧令牌还在站点上（从没被删），凭据库照常可用；此时若
// 照搬 replaced>0 的文案，操作者会以为被锁在站点外而去做一次不必要的重新登录。
// 两条文案的唯一差别就是操作者判断「本地还能不能用」的依据。
func TestLoginTokenRefreshReportsIdentityCheckFailureWithoutReplacedOld(t *testing.T) {
	fake := newTokenRefreshServer(t, nil)
	fake.identityStatus = http.StatusUnauthorized
	storePath := seedRefreshStore(t, fake.server.URL)

	runErr, out := runRefresh(t, fake.server.URL)
	if runErr == nil {
		t.Fatalf("自检失败应报错：\n%s", out)
	}
	for _, want := range []string{"身份校验失败", "未保存", "Applications 页面"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("错误应提到 %q：%v", want, runErr)
		}
	}
	if strings.Contains(runErr.Error(), "已被删除") {
		t.Errorf("没有旧令牌被删时不该声称凭据库已失效：%v", runErr)
	}
	// 本地凭据必须原样保留旧令牌：自检失败不能顺手动它。
	stored, err := credentials.Load(storePath)
	if err != nil {
		t.Fatal(err)
	}
	credential, ok := stored.CredentialForUser(fake.server.URL, "ge", credentials.PurposeMCP)
	if !ok || credential.Token != "old-mcp-token" {
		t.Errorf("自检失败不该改写凭据库，got %+v", credential)
	}
}

// TestLoginTokenRefreshReportsIdentityCheckFailureAfterDelete 断言删过同名旧令牌
// 之后自检失败时，错误把「本地凭据库仍指向失效令牌、要重新登录」讲全。
//
// 站点上那条被本地指向的旧令牌已经删掉了，凭据库现在指向一份谁也认不出的令牌。
// 只说「身份校验失败」会让操作者以为重跑一次就好，而重跑只会再删一次、再失败一次。
// 后果与两条恢复路径（重新登录 / Applications 页面检查）必须一起出现。
func TestLoginTokenRefreshReportsIdentityCheckFailureAfterDelete(t *testing.T) {
	fake := newTokenRefreshServer(t, []int64{7})
	fake.identityStatus = http.StatusUnauthorized
	seedRefreshStore(t, fake.server.URL)

	runErr, out := runRefresh(t, fake.server.URL)
	if runErr == nil {
		t.Fatalf("自检失败应报错：\n%s", out)
	}
	for _, want := range []string{"1 条同名旧令牌已被删除", "本地凭据库仍指向失效令牌", "assistant login add"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("错误应提到 %q：%v", want, runErr)
		}
	}
}

// TestLoginTokenRefreshReportsCreateFailureAfterDelete 断言同名旧令牌已删、新建失败时
// 错误点明「站点上旧令牌没了、本地仍指向它」，并给出重新登录的恢复路径。
//
// 这是轮换唯一会把操作者锁在站点外的路径：删除成功、创建失败，站点上一条可用令牌
// 都不剩。只报 create 的底层错误（如 500）而不说后果，操作者会以为本地凭据照常
// 可用，直到下一次推送或评审以 401 失败才发现。
func TestLoginTokenRefreshReportsCreateFailureAfterDelete(t *testing.T) {
	fake := newTokenRefreshServer(t, []int64{7, 8})
	fake.createStatus = http.StatusInternalServerError
	seedRefreshStore(t, fake.server.URL)

	runErr, out := runRefresh(t, fake.server.URL)
	if runErr == nil {
		t.Fatalf("重建失败应报错：\n%s", out)
	}
	for _, want := range []string{"2 条同名旧令牌已被删除", "本地凭据库仍指向失效令牌", "assistant login add"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("错误应提到 %q：%v", want, runErr)
		}
	}
	if len(fake.deletes) != 2 {
		t.Errorf("应删掉两条同名旧令牌，实际 %v", fake.deletes)
	}
}
