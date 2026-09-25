package setup

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
)

type fakeAdmin struct {
	adminLogin    string
	users         map[string]bool
	tokens        map[string]string
	passwords     map[string]string
	tokenSeq      int
	repos         map[string]RepoInfo
	createdRepos  []string
	collaborators map[string]string
	protections   map[string]ProtectionOptions
	labels        map[string]bool
	secrets       map[string]string
}

func newFakeAdmin(repos ...string) *fakeAdmin {
	fake := &fakeAdmin{
		adminLogin:    "admin",
		users:         map[string]bool{"admin": true},
		tokens:        map[string]string{},
		passwords:     map[string]string{},
		repos:         map[string]RepoInfo{},
		collaborators: map[string]string{},
		protections:   map[string]ProtectionOptions{},
		labels:        map[string]bool{},
		secrets:       map[string]string{},
	}
	for _, repo := range repos {
		fake.repos[repo] = RepoInfo{DefaultBranch: "main"}
	}
	return fake
}

func (f *fakeAdmin) AuthenticatedUser(context.Context) (string, bool, error) {
	return f.adminLogin, true, nil
}

func (f *fakeAdmin) UserExists(_ context.Context, name string) (bool, error) {
	return f.users[name], nil
}

func (f *fakeAdmin) CreateUser(_ context.Context, name, _ string) error {
	f.users[name] = true
	return nil
}

// tokenKey 模拟 Gitea 的「账号 + 令牌名」唯一键。
func tokenKey(name, tokenName string) string { return name + "\x00" + tokenName }

func (f *fakeAdmin) EnsurePassword(_ context.Context, name string) (string, error) {
	if f.passwords == nil {
		f.passwords = map[string]string{}
	}
	if f.passwords[name] == "" {
		f.passwords[name] = "generated-password"
	}
	return f.passwords[name], nil
}

// ConvergeToken 模拟 review/merge 共用语义：只替换名为 tokenName 的本工具令牌，
// 其他命名的令牌一律不动。
func (f *fakeAdmin) ConvergeToken(_ context.Context, name, _ /*password*/, tokenName, keepToken string) (string, bool, error) {
	if keepToken != "" {
		for key, value := range f.tokens {
			if strings.HasPrefix(key, name+"\x00") && value == keepToken {
				return keepToken, false, nil
			}
		}
	}
	for key := range f.tokens {
		if key == tokenKey(name, tokenName) {
			delete(f.tokens, key)
		}
	}
	f.tokenSeq++
	token := fmt.Sprintf("token-%s-%d", name, f.tokenSeq)
	f.tokens[tokenKey(name, tokenName)] = token
	return token, true, nil
}

