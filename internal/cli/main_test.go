package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
)

func TestRootCommandShowsHelpWithoutArguments(t *testing.T) {
	var stdout bytes.Buffer
	command := newRootCommand(
		&stdout,
		&bytes.Buffer{},
		func(context.Context, io.Writer, io.Writer, commandOptions) error {
			t.Fatal("sync runner was called")
			return nil
		},
		func(context.Context, io.Writer, io.Writer, commandOptions) error {
			t.Fatal("automerge runner was called")
			return nil
		},
	)
	command.SetArgs(nil)

	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "Available Commands:") ||
		// 不能用字面量 "action" 断言：root 的 Example 里就含
		// `assistant action label-sync`，cobra 会把 Example 一并打印进 help。
		!strings.Contains(output, "执行仓库自动化动作") ||
		!strings.Contains(output, "setup") ||
		!strings.Contains(output, "install") ||
		!strings.Contains(output, "uninstall") ||
		!strings.Contains(output, "mcp") ||
		!strings.Contains(output, "doctor") ||
		!strings.Contains(output, "login") ||
		!strings.Contains(output, "init") ||
		!strings.Contains(output, "run") {
		t.Errorf("help output = %q", output)
	}
	// 只读旁路（check / list / review / triage）不再对用户暴露，按命令树而不是
	// 按文案断言：help 里仍有 review/triage 字样（来自 review skill 与 session
	// 相关说明），按文案断言会误判。
	topLevel := map[string]bool{}
	for _, child := range command.Commands() {
		topLevel[child.Name()] = true
	}
	for _, removed := range []string{"check", "list", "review", "triage"} {
		if topLevel[removed] {
			t.Errorf("命令 %s 已移除，不应再挂在命令树上", removed)
		}
	}
	// 上面只能证明 help 文案出现，这里直接确认 action 及其子命令挂在命令树上。
	if _, _, err := command.Find([]string{"action", "label-sync"}); err != nil {
		t.Errorf("Find(action label-sync) error = %v", err)
	}
}

// TestDeprecatedAutomationCommandsStillDispatch 保证旧的顶层 `assistant sync` /
// `assistant automerge` 仍转发到对应 runner：已 install 的目标仓库其 workflow
// 调用的还是旧命令，镜像更新后不能让它们变成 unknown command。
func TestDeprecatedAutomationCommandsStillDispatch(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
		want string
	}{
		{"sync", []string{"sync"}, "label-sync"},
		{"automerge", []string{"automerge"}, "automerge"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var called []string
			command := newRootCommand(
				&bytes.Buffer{},
				&bytes.Buffer{},
				func(context.Context, io.Writer, io.Writer, commandOptions) error {
					called = append(called, "label-sync")
					return nil
				},
				func(context.Context, io.Writer, io.Writer, commandOptions) error {
					called = append(called, "automerge")
					return nil
				},
			)
			command.SetArgs(testCase.args)

			if err := command.ExecuteContext(t.Context()); err != nil {
				t.Fatalf("ExecuteContext() error = %v", err)
			}
			if len(called) != 1 || called[0] != testCase.want {
				t.Errorf("called = %v, want [%s]", called, testCase.want)
			}
		})
	}
}

// TestDeprecatedAutomationCommandsHiddenFromHelp 确认旧入口不出现在帮助列表里，
// 但带着弃用提示保留可达（cobra 对 Deprecated 命令的约定）。
func TestDeprecatedAutomationCommandsHiddenFromHelp(t *testing.T) {
	var stdout bytes.Buffer
	runner := func(context.Context, io.Writer, io.Writer, commandOptions) error {
		t.Fatal("runner was called")
		return nil
	}
	command := newRootCommand(&stdout, &bytes.Buffer{}, runner, runner)
	command.SetArgs(nil)

	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if strings.Contains(stdout.String(), "已弃用：请改用") {
		t.Errorf("弃用的顶层入口不应出现在帮助列表里: %q", stdout.String())
	}
	for _, name := range []string{"sync", "automerge"} {
		legacy, _, err := command.Find([]string{name})
		if err != nil {
			t.Fatalf("Find(%q) error = %v", name, err)
		}
		if legacy.Deprecated == "" {
			t.Errorf("%s 应为带弃用提示的兼容入口", name)
		}
	}
}

