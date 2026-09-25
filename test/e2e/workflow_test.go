//go:build e2e

// 工作流级 e2e：真实 act_runner 执行仓库内 workflow（action label-sync + action automerge），覆盖
// 三种身份：admin（仓库 owner）、writer（写权限协作者）、reader（只读协作者），
// 以及唯一需要配置的仓库 secret（merge 令牌）。身份名是约定：内容评审 ai、
// 状态评审/合并 merge。
package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/repoinstall"
	"github.com/Cosmic-Developers-Union/assistant/internal/setup"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
	"github.com/Cosmic-Developers-Union/assistant/skills"
)

const (
	writerUser     = "e2ewriter"
	writerPassword = "E2e-Writer-Pass1"
	readerUser     = "e2ereader"
	readerPassword = "E2e-Reader-Pass1"
)

type pullState struct {
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	RequestedReviewers []struct {
		Login string `json:"login"`
	} `json:"requested_reviewers"`
	Merged bool   `json:"merged"`
	State  string `json:"state"`
}

func (p pullState) hasLabel(name string) bool {
	for _, label := range p.Labels {
		if label.Name == name {
			return true
		}
	}
	return false
}

func (p pullState) hasRequestedReviewer(login string) bool {
	for _, reviewer := range p.RequestedReviewers {
		if reviewer.Login == login {
			return true
		}
	}
	return false
}

