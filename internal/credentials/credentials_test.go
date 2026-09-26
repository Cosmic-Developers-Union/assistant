package credentials

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "credentials.json")
	file := &File{}
	file.SetIdentity(Identity{Host: "https://Gitea.Example.com/", User: "alice", IsAdmin: true})
	file.SetCredential(Credential{
		Host: "https://Gitea.Example.com/", User: "alice", Purpose: PurposeMCP,
		Token: "mcp-token", TokenName: "assistant-mcp-gitea.example.com-alice",
		LastEight: LastEight("mcp-token"), Scopes: MCPScopes(), Source: "password",
	})
	if err := Save(path, file); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("凭据文件权限 = %v, want 0600", info.Mode().Perm())
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := loaded.IdentityFor("https://gitea.example.com")
	if !ok || identity.User != "alice" || !identity.IsAdmin || identity.VerifiedAt == "" {
		t.Fatalf("identity = %+v ok=%v", identity, ok)
	}
	credential, ok := loaded.CredentialForUser("https://gitea.example.com/", "alice", PurposeMCP)
	if !ok || credential.Token != "mcp-token" || credential.TokenName != "assistant-mcp-gitea.example.com-alice" {
		t.Fatalf("credential = %+v ok=%v", credential, ok)
	}
	if got, want := credential.LastEight, "cp-token"; got != want {
		t.Fatalf("LastEight = %q, want %q", got, want)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	file, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !file.Empty() || file.Version != CurrentVersion {
		t.Fatalf("file = %+v, want empty v%d", file, CurrentVersion)
	}
	// 空库不写文件
	if err := Save(filepath.Join(t.TempDir(), "empty.json"), file); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("损坏的凭据文件必须报错，不能静默当作空库")
	}
}

// 索引唯一性：(host,user,purpose) 唯一；用途与账号互不覆盖。
func TestCredentialIndexing(t *testing.T) {
	file := &File{}
	file.SetCredential(Credential{Host: "https://a.example.com", User: "alice", Purpose: PurposeMCP, Token: "a1"})
	file.SetCredential(Credential{Host: "https://a.example.com", User: "alice", Purpose: PurposeMCP, Token: "a2"})
	file.SetCredential(Credential{Host: "https://a.example.com", User: "alice", Purpose: PurposeAdmin, Token: "admin"})
	file.SetCredential(Credential{Host: "https://a.example.com", User: "bob", Purpose: PurposeMCP, Token: "b1"})
	file.SetCredential(Credential{Host: "https://b.example.com", User: "alice", Purpose: PurposeMCP, Token: "other-host"})
	if len(file.Credentials) != 4 {
		t.Fatalf("credentials = %d, want 4（同键替换）", len(file.Credentials))
	}
	if credential, _ := file.CredentialForUser("https://a.example.com", "alice", PurposeMCP); credential.Token != "a2" {
		t.Fatalf("同键应替换为最新值，got %+v", credential)
	}
	// 同用途多账号：不猜身份，要求指定
	if _, _, err := file.CredentialFor("https://a.example.com", PurposeMCP); err == nil ||
		!strings.Contains(err.Error(), "alice") || !strings.Contains(err.Error(), "bob") {
		t.Fatalf("多账号应报错并列出候选，got %v", err)
	}
	// 唯一匹配按 host 规范化比较（大小写/尾斜杠）
	if credential, ok, err := file.CredentialFor("https://B.Example.com/", PurposeMCP); err != nil || !ok ||
		credential.Token != "other-host" {
		t.Fatalf("规范化匹配失败：%+v ok=%v err=%v", credential, ok, err)
	}
	if _, ok, err := file.CredentialFor("https://unknown.example.com", PurposeMCP); err != nil || ok {
		t.Fatalf("未知站点应为空，got ok=%v err=%v", ok, err)
	}
}

func TestIdentityIsPerHost(t *testing.T) {
	file := &File{}
	file.SetIdentity(Identity{Host: "https://a.example.com", User: "alice", IsAdmin: true})
	file.SetIdentity(Identity{Host: "https://a.example.com/", User: "bob", IsAdmin: false})
	file.SetIdentity(Identity{Host: "https://b.example.com", User: "alice", IsAdmin: true})
	if len(file.Identity) != 2 {
		t.Fatalf("identity = %+v, want 每站点一条", file.Identity)
	}
	identity, ok := file.IdentityFor("https://a.example.com")
	if !ok || identity.User != "bob" || identity.IsAdmin {
		t.Fatalf("换账号应替换站点身份：%+v", identity)
	}
	if users := file.Users("https://a.example.com"); len(users) != 1 || users[0] != "bob" {
		t.Fatalf("users = %v, want [bob]", users)
	}
}

