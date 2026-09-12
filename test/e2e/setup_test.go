//go:build e2e

// Package e2e 是针对临时 Gitea（test/gitea/docker-compose.yaml）的端到端测试：
// 先运行 test/gitea/up.sh（生成 .env 中的 HOST/ADMIN_TOKEN），再以
//
//	make test-e2e
//
// 触发。没有测试环境变量时自动跳过。
package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gitea "gitea.dev/sdk"

	"assistant/internal/instances"
	"assistant/internal/setup"
	"assistant/internal/status"
)

type e2eEnv struct {
	Host          string
	AdminUser     string
	AdminPassword string
	AdminToken    string
}

func environment(t *testing.T) e2eEnv {
	t.Helper()
	env := e2eEnv{
		Host:          os.Getenv("ASSISTANT_E2E_HOST"),
		AdminUser:     os.Getenv("ASSISTANT_E2E_ADMIN_USER"),
		AdminPassword: os.Getenv("ASSISTANT_E2E_ADMIN_PASSWORD"),
		AdminToken:    os.Getenv("ASSISTANT_E2E_ADMIN_TOKEN"),
	}
	// up.sh 会把凭据写到包目录下的 .env
	if env.Host == "" || env.AdminToken == "" {
		if data, err := os.ReadFile(".env"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
				if !ok {
					continue
				}
				switch key {
				case "ASSISTANT_E2E_HOST":
					env.Host = value
				case "ASSISTANT_E2E_ADMIN_USER":
					env.AdminUser = value
				case "ASSISTANT_E2E_ADMIN_PASSWORD":
					env.AdminPassword = value
				case "ASSISTANT_E2E_ADMIN_TOKEN":
					env.AdminToken = value
				}
			}
		}
	}
	if env.AdminUser == "" {
		env.AdminUser = "e2eadmin"
	}
	if env.AdminPassword == "" {
		env.AdminPassword = "admin-e2e-password"
	}
	if env.Host == "" || env.AdminToken == "" {
		t.Skip("缺少 ASSISTANT_E2E_HOST / ASSISTANT_E2E_ADMIN_TOKEN（先运行 test/gitea/up.sh）")
	}
	return env
}

