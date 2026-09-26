package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"

	"github.com/spf13/cobra"
)

// initStub 是 init 命令族需要的假 Gitea：协作者列表（判定调用者是否管理员）、
// 加协作者、标签、分支保护。按「方法 + 路径」记录请求，便于断言命令真的发了
// 对应调用，而不是只打印了一行日志。
type initStub struct {
	mu       sync.Mutex
	requests []string
	// login 是 GET /api/v1/user 返回的账号（即调用者自己）
	login string
	// permission 是调用者在目标仓库的权限（admin/write）；非 admin 时
	// newRepoClientForTarget 必须拒绝。
	permission string
	// labels 是 GET …/labels 的内容；创建的标签追加进来自持。
	labels []map[string]any
	nextID int
	// failWrites 让写操作返回失败（注入权限不足/服务端错误）
	failWrites bool
}

func newInitStub(t *testing.T, login string, admin bool) *initStub {
	t.Helper()
	permission := "write"
	if admin {
		permission = "admin"
	}
	return &initStub{login: login, permission: permission, nextID: 100}
}

func (s *initStub) record(request *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request.Method+" "+request.URL.Path)
}

func (s *initStub) called(method, pathSuffix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.requests {
		if strings.HasPrefix(entry, method+" ") && strings.HasSuffix(entry, pathSuffix) {
			return true
		}
	}
	return false
}

func (s *initStub) addLabel(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Name      string `json:"name"`
		Exclusive bool   `json:"exclusive"`
	}
	_ = json.NewDecoder(request.Body).Decode(&payload)
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	label := map[string]any{"id": id, "name": payload.Name, "exclusive": payload.Exclusive}
	s.labels = append(s.labels, label)
	s.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(writer).Encode(label)
}