func TestRemoveUser(t *testing.T) {
	file := &File{}
	file.SetIdentity(Identity{Host: "https://a.example.com", User: "alice"})
	file.SetCredential(Credential{Host: "https://a.example.com", User: "alice", Purpose: PurposeMCP, Token: "t1"})
	file.SetCredential(Credential{Host: "https://a.example.com", User: "alice", Purpose: PurposeAdmin, Token: "t2"})
	file.SetCredential(Credential{Host: "https://a.example.com", User: "bob", Purpose: PurposeMCP, Token: "t3"})
	if removed := file.RemoveUser("https://a.example.com/", "alice"); removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if _, ok := file.IdentityFor("https://a.example.com"); ok {
		t.Fatal("identity 应一并移除")
	}
	if _, ok := file.CredentialForUser("https://a.example.com", "bob", PurposeMCP); !ok {
		t.Fatal("不应影响其他账号")
	}
}

func TestValidateRejectsIncompleteEntries(t *testing.T) {
	cases := []struct {
		name string
		file *File
		want string
	}{
		{"unknown purpose", &File{Credentials: []Credential{{Host: "h", User: "u", Purpose: "nope", Token: "t"}}}, "未知 purpose"},
		{"missing token", &File{Credentials: []Credential{{Host: "h", User: "u", Purpose: PurposeMCP}}}, "缺少令牌"},
		{"missing user", &File{Credentials: []Credential{{Host: "h", Purpose: PurposeMCP, Token: "t"}}}, "缺少 host 或 user"},
		{"duplicate", &File{Credentials: []Credential{
			{Host: "h", User: "u", Purpose: PurposeMCP, Token: "a"},
			{Host: "h", User: "u", Purpose: PurposeMCP, Token: "b"},
		}}, "重复"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.file.Validate(); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("err = %v, want 包含 %q", err, testCase.want)
			}
		})
	}
}

func TestTokenNameIsDerivedAndStable(t *testing.T) {
	name, err := TokenName("https://gitea.example.com/", "alice", PurposeMCP)
	if err != nil {
		t.Fatal(err)
	}
	if name != "assistant-mcp-gitea.example.com-alice" {
		t.Fatalf("TokenName = %q", name)
	}
	same, err := TokenName("gitea.example.com", "alice", PurposeMCP)
	if err != nil || same != name {
		t.Fatalf("host 规范化后应同名：%q vs %q err=%v", same, name, err)
	}
	other, err := TokenName("https://gitea.example.com", "bob", PurposeMCP)
	if err != nil || other == name {
		t.Fatalf("不同账号不应同名：%q", other)
	}
	withPort, err := TokenName("http://127.0.0.1:3000", "alice", PurposeMerge)
	if err != nil || !strings.HasPrefix(withPort, "assistant-merge-127.0.0.1-3000-") {
		t.Fatalf("带端口 TokenName = %q err=%v", withPort, err)
	}
	if _, err := TokenName("https://gitea.example.com", "", PurposeMCP); err == nil {
		t.Fatal("缺账号应报错")
	}
	if _, err := TokenName("https://gitea.example.com", "alice", "nope"); err == nil {
		t.Fatal("未知用途应报错")
	}
}

// 凭据落点是平台标准配置目录（不是 config.json 同目录、不是 cwd）：从任意
// 目录启动的 MCP/CLI 都解析到同一份。ASSISTANT_CREDENTIALS 显式覆盖。
func TestPathFollowsPlatformConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "Cosmic-Developers-Union", "assistant", "credentials.json")
	if path != want {
		t.Fatalf("Path = %q, want %q", path, want)
	}
	// 与 config.json 位置无关：换任何配置路径都不影响凭据落点
	t.Setenv("ASSISTANT_CREDENTIALS", "/tmp/custom-credentials.json")
	if path, err := Path(); err != nil || path != "/tmp/custom-credentials.json" {
		t.Fatalf("ASSISTANT_CREDENTIALS 覆盖失败：%q err=%v", path, err)
	}
}

