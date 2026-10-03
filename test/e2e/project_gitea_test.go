//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	gitea "gitea.dev/sdk"
	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/project"
	"github.com/Cosmic-Developers-Union/assistant/internal/runtime"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// TestProjectGiteaBootstrapAndCrossRepositoryMention 验证真实站点的账号、令牌、权限、
// 标签/分支保护、Actions secret，以及未安装 workflow 的公开仓库也能召唤账号。
func TestProjectGiteaBootstrapAndCrossRepositoryMention(t *testing.T) {
	host := os.Getenv("ASSISTANT_E2E_HOST")
	if host == "" {
		t.Skip("未配置隔离 Gitea 测试环境")
	}
	user, password := os.Getenv("ASSISTANT_E2E_ADMIN_USER"), os.Getenv("ASSISTANT_E2E_ADMIN_PASSWORD")
	if user == "" || password == "" {
		t.Skip("未配置测试管理员")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	sdk, err := gitea.NewClient(host, gitea.SetBasicAuth(user, password), gitea.SetGiteaVersion(""))
	if err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:10]
	adminToken, _, err := sdk.Users.CreateAccessToken(ctx, gitea.CreateAccessTokenOption{Name: "assistant-e2e-" + suffix, Scopes: project.TokenScopes(true)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = sdk.Users.DeleteAccessToken(cleanup, adminToken.ID)
	})
	client, err := project.NewClient(credentials.Gitea{Name: "test-admin", URL: host, Username: user, Token: adminToken.Token})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Entry.SetPassword(password); err != nil {
		t.Fatal(err)
	}
	reviewer, merger := "ai-"+suffix, "merge-"+suffix
	var accounts []credentials.Gitea
	for _, name := range []string{reviewer, merger} {
		entry, err := client.ProvisionAccount(ctx, name, name, name+"@assistant.invalid", "", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, entry)
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := client.SDK.Admin.DeleteUser(cleanup, name); err != nil {
				t.Error(err)
			}
		})
		again, err := client.ProvisionAccount(ctx, name, name, name+"@assistant.invalid", "", &entry, false)
		if err != nil || again != entry {
			t.Fatal("重复接入未复用", err)
		}
	}
	// 管理员不提供机器人密码，给已有账号发令牌；验证实际令牌属于目标账号。
	adminPassword, err := client.Entry.PasswordValue()
	if err != nil {
		t.Fatal(err)
	}
	delegated, err := project.IssueTokenForUser(ctx, client.Entry, reviewer, adminPassword, "")
	if err != nil {
		t.Fatal("管理员代发令牌失败", err)
	}
	delegatedClient, err := project.NewClient(credentials.Gitea{URL: host, Username: reviewer, Token: delegated})
	if err != nil {
		t.Fatal(err)
	}
	identity, _, err := delegatedClient.SDK.Users.GetMyUserInfo(ctx)
	if err != nil || identity.UserName != reviewer || identity.IsAdmin {
		t.Fatal("代发令牌身份错误", err)
	}
	botPassword, err := accounts[0].PasswordValue()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := project.IssueTokenForUser(ctx, accounts[0], merger, botPassword, ""); err == nil {
		t.Fatal("普通账号能给其他用户发令牌")
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := credentials.Save(path, &credentials.File{Instances: credentials.Instances{Gitea: accounts}}); err != nil {
		t.Fatal(err)
	}
	for _, repoName := range []string{"managed-" + suffix, "public-" + suffix} {
		if _, _, err := sdk.Repositories.CreateRepo(ctx, gitea.CreateRepoOption{Name: repoName, AutoInit: true, DefaultBranch: "main"}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := client.SDK.Repositories.DeleteRepo(cleanup, user, repoName); err != nil {
				t.Error(err)
			}
		})
	}
	managed := "managed-" + suffix
	if err := client.Configure(ctx, project.RepositoryOptions{Owner: user, Name: managed, Reviewer: reviewer, Merger: merger}); err != nil {
		t.Fatal(err)
	}
	protection, _, err := client.SDK.Repositories.GetBranchProtection(ctx, user, managed, "main")
	if err != nil {
		t.Fatal(err)
	}
	if protection.RequiredApprovals != 2 || !protection.BlockAdminMergeOverride || !protection.DismissStaleApprovals || len(protection.MergeWhitelistUsernames) != 1 || protection.MergeWhitelistUsernames[0] != merger {
		t.Fatal("保护不符合双批准契约", protection)
	}
	dir := t.TempDir()
	if err := project.InstallWorkflow(dir, project.WorkflowOptions{Version: "v1.2.3", Remove: false, DryRun: false}); err != nil {
		t.Fatal(err)
	}
	if err := project.ConfigureMCP(dir, project.MCPOptions{Remove: false, DryRun: false}); err != nil {
		t.Fatal(err)
	}
	if err := project.InstallWorkflow(dir, project.WorkflowOptions{Version: "v1.2.3", Remove: true, DryRun: false}); err != nil {
		t.Fatal(err)
	}
	if err := project.ConfigureMCP(dir, project.MCPOptions{Remove: true, DryRun: false}); err != nil {
		t.Fatal(err)
	}
	public := "public-" + suffix
	if _, _, err := client.SDK.Repositories.CreateBranch(ctx, user, public, gitea.CreateBranchOption{BranchName: "feature", OldBranchName: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.SDK.Repositories.CreateFile(ctx, user, public, "feature.txt", gitea.CreateFileOptions{FileOptions: gitea.FileOptions{BranchName: "feature", Message: "测试召唤"}, Content: base64.StdEncoding.EncodeToString([]byte("test\n"))}); err != nil {
		t.Fatal(err)
	}
	pull, _, err := client.SDK.PullRequests.CreatePullRequest(ctx, user, public, gitea.CreatePullRequestOption{Head: "feature", Base: "main", Title: "跨仓库召唤", Body: "请 @" + reviewer + " 审查"})
	if err != nil {
		t.Fatal(err)
	}
	reviewAPI, err := status.NewClient(host, accounts[0].Token)
	if err != nil {
		t.Fatal(err)
	}
	source := runtime.GiteaSource{API: reviewAPI, Host: host, Identity: reviewer, ReviewBot: "review"}
	events, err := source.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Repo == user+"/"+public && event.Kind == "gitea-review" {
			found = true
		}
	}
	if !found {
		t.Fatal("无协作关系、无 workflow 的公开仓库 @ 召唤未被发现", events)
	}
	// 同一账号正式回应后，旧正文 mention 不会每轮重试。
	if err := reviewAPI.CreatePullReview(ctx, status.Repository{Owner: user, Name: public}, pull.Index, status.ReviewInput{State: status.ReviewStateComment, CommitID: pull.Head.Sha, Body: "已审查，需要作者补充"}); err != nil {
		t.Fatal(err)
	}
	events, err = source.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Repo == user+"/"+public {
			t.Fatal("旧 mention 在正式回应后仍入队", events)
		}
	}
	// Gitea 时间字段按秒输出，跨到下一秒再验证新的 @ 评论。
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(1100 * time.Millisecond):
	}
	if _, _, err := client.SDK.Issues.CreateIssueComment(ctx, user, public, pull.Index, gitea.CreateIssueCommentOption{Body: "请 @" + reviewer + " 再次审查"}); err != nil {
		t.Fatal(err)
	}
	events, err = source.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, event := range events {
		found = found || event.Repo == user+"/"+public
	}
	if !found {
		t.Fatal("正式回应后的新 mention 未入队", events)
	}

}