func (s *initStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(writer http.ResponseWriter, value any) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(value)
	}
	mux.HandleFunc("/api/v1/version", func(writer http.ResponseWriter, request *http.Request) {
		s.record(request)
		writeJSON(writer, map[string]string{"version": "1.27.0"})
	})
	mux.HandleFunc("/api/v1/user", func(writer http.ResponseWriter, request *http.Request) {
		s.record(request)
		writeJSON(writer, map[string]any{"login": s.login, "is_admin": false})
	})
	mux.HandleFunc("/api/v1/repos/acme/repo/collaborators", func(writer http.ResponseWriter, request *http.Request) {
		s.record(request)
		// SDK 的 ListCollaborators：只列成员，权限由 …/permission 单查
		writeJSON(writer, []map[string]any{{"login": s.login, "id": 1}})
	})
	mux.HandleFunc("/api/v1/repos/acme/repo/collaborators/", func(writer http.ResponseWriter, request *http.Request) {
		s.record(request)
		switch {
		case request.Method == http.MethodPut:
			if s.failWrites {
				writer.WriteHeader(http.StatusForbidden)
				return
			}
			writer.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(request.URL.Path, "/permission"):
			writeJSON(writer, map[string]any{"permission": s.permission})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/api/v1/repos/acme/repo/labels", func(writer http.ResponseWriter, request *http.Request) {
		s.record(request)
		if request.Method == http.MethodPost {
			s.addLabel(writer, request)
			return
		}
		s.mu.Lock()
		labels := append([]map[string]any(nil), s.labels...)
		s.mu.Unlock()
		writeJSON(writer, labels)
	})
	// 标签的互斥设置与删除走 /labels/{id}
	mux.HandleFunc("/api/v1/repos/acme/repo/labels/", func(writer http.ResponseWriter, request *http.Request) {
		s.record(request)
		writer.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/api/v1/repos/acme/repo/branch_protections", func(writer http.ResponseWriter, request *http.Request) {
		s.record(request)
		writeJSON(writer, []any{})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// initFixture 铺好 init 命令族需要的现场：带 gitea remote 的检出、指向假服务的
// config.json、以及 purpose=mcp 的开发者令牌。返回可执行命令与桩。
func initFixture(t *testing.T, admin bool) (*cobra.Command, *initStub, string) {
	t.Helper()
	stub := newInitStub(t, "dev", admin)
	server := stub.server(t)
	// remote 里的端口要指向假服务，否则探测不会命中该平台
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "remote", "add", "gitea", server.URL+"/acme/repo.git")
	t.Chdir(dir)

	configPath := filepath.Join(t.TempDir(), "config.json")
	file := &instances.File{Channels: []instances.Channel{{
		Type: instances.ChannelGitea,
		Host: server.URL,
		Repos: []instances.Repo{
			{Name: "acme/repo"},
		},
	}}}
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}
	store := &credentials.File{}
	store.SetCredential(credentials.Credential{
		Host: server.URL, User: "dev", Purpose: credentials.PurposeMCP, Token: "dev-token",
	})
	credentialsPath := filepath.Join(t.TempDir(), "credentials.json")
	if err := credentials.Save(credentialsPath, store); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", credentialsPath)
	t.Setenv("ASSISTANT_CONFIG", configPath)

	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.SetOut(&initOutputBuffer{})
	command.Flags().String("repo", "acme/repo", "")
	if err := command.Flags().Set("repo", "acme/repo"); err != nil {
		t.Fatal(err)
	}
	return command, stub, configPath
}

// newSyncBuffer 是并发安全的输出缓冲（命令在 goroutine 里也可能写）。
type initOutputBuffer struct {
	mu   sync.Mutex
	data strings.Builder
}

func (b *initOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}

func (b *initOutputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

// init 命令族共用 newRepoClientForTarget：它必须拒绝非管理员调用者——init 是
// dev 专用，权限不足时若放行，后续每个写操作都会以难懂的服务端错误失败。
func TestNewRepoClientForTargetRejectsNonAdmin(t *testing.T) {
	command, _, configPath := initFixture(t, false)
	_, _, err := newRepoClientForTarget(context.Background(), command, configPath, func(string, ...any) {})
	if err == nil {
		t.Fatal("error = nil, want 非管理员被拒")
	}
	if !strings.Contains(err.Error(), "管理员") {
		t.Errorf("error = %v, want 指出需要仓库管理员权限", err)
	}
	if !strings.Contains(err.Error(), "assistant setup") {
		t.Errorf("error = %v, want 指出站点级配置该用 setup", err)
	}
}

// 缺 purpose=mcp 的令牌时要给出补凭据的具体指引，而不是一句「未授权」。
func TestNewRepoClientForTargetRequiresMCPToken(t *testing.T) {
	command, _, configPath := initFixture(t, true)
	// 清空凭据库
	empty := &credentials.File{}
	empty.SetIdentity(credentials.Identity{Host: serverHostFrom(t), User: "dev"})
	path := filepath.Join(t.TempDir(), "empty-credentials.json")
	if err := credentials.Save(path, empty); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", path)

	_, _, err := newRepoClientForTarget(context.Background(), command, configPath, func(string, ...any) {})
	if err == nil {
		t.Fatal("error = nil, want 缺令牌报错")
	}
	if !strings.Contains(err.Error(), "purpose=mcp") {
		t.Errorf("error = %v, want 点名缺失的用途", err)
	}
}

func serverHostFrom(t *testing.T) string {
	t.Helper()
	path := os.Getenv("ASSISTANT_CONFIG")
	file, err := instances.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return file.Channels[0].Host
}

// runInitReviewer/runInitMerge 在 dry-run 下只打印将要做的操作，**不发任何
// 写请求**——dry-run 是运维在真实仓库上预演的保障，漏一个写操作就会造成
// 不可逆的现场改动。
func TestRunInitDryRunDoesNotWrite(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*cobra.Command, string, *initOptions) error
		want string
	}{
		{name: "reviewer", run: runInitReviewer, want: "dry-run：将把 ai 加为 acme/repo 的协作者（write）"},
		{name: "merge", run: runInitMerge, want: "dry-run：将把 merge 加为 acme/repo 的协作者（admin）"},
		{name: "labels", run: runInitLabels, want: "dry-run：将收敛 acme/repo 的标签体系"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command, stub, configPath := initFixture(t, true)
			options := &initOptions{
				DryRun:   true,
				Reviewer: instances.DefaultReviewerName,
				Merger:   instances.DefaultMergerName,
			}
			if err := test.run(command, configPath, options); err != nil {
				t.Fatalf("%s() error = %v", test.name, err)
			}
			output := command.OutOrStdout().(*initOutputBuffer).String()
			if !strings.Contains(output, test.want) {
				t.Errorf("输出缺少 %q：\n%s", test.want, output)
			}
			// 不得发出任何写请求（PUT/POST/PATCH/DELETE）
			for _, method := range []string{"PUT ", "POST ", "PATCH ", "DELETE "} {
				if stub.called(method, "") {
					t.Errorf("dry-run 不应发出 %s 请求：%+v", method, stub.requests)
				}
			}
		})
	}
}