func TestScopesAreMinimal(t *testing.T) {
	for _, scope := range MCPScopes() {
		if strings.Contains(scope, "organization") || strings.Contains(scope, "package") {
			t.Fatalf("MCP 令牌不应带过宽 scope：%v", MCPScopes())
		}
	}
}

// 同用途多账号时，站点当前身份是有依据的裁决者；没有身份记录才要求指定账号。
func TestCredentialForIdentity(t *testing.T) {
	file := &File{}
	file.SetCredential(Credential{Host: "https://a.example.com", User: "alice", Purpose: PurposeMCP, Token: "a"})
	file.SetCredential(Credential{Host: "https://a.example.com", User: "bob", Purpose: PurposeMCP, Token: "b"})
	if _, _, err := file.CredentialForIdentity("https://a.example.com", PurposeMCP); err == nil {
		t.Fatal("无身份记录且多账号时应要求指定账号")
	}
	file.SetIdentity(Identity{Host: "https://a.example.com", User: "bob"})
	credential, ok, err := file.CredentialForIdentity("https://a.example.com", PurposeMCP)
	if err != nil || !ok || credential.Token != "b" {
		t.Fatalf("应按当前身份取令牌：%+v ok=%v err=%v", credential, ok, err)
	}
	// 身份存在但该用途没有令牌时退回唯一匹配
	file.SetCredential(Credential{Host: "https://b.example.com", User: "carol", Purpose: PurposeMCP, Token: "c"})
	file.SetIdentity(Identity{Host: "https://b.example.com", User: "dave"})
	if credential, ok, err := file.CredentialForIdentity("https://b.example.com", PurposeMCP); err != nil || !ok ||
		credential.Token != "c" {
		t.Fatalf("身份无令牌时应退回唯一匹配：%+v ok=%v err=%v", credential, ok, err)
	}
}

// 保存凭据会就地排序 Scopes：不能因此改掉全局定义（共享 backing array 的话，
// 一次保存就会污染所有后续登录的 scope 顺序）。
func TestSaveDoesNotMutateScopeDefinition(t *testing.T) {
	want := []string{"read:repository", "write:repository", "read:issue", "write:issue", "read:user"}
	file := &File{}
	file.SetCredential(Credential{
		Host: "https://a.example.com", User: "alice", Purpose: PurposeMCP,
		Token: "t", Scopes: MCPScopes(),
	})
	if err := Save(filepath.Join(t.TempDir(), "credentials.json"), file); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(MCPScopes(), want) {
		t.Fatalf("MCPScopes() = %v, want %v（全局定义被就地修改）", MCPScopes(), want)
	}
}

// 权限集是发给 Gitea 的契约：管理令牌要能建账号/配分支保护，机器人令牌要能
// 合并与评审，两者都不能少 scope，也不能互相借用。
func TestScopeSetsAndDefaults(t *testing.T) {
	mcp := MCPScopes()
	bot := BotScopes()
	if !slices.Equal(mcp, bot) {
		t.Fatalf("MCP 与机器人令牌权限集应一致：%v vs %v", mcp, bot)
	}
	if !slices.Contains(AdminScopes(), "write:admin") {
		t.Fatalf("管理令牌必须带 write:admin：%v", AdminScopes())
	}
	if slices.Contains(mcp, "write:admin") {
		t.Fatalf("机器人令牌不应带 write:admin：%v", mcp)
	}
	for _, purpose := range []string{PurposeMCP, PurposeAdmin} {
		got, err := DefaultScopes(purpose)
		if err != nil {
			t.Fatal(err)
		}
		want, err := DefaultScopes(purpose)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("DefaultScopes(%q) 不稳定：%v", purpose, got)
		}
	}
	for _, purpose := range []string{PurposeReview, PurposeMerge} {
		scopes, err := DefaultScopes(purpose)
		if err != nil || !slices.Equal(scopes, bot) {
			t.Fatalf("DefaultScopes(%q) = %v err=%v, want 机器人权限集", purpose, scopes, err)
		}
	}
	// 未知用途绝不兜底猜一个权限集：用错 scope 的令牌是安全隐患。
	_, err := DefaultScopes("nope")
	if err == nil || !strings.Contains(err.Error(), "未知用途") || !strings.Contains(err.Error(), PurposeMerge) {
		t.Fatalf("未知用途必须显式报错并列出支持项：%v", err)
	}
	// 每次返回新切片：调用方排序/序列化不能污染全局定义。
	first := MCPScopes()
	slices.Sort(first)
	if !slices.Equal(MCPScopes(), bot) {
		t.Fatalf("MCPScopes() 返回了共享切片：%v", MCPScopes())
	}
}