func TestSetupInitializesInstanceEndToEnd(t *testing.T) {
	env := environment(t)
	host, adminToken := env.Host, env.AdminToken
	ctx := context.Background()
	adminClient, err := status.NewClient(host, adminToken)
	if err != nil {
		t.Fatal(err)
	}
	adminLogin, err := adminClient.AuthenticatedUser(ctx)
	if err != nil {
		t.Fatalf("管理员令牌不可用: %v", err)
	}
	repositoryName := fmt.Sprintf("e2e-%d", time.Now().Unix())
	fullName := adminLogin + "/" + repositoryName

	options := setup.Options{
		Host:        host,
		AdminToken:  adminToken,
		Repos:       []string{fullName},
		CreateRepos: true,
		Log:         t.Logf,
	}
	admin, err := setup.NewAdmin(ctx, options)
	if err != nil {
		t.Fatalf("NewAdmin() error = %v", err)
	}
	instance, err := setup.Run(ctx, options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if instance.Reviewer.Token == "" || instance.Merger.Token == "" {
		t.Fatalf("tokens missing: %+v", instance)
	}

	// 机器人令牌可用且身份正确
	for _, account := range []instances.Account{instance.Reviewer, instance.Merger} {
		client, err := status.NewClient(host, account.Token)
		if err != nil {
			t.Fatal(err)
		}
		login, err := client.AuthenticatedUser(ctx)
		if err != nil {
			t.Fatalf("AuthenticatedUser(%s) error = %v", account.Name, err)
		}
		if login != account.Name {
			t.Errorf("token identity = %q, want %q", login, account.Name)
		}
	}

	repository := status.Repository{Owner: adminLogin, Name: repositoryName}
	reviewerClient, err := status.NewClient(host, instance.Reviewer.Token)
	if err != nil {
		t.Fatal(err)
	}
	// 分支保护读取需要 repo admin：与运行期同口径，用 admin 令牌读取
	if err := reviewerClient.UseBranchProtectionToken(instance.AdminToken); err != nil {
		t.Fatal(err)
	}

	// 协作者
	collaborators, err := reviewerClient.ListCollaboratorLogins(ctx, repository)
	if err != nil {
		t.Fatalf("ListCollaboratorLogins() error = %v", err)
	}
	for _, want := range []string{instance.Reviewer.Name, instance.Merger.Name} {
		if !slices.Contains(collaborators, want) {
			t.Errorf("collaborators = %v, want %s", collaborators, want)
		}
	}

	// 分支保护
	protections, err := reviewerClient.ListBranchProtections(ctx, repository)
	if err != nil {
		t.Fatalf("ListBranchProtections() error = %v", err)
	}
	found := false
	for _, protection := range protections {
		if protection.RuleName == "main" {
			found = true
			if protection.RequiredApprovals != 2 {
				t.Errorf("RequiredApprovals = %d, want 2", protection.RequiredApprovals)
			}
		}
	}
	if !found {
		t.Errorf("no branch protection for main: %+v", protections)
	}

	// 标签体系（与 sync 同口径）
	labels, err := reviewerClient.ListRepositoryLabels(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	labelNames := make([]string, 0, len(labels))
	for _, label := range labels {
		labelNames = append(labelNames, label.Name)
	}
	for _, want := range []string{"status/triage", "status/review", "awaiting/reviewer", "awaiting/merge", "type/bug"} {
		if !slices.Contains(labelNames, want) {
			t.Errorf("labels missing %s: %v", want, labelNames)
		}
	}

	// 完整流程：建 Issue → sync 打 status/triage → check 列待办
	sdkClient, err := gitea.NewClient(host, gitea.SetToken(adminToken))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sdkClient.Issues.CreateIssue(ctx, adminLogin, repositoryName, gitea.CreateIssueOption{
		Title: "e2e triage me",
	}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	syncClient, err := status.NewClient(host, instance.Reviewer.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncClient.UseBranchProtectionToken(instance.AdminToken); err != nil {
		t.Fatal(err)
	}
	manager := status.NewManager(syncClient, status.WithRepository(repository))
	if err := manager.Sync(ctx); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	report, err := manager.Check(ctx)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(report.NeedsTriage) != 1 || report.NeedsTriage[0].Index != 1 {
		t.Errorf("NeedsTriage = %+v, want issue #1", report.NeedsTriage)
	}

	// 评审请求不被门禁阻断：必要检查失败 + /review 评论 → 仍进 status/review
	// 队列（status/review = 「评审请求中」，门禁只在合并时校验）。
	if _, _, err := sdkClient.Repositories.CreateFile(
		ctx, adminLogin, repositoryName, "feature.txt",
		gitea.CreateFileOptions{
			FileOptions: gitea.FileOptions{
				Message:       "e2e: change for review",
				BranchName:    "main",
				NewBranchName: "e2e-feature",
			},
			Content: base64.StdEncoding.EncodeToString([]byte("change\n")),
		},
	); err != nil {
		t.Fatalf("CreateFile() error = %v", err)
	}
	pull, _, err := sdkClient.PullRequests.CreatePullRequest(ctx, adminLogin, repositoryName, gitea.CreatePullRequestOption{
		Head:  "e2e-feature",
		Base:  "main",
		Title: "e2e review request",
	})
	if err != nil {
		t.Fatalf("CreatePullRequest() error = %v", err)
	}
	if _, _, err := sdkClient.Repositories.CreateStatus(ctx, adminLogin, repositoryName, pull.Head.Sha, gitea.CreateStatusOption{
		State:       gitea.StatusFailure,
		Context:     "ci / required",
		Description: "e2e failure",
	}); err != nil {
		t.Fatalf("CreateStatus() error = %v", err)
	}
	if _, _, err := sdkClient.Issues.CreateIssueComment(ctx, adminLogin, repositoryName, pull.Index, gitea.CreateIssueCommentOption{
		Body: "/review",
	}); err != nil {
		t.Fatalf("CreateIssueComment() error = %v", err)
	}
	if err := manager.Sync(ctx); err != nil {
		t.Fatalf("Sync() after /review error = %v", err)
	}
	updated, err := syncClient.GetPullRequest(ctx, repository, pull.Index)
	if err != nil {
		t.Fatalf("GetPullRequest() error = %v", err)
	}
	labelSet := map[string]bool{}
	for _, label := range updated.Labels {
		labelSet[label.Name] = true
	}
	if !labelSet["status/review"] {
		t.Errorf("labels = %v, want status/review despite failing checks", updated.Labels)
	}
	if labelSet["status/changes-requested"] {
		t.Errorf("labels = %v, failing checks must not auto-reject review requests", updated.Labels)
	}
	queue, err := syncClient.ListReviewPullRequests(ctx, repository)
	if err != nil {
		t.Fatalf("ListReviewPullRequests() error = %v", err)
	}
	if len(queue) != 1 || queue[0].Index != pull.Index {
		t.Errorf("review queue = %+v, want pull #%d", queue, pull.Index)
	}

	// 幂等：重复 setup 复用令牌
	rerunOptions := options
	rerunOptions.Existing = &instance
	rerunOptions.Log = t.Logf
	admin2, err := setup.NewAdmin(ctx, rerunOptions)
	if err != nil {
		t.Fatal(err)
	}
	instance2, err := setup.Run(ctx, rerunOptions, admin2)
	if err != nil {
		t.Fatalf("rerun Run() error = %v", err)
	}
	if instance2.Reviewer.Token != instance.Reviewer.Token || instance2.Merger.Token != instance.Merger.Token {
		t.Errorf("rerun regenerated tokens: %+v", instance2)
	}

	// 配置落盘可回读
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{Instances: []instances.Instance{instance}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := instances.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Instances[0].Reviewer.Token != instance.Reviewer.Token {
		t.Errorf("config round-trip lost token")
	}
}
