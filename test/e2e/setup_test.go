//go:build e2e

// Package e2e 是针对临时 Gitea（test/gitea/docker-compose.yaml）的端到端测试：
// 先运行 test/gitea/up.sh（生成 .env 中的 HOST/ADMIN_TOKEN），再以
//
//	make test-e2e
//
// 触发。没有测试环境变量时自动跳过。
//
// 新认证模型下，instance 配置里没有任何令牌：管理员令牌由调用方从环境注入，
// review/merge 令牌是 setup.Run 返回的 result.Credentials，按
// (host, 账号, purpose) 唯一，写回凭据库（credentials.json）。
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

	"assistant/internal/credentials"
	"assistant/internal/instances"
	"assistant/internal/setup"
	"assistant/internal/status"
)

type e2eEnv struct {
	Host          string
	AdminUser     string
	AdminPassword string
	AdminToken    string
	Image         string
}

func environment(t *testing.T) e2eEnv {
	t.Helper()
	env := e2eEnv{
		Host:          os.Getenv("ASSISTANT_E2E_HOST"),
		AdminUser:     os.Getenv("ASSISTANT_E2E_ADMIN_USER"),
		AdminPassword: os.Getenv("ASSISTANT_E2E_ADMIN_PASSWORD"),
		AdminToken:    os.Getenv("ASSISTANT_E2E_ADMIN_TOKEN"),
		Image:         os.Getenv("ASSISTANT_E2E_IMAGE"),
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
				case "ASSISTANT_E2E_IMAGE":
					env.Image = value
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

// credentialsFor 返回 result.Credentials 里指定用途的令牌（0 条或多条都返回）。
func credentialsFor(result setup.Result, purpose string) []credentials.Credential {
	var matches []credentials.Credential
	for _, credential := range result.Credentials {
		if credential.Purpose == purpose {
			matches = append(matches, credential)
		}
	}
	return matches
}

// credentialFor 取指定用途的唯一令牌：(host, purpose) 只应有一条，多/少都算失败。
func credentialFor(t *testing.T, result setup.Result, purpose string) credentials.Credential {
	t.Helper()
	matches := credentialsFor(result, purpose)
	if len(matches) != 1 {
		t.Fatalf("result.Credentials 中 purpose=%s 的令牌有 %d 条，want 1: %+v", purpose, len(matches), result.Credentials)
	}
	return matches[0]
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
	stamp := time.Now().Unix()
	repositoryName := fmt.Sprintf("e2e-%d", stamp)
	secondRepositoryName := fmt.Sprintf("e2e-%d-b", stamp)
	fullName := adminLogin + "/" + repositoryName
	secondFullName := adminLogin + "/" + secondRepositoryName

	options := setup.Options{
		Host:        host,
		AdminToken:  adminToken,
		Repos:       []string{fullName, secondFullName},
		CreateRepos: true,
		Log:         t.Logf,
	}
	admin, err := setup.NewAdmin(ctx, options)
	if err != nil {
		t.Fatalf("NewAdmin() error = %v", err)
	}
	result, err := setup.Run(ctx, options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	instance := result.Instance
	if len(instance.Repos) != 2 {
		t.Fatalf("instance.Repos = %+v, want 2 个仓库", instance.Repos)
	}

	// 凭据只在凭据库（result.Credentials）：review / merge 各一条，按
	// (host, 账号, purpose) 唯一；instance 里只有账号名，没有令牌。
	reviewCredential := credentialFor(t, result, credentials.PurposeReview)
	mergeCredential := credentialFor(t, result, credentials.PurposeMerge)
	if len(result.Credentials) != 2 {
		t.Errorf("result.Credentials 有 %d 条，want 2（review + merge）: %+v", len(result.Credentials), result.Credentials)
	}
	for _, want := range []struct {
		purpose    string
		user       string
		credential credentials.Credential
	}{
		{credentials.PurposeReview, instance.Reviewer.Name, reviewCredential},
		{credentials.PurposeMerge, instance.Merger.Name, mergeCredential},
	} {
		if want.credential.Host != credentials.NormalizeHost(host) {
			t.Errorf("%s 凭据 host = %q, want %q", want.purpose, want.credential.Host, credentials.NormalizeHost(host))
		}
		if want.credential.User != want.user {
			t.Errorf("%s 凭据 user = %q, want %q", want.purpose, want.credential.User, want.user)
		}
		if want.credential.Token == "" {
			t.Errorf("%s 凭据缺令牌: %+v", want.purpose, want.credential)
		}
		if want.credential.TokenName != setup.ReviewerTokenName {
			t.Errorf("%s 凭据 token name = %q, want %q", want.purpose, want.credential.TokenName, setup.ReviewerTokenName)
		}
		if want.credential.Source != credentials.SourceSetup {
			t.Errorf("%s 凭据 source = %q, want %q", want.purpose, want.credential.Source, credentials.SourceSetup)
		}
		if want.credential.LastEight != credentials.LastEight(want.credential.Token) {
			t.Errorf("%s 凭据 last_eight = %q, want %q", want.purpose, want.credential.LastEight, credentials.LastEight(want.credential.Token))
		}
	}

	// merge 是「一个站点一个账号一条令牌」：两个仓库共用同一条 MERGE_TOKEN，
	// 新模型里不存在 per-repo merger token。
	if got := len(credentialsFor(result, credentials.PurposeMerge)); got != 1 {
		t.Errorf("merge 凭据有 %d 条，want 1（同一站点 merge 账号只允许一条令牌）", got)
	}

	// 机器人令牌可用且身份正确（review 与 merge 各用自己那条令牌）。
	for _, credential := range []credentials.Credential{reviewCredential, mergeCredential} {
		client, err := status.NewClient(host, credential.Token)
		if err != nil {
			t.Fatal(err)
		}
		login, err := client.AuthenticatedUser(ctx)
		if err != nil {
			t.Fatalf("AuthenticatedUser(%s) error = %v", credential.User, err)
		}
		if login != credential.User {
			t.Errorf("token identity = %q, want %q", login, credential.User)
		}
	}

	repository := status.Repository{Owner: adminLogin, Name: repositoryName}
	reviewerClient, err := status.NewClient(host, reviewCredential.Token)
	if err != nil {
		t.Fatal(err)
	}
	// 分支保护读取需要 repo admin：与运行期同口径，用 admin 令牌读取
	if err := reviewerClient.UseBranchProtectionToken(adminToken); err != nil {
		t.Fatal(err)
	}

	// 协作者：reviewer 写权限，merger 管理员权限（合并白名单 + 保护读取）
	for _, want := range []struct{ name, permission string }{
		{instance.Reviewer.Name, "write"},
		{instance.Merger.Name, "admin"},
	} {
		permission := collaboratorPermission(t, host, adminToken, adminLogin, repositoryName, want.name)
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
	syncClient, err := status.NewClient(host, reviewCredential.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncClient.UseBranchProtectionToken(adminToken); err != nil {
		t.Fatal(err)
	}
	manager := status.NewManager(syncClient,
		status.WithRepository(repository),
		status.WithContentReviewer(instance.Reviewer.Name),
		status.WithProgress(t.Logf),
	)
	// 非规范标签：sync 强制收敛标签体系时应删除
	if _, err := reviewerClient.CreateLabel(ctx, repository, status.LabelDefinition{
		Name: "stale/topic", Color: "CCCCCC", Description: "e2e 非规范标签",
	}); err != nil {
		t.Fatalf("CreateLabel(stale) error = %v", err)
	}
	if err := manager.Sync(ctx); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	afterSync, err := reviewerClient.ListRepositoryLabels(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range afterSync {
		if label.Name == "stale/topic" {
			t.Errorf("非规范标签未被删除：%+v", afterSync)
		}
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
	if err := manager.ReconcileReviewRequests(ctx); err != nil {
		t.Fatalf("ReconcileReviewRequests() after /review error = %v", err)
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
	if err := manager.ReconcileReviewRequests(ctx); err != nil {
		t.Fatalf("ReconcileReviewRequests() after approval error = %v", err)
	}
	afterApproval, err := syncClient.GetPullRequest(ctx, repository, pull.Index)
	if err != nil {
		t.Fatal(err)
	}

	// merge 会签（第二票）后以 merge 身份合并：白名单 + 管理员须遵守 +
	// official review request 门禁下仍能合入（回应后请求行已删）。
	mergerClient, err := status.NewClient(host, mergeCredential.Token)
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

	// Actions 配置：每个受管仓库写 MERGE_TOKEN，值是同一条 merge 令牌（整站
	// 唯一）。secret 只写不可读，无法回读比对，因此这里校验两块：result 里
	// merge 凭据恰好一条 + 每个仓库都存在 MERGE_TOKEN；令牌值正确性由
	// workflow_test 的 automerge job（真实的 merge 身份合并）端到端验证。
	actionsAdmin, err := setup.NewAdmin(ctx, setup.Options{Host: host, AdminToken: adminToken})
	if err != nil {
		t.Fatal(err)
	}
	// 写两次：同值 PUT 幂等（Gitea secret 只能覆盖写，重复执行不报错）。
	for round := 0; round < 2; round++ {
		if err := setup.ConfigureActions(ctx, actionsAdmin, instance, mergeCredential.Token, false, t.Logf); err != nil {
			t.Fatalf("ConfigureActions() round %d error = %v", round+1, err)
		}
	}
	for _, repo := range instance.Repos {
		owner, name, err := instances.ParseRepoName(repo.Name)
		if err != nil {
			t.Fatal(err)
		}
		secrets := listActionSecrets(t, host, adminToken, owner, name)
		if !slices.Contains(secrets, setup.ActionsSecretMergeToken) {
			t.Errorf("%s secrets = %v, want %s", repo.Name, secrets, setup.ActionsSecretMergeToken)
		}
	}

	// 幂等：重复 setup 复用凭据库里的同一条令牌（按 purpose 比对，而不是
	// 旧模型 instance 上的令牌字段）。
	rerunOptions := options
	rerunOptions.Existing = &instance
	rerunOptions.ExistingCredentials = result.Credentials
	rerunOptions.Log = t.Logf
	admin2, err := setup.NewAdmin(ctx, rerunOptions)
	if err != nil {
		t.Fatal(err)
	}
	rerun, err := setup.Run(ctx, rerunOptions, admin2)
	if err != nil {
		t.Fatalf("rerun Run() error = %v", err)
	}
	for _, purpose := range []string{credentials.PurposeReview, credentials.PurposeMerge} {
		before := credentialFor(t, result, purpose)
		after := credentialFor(t, rerun, purpose)
		if after.Token == "" {
			t.Errorf("rerun %s 凭据缺令牌: %+v", purpose, after)
			continue
		}
		if after.Token != before.Token || after.LastEight != before.LastEight || after.User != before.User {
			t.Errorf("rerun %s 令牌未复用：before(last_eight)=%s after(last_eight)=%s",
				purpose, before.LastEight, after.LastEight)
		}
	}
	if rerun.Instance.Reviewer.Name != instance.Reviewer.Name || rerun.Instance.Merger.Name != instance.Merger.Name {
		t.Errorf("rerun instance 机器人账号变化：%+v，want reviewer=%s merger=%s",
			rerun.Instance, instance.Reviewer.Name, instance.Merger.Name)
	}

	// 配置落盘可回读，且不落任何令牌（凭据只在凭据库）。
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{Instances: []instances.Instance{instance}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := instances.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Instances[0].Reviewer.Name != instance.Reviewer.Name ||
		loaded.Instances[0].Merger.Name != instance.Merger.Name ||
		len(loaded.Instances[0].Repos) != len(instance.Repos) {
		t.Errorf("config round-trip lost instance 配置：%+v", loaded.Instances[0])
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range result.Credentials {
		if credential.Token != "" && strings.Contains(string(data), credential.Token) {
			t.Errorf("config.json 不应包含 %s 令牌明文（凭据只在 credentials.json）", credential.Purpose)
		}
	}
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