// Hosts 是「这台机器登录过哪些站点」的事实来源：身份与用途令牌的并集，
// 规范化后排序去重；空库返回 nil（不是空切片）。
func TestHostsIsUnionOfIdentityAndCredentials(t *testing.T) {
	var empty *File
	if hosts := empty.Hosts(); hosts != nil {
		t.Fatalf("空库应为 nil，got %v", hosts)
	}
	file := &File{}
	file.SetIdentity(Identity{Host: "https://b.example.com", User: "alice"})
	file.SetIdentity(Identity{Host: "https://b.example.com/", User: "alice"})
	file.SetCredential(Credential{Host: "https://a.example.com/", User: "bob", Purpose: PurposeMCP, Token: "t"})
	file.SetCredential(Credential{Host: "https://B.Example.com", User: "bob", Purpose: PurposeMCP, Token: "t"})
	want := []string{"https://a.example.com", "https://b.example.com"}
	if hosts := file.Hosts(); !slices.Equal(hosts, want) {
		t.Fatalf("Hosts = %v, want %v（并集 + 规范化 + 去重）", hosts, want)
	}
}

// Users 汇合身份与令牌两侧的账号，并去重：站点上「有哪些账号」不能只靠一侧。
func TestUsersAreSortedAndDeduped(t *testing.T) {
	file := &File{}
	file.SetIdentity(Identity{Host: "https://a.example.com", User: "carol"})
	file.SetCredential(Credential{Host: "https://a.example.com", User: "alice", Purpose: PurposeMCP, Token: "t"})
	file.SetCredential(Credential{Host: "https://a.example.com", User: "carol", Purpose: PurposeReview, Token: "t"})
	file.SetCredential(Credential{Host: "https://b.example.com", User: "dave", Purpose: PurposeMCP, Token: "t"})
	want := []string{"alice", "carol"}
	if users := file.Users("https://A.Example.com/"); !slices.Equal(users, want) {
		t.Fatalf("Users = %v, want %v", users, want)
	}
	var empty *File
	if users := empty.Users("https://a.example.com"); users != nil {
		t.Fatalf("空库应为 nil，got %v", users)
	}
}

// Normalize 是文件级的收敛：排序 + 同键后者胜（重登复用而不是堆积），
// 并补齐版本。顺序稳定意味着 diff 不抖动。
func TestNormalizeSortsAndDedupes(t *testing.T) {
	file := &File{}
	file.Normalize()
	if file.Version != CurrentVersion {
		t.Fatalf("Version = %d, want %d", file.Version, CurrentVersion)
	}
	// 同一 host 的重复条目写在相邻位置：Normalize 稳定排序后相邻者折叠，后者胜。
	file.Identity = []Identity{
		{Host: "https://b.example.com", User: "bob"},
		{Host: "https://a.example.com", User: "old"},
		{Host: "https://A.Example.com/", User: "new"},
	}
	file.Credentials = []Credential{
		{Host: "https://b.example.com", User: "bob", Purpose: PurposeMerge, Token: "old"},
		{Host: "https://b.example.com", User: "bob", Purpose: PurposeMerge, Token: "new", Scopes: []string{"write:issue", "read:issue"}},
		{Host: "https://a.example.com", User: "alice", Purpose: PurposeMCP, Token: "keep"},
	}
	file.Normalize()
	if len(file.Identity) != 2 || file.Identity[0].User != "new" || file.Identity[1].User != "bob" {
		t.Fatalf("identity = %+v, want 按 host 排序且后者胜", file.Identity)
	}
	if len(file.Credentials) != 2 {
		t.Fatalf("credentials = %d, want 2（同键去重）", len(file.Credentials))
	}
	if file.Credentials[0].Token != "keep" || file.Credentials[1].Token != "new" {
		t.Fatalf("credentials = %+v, want 同键后者胜", file.Credentials)
	}
	if want := []string{"read:issue", "write:issue"}; !slices.Equal(file.Credentials[1].Scopes, want) {
		t.Fatalf("Scopes = %v, want %v（排序）", file.Credentials[1].Scopes, want)
	}
	// 排序结果本身稳定：再跑一次不改变内容
	before := make([]string, 0, len(file.Credentials))
	for _, credential := range file.Credentials {
		before = append(before, credentialKey(credential)+"="+credential.Token)
	}
	file.Normalize()
	after := make([]string, 0, len(file.Credentials))
	for _, credential := range file.Credentials {
		after = append(after, credentialKey(credential)+"="+credential.Token)
	}
	if !slices.Equal(before, after) {
		t.Fatalf("Normalize 不幂等：%v → %v", before, after)
	}
}