// runInitReviewer 真正执行时要把 reviewer 加为 write 协作者，并提示下一步
// （把 required approvals 升为 2 需重跑 branch-protection）。
func TestRunInitReviewerAddsCollaborator(t *testing.T) {
	command, stub, configPath := initFixture(t, true)
	options := &initOptions{Reviewer: "reviewer-bot"}
	if err := runInitReviewer(command, configPath, options); err != nil {
		t.Fatalf("runInitReviewer() error = %v", err)
	}
	if !stub.called("PUT", "/collaborators/reviewer-bot") {
		t.Errorf("未把 reviewer 加为协作者：%+v", stub.requests)
	}
	output := command.OutOrStdout().(*initOutputBuffer).String()
	if !strings.Contains(output, "已把 reviewer-bot 加为 acme/repo 的协作者（write）") {
		t.Errorf("输出未确认结果：\n%s", output)
	}
	if !strings.Contains(output, "assistant init branch-protection") {
		t.Errorf("输出未给出下一步：\n%s", output)
	}
}

// runInitMerge 加的是 admin 权限并说明 MERGE_TOKEN 由 setup 分发——写错权限
// 会让 merge 读不了分支保护、无法会签合并。
func TestRunInitMergeAddsAdminCollaborator(t *testing.T) {
	command, stub, configPath := initFixture(t, true)
	options := &initOptions{Merger: "merger-bot"}
	if err := runInitMerge(command, configPath, options); err != nil {
		t.Fatalf("runInitMerge() error = %v", err)
	}
	if !stub.called("PUT", "/collaborators/merger-bot") {
		t.Errorf("未把 merge 加为协作者：%+v", stub.requests)
	}
	output := command.OutOrStdout().(*initOutputBuffer).String()
	if !strings.Contains(output, "（admin）") {
		t.Errorf("输出未确认 admin 权限：\n%s", output)
	}
	if !strings.Contains(output, "MERGE_TOKEN") {
		t.Errorf("输出未说明 secret 由 setup 分发：\n%s", output)
	}
}

// runInitLabels 走与 action label-sync / setup 同一口径的收敛（补齐缺失、scoped
// 互斥、删除体系外），收敛后标签应完全合规。
func TestRunInitLabelsReconciles(t *testing.T) {
	command, stub, configPath := initFixture(t, true)
	options := &initOptions{}
	if err := runInitLabels(command, configPath, options); err != nil {
		t.Fatalf("runInitLabels() error = %v", err)
	}
	if !stub.called("GET", "/labels") {
		t.Errorf("未读取现有标签：%+v", stub.requests)
	}
	// 空标签库 ⇒ 规范体系全部要创建
	if !stub.called("POST", "/labels") {
		t.Errorf("未补齐缺失标签：%+v", stub.requests)
	}
	output := command.OutOrStdout().(*initOutputBuffer).String()
	if !strings.Contains(output, "标签体系已收敛（acme/repo）") {
		t.Errorf("输出未确认收敛：\n%s", output)
	}
}

// 写操作失败必须带上语境上抛（不能静默成功）：加协作者被拒是 init 最常见的
// 失败，操作者要能一眼看出是哪一步、哪个账号。
func TestRunInitSurfacesWriteFailures(t *testing.T) {
	command, stub, configPath := initFixture(t, true)
	stub.mu.Lock()
	stub.failWrites = true
	stub.mu.Unlock()

	err := runInitReviewer(command, configPath, &initOptions{Reviewer: "reviewer-bot"})
	if err == nil {
		t.Fatal("error = nil, want 加协作者失败上抛")
	}
}

// commandLogger 的输出形态：带 [<子命令>] 前缀写到命令的 stdout——init 的每一步
// 都要能看出是哪个子命令在做，混合输出里否则无从对应。
func TestCommandLoggerPrefixesStdout(t *testing.T) {
	command := &cobra.Command{}
	command.SetOut(&initOutputBuffer{})
	logf := commandLogger(command, "init ai")
	logf("已把 %s 加为协作者", "bob")

	got := command.OutOrStdout().(*initOutputBuffer).String()
	if want := "[init ai] 已把 bob 加为协作者\n"; got != want {
		t.Errorf("输出 = %q, want %q", got, want)
	}
}
