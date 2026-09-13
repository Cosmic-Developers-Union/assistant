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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	if instance.Reviewer.Token == "" || len(instance.Repos) != 1 || instance.Repos[0].MergerToken == "" {
		t.Fatalf("tokens missing: %+v", instance)
	}
	if instance.Merger.Token != "" {
		t.Errorf("Merger.Token = %q, want per-repo tokens only", instance.Merger.Token)
	}

	// 机器人令牌可用且身份正确（merger 用仓库专属令牌）
	for _, account := range []instances.Account{instance.Reviewer, {Name: instance.Merger.Name, Token: instance.Repos[0].MergerToken}} {
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

	// 协作者：reviewer 写权限，merger 管理员权限（合并白名单 + 保护读取）
	for _, want := range []struct{ name, permission string }{
		{instance.Reviewer.Name, "write"},
		{instance.Merger.Name, "admin"},
	} {
		permission := collaboratorPermission(t, host, instance.AdminToken, adminLogin, repositoryName, want.name)
		if permission != want.permission {
			t.Errorf("collaborator %s permission = %q, want %q", want.name, permission, want.permission)
		}
	}

	// 分支保护：统一策略
	protections, err := reviewerClient.ListBranchProtections(ctx, repository)
	if err != nil {
		t.Fatalf("ListBranchProtections() error = %v", err)
	}
	found := false
	for _, protection := range protections {
		if protection.RuleName != "main" {
			continue
		}
		found = true
		if protection.RequiredApprovals != 2 {
			t.Errorf("RequiredApprovals = %d, want 2", protection.RequiredApprovals)
		}
		if !protection.EnableMergeWhitelist || !slices.Contains(protection.MergeWhitelistUsernames, instance.Merger.Name) {
			t.Errorf("merge whitelist = %v/%v, want only %s",
				protection.EnableMergeWhitelist, protection.MergeWhitelistUsernames, instance.Merger.Name)
		}
		if !protection.BlockOnRejectedReviews {
			t.Error("BlockOnRejectedReviews = false, want true")
		}
		if protection.BlockOnOfficialReviewRequests != true {
			t.Error("BlockOnOfficialReviewRequests = false, want true（/review 会登记请求，回应后自动清除）")
		}
		if !protection.BlockAdminMergeOverride {
			t.Error("BlockAdminMergeOverride = false, want true（管理员须遵守分支保护规则）")
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
	manager := status.NewManager(syncClient,
		status.WithRepository(repository),
		status.WithContentReviewer(instance.Reviewer.Name),
		status.WithProgress(t.Logf),
	)
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
	// /review 已被登记为官方评审请求（block_on_official_review_requests 门禁
	// 按它判定）
	if !slices.Contains(updated.RequestedReviewers, instance.Reviewer.Name) {
		t.Errorf("requested_reviewers = %v, want %s（/review 自动登记）",
			updated.RequestedReviewers, instance.Reviewer.Name)
	}

	// ai 批准 → Gitea 删除其 request 行（API 的 requested_reviewers 字段有
	// 显示滞后，不作为断言依据）；sync 的撤回是版本兼容兜底。
	if err := reviewerClient.CreatePullReview(ctx, repository, pull.Index, status.ReviewInput{
		State:    status.ReviewStateApproved,
		Body:     "e2e approve",
		CommitID: updated.HeadSHA,
	}); err != nil {
		t.Fatalf("CreatePullReview(ai) error = %v", err)
	}
	if err := manager.Sync(ctx); err != nil {
		t.Fatalf("Sync() after approval error = %v", err)
	}
	afterApproval, err := syncClient.GetPullRequest(ctx, repository, pull.Index)
	if err != nil {
		t.Fatal(err)
	}

	// merge 会签（第二票）后以 merge 身份合并：白名单 + 管理员须遵守 +
	// official review request 门禁下仍能合入（回应后请求行已删）。
	mergerClient, err := status.NewClient(host, instance.Repos[0].MergerToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := mergerClient.CreatePullReview(ctx, repository, pull.Index, status.ReviewInput{
		State:    status.ReviewStateApproved,
		Body:     "e2e countersign",
		CommitID: afterApproval.HeadSHA,
	}); err != nil {
		t.Fatalf("CreatePullReview(merge) error = %v", err)
	}
	// mergeability 由 Gitea 异步计算：等它就绪再合并，避免「Please try again later」
	deadline := time.Now().Add(20 * time.Second)
	for {
		current, err := syncClient.GetPullRequest(ctx, repository, pull.Index)
		if err != nil {
			t.Fatal(err)
		}
		if current.Mergeable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("PR 长时间不可合并：%+v", current)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if err := mergerClient.MergePullRequest(ctx, repository, pull.Index); err != nil {
		t.Fatalf("MergePullRequest(merge) error = %v（评审请求门禁/白名单异常）", err)
	}
	// 合并状态回写可能稍滞后，轮询确认
	mergeDeadline := time.Now().Add(10 * time.Second)
	for {
		merged, err := syncClient.GetPullRequest(ctx, repository, pull.Index)
		if err != nil {
			t.Fatal(err)
		}
		if !merged.Open {
			break
		}
		if time.Now().After(mergeDeadline) {
			t.Fatalf("合并后 PR 仍为 open：%+v", merged)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Actions 配置：写 variable/secret 并回读校验（secret 只写不可读，只能
	// 校验列表里存在；variable 可读回比对值）。
	actionsAdmin, err := setup.NewAdmin(ctx, setup.Options{Host: host, AdminToken: adminToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.ConfigureActions(ctx, actionsAdmin, instance, false, t.Logf); err != nil {
		t.Fatalf("ConfigureActions() error = %v", err)
	}
	if value := getActionVariable(t, host, adminToken, adminLogin, repositoryName, setup.ActionsVariableStateReviewer); value != instance.Merger.Name {
		t.Errorf("GITEA_STATE_REVIEWER = %q, want %q", value, instance.Merger.Name)
	}
	secrets := listActionSecrets(t, host, adminToken, adminLogin, repositoryName)
	for _, want := range []string{setup.ActionsSecretStateToken, setup.ActionsSecretBranchProtectionToken} {
		if !slices.Contains(secrets, want) {
			t.Errorf("secrets = %v, want %s", secrets, want)
		}
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
	if instance2.Reviewer.Token != instance.Reviewer.Token ||
		instance2.Repos[0].MergerToken != instance.Repos[0].MergerToken {
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

// getActionVariable 读回仓库级 Actions variable（可读；响应字段是 data）。
func getActionVariable(t *testing.T, host, token, owner, repo, name string) string {
	t.Helper()
	body := apiGet(t, host+"/api/v1/repos/"+owner+"/"+repo+"/actions/variables/"+name, token)
	var payload struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("解析 variable 响应: %v（%s）", err, body)
	}
	return payload.Data
}

// listActionSecrets 列出仓库级 Actions secret 名（值只写不可读；响应是数组）。
func listActionSecrets(t *testing.T, host, token, owner, repo string) []string {
	t.Helper()
	body := apiGet(t, host+"/api/v1/repos/"+owner+"/"+repo+"/actions/secrets", token)
	var payload []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("解析 secrets 响应: %v（%s）", err, body)
	}
	names := make([]string, 0, len(payload))
	for _, secret := range payload {
		names = append(names, secret.Name)
	}
	return names
}

// collaboratorPermission 读仓库协作者权限（admin/write/read）。
func collaboratorPermission(t *testing.T, host, token, owner, repo, user string) string {
	t.Helper()
	body := apiGet(t, host+"/api/v1/repos/"+owner+"/"+repo+"/collaborators/"+user+"/permission", token)
	var payload struct {
		Permission string `json:"permission"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("解析 collaborator permission: %v（%s）", err, body)
	}
	return payload.Permission
}

func apiGet(t *testing.T, url, token string) []byte {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "token "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode/100 != 2 {
		t.Fatalf("GET %s: HTTP %d: %s", url, response.StatusCode, body)
	}
	return body
}