// SetCredential 必须复制 Scopes：调用方后续复用/修改自己的切片不能改到库里。
func TestSetCredentialCopiesScopes(t *testing.T) {
	scopes := []string{"write:issue", "read:issue"}
	file := &File{}
	file.SetCredential(Credential{Host: "h", User: "u", Purpose: PurposeMCP, Token: "t", Scopes: scopes})
	scopes[0] = "mutated"
	stored := file.Credentials[0].Scopes
	if slices.Contains(stored, "mutated") {
		t.Fatalf("Scopes 未复制，库被调用方切片改到：%v", stored)
	}
}

// 缺 host/user 的身份记录同样不可放行：否则「当前是谁」无从判定。
func TestValidateRejectsIdentityWithoutHostOrUser(t *testing.T) {
	cases := map[string]*File{
		"missing host": {Identity: []Identity{{User: "alice"}}},
		"missing user": {Identity: []Identity{{Host: "https://a.example.com"}}},
	}
	for name, file := range cases {
		t.Run(name, func(t *testing.T) {
			if err := file.Validate(); err == nil || !strings.Contains(err.Error(), "identity 缺少") {
				t.Fatalf("err = %v, want 包含 identity 缺少", err)
			}
		})
	}
}

func TestKnownPurposeContract(t *testing.T) {
	if want := []string{PurposeMCP, PurposeAdmin, PurposeReview, PurposeMerge}; !slices.Equal(Purposes(), want) {
		t.Fatalf("Purposes = %v, want %v", Purposes(), want)
	}
	for _, purpose := range Purposes() {
		if !KnownPurpose(purpose) {
			t.Fatalf("KnownPurpose(%q) = false", purpose)
		}
	}
	if KnownPurpose("") || KnownPurpose("MCP") || KnownPurpose("nope") {
		t.Fatal("未知用途必须返回 false（大小写敏感，不做模糊匹配）")
	}
}

func TestLastEightBoundaries(t *testing.T) {
	cases := map[string]struct{ token, want string }{
		"长于八位取末八位":    {"abcdefghij", "cdefghij"},
		"恰好八位原样":      {"12345678", "12345678"},
		"短于八位原样":      {"abc", "abc"},
		"空令牌":         {"", ""},
		"带空格的长令牌取末八位": {"  token-12345678  ", "12345678"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := LastEight(testCase.token); got != testCase.want {
				t.Fatalf("LastEight(%q) = %q, want %q", testCase.token, got, testCase.want)
			}
		})
	}
}