func (f *fakeAdmin) ValidateToken(_ context.Context, name, token string) (bool, error) {
	for key, value := range f.tokens {
		if strings.HasPrefix(key, name+"\x00") && value == token {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeAdmin) GetRepo(_ context.Context, fullName string) (RepoInfo, bool, error) {
	info, ok := f.repos[fullName]
	return info, ok, nil
}

func (f *fakeAdmin) CreateRepo(_ context.Context, fullName string) error {
	f.repos[fullName] = RepoInfo{DefaultBranch: "main"}
	f.createdRepos = append(f.createdRepos, fullName)
	return nil
}

func (f *fakeAdmin) AddCollaborator(_ context.Context, fullName, user, permission string) error {
	f.collaborators[fullName+"/"+user] = permission
	return nil
}

func (f *fakeAdmin) EnsureBranchProtection(_ context.Context, fullName string, options ProtectionOptions) error {
	f.protections[fullName] = options
	return nil
}

func (f *fakeAdmin) ReconcileLabels(_ context.Context, fullName, _ string) error {
	f.labels[fullName] = true
	return nil
}

func (f *fakeAdmin) SetRepoSecret(_ context.Context, fullName, name, value string) error {
	f.secrets[fullName+"/"+name] = value
	return nil
}

func (f *fakeAdmin) ListCollaborators(_ context.Context, fullName string) ([]Collaborator, error) {
	var result []Collaborator
	for key, permission := range f.collaborators {
		index := strings.LastIndex(key, "/")
		if index > 0 && key[:index] == fullName {
			result = append(result, Collaborator{Name: key[index+1:], Permission: permission})
		}
	}
	return result, nil
}

func (f *fakeAdmin) ListAllRepos(context.Context) ([]string, error) {
	names := make([]string, 0, len(f.repos))
	for name := range f.repos {
		names = append(names, name)
	}
	return names, nil
}

func TestSyncMergeSecretsWritesOnlyMergeAdminRepos(t *testing.T) {
	admin := newFakeAdmin("acme/one", "acme/two", "acme/three")
	admin.AddCollaborator(context.Background(), "acme/one", "merge", "admin")
	admin.AddCollaborator(context.Background(), "acme/two", "merge", "write")
	// acme/three 没有 merge 协作者

	if err := SyncMergeSecrets(context.Background(), admin, "merge", "merger-token", false, nil); err != nil {
		t.Fatalf("SyncMergeSecrets() error = %v", err)
	}
	if got := admin.secrets["acme/one/"+ActionsSecretMergeToken]; got != "merger-token" {
		t.Errorf("acme/one secret = %q, want merger-token", got)
	}
	if len(admin.secrets) != 1 {
		t.Errorf("secrets = %+v, want only acme/one", admin.secrets)
	}
}

// merge 令牌来自凭据库（setup 的 Result.Credentials）：缺失/空令牌必须被拒绝。
func TestSyncMergeSecretsRequiresMergeToken(t *testing.T) {
	admin := newFakeAdmin("acme/one")
	admin.AddCollaborator(context.Background(), "acme/one", "merge", "admin")
	if err := SyncMergeSecrets(context.Background(), admin, "merge", "", false, nil); err == nil {
		t.Error("SyncMergeSecrets() error = nil, want missing merge token error")
	}
	if len(admin.secrets) != 0 {
		t.Errorf("missing token must not write secrets: %+v", admin.secrets)
	}
}

func testOptions() Options {
	return Options{
		Host:        "https://gitea.example.com",
		AdminToken:  "admin-token",
		Repos:       []string{"acme/repo"},
		CreateRepos: false,
	}
}

// credentialFor 在 Result.Credentials 里按 (user, purpose) 取凭据。
func credentialFor(result Result, user, purpose string) (credentials.Credential, bool) {
	for _, credential := range result.Credentials {
		if credential.User == user && credential.Purpose == purpose {
			return credential, true
		}
	}
	return credentials.Credential{}, false
}

// assertBotCredential 断言 Result.Credentials 恰好有一条该机器人凭据并核对元数据。
func assertBotCredential(t *testing.T, result Result, user, purpose, tokenPrefix string) {
	t.Helper()
	matches := 0
	for _, item := range result.Credentials {
		if item.User == user && item.Purpose == purpose {
			matches++
		}
	}
	credential, ok := credentialFor(result, user, purpose)
	if !ok || matches != 1 {
		t.Fatalf("credentials for %s/%s = %+v, want exactly one", user, purpose, result.Credentials)
	}
	if credential.Host != "https://gitea.example.com" {
		t.Errorf("credential.Host = %q", credential.Host)
	}
	if credential.TokenName != ReviewerTokenName || credential.Source != credentials.SourceSetup {
		t.Errorf("credential = %+v", credential)
	}
	if !strings.HasPrefix(credential.Token, tokenPrefix) {
		t.Errorf("credential.Token = %q, want prefix %q", credential.Token, tokenPrefix)
	}
	if credential.LastEight != credentials.LastEight(credential.Token) {
		t.Errorf("credential.LastEight = %q", credential.LastEight)
	}
	if len(credential.Scopes) != len(credentials.BotScopes()) {
		t.Errorf("credential.Scopes = %v", credential.Scopes)
	}
}

func TestRunInitializesInstance(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	result, err := Run(context.Background(), testOptions(), admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	instance := result.Instance
	if instance.Host != "https://gitea.example.com" {
		t.Errorf("Host = %q", instance.Host)
	}
	if instance.Reviewer.Name != instances.DefaultReviewerName {
		t.Errorf("Reviewer = %+v", instance.Reviewer)
	}
	if instance.Merger.Name != instances.DefaultMergerName {
		t.Errorf("Merger = %+v", instance.Merger)
	}
	if len(instance.Repos) != 1 || instance.Repos[0].Name != "acme/repo" {
		t.Errorf("Repos = %+v", instance.Repos)
	}
	if !admin.users[instances.DefaultReviewerName] || !admin.users[instances.DefaultMergerName] {
		t.Errorf("bot users not created: %v", admin.users)
	}
	wantCollaborators := map[string]string{
		"acme/repo/" + instances.DefaultReviewerName: "write",
		"acme/repo/" + instances.DefaultMergerName:   "admin",
	}
	for key, want := range wantCollaborators {
		if got := admin.collaborators[key]; got != want {
			t.Errorf("collaborator %s = %q, want %q", key, got, want)
		}
	}
	protection := admin.protections["acme/repo"]
	if protection.Branch != "main" || protection.RequiredApprovals != 2 ||
		protection.MergerName != instances.DefaultMergerName || protection.AllowAdminOverride {
		t.Errorf("protection = %+v", protection)
	}
	if !admin.labels["acme/repo"] {
		t.Error("labels were not reconciled")
	}
	// 机器人令牌只回 Result.Credentials，绝不进 instance。
	if len(result.Credentials) != 2 {
		t.Fatalf("Credentials = %+v, want exactly review + merge", result.Credentials)
	}
	assertBotCredential(t, result, instances.DefaultReviewerName, credentials.PurposeReview,
		"token-"+instances.DefaultReviewerName+"-")
	assertBotCredential(t, result, instances.DefaultMergerName, credentials.PurposeMerge,
		"token-"+instances.DefaultMergerName+"-")
}

func TestRunReusesValidToken(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	admin.users[instances.DefaultReviewerName] = true
	admin.tokens[tokenKey(instances.DefaultReviewerName, ReviewerTokenName)] = "existing-token"
	options := testOptions()
	options.Existing = &instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: instances.DefaultReviewerName},
	}
	options.ExistingCredentials = []credentials.Credential{{
		Host: "https://gitea.example.com", User: instances.DefaultReviewerName,
		Purpose: credentials.PurposeReview, Token: "existing-token",
	}}
	result, err := Run(context.Background(), options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	credential, ok := credentialFor(result, instances.DefaultReviewerName, credentials.PurposeReview)
	if !ok || credential.Token != "existing-token" {
		t.Errorf("review credential = %+v ok=%v, want reused token", credential, ok)
	}
}

func TestRunDryRunMakesNoMutations(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	options := testOptions()
	options.DryRun = true
	result, err := Run(context.Background(), options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if admin.users[instances.DefaultReviewerName] || admin.users[instances.DefaultMergerName] {
		t.Errorf("dry-run created users: %v", admin.users)
	}
	if len(admin.collaborators) != 0 || len(admin.protections) != 0 || len(admin.labels) != 0 {
		t.Errorf("dry-run mutated: %+v %+v %+v", admin.collaborators, admin.protections, admin.labels)
	}
	if len(result.Credentials) != 0 {
		t.Errorf("dry-run should not invent credentials: %+v", result.Credentials)
	}
	for _, repo := range result.Instance.Repos {
		if repo.Name == "" {
			t.Errorf("dry-run lost repo config: %+v", result.Instance.Repos)
		}
	}
}

func TestRunMissingRepoNeedsCreateRepos(t *testing.T) {
	admin := newFakeAdmin()
	options := testOptions()
	options.Repos = []string{"acme/missing"}
	if _, err := Run(context.Background(), options, admin); err == nil || !strings.Contains(err.Error(), "--create-repos") {
		t.Errorf("error = %v, want --create-repos hint", err)
	}
	options.CreateRepos = true
	if _, err := Run(context.Background(), options, admin); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(admin.createdRepos) != 1 || admin.createdRepos[0] != "acme/missing" {
		t.Errorf("createdRepos = %v", admin.createdRepos)
	}
	if !admin.labels["acme/missing"] {
		t.Error("labels were not reconciled for created repo")
	}
}

func TestRunWithoutReposInitializesInstanceOnly(t *testing.T) {
	admin := newFakeAdmin()
	options := testOptions()
	options.Repos = nil
	result, err := Run(context.Background(), options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(result.Instance.Repos) != 0 {
		t.Errorf("Repos = %+v, want empty", result.Instance.Repos)
	}
	for user, purpose := range map[string]string{
		instances.DefaultReviewerName: credentials.PurposeReview,
		instances.DefaultMergerName:   credentials.PurposeMerge,
	} {
		if _, ok := credentialFor(result, user, purpose); !ok {
			t.Errorf("缺少 %s/%s 凭据：%+v", user, purpose, result.Credentials)
		}
	}
	if len(admin.collaborators) != 0 || len(admin.protections) != 0 || len(admin.labels) != 0 {
		t.Errorf("repo-level mutations with no repos: %+v %+v %+v",
			admin.collaborators, admin.protections, admin.labels)
	}
}

func TestRunValidatesOptions(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	tests := map[string]func(*Options){
		"no host":              func(o *Options) { o.Host = "" },
		"no admin credentials": func(o *Options) { o.AdminToken = "" },
		"bad repo":             func(o *Options) { o.Repos = []string{"nope"} },
		"same account":         func(o *Options) { o.ReviewerName, o.MergerName = "bot", "bot" },
		"bad account name":     func(o *Options) { o.ReviewerName = "bad name" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			options := testOptions()
			mutate(&options)
			if _, err := Run(context.Background(), options, admin); err == nil {
				t.Errorf("Run() error = nil, want validation error")
			}
		})
	}
}

func TestRunOverridesExistingReposAndKeepsDirs(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	options := testOptions()
	options.Existing = &instances.Instance{
		Host: "https://gitea.example.com",
		Repos: []instances.Repo{
			{Name: "acme/repo", Dir: "/srv/repo"},
			{Name: "acme/stale"},
		},
	}
	result, err := Run(context.Background(), options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(result.Instance.Repos) != 1 || result.Instance.Repos[0].Dir != "/srv/repo" {
		t.Errorf("Repos = %+v, want preserved dir for acme/repo", result.Instance.Repos)
	}
}

func TestDeriveEmailDomain(t *testing.T) {
	tests := map[string]string{
		"https://gitea.example.com":  "gitea.example.com",
		"http://gitea.internal:3000": "gitea.internal",
		"http://192.168.1.10:3000":   "assistant.local",
		"http://localhost:3000":      "assistant.local",
	}
	for host, want := range tests {
		if got := DeriveEmailDomain(host); got != want {
			t.Errorf("DeriveEmailDomain(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestRandomPassword(t *testing.T) {
	first, err := RandomPassword()
	if err != nil {
		t.Fatal(err)
	}
	second, err := RandomPassword()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 24 || first == second {
		t.Errorf("passwords look weak: %q %q", first, second)
	}
}
