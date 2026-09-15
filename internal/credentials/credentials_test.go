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

func TestPathForFollowsConfig(t *testing.T) {
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	path, err := PathFor("/etc/assistant/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/etc/assistant/credentials.json" {
		t.Fatalf("PathFor = %q", path)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", "/tmp/custom-credentials.json")
	if path, err := PathFor("/etc/assistant/config.json"); err != nil || path != "/tmp/custom-credentials.json" {
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
