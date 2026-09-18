//go:build e2e

// 原生 auto-merge 端到端（issue #1）：分支保护配置必要检查 context 后，
// automerge 在检查运行期间武装 Gitea 原生 auto-merge（会签前移到武装之前），
// 检查变绿由 Gitea 服务端即时 squash 合并。全程不经 runner——直接驱动
// Manager 与 commit status API，钉死 Gitea 1.27.3 的服务端语义。
package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"assistant/internal/credentials"
	"assistant/internal/setup"
	"assistant/internal/status"
)

func TestAutoMergeArmsAndGiteaMergesWhenChecksTurnGreen(t *testing.T) {
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
	repositoryName := fmt.Sprintf("e2e-arm-%d", time.Now().Unix())
	fullName := adminLogin + "/" + repositoryName

	// 1) 实例初始化（与生产 setup 同口径），分支保护带必要检查 context——
	//    这是武装原生 auto-merge 的前提
	options := setup.Options{
		Host:                host,
		AdminToken:          adminToken,
		Repos:               []string{fullName},
		CreateRepos:         true,
		StatusCheckContexts: []string{"e2e/required"},
		Log:                 t.Logf,
	}
	admin, err := setup.NewAdmin(ctx, options)
	if err != nil {
		t.Fatalf("NewAdmin() error = %v", err)
	}
	result, err := setup.Run(ctx, options, admin)
	if err != nil {
		t.Fatalf("setup.Run() error = %v", err)
	}
	reviewerClient, err := status.NewClient(host, credentialFor(t, result, credentials.PurposeReview).Token)
	if err != nil {
		t.Fatal(err)
	}
	mergerClient, err := status.NewClient(host, credentialFor(t, result, credentials.PurposeMerge).Token)
	if err != nil {
		t.Fatal(err)
	}
	repository := status.Repository{Owner: adminLogin, Name: repositoryName}

	// 2) 开一笔 PR，给必要 context 打 pending 状态（模拟 CI 运行中）
	if statusCode, body := apiRequest(t, http.MethodPost, repoAPI(host, fullName)+"/contents/note.txt", adminToken,
		map[string]any{
			"content":    base64.StdEncoding.EncodeToString([]byte("arm probe\n")),
			"message":    "e2e: arm probe",
			"branch":     "main",
			"new_branch": "e2e-arm",
		}); statusCode != http.StatusCreated {
		t.Fatalf("push branch: HTTP %d: %s", statusCode, body)
	}
	// contents API 的 push hook 异步处理：等 push 事件落定再开 PR（dismiss_stale_approvals 竞态）
	time.Sleep(2 * time.Second)
	statusCode, body := apiRequest(t, http.MethodPost, repoAPI(host, fullName)+"/pulls", adminToken,
		map[string]any{"title": "e2e: native auto merge", "head": "e2e-arm", "base": "main"})
	if statusCode != http.StatusCreated {
		t.Fatalf("CreatePullRequest: HTTP %d: %s", statusCode, body)
	}
	pullIndex, headSHA := parseCreatedPull(t, body)
	postStatus(t, host, adminToken, fullName, headSHA, "pending")

	// 3) ai 内容批准（合并门禁），等合并性计算完成
	submitApproval(t, reviewerClient, repository, pullIndex, headSHA)
	waitMergeable(t, mergerClient, repository, pullIndex)

	// 4) automerge：检查运行中 → 会签前移 + 武装，本轮不合并
	manager := status.NewManager(mergerClient,
		status.WithRepository(repository),
		status.WithContentReviewer("ai"),
		status.WithStateReviewer("merge"),
		status.WithProgress(t.Logf),
	)
	if err := manager.AutoMerge(ctx); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	// 武装落库：再次武装回 409 → AlreadyArmed；会签已在 head 上
	armResult, err := mergerClient.ArmAutoMerge(ctx, repository, pullIndex, headSHA)
	if err != nil {
		t.Fatalf("复查武装状态 ArmAutoMerge() error = %v", err)
	}
	if armResult != status.AutoMergeAlreadyArmed {
		t.Fatalf("ArmAutoMerge() = %q, want %q（此前一轮应已排定）", armResult, status.AutoMergeAlreadyArmed)
	}
	reviews, err := mergerClient.ListPullReviews(ctx, repository, pullIndex)
	if err != nil {
		t.Fatal(err)
	}
	countersigned := false
	for _, review := range reviews {
		if review.User == "merge" && review.State == status.ReviewStateApproved &&
			review.CommitID == headSHA && !review.Dismissed {
			countersigned = true
		}
	}
	if !countersigned {
		t.Error("未找到 merge 在 head 上的会签（武装前应先会签满足批准门禁）")
	}
	if pull, err := mergerClient.GetPullRequest(ctx, repository, pullIndex); err != nil {
		t.Fatal(err)
	} else if pull.Open {
		t.Log("武装完成，PR 保持 open（检查运行中不合并）——正确")
	}

	// 5) 必要检查变绿：Gitea 原生 auto-merge 即时合并，无任何 assistant 参与
	postStatus(t, host, adminToken, fullName, headSHA, "success")
	deadline := time.Now().Add(60 * time.Second)
	for {
		pull, err := mergerClient.GetPullRequest(ctx, repository, pullIndex)
		if err != nil {
			t.Fatal(err)
		}
		if !pull.Open {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("检查变绿后 60s 内未由原生 auto-merge 合并（盲区仍在）")
		}
		time.Sleep(time.Second)
	}

	// 6) 合并形态：squash、合并者是 merge 账号、head 分支随排定删除
	if statusCode, _ := apiRequest(t, http.MethodGet, repoAPI(host, fullName)+"/branches/e2e-arm", adminToken, nil); statusCode != http.StatusNotFound {
		t.Errorf("head 分支应随原生合并删除，GET branches/e2e-arm = HTTP %d", statusCode)
	}
	statusCode, body = apiRequest(t, http.MethodGet,
		fmt.Sprintf("%s/pulls/%d", repoAPI(host, fullName), pullIndex), adminToken, nil)
	if statusCode != http.StatusOK {
		t.Fatalf("GetPullRequest: HTTP %d: %s", statusCode, body)
	}
	var merged struct {
		Merged   bool `json:"merged"`
		MergedBy struct {
			Login string `json:"login"`
		} `json:"merged_by"`
	}
	if err := json.Unmarshal(body, &merged); err != nil {
		t.Fatal(err)
	}
	if !merged.Merged {
		t.Fatal("PR 未标记为已合并")
	}
	if merged.MergedBy.Login != "merge" {
		t.Errorf("merged_by = %q, want %q（合并者为武装账号）", merged.MergedBy.Login, "merge")
	}
}

// postStatus 在 head commit 上以 e2e/required context 写 commit status。
func postStatus(t *testing.T, host, token, fullName, sha, state string) {
	t.Helper()
	statusCode, body := apiRequest(t, http.MethodPost,
		fmt.Sprintf("%s/statuses/%s", repoAPI(host, fullName), sha), token,
		map[string]any{"state": state, "context": "e2e/required", "description": "e2e " + state})
	if statusCode != http.StatusCreated {
		t.Fatalf("post status %s: HTTP %d: %s", state, statusCode, body)
	}
}