func TestInstalledWorkflowRunsOnGiteaRunner(t *testing.T) {
	env := environment(t)
	if env.Image == "" {
		t.Skip("缺少 ASSISTANT_E2E_IMAGE（先运行 test/gitea/up.sh）")
	}
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
	repositoryName := fmt.Sprintf("e2e-flow-%d", time.Now().Unix())
	fullName := adminLogin + "/" + repositoryName

	// 1) 实例初始化：机器人账号、分支保护、标签（与生产 setup 同口径）
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
	result, err := setup.Run(ctx, options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	instance := result.Instance
	// 机器人令牌只在凭据库结果里：review / merge 按 purpose 各一条。
	reviewCredential := credentialFor(t, result, credentials.PurposeReview)
	mergeCredential := credentialFor(t, result, credentials.PurposeMerge)
	// 2) Actions 配置：setup.Run 已分发 MERGE_TOKEN，这里补一次幂等双写，
	//    与 setup_test 同口径校验 secret 存在
	actionsAdmin, err := setup.NewAdmin(ctx, setup.Options{Host: host, AdminToken: adminToken})
	if err != nil {
		t.Fatalf("NewAdmin() error = %v", err)
	}
	if err := setup.SyncMergeSecrets(ctx, actionsAdmin, instance.Merger.Name, mergeCredential.Token, false, t.Logf); err != nil {
		t.Fatalf("SyncMergeSecrets() error = %v", err)
	}

	// 3) 权限模型：owner=admin，另加 write / read 两个协作者
	createUser(t, host, adminToken, writerUser, writerPassword)
	createUser(t, host, adminToken, readerUser, readerPassword)
	setCollaborator(t, host, adminToken, adminLogin, repositoryName, writerUser, "write")
	setCollaborator(t, host, adminToken, adminLogin, repositoryName, readerUser, "read")
	writerToken := createUserToken(t, host, writerUser, writerPassword)
	readerToken := createUserToken(t, host, readerUser, readerPassword)

	// 4) 用 install 的模板渲染生成仓库资产，经 PR 进入受保护的 main：直接推送
	//    被分支保护拒绝，必须 2 票批准（ai + merge）后由 merge 合并（workflow
	//    只有存在于默认分支才会被事件触发）
	reviewerClient, err := status.NewClient(host, reviewCredential.Token)
	if err != nil {
		t.Fatal(err)
	}
	mergerClient, err := status.NewClient(host, mergeCredential.Token)
	if err != nil {
		t.Fatal(err)
	}
	repository := status.Repository{Owner: adminLogin, Name: repositoryName}
	installAssetsToMain(t, host, adminToken, adminLogin, repositoryName, env.Image,
		adminClient, reviewerClient, mergerClient, repository)

	// 5) writer 建 Issue → sync job 打 status/triage（无直接调用 Manager 的证据）
	statusCode, body := apiRequest(t, http.MethodPost, repoAPI(host, fullName)+"/issues", writerToken,
		map[string]any{"title": "e2e workflow triage"})
	if statusCode != http.StatusCreated {
		t.Fatalf("writer CreateIssue: HTTP %d: %s", statusCode, body)
	}
	var createdIssue struct {
		Number int64 `json:"number"`
	}
	if err := json.Unmarshal(body, &createdIssue); err != nil {
		t.Fatal(err)
	}
	waitForIssueLabel(t, host, adminToken, fullName, createdIssue.Number, "status/triage", 4*time.Minute)

	// 6) writer 推送分支并开 PR；reader 只读，推送必须被拒
	if statusCode, body := apiRequest(t, http.MethodPost, repoAPI(host, fullName)+"/contents/feature.txt", writerToken,
		map[string]any{
			"content":    base64.StdEncoding.EncodeToString([]byte("change\n")),
			"message":    "e2e: feature",
			"branch":     "main",
			"new_branch": "e2e-feature",
		}); statusCode != http.StatusCreated {
		t.Fatalf("writer push branch: HTTP %d: %s", statusCode, body)
	}
	if statusCode, readerBody := apiRequest(t, http.MethodPost, repoAPI(host, fullName)+"/contents/reader.txt", readerToken,
		map[string]any{
			"content":    base64.StdEncoding.EncodeToString([]byte("nope\n")),
			"message":    "reader must not push",
			"branch":     "main",
			"new_branch": "e2e-reader",
		}); statusCode == http.StatusCreated {
		t.Error("read-only collaborator pushed a branch; want rejection")
	} else {
		t.Logf("reader push rejected: HTTP %d %s", statusCode, readerBody)
	}
	statusCode, body = apiRequest(t, http.MethodPost, repoAPI(host, fullName)+"/pulls", writerToken,
		map[string]any{"title": "e2e workflow review", "head": "e2e-feature", "base": "main"})
	if statusCode != http.StatusCreated {
		t.Fatalf("writer CreatePullRequest: HTTP %d: %s", statusCode, body)
	}
	pullIndex, headSHA := parseCreatedPull(t, body)
	t.Logf("PR #%d head=%s", pullIndex, headSHA[:min(8, len(headSHA))])

	// 7) /review 评论 → sync job 登记官方评审请求并进 review 队列
	if statusCode, body := apiRequest(t, http.MethodPost,
		fmt.Sprintf("%s/issues/%d/comments", repoAPI(host, fullName), pullIndex), writerToken,
		map[string]any{"body": "/review"}); statusCode != http.StatusCreated {
		t.Fatalf("writer /review comment: HTTP %d: %s", statusCode, body)
	}
	waitForPull(t, host, adminToken, fullName, pullIndex, 4*time.Minute, "status/review 与 ai 请求",
		func(state pullState) bool {
			return state.hasLabel("status/review") && state.hasRequestedReviewer(repoinstall.ConventionReviewer)
		})

	// 8) reader 不能合并（只读 + 合并白名单只含 merge）
	readerClient, err := status.NewClient(host, readerToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := readerClient.MergePullRequest(ctx, repository, pullIndex); err == nil {
		t.Error("read-only collaborator merged a PR; want rejection")
	} else {
		t.Logf("reader merge rejected: %v", err)
	}

	// 9) ai 批准（内容评审结论）→ pull_request_review 事件触发 sync（标签）+
	//    automerge job（merge 身份：撤回遗留请求→会签→合并），全程事件驱动
	submitApproval(t, reviewerClient, repository, pullIndex, headSHA)
	waitForPull(t, host, adminToken, fullName, pullIndex, 4*time.Minute, "状态批准并合入",
		func(state pullState) bool {
			return (state.hasLabel("status/approved") || state.Merged || state.State == "closed") &&
				(state.Merged || state.State == "closed")
		})
}

// stubE2ESkills 模拟 skills CLI：把仓库内的 review 技能源复制到 claude 目录
// （e2e 不依赖 bun/网络；技能安装本身由 repoinstall 单测覆盖）。
func stubE2ESkills(request repoinstall.SkillsRequest) error {
	path := filepath.Join(request.Dir, repoinstall.ManagedSkillPath())
	if request.Remove {
		return os.Remove(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(skills.Review), 0o644)
}

// repoAPI 返回仓库 API 前缀。
func repoAPI(host, fullName string) string {
	return host + "/api/v1/repos/" + fullName
}

// apiRequest 发送带令牌的 JSON 请求并返回状态码与响应体。
func apiRequest(t *testing.T, method, url, token string, payload any) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "token "+token)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

// createUser 以管理员身份建测试账号（已存在时忽略）。
func createUser(t *testing.T, host, adminToken, name, password string) {
	t.Helper()
	statusCode, body := apiRequest(t, http.MethodPost, host+"/api/v1/admin/users", adminToken, map[string]any{
		"username":             name,
		"password":             password,
		"email":                name + "@assistant.local",
		"must_change_password": false,
	})
	if statusCode != http.StatusCreated && statusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create user %s: HTTP %d: %s", name, statusCode, body)
	}
}

// createUserToken 用 Basic Auth 为测试账号生成访问令牌。
func createUserToken(t *testing.T, host, name, password string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, host+"/api/v1/users/"+name+"/tokens",
		bytes.NewReader([]byte(`{"name":"e2e","scopes":["all"]}`)))
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(name, password)
	request.Header.Set("Content-Type", "application/json")
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
		t.Fatalf("create token %s: HTTP %d: %s", name, response.StatusCode, body)
	}
	var payload struct {
		Sha1 string `json:"sha1"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Sha1 == "" {
		t.Fatalf("parse token %s: %v (%s)", name, err, body)
	}
	return payload.Sha1
}

// setCollaborator 设置协作者权限（admin/write/read）。
func setCollaborator(t *testing.T, host, adminToken, owner, repo, user, permission string) {
	t.Helper()
	statusCode, body := apiRequest(t, http.MethodPut,
		fmt.Sprintf("%s/api/v1/repos/%s/%s/collaborators/%s", host, owner, repo, user), adminToken,
		map[string]any{"permission": permission})
	if statusCode/100 != 2 {
		t.Fatalf("set collaborator %s=%s: HTTP %d: %s", user, permission, statusCode, body)
	}
}

// submitApproval 提交批准并在被迟到的 push 事件作废时重试：Gitea 的
// dismiss_stale_approvals 由异步 push hook 触发，审核紧跟推送时可能踩到竞态。
func submitApproval(t *testing.T, client *status.Client, repository status.Repository, index int64, sha string) {
	t.Helper()
	ctx := context.Background()
	for attempt := 0; attempt < 5; attempt++ {
		if err := client.CreatePullReview(ctx, repository, index, status.ReviewInput{
			State:    status.ReviewStateApproved,
			Body:     "e2e approve",
			CommitID: sha,
		}); err != nil {
			t.Fatalf("CreatePullReview error = %v", err)
		}
		reviews, err := client.ListPullReviews(ctx, repository, index)
		if err != nil {
			t.Fatalf("ListPullReviews() error = %v", err)
		}
		alive := false
		for _, review := range reviews {
			if review.State == status.ReviewStateApproved && review.CommitID == sha && !review.Dismissed {
				alive = true
			}
		}
		if alive {
			return
		}
		t.Logf("PR #%d 批准被迟到的 push 事件作废，重试（%d）", index, attempt+1)
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("PR #%d 批准反复被 push 事件作废", index)
}

// installAssetsToMain 用 install 的模板渲染资产生成到临时目录，经独立分支开
// PR，以 ai + merge 两票批准后由 merge 合并进受保护的 main（workflow 必须存在
// 于默认分支；直接推送 main 会被分支保护拒绝）。
func installAssetsToMain(
	t *testing.T,
	host, adminToken, owner, repo, image string,
	adminClient, reviewerClient, mergerClient *status.Client,
	repository status.Repository,
) {
	t.Helper()
	ctx := context.Background()
	fullName := owner + "/" + repo
	dir := t.TempDir()
	installOptions := repoinstall.Options{
		Dir:             dir,
		Tools:           []string{"claude"},
		CodexConfigPath: filepath.Join(dir, "codex.toml"),
		Image:           image,
		RunSkills:       stubE2ESkills,
		Log:             t.Logf,
	}
	if err := repoinstall.Install(ctx, installOptions); err != nil {
		t.Fatalf("repoinstall.Install() error = %v", err)
	}
	// install 后 doctor 必须全绿（对照当前模板/镜像）
	findings, err := repoinstall.Doctor(installOptions)
	if err != nil {
		t.Fatalf("repoinstall.Doctor() error = %v", err)
	}
	for _, finding := range findings {
		if !finding.OK() {
			t.Errorf("doctor 应报告配置正常: %+v", finding)
		}
	}
	const branch = "e2e-install"
	assets := append([]string{repoinstall.ManagedSkillPath(), repoinstall.ManagedAgentPath(), repoinstall.ManagedClaudePath()}, repoinstall.ManagedWorkflowPaths()...)
	for index, relative := range assets {
		content, err := os.ReadFile(filepath.Join(dir, relative))
		if err != nil {
			t.Fatal(err)
		}
		payload := map[string]any{
			"content": base64.StdEncoding.EncodeToString(content),
			"message": "e2e: assistant install",
			"branch":  "main",
		}
		if index == 0 {
			payload["new_branch"] = branch
		} else {
			payload["branch"] = branch
		}
		if statusCode, body := apiRequest(t, http.MethodPost,
			repoAPI(host, fullName)+"/contents/"+relative, adminToken, payload); statusCode != http.StatusCreated {
			t.Fatalf("put %s: HTTP %d: %s", relative, statusCode, body)
		}
	}
	// contents API 每个文件一个提交，push hook 是异步处理的：等推送事件落定再
	// 开 PR，否则迟到的 push 事件会按 dismiss_stale_approvals 作废刚提交的批准
	time.Sleep(2 * time.Second)
	statusCode, body := apiRequest(t, http.MethodPost, repoAPI(host, fullName)+"/pulls", adminToken,
		map[string]any{"title": "e2e: install assistant assets", "head": branch, "base": "main"})
	if statusCode != http.StatusCreated {
		t.Fatalf("bootstrap CreatePullRequest: HTTP %d: %s", statusCode, body)
	}
	index, headSHA := parseCreatedPull(t, body)
	for _, client := range []*status.Client{reviewerClient, mergerClient} {
		submitApproval(t, client, repository, index, headSHA)
	}
	waitMergeable(t, adminClient, repository, index)
	if err := mergerClient.MergePullRequest(ctx, repository, index); err != nil {
		t.Fatalf("bootstrap MergePullRequest error = %v", err)
	}
	waitClosed(t, adminClient, repository, index)
}

// parseCreatedPull 从创建 PR 的响应中取编号与 head sha。
func parseCreatedPull(t *testing.T, body []byte) (int64, string) {
	t.Helper()
	var created struct {
		Number int64 `json:"number"`
		Head   struct {
			Sha string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("parse created pull: %v (%s)", err, body)
	}
	if created.Number == 0 || created.Head.Sha == "" {
		t.Fatalf("unexpected created pull payload: %s", body)
	}
	return created.Number, created.Head.Sha
}

// waitMergeable 等 Gitea 异步计算完 mergeability（否则合并报「请稍后再试」）。
func waitMergeable(t *testing.T, client *status.Client, repository status.Repository, index int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		pull, err := client.GetPullRequest(context.Background(), repository, index)
		if err != nil {
			t.Fatalf("GetPullRequest() error = %v", err)
		}
		if pull.Mergeable {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("PR #%d 长时间不可合并：%+v", index, pull)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// waitClosed 轮询等待 PR 关闭（合并回写可能稍滞后）。
func waitClosed(t *testing.T, client *status.Client, repository status.Repository, index int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		pull, err := client.GetPullRequest(context.Background(), repository, index)
		if err != nil {
			t.Fatalf("GetPullRequest() error = %v", err)
		}
		if !pull.Open {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("PR #%d 合并后仍为 open：%+v", index, pull)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForPull 轮询 PR 直到 predicate 成立；超时输出最近 workflow run 辅助定位。
func waitForPull(
	t *testing.T,
	host, token, fullName string,
	index int64,
	timeout time.Duration,
	description string,
	predicate func(pullState) bool,
) pullState {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last pullState
	var lastBody []byte
	for {
		statusCode, body := apiRequest(t, http.MethodGet,
			fmt.Sprintf("%s/pulls/%d", repoAPI(host, fullName), index), token, nil)
		if statusCode == http.StatusOK {
			if err := json.Unmarshal(body, &last); err != nil {
				t.Fatalf("parse pull: %v (%s)", err, body)
			}
			if predicate(last) {
				return last
			}
			lastBody = body
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 超时；PR 状态: %s\n最近 runs: %s", description, lastBody, recentRuns(host, token, fullName))
		}
		time.Sleep(2 * time.Second)
	}
}

// waitForIssueLabel 轮询 Issue 标签（sync job 的产物）。
func waitForIssueLabel(t *testing.T, host, token, fullName string, index int64, label string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastBody []byte
	for {
		statusCode, body := apiRequest(t, http.MethodGet,
			fmt.Sprintf("%s/issues/%d", repoAPI(host, fullName), index), token, nil)
		if statusCode == http.StatusOK {
			var payload struct {
				Labels []struct {
					Name string `json:"name"`
				} `json:"labels"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("parse issue: %v (%s)", err, body)
			}
			for _, item := range payload.Labels {
				if item.Name == label {
					return
				}
			}
			lastBody = body
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 Issue 标签 %s 超时；Issue 状态: %s\n最近 runs: %s",
				label, lastBody, recentRuns(host, token, fullName))
		}
		time.Sleep(2 * time.Second)
	}
}

// recentRuns 返回最近的 workflow run 摘要（诊断用；API 不可用时返回错误文本）。
func recentRuns(host, token, fullName string) string {
	request, err := http.NewRequest(http.MethodGet, repoAPI(host, fullName)+"/actions/runs?limit=5", nil)
	if err != nil {
		return err.Error()
	}
	request.Header.Set("Authorization", "token "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err.Error()
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err.Error()
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Sprintf("actions/runs HTTP %d: %s", response.StatusCode, body)
	}
	return string(body)
}
