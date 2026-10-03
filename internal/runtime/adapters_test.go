package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/claude"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

type platformFake struct {
	pulls  []status.PullRequest
	issues []status.Issue
	err    error
	listed bool
}

func (p *platformFake) ListRepositories(context.Context) ([]status.Repository, error) {
	p.listed = true
	return []status.Repository{{Owner: "acme", Name: "repo"}}, p.err
}
func (p *platformFake) ListOpenPullRequests(context.Context, status.Repository) ([]status.PullRequest, error) {
	return p.pulls, p.err
}
func (p *platformFake) ListTriageIssues(context.Context, status.Repository) ([]status.Issue, error) {
	return p.issues, p.err
}
func (p *platformFake) ListMentionedPullRequests(context.Context, string) ([]status.PullRequest, error) {
	return nil, p.err
}
func (p *platformFake) ListPullReviews(context.Context, status.Repository, int64) ([]status.Review, error) {
	return nil, p.err
}
func (p *platformFake) ListIssueCommentsSince(context.Context, status.Repository, int64, time.Time) ([]status.Comment, error) {
	return nil, p.err
}
func TestSourceUsesLabelsAndPlatformState(t *testing.T) {
	api := &platformFake{pulls: []status.PullRequest{
		{Index: 1, Open: true, HeadSHA: "h1", BaseRef: "main", Labels: []status.Label{{Name: status.LabelReview}}},
		{Index: 2, Open: true, RequestedReviewers: []string{"ai"}},
		{Index: 3, Open: true, Draft: true, Labels: []status.Label{{Name: status.LabelReview}}},
		{Index: 4, Open: true, Title: "WIP: test", Labels: []status.Label{{Name: status.LabelReview}}},
		{Index: 5, Open: false, Labels: []status.Label{{Name: status.LabelReview}}},
	}, issues: []status.Issue{{Index: 9, Title: "triage"}, {Index: 10, IsPull: true}}}
	source := GiteaSource{API: api, Host: "https://site", ReviewBot: "review", TriageBot: "triage"}
	events, err := source.Poll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !api.listed || len(events) != 2 || events[0].Head != "h1" || events[1].Kind != "triage" {
		t.Fatalf("错误待办 %+v", events)
	}
	events, err = source.Poll(t.Context())
	if err != nil || !api.listed || len(events) != 2 {
		t.Fatal(err)
	}
	api.err = errors.New("平台离线")
	if _, err := source.Poll(t.Context()); err == nil {
		t.Fatal("平台错误被吞掉")
	}
	if _, err := source.Poll(t.Context()); err == nil {
		t.Fatal("仓库查询错误被吞掉")
	}
	for _, title := range []string{"WIP", "[WIP] test", "wip test"} {
		if !isWIP(title) {
			t.Fatal(title)
		}
	}
}

type processFunc func(context.Context, claude.Spec, func([]byte)) error