// 读取侧的空库分支：nil 库不 panic，缺项不当作命中。
func TestLookupsOnNilAndMissingEntries(t *testing.T) {
	var file *File
	if identity, ok := file.IdentityFor("https://a.example.com"); ok || identity.User != "" {
		t.Fatalf("nil 库 IdentityFor = %+v ok=%v", identity, ok)
	}
	if credential, ok := file.CredentialForUser("https://a.example.com", "alice", PurposeMCP); ok || credential.Token != "" {
		t.Fatalf("nil 库 CredentialForUser = %+v ok=%v", credential, ok)
	}
	if _, ok, err := file.CredentialFor("https://a.example.com", PurposeMCP); ok || err != nil {
		t.Fatalf("nil 库 CredentialFor ok=%v err=%v", ok, err)
	}
	if removed := file.RemoveUser("https://a.example.com", "alice"); removed != 0 {
		t.Fatalf("nil 库 RemoveUser = %d, want 0", removed)
	}

	stored := &File{}
	stored.SetIdentity(Identity{Host: "https://a.example.com", User: "alice"})
	stored.SetCredential(Credential{Host: "https://a.example.com", User: "alice", Purpose: PurposeMCP, Token: "t"})
	if _, ok := stored.IdentityFor("https://b.example.com"); ok {
		t.Fatal("未登录站点不应有身份")
	}
	// host 规范化口径：大小写与尾斜杠不改变命中，不同站点不误命中
	if _, ok := stored.IdentityFor("https://A.Example.com/"); !ok {
		t.Fatal("规范化后的 host 应命中")
	}
	if _, ok := stored.CredentialForUser("https://a.example.com", "alice", PurposeMerge); ok {
		t.Fatal("未登记的用途不应命中")
	}
	if _, ok := stored.CredentialForUser("https://a.example.com", "bob", PurposeMCP); ok {
		t.Fatal("未登记的账号不应命中")
	}
	if removed := stored.RemoveUser("https://a.example.com", "bob"); removed != 0 {
		t.Fatalf("删除不存在账号 = %d, want 0", removed)
	}
	if len(stored.Credentials) != 1 || len(stored.Identity) != 1 {
		t.Fatalf("无关账号的删除不应改动库：%+v", stored)
	}
}