func TestCompletionCommandGeneratesScript(t *testing.T) {
	var stdout bytes.Buffer
	runner := func(context.Context, io.Writer, io.Writer, commandOptions) error {
		t.Fatal("runner was called")
		return nil
	}
	command := newRootCommand(&stdout, &bytes.Buffer{}, runner, runner)
	command.SetArgs([]string{"completion", "bash"})
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "bash completion") {
		t.Errorf("completion output = %q", stdout.String())
	}
}

func TestUnknownCommandFails(t *testing.T) {
	runner := func(context.Context, io.Writer, io.Writer, commandOptions) error {
		t.Fatal("runner was called")
		return nil
	}
	command := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{}, runner, runner)
	command.SetArgs([]string{"nope"})
	if err := command.ExecuteContext(t.Context()); err == nil {
		t.Fatal("ExecuteContext() error = nil")
	}
}

func TestCommandsAcceptVerboseAndRepoFlags(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{"action label-sync", []string{"action", "label-sync"}},
		{"action automerge", []string{"action", "automerge"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var got commandOptions
			runner := func(_ context.Context, _, _ io.Writer, options commandOptions) error {
				got = options
				return nil
			}
			command := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{}, runner, runner)
			args := append([]string{"--repo", "acme/video"}, testCase.args...)
			command.SetArgs(append(args, "-v"))

			if err := command.ExecuteContext(t.Context()); err != nil {
				t.Fatalf("ExecuteContext() error = %v", err)
			}
			if got.Repository != "acme/video" || !got.Verbose {
				t.Errorf("options = %+v", got)
			}
		})
	}
}

func TestCommandsDispatchToMatchingRunner(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
		want string
	}{
		{"action label-sync", []string{"action", "label-sync"}, "label-sync"},
		{"action automerge", []string{"action", "automerge"}, "automerge"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var called []string
			command := newRootCommand(
				&bytes.Buffer{},
				&bytes.Buffer{},
				func(context.Context, io.Writer, io.Writer, commandOptions) error {
					called = append(called, "label-sync")
					return nil
				},
				func(context.Context, io.Writer, io.Writer, commandOptions) error {
					called = append(called, "automerge")
					return nil
				},
			)
			command.SetArgs(testCase.args)

			if err := command.ExecuteContext(t.Context()); err != nil {
				t.Fatalf("ExecuteContext() error = %v", err)
			}
			if len(called) != 1 || called[0] != testCase.want {
				t.Errorf("called = %v, want [%s]", called, testCase.want)
			}
		})
	}
}

func TestCommandsRejectInvalidRepository(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{"action label-sync", []string{"action", "label-sync"}},
		{"action automerge", []string{"action", "automerge"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runner := func(context.Context, io.Writer, io.Writer, commandOptions) error {
				t.Fatal("runner was called")
				return nil
			}
			command := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{}, runner, runner)
			command.SetArgs(append([]string{"--repo", "video"}, testCase.args...))
			if err := command.ExecuteContext(t.Context()); err == nil {
				t.Fatal("ExecuteContext() error = nil")
			}
		})
	}
}

func withReviewCredential(t *testing.T, host string) {
	t.Helper()
	path, err := credentials.Path()
	if err != nil {
		t.Fatal(err)
	}
	store, err := credentials.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store.SetCredential(credentials.Credential{
		Host: host, User: "ai", Purpose: credentials.PurposeReview,
		Token: "reviewer-token", TokenName: "assistant",
	})
	if err := credentials.Save(path, store); err != nil {
		t.Fatal(err)
	}
}
