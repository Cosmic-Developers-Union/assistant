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
	collaborators map[string][]string
	protections   map[string]string
	labels        map[string]bool
	oauth         *instances.OAuthCredential
}

func newFakeAdmin(repos ...string) *fakeAdmin {
	fake := &fakeAdmin{
		adminLogin:    "admin",
		adminToken:    "admin-token",
		users:         map[string]bool{"admin": true},
		tokens:        map[string]string{},
		passwords:     map[string]string{},
		repos:         map[string]RepoInfo{},
		collaborators: map[string][]string{},
		protections:   map[string]string{},
		labels:        map[string]bool{},
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

func (f *fakeAdmin) EnsurePassword(_ context.Context, name string) (string, error) {
	if f.passwords == nil {
		f.passwords = map[string]string{}
	}
	if f.passwords[name] == "" {
		f.passwords[name] = "generated-password"
	}
	return f.passwords[name], nil
}

// ConvergeToken 模拟真实实现的收敛语义：保留有效令牌或新建，账号下至多一个。
func (f *fakeAdmin) ConvergeToken(_ context.Context, name, _ /*password*/, keepToken string) (string, bool, error) {
	if keepToken != "" && f.tokens[name] == keepToken {
		return keepToken, false, nil
	}
	f.tokenSeq++
	token := fmt.Sprintf("token-%s-%d", name, f.tokenSeq)
	f.tokens[name] = token
	return token, true, nil
}

func (f *fakeAdmin) ValidateToken(_ context.Context, name, token string) (bool, error) {
	return f.tokens[name] == token, nil
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

func (f *fakeAdmin) AddCollaborator(_ context.Context, fullName, user string) error {
	f.collaborators[fullName] = append(f.collaborators[fullName], user)
	return nil
}

func (f *fakeAdmin) EnsureBranchProtection(_ context.Context, fullName, branch string, requiredApprovals int64) error {
	f.protections[fullName] = fmt.Sprintf("%s/%d", branch, requiredApprovals)
	return nil
}

func (f *fakeAdmin) ReconcileLabels(_ context.Context, fullName, _ string) error {
	f.labels[fullName] = true
	return nil
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
	if instance.Merger.Name != instances.DefaultMergerName || instance.Merger.Token == "" {
		t.Errorf("Merger = %+v", instance.Merger)
	}
	if !admin.users[instances.DefaultReviewerName] || !admin.users[instances.DefaultMergerName] {
		t.Errorf("bot users not created: %v", admin.users)
	}
	wantCollaborators := []string{instances.DefaultReviewerName, instances.DefaultMergerName}
	if got := admin.collaborators["acme/repo"]; strings.Join(got, ",") != strings.Join(wantCollaborators, ",") {
		t.Errorf("collaborators = %v, want %v", got, wantCollaborators)
	}
	if admin.protections["acme/repo"] != "main/2" {
		t.Errorf("protection = %q, want main/2", admin.protections["acme/repo"])
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
	admin.tokens[instances.DefaultReviewerName] = "existing-token"
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

func TestRunValidatesOptions(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	tests := map[string]func(*Options){
		"no host":              func(o *Options) { o.Host = "" },
		"no admin credentials": func(o *Options) { o.AdminToken = "" },
		"no repos":             func(o *Options) { o.Repos = nil },
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