// 损坏的凭据文件必须报错而不是当作空库：静默空库等于把用户静默登出。
func TestLoadReportsUnreadableAndInvalidStore(t *testing.T) {
	directory := t.TempDir()
	unreadable := filepath.Join(directory, "unreadable.json")
	if err := os.WriteFile(unreadable, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(unreadable); err == nil {
		t.Skip("以 root 运行：权限位不生效，本机无法覆盖不可读分支")
	}
	if _, err := os.ReadFile(unreadable); err == nil {
		t.Fatal("不可读文件竟然读到了")
	}
	rejected, err := Load(unreadable)
	if err == nil || rejected != nil {
		t.Fatalf("不可读文件必须报错且不返回库：file=%+v err=%v", rejected, err)
	}
	if err := os.Chmod(unreadable, 0o600); err != nil {
		t.Fatal(err)
	}
	// 目录当文件读：报错并带上路径语境（路径是排障的唯一线索）
	if _, err := Load(directory); err == nil || !strings.Contains(err.Error(), directory) {
		t.Fatalf("err = %v, want 含路径 %q", err, directory)
	}

	// 结构合法但内容非法：Load 不能静默丢弃非法条目
	invalid := filepath.Join(directory, "invalid.json")
	body := `{"version":1,"credentials":[{"host":"https://a.example.com","user":"alice","purpose":"mcp"}]}`
	if err := os.WriteFile(invalid, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(invalid); err == nil || !strings.Contains(err.Error(), "凭据文件") {
		t.Fatalf("err = %v, want 包含 凭据文件（结构合法但缺令牌）", err)
	}
}

// Save 先校验后落盘：非法库不能写出半截文件，不留任何痕迹。
func TestSaveRejectsInvalidStoreWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	file := &File{Credentials: []Credential{{Host: "h", User: "u", Purpose: "nope", Token: "t"}}}
	err := Save(path, file)
	if err == nil || !strings.Contains(err.Error(), "未知 purpose") {
		t.Fatalf("err = %v, want 未知 purpose", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("非法库不应写出文件：stat err = %v", statErr)
	}
}

func TestSaveFailsWhenDirectoryIsAFile(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(directory, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Save(filepath.Join(directory, "credentials.json"), &File{})
	if err == nil {
		t.Fatal("父路径不是目录时必须报错")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("err = %v, want 含冲突路径", err)
	}
}

// 凭据落点绝不能被 cwd 或 config.json 位置带动：换目录启动必须解析到同一份。
func TestPathIsIndependentOfWorkingDirectory(t *testing.T) {
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	before, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	after, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("凭据路径随 cwd 变化：%q → %q", before, after)
	}
	want := filepath.Join(configHome, "Cosmic-Developers-Union", "assistant", "credentials.json")
	if after != want {
		t.Fatalf("Path = %q, want %q", after, want)
	}
	// 空值/纯空白不构成覆盖：仍落平台标准目录
	for _, override := range []string{"", "   ", "\t"} {
		t.Setenv("ASSISTANT_CREDENTIALS", override)
		if path, err := Path(); err != nil || path != want {
			t.Fatalf("ASSISTANT_CREDENTIALS=%q 时 Path = %q err=%v", override, path, err)
		}
	}
	// 显式覆盖原样返回（含相对路径），路径解析不在这里做清洗
	t.Setenv("ASSISTANT_CREDENTIALS", "relative/credentials.json")
	if path, err := Path(); err != nil || path != "relative/credentials.json" {
		t.Fatalf("显式覆盖应原样返回：%q err=%v", path, err)
	}
}

// 取消设置 XDG_CONFIG_HOME 时退回平台默认配置目录：Path 必须照常给出可用落点。
func TestPathFallsBackToUserConfigDir(t *testing.T) {
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("本机没有标准配置目录：%v", err)
	}
	want := filepath.Join(directory, "Cosmic-Developers-Union", "assistant", "credentials.json")
	if path != want {
		t.Fatalf("Path = %q, want %q", path, want)
	}
}

// 落盘失败必须逐条报错、且不留下半截文件：凭据是「静默丢失即静默登出」的数据，
// 写失败宁可响亮地失败。
func TestSaveFailuresLeaveNoPartialFile(t *testing.T) {
	directory := t.TempDir()
	// 只读目录：CreateTemp 失败（以 root 运行时权限位不生效，跳过）
	readOnly := filepath.Join(directory, "readonly")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(readOnly, "credentials.json")
	if err := Save(blocked, &File{}); err == nil {
		t.Skip("以 root 运行：只读目录不生效，本机无法覆盖不可写分支")
	}
	if _, statErr := os.Stat(blocked); !os.IsNotExist(statErr) {
		t.Fatalf("失败路径不应留下半截文件：stat err = %v", statErr)
	}
	if entries, err := os.ReadDir(readOnly); err != nil || len(entries) != 0 {
		t.Fatalf("失败路径不应残留临时文件：%v err=%v", entries, err)
	}
	// 覆盖既有文件：写入必须原子生效，内容完整可读
	path := filepath.Join(directory, "credentials.json")
	first := &File{}
	first.SetCredential(Credential{Host: "https://a.example.com", User: "alice", Purpose: PurposeMCP, Token: "t1"})
	if err := Save(path, first); err != nil {
		t.Fatal(err)
	}
	second := &File{}
	second.SetCredential(Credential{Host: "https://a.example.com", User: "bob", Purpose: PurposeMerge, Token: "t2"})
	if err := Save(path, second); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.CredentialForUser("https://a.example.com", "alice", PurposeMCP); ok {
		t.Fatal("覆盖写入后不应残留旧内容")
	}
	if _, ok := loaded.CredentialForUser("https://a.example.com", "bob", PurposeMerge); !ok {
		t.Fatal("覆盖写入后应读到新内容")
	}
}

// 令牌名基于实例 slug：站点地址无法解析时不能编出一个可能撞名的名字。
func TestTokenNameRejectsUnparsableHost(t *testing.T) {
	if _, err := TokenName("http://[::1", "alice", PurposeMCP); err == nil {
		t.Fatal("无法解析的站点地址必须报错")
	}
}

// 目录不可写时：不静默失败、不留下临时文件，且不影响既有凭据文件。
func TestSaveIntoUnwritableDirectory(t *testing.T) {
	directory := t.TempDir()
	file := &File{}
	file.SetCredential(Credential{Host: "https://a.example.com", User: "alice", Purpose: PurposeMCP, Token: "t"})
	readOnly := filepath.Join(directory, "readonly")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readOnly, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	err := Save(filepath.Join(readOnly, "credentials.json"), file)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want 权限错误", err)
	}
	if entries, err := os.ReadDir(readOnly); err != nil || len(entries) != 0 {
		t.Fatalf("写失败不应残留临时文件：%v err=%v", entries, err)
	}
}

// 连标准配置目录都无法定位时：Path 报错而不是编造一个落点——凭据写错位置的
// 后果是「换了台机器就找不到登录态」。
func TestPathErrorsWithoutConfigDirectory(t *testing.T) {
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	path, err := Path()
	if err == nil || path != "" {
		t.Fatalf("Path = %q err = %v, want 报错", path, err)
	}
}