func (f processFunc) Run(ctx context.Context, spec claude.Spec, onLine func([]byte)) error {
	return f(ctx, spec, onLine)
}
func TestClaudeRunResumeAndToolScope(t *testing.T) {
	root := t.TempDir()
	ev := testEvent()
	session := LocalSession{Root: root}
	dir, _ := session.Dir(t.Context(), ev)
	id := session.ID(ev)
	for _, resume := range []bool{false, true} {
		if resume {
			writeTranscript(t, dir, id)
		}
		runner := ClaudeRunner{Process: processFunc(func(ctx context.Context, spec claude.Spec, feed func([]byte)) error {
			flag := "--session-id"
			if resume {
				flag = "--resume"
			}
			joined := strings.Join(spec.Args, " ")
			if !strings.Contains(joined, flag+" "+id) || !strings.Contains(joined, "--strict-mcp-config") || !strings.Contains(joined, "review pr") {
				t.Fatal(joined)
			}
			if !strings.Contains(strings.Join(spec.Env, " "), "CLAUDE_CONFIG_DIR="+dir) {
				t.Fatal(spec.Env)
			}
			var path string
			for i, arg := range spec.Args {
				if arg == "--mcp-config" {
					path = spec.Args[i+1]
				}
			}
			data, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(data), "private-token") {
				t.Fatal("MCP 未注入", err)
			}
			feed([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"done"}`))
			return nil
		})}
		outcome, err := runner.Run(t.Context(), AgentSpec{SessionDir: dir, Servers: map[string]MCP{"gitea": {Cmd: "assistant", Args: []string{"mcp", "gitea"}, Env: map[string]string{"GITEA_ACCESS_TOKEN": "private-token"}}}}, ev, root, id)
		if err != nil || outcome.Resumed != resume || outcome.Result != "done" {
			t.Fatalf("%+v %v", outcome, err)
		}
		files, _ := filepath.Glob(filepath.Join(dir, ".mcp-*"))
		if len(files) != 0 {
			t.Fatal("临时令牌配置残留")
		}
	}
	runner := ClaudeRunner{Process: processFunc(func(context.Context, claude.Spec, func([]byte)) error { return nil })}
	if _, err := runner.Run(t.Context(), AgentSpec{SessionDir: dir}, ev, root, id); err == nil {
		t.Fatal("无结果帧被当作成功")
	}
	sentinel := errors.New("进程退出")
	runner.Process = processFunc(func(context.Context, claude.Spec, func([]byte)) error { return sentinel })
	if _, err := runner.Run(t.Context(), AgentSpec{SessionDir: dir}, ev, root, id); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if _, err := runner.Run(t.Context(), AgentSpec{SessionDir: filepath.Join(root, "missing")}, ev, root, id); err == nil {
		t.Fatal("坏记忆目录被接受")
	}
}
func TestWorkspaceFailureAndStableDirectory(t *testing.T) {
	root := t.TempDir()
	w := DirectoryWorkspace{Root: root}
	ev := Event{Host: "telegram", User: "../../user"}
	dir, cleanup, err := w.Prepare(t.Context(), ev)
	if err != nil || cleanup == nil {
		t.Fatal(err)
	}
	again, _, _ := w.Prepare(t.Context(), ev)
	if dir != again {
		t.Fatal("聊天工作区不稳定")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := w.Prepare(ctx, ev); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	bad := &WorktreeWorkspace{Root: root, Connect: Connect{URL: "https://site"}, Spec: WorkspaceSpec{Repo: "../escape"}}
	if _, _, err := bad.Prepare(t.Context(), testEvent()); err == nil {
		t.Fatal("越界仓库被接受")
	}
	bad.Spec.Repo = "acme/repo"
	if _, _, err := bad.Prepare(ctx, testEvent()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := git(ctx, root, nil, "status"); err == nil {
		t.Fatal("Git 取消未传播")
	}
	if err := pinStandard(root, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerFailureFramesAndChatPrompt(t *testing.T) {
	root := t.TempDir()
	spec := AgentSpec{SessionDir: root, Bin: "claude-override", Prompt: "附加", Servers: map[string]MCP{"gitea": {Env: map[string]string{"TOKEN": "private-token"}}}}
	var progress strings.Builder
	runner := ClaudeRunner{Debug: true, Log: func(format string, args ...any) { progress.WriteString(fmt.Sprintf(format, args...)) }, Process: processFunc(func(ctx context.Context, invocation claude.Spec, feed func([]byte)) error {
		joined := strings.Join(invocation.Args, " ")
		if invocation.Bin != "claude-override" || !strings.Contains(joined, "hello") || !strings.Contains(joined, "附加") {
			t.Fatal(joined)
		}
		feed([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"private-token"}]}}`))
		feed([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true}`))
		return nil
	})}
	if _, err := runner.Run(t.Context(), spec, Event{Bot: "chat", Kind: "chat", Text: "hello"}, root, "id"); err == nil {
		t.Fatal("失败结果帧被当作成功")
	}
	if strings.Contains(progress.String(), "private-token") {
		t.Fatal("工具日志泄露密钥")
	}
	runner.Process = processFunc(func(ctx context.Context, spec claude.Spec, feed func([]byte)) error {
		if !strings.Contains(strings.Join(spec.Args, " "), "triage issue") {
			t.Fatal(spec.Args)
		}
		feed([]byte(`{"type":"result","subtype":"success","is_error":false}`))
		return nil
	})
	if _, err := runner.Run(t.Context(), AgentSpec{SessionDir: root}, Event{Kind: "triage"}, root, "other"); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerProjectRulesAndProcessErrorRedaction(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".assistant"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".assistant", "review.md"), []byte("项目规定"), 0600); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("原始故障")
	runner := ClaudeRunner{Process: processFunc(func(ctx context.Context, spec claude.Spec, feed func([]byte)) error {
		if !strings.Contains(strings.Join(spec.Args, " "), "项目规定") {
			t.Fatal(spec.Args)
		}
		return fmt.Errorf("private-token: %w", cause)
	})}
	_, err := runner.Run(t.Context(), AgentSpec{SessionDir: root, Servers: map[string]MCP{"tool": {Env: map[string]string{"API_TOKEN": "private-token"}}}}, Event{Kind: "chat"}, root, "id")
	if !errors.Is(err, cause) || strings.Contains(err.Error(), "private-token") {
		t.Fatal(err)
	}
}
