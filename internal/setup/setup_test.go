package setup

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"assistant/internal/instances"
)

type fakeAdmin struct {
	adminLogin    string
	adminToken    string
	users         map[string]bool
	tokens        map[string]string
	passwords     map[string]string
	tokenSeq      int
	repos         map[string]RepoInfo
	createdRepos  []string
	collaborators map[string]string
	protections   map[string]ProtectionOptions
	labels        map[string]bool
	oauth         *instances.OAuthCredential
	secrets       map[string]string
}

func newFakeAdmin(repos ...string) *fakeAdmin {
	fake := &fakeAdmin{
		adminLogin:    "admin",
		adminToken:    "admin-token",
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

func (f *fakeAdmin) AdminToken() string { return f.adminToken }

func (f *fakeAdmin) PersistentToken() string { return f.adminToken }

func (f *fakeAdmin) AdminOAuth() *instances.OAuthCredential {
	if f.oauth == nil {
		return nil
	}
	copied := *f.oauth
	return &copied
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

// ConvergeToken 模拟 reviewer 语义：账号下只保留一个令牌。
func (f *fakeAdmin) ConvergeToken(_ context.Context, name, _ /*password*/, tokenName, keepToken string) (string, bool, error) {
	if keepToken != "" {
		for key, value := range f.tokens {
			if strings.HasPrefix(key, name+"\x00") && value == keepToken {
				for other := range f.tokens {
					if strings.HasPrefix(other, name+"\x00") && other != key {
						delete(f.tokens, other)
					}
				}
				return keepToken, false, nil
			}
		}
	}
	for key := range f.tokens {
		if strings.HasPrefix(key, name+"\x00") {
			delete(f.tokens, key)
		}
	}
	f.tokenSeq++
	token := fmt.Sprintf("token-%s-%d", name, f.tokenSeq)
	f.tokens[tokenKey(name, tokenName)] = token
	return token, true, nil
}

// EnsureRepoToken 模拟 merger 语义：每个令牌名一个独立令牌，其他仓库的令牌
// 不受影响。
func (f *fakeAdmin) EnsureRepoToken(_ context.Context, name, _ /*password*/, tokenName, keepToken string) (string, bool, error) {
	if keepToken != "" && f.tokens[tokenKey(name, tokenName)] == keepToken {
		return keepToken, false, nil
	}
	delete(f.tokens, tokenKey(name, tokenName))
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

func (f *fakeAdmin) RemoveCollaborator(_ context.Context, fullName, user string) error {
	delete(f.collaborators, fullName+"/"+user)
	return nil
}

func (f *fakeAdmin) DeleteBranchProtection(_ context.Context, fullName, _ string) error {
	delete(f.protections, fullName)
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

func (f *fakeAdmin) DeleteRepoSecret(_ context.Context, fullName, name string) error {
	delete(f.secrets, fullName+"/"+name)
	return nil
}

func TestConfigureActionsWritesExpectedRepoConfig(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	instance := instances.Instance{
		Host:       "https://gitea.example.com",
		AdminToken: "admin-token",
		Merger:     instances.Account{Name: "merge"},
		Repos:      []instances.Repo{{Name: "acme/repo", MergerToken: "merger-token"}},
	}
	if err := ConfigureActions(context.Background(), admin, instance, false, nil); err != nil {
		t.Fatalf("ConfigureActions() error = %v", err)
	}
	if got := admin.secrets["acme/repo/"+ActionsSecretMergeToken]; got != "merger-token" {
		t.Errorf("state secret = %q", got)
	}
	if len(admin.secrets) != 1 {
		t.Errorf("secrets = %+v, want only %s", admin.secrets, ActionsSecretMergeToken)
	}
}

func TestConfigureActionsRequiresRepoToken(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	instance := instances.Instance{
		Host:   "https://gitea.example.com",
		Merger: instances.Account{Name: "merge"},
		Repos:  []instances.Repo{{Name: "acme/repo"}},
	}
	if err := ConfigureActions(context.Background(), admin, instance, false, nil); err == nil {
		t.Error("ConfigureActions() error = nil, want missing merger token error")
	}
}

func TestConfigureActionsDryRunMakesNoWrites(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	instance := instances.Instance{
		Host:   "https://gitea.example.com",
		Merger: instances.Account{Name: "merge"},
		Repos:  []instances.Repo{{Name: "acme/repo"}},
	}
	if err := ConfigureActions(context.Background(), admin, instance, true, nil); err != nil {
		t.Fatalf("ConfigureActions() error = %v", err)
	}
	if len(admin.secrets) != 0 {
		t.Errorf("dry-run mutated: %+v", admin.secrets)
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

func TestRunInitializesInstance(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	instance, err := Run(context.Background(), testOptions(), admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if instance.Host != "https://gitea.example.com" {
		t.Errorf("Host = %q", instance.Host)
	}
	if instance.Reviewer.Name != instances.DefaultReviewerName || instance.Reviewer.Token == "" {
		t.Errorf("Reviewer = %+v", instance.Reviewer)
	}
	if instance.Merger.Name != instances.DefaultMergerName || instance.Merger.Token != "" {
		t.Errorf("Merger = %+v, want account without instance-level token", instance.Merger)
	}
	if len(instance.Repos) != 1 || instance.Repos[0].MergerToken == "" {
		t.Errorf("Repos = %+v, want per-repo merger token", instance.Repos)
	}
	if !strings.HasPrefix(instance.Repos[0].MergerToken, "token-"+instances.DefaultMergerName+"-") {
		t.Errorf("repo merger token = %q", instance.Repos[0].MergerToken)
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
	if len(instance.Repos) != 1 || instance.Repos[0].Name != "acme/repo" {
		t.Errorf("Repos = %+v", instance.Repos)
	}
	if instance.AdminToken != "admin-token" {
		t.Errorf("AdminToken = %q", instance.AdminToken)
	}
}

func TestRunReusesValidToken(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	admin.users[instances.DefaultReviewerName] = true
	admin.tokens[tokenKey(instances.DefaultReviewerName, ReviewerTokenName)] = "existing-token"
	options := testOptions()
	options.Existing = &instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: instances.DefaultReviewerName, Token: "existing-token"},
	}
	instance, err := Run(context.Background(), options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if instance.Reviewer.Token != "existing-token" {
		t.Errorf("Reviewer.Token = %q, want reused token", instance.Reviewer.Token)
	}
}

func TestRunDryRunMakesNoMutations(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	options := testOptions()
	options.DryRun = true
	instance, err := Run(context.Background(), options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if admin.users[instances.DefaultReviewerName] || admin.users[instances.DefaultMergerName] {
		t.Errorf("dry-run created users: %v", admin.users)
	}
	if len(admin.collaborators) != 0 || len(admin.protections) != 0 || len(admin.labels) != 0 {
		t.Errorf("dry-run mutated: %+v %+v %+v", admin.collaborators, admin.protections, admin.labels)
	}
	if instance.Reviewer.Token != "" || instance.Merger.Token != "" {
		t.Errorf("dry-run should not invent tokens: %+v", instance)
	}
	for _, repo := range instance.Repos {
		if repo.MergerToken != "" {
			t.Errorf("dry-run should not invent repo merger tokens: %+v", repo)
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
	instance, err := Run(context.Background(), options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(instance.Repos) != 0 {
		t.Errorf("Repos = %+v, want empty", instance.Repos)
	}
	if instance.Reviewer.Token == "" {
		t.Errorf("reviewer token missing: %+v", instance)
	}
	if instance.Merger.Token != "" {
		t.Errorf("Merger.Token = %q, want no instance-level merger token", instance.Merger.Token)
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
	instance, err := Run(context.Background(), options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(instance.Repos) != 1 || instance.Repos[0].Dir != "/srv/repo" {
		t.Errorf("Repos = %+v, want preserved dir for acme/repo", instance.Repos)
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

// 显式指定的 OAuth 客户端（如管理员在 /-/admin/applications 创建的全局应用）
// 直接采用，不再自动注册用户级应用。
