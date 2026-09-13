package main

import (
	"assistant/internal/status"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRootCommandShowsHelpWithoutArguments(t *testing.T) {
	var stdout bytes.Buffer
	command := newRootCommand(
		&stdout,
		&bytes.Buffer{},
		func(context.Context, io.Writer, io.Writer, commandOptions) error {
			t.Fatal("check runner was called")
			return nil
		},
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
		!strings.Contains(output, "check") ||
		!strings.Contains(output, "sync") ||
		!strings.Contains(output, "automerge") ||
		!strings.Contains(output, "setup") ||
		!strings.Contains(output, "actions") ||
		!strings.Contains(output, "install") ||
		!strings.Contains(output, "uninstall") ||
		!strings.Contains(output, "mcp") ||
		!strings.Contains(output, "run") ||
		!strings.Contains(output, "list") ||
		!strings.Contains(output, "review") ||
		!strings.Contains(output, "triage") {
		t.Errorf("help output = %q", output)
	}
}

func TestCompletionCommandGeneratesScript(t *testing.T) {
	var stdout bytes.Buffer
	runner := func(context.Context, io.Writer, io.Writer, commandOptions) error {
		t.Fatal("runner was called")
		return nil
	}
	command := newRootCommand(&stdout, &bytes.Buffer{}, runner, runner, runner)
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
	command := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{}, runner, runner, runner)
	command.SetArgs([]string{"nope"})
	if err := command.ExecuteContext(t.Context()); err == nil {
		t.Fatal("ExecuteContext() error = nil")
	}
}

func TestCommandsAcceptVerboseAndRepoFlags(t *testing.T) {
	for _, subcommand := range []string{"check", "sync", "automerge"} {
		t.Run(subcommand, func(t *testing.T) {
			var got commandOptions
			runner := func(_ context.Context, _, _ io.Writer, options commandOptions) error {
				got = options
				return nil
			}
			command := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{}, runner, runner, runner)
			command.SetArgs([]string{"--repo", "acme/video", subcommand, "-v"})

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
	for _, subcommand := range []string{"check", "sync", "automerge"} {
		t.Run(subcommand, func(t *testing.T) {
			var called []string
			command := newRootCommand(
				&bytes.Buffer{},
				&bytes.Buffer{},
				func(context.Context, io.Writer, io.Writer, commandOptions) error {
					called = append(called, "check")
					return nil
				},
				func(context.Context, io.Writer, io.Writer, commandOptions) error {
					called = append(called, "sync")
					return nil
				},
				func(context.Context, io.Writer, io.Writer, commandOptions) error {
					called = append(called, "automerge")
					return nil
				},
			)
			command.SetArgs([]string{subcommand})

			if err := command.ExecuteContext(t.Context()); err != nil {
				t.Fatalf("ExecuteContext() error = %v", err)
			}
			if len(called) != 1 || called[0] != subcommand {
				t.Errorf("called = %v, want [%s]", called, subcommand)
			}
		})
	}
}

func TestCommandsRejectInvalidRepository(t *testing.T) {
	for _, subcommand := range []string{"check", "sync", "automerge"} {
		t.Run(subcommand, func(t *testing.T) {
			runner := func(context.Context, io.Writer, io.Writer, commandOptions) error {
				t.Fatal("runner was called")
				return nil
			}
			command := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{}, runner, runner, runner)
			command.SetArgs([]string{"--repo", "video", subcommand})
			if err := command.ExecuteContext(t.Context()); err == nil {
				t.Fatal("ExecuteContext() error = nil")
			}
		})
	}
}

func TestCheckCommandAcceptsWaitAndIntervalFlags(t *testing.T) {
	var got commandOptions
	runner := func(_ context.Context, _, _ io.Writer, options commandOptions) error {
		got = options
		return nil
	}
	command := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{}, runner, runner, runner)
	command.SetArgs([]string{"check", "--wait", "--interval", "5s"})

	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if !got.Wait || got.Timeout != 0 || got.Interval != 5*time.Second {
		t.Errorf("options = %+v", got)
	}
}

func TestCheckCommandAcceptsDayDurationTimeout(t *testing.T) {
	var got commandOptions
	runner := func(_ context.Context, _, _ io.Writer, options commandOptions) error {
		got = options
		return nil
	}
	command := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{}, runner, runner, runner)
	command.SetArgs([]string{"check", "--timeout", "1d1m1s"})

	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	want := 24*time.Hour + time.Minute + time.Second
	if got.Timeout != want {
		t.Errorf("Timeout = %v, want %v", got.Timeout, want)
	}
}

func TestCheckCommandRejectsWaitTogetherWithTimeout(t *testing.T) {
	runner := func(context.Context, io.Writer, io.Writer, commandOptions) error {
		t.Fatal("runner was called")
		return nil
	}
	command := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{}, runner, runner, runner)
	command.SetArgs([]string{"check", "--wait", "--timeout", "10m"})
	if err := command.ExecuteContext(t.Context()); err == nil {
		t.Fatal("ExecuteContext() error = nil")
	}
}

func TestCheckCommandRejectsNonPositiveIntervalWhenWaiting(t *testing.T) {
	for _, args := range [][]string{
		{"check", "--wait", "--interval", "0s"},
		{"check", "--timeout", "10m", "--interval", "0s"},
	} {
		runner := func(context.Context, io.Writer, io.Writer, commandOptions) error {
			t.Fatal("runner was called")
			return nil
		}
		command := newRootCommand(&bytes.Buffer{}, &bytes.Buffer{}, runner, runner, runner)
		command.SetArgs(args)
		if err := command.ExecuteContext(t.Context()); err == nil {
			t.Fatalf("ExecuteContext(%v) error = nil", args)
		}
	}
}

func TestParseDayDuration(t *testing.T) {
	tests := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"1d1m1s", 24*time.Hour + time.Minute + time.Second, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"1.5d", 36 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"500ms", 500 * time.Millisecond, false},
		{"0s", 0, false},
		{"0", 0, false},
		{"1000000w", 0, true},
		{"106751d106751d", 0, true},
		{"1x", 0, true},
		{"d", 0, true},
		{"", 0, true},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := parseDayDuration(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseDayDuration(%q) error = nil", test.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDayDuration(%q) error = %v", test.input, err)
			}
			if got != test.want {
				t.Errorf("parseDayDuration(%q) = %v, want %v", test.input, got, test.want)
			}
		})
	}
}

// fakeChecker 按调用顺序逐条返回预置报告；报告用尽后重复最后一条。
type fakeChecker struct {
	reports []status.Report
	calls   int
}

func (f *fakeChecker) Check(context.Context) (status.Report, error) {
	report := f.reports[min(f.calls, len(f.reports)-1)]
	f.calls++
	return report, nil
}

var workReport = status.Report{
	NeedsReview: []status.PullRequestSummary{{
		Repository: status.Repository{Owner: "acme", Name: "video"},
		Index:      7,
		Title:      "add thing",
		HTMLURL:    "https://gitea.example/acme/video/pulls/7",
	}},
}

func TestWaitForReportReturnsImmediatelyWhenWorkExists(t *testing.T) {
	checker := &fakeChecker{reports: []status.Report{workReport}}
	var stdout bytes.Buffer

	if err := waitForReport(t.Context(), &stdout, checker, false, 10*time.Minute, 10*time.Millisecond); err != nil {
		t.Fatalf("waitForReport() error = %v", err)
	}
	if checker.calls != 1 {
		t.Errorf("calls = %d, want 1", checker.calls)
	}
	if !strings.Contains(stdout.String(), "acme/video#7") {
		t.Errorf("output = %q", stdout.String())
	}
}

func TestWaitForReportWithoutWaitingKeepsImmediateEmptyResult(t *testing.T) {
	checker := &fakeChecker{reports: []status.Report{{}}}
	var stdout bytes.Buffer

	if err := waitForReport(t.Context(), &stdout, checker, false, 0, 10*time.Millisecond); err != nil {
		t.Fatalf("waitForReport() error = %v", err)
	}
	if checker.calls != 1 {
		t.Errorf("calls = %d, want 1", checker.calls)
	}
	if !strings.Contains(stdout.String(), "没有 Issue 或 PR 需要处理") {
		t.Errorf("output = %q", stdout.String())
	}
}

func TestWaitForReportPollsUntilWorkAppears(t *testing.T) {
	checker := &fakeChecker{reports: []status.Report{{}, {}, workReport}}
	var stdout bytes.Buffer

	if err := waitForReport(t.Context(), &stdout, checker, false, 10*time.Second, time.Millisecond); err != nil {
		t.Fatalf("waitForReport() error = %v", err)
	}
	if checker.calls != 3 {
		t.Errorf("calls = %d, want 3", checker.calls)
	}
	if !strings.Contains(stdout.String(), "acme/video#7") {
		t.Errorf("output = %q", stdout.String())
	}
}

func TestWaitForReportForeverPollsUntilWorkAppears(t *testing.T) {
	checker := &fakeChecker{reports: []status.Report{{}, {}, workReport}}
	var stdout bytes.Buffer

	if err := waitForReport(t.Context(), &stdout, checker, true, 0, time.Millisecond); err != nil {
		t.Fatalf("waitForReport() error = %v", err)
	}
	if checker.calls != 3 {
		t.Errorf("calls = %d, want 3", checker.calls)
	}
	if !strings.Contains(stdout.String(), "acme/video#7") {
		t.Errorf("output = %q", stdout.String())
	}
}

func TestWaitForReportStopsAtTimeout(t *testing.T) {
	checker := &fakeChecker{reports: []status.Report{{}}}
	var stdout bytes.Buffer

	if err := waitForReport(t.Context(), &stdout, checker, false, 20*time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatalf("waitForReport() error = %v", err)
	}
	if checker.calls < 2 {
		t.Errorf("calls = %d, want >= 2", checker.calls)
	}
	if !strings.Contains(stdout.String(), "超时") {
		t.Errorf("output = %q", stdout.String())
	}
}

func TestWaitForReportStopsOnCancellation(t *testing.T) {
	checker := &fakeChecker{reports: []status.Report{{}}}
	ctx, cancel := context.WithCancel(t.Context())
	var stdout bytes.Buffer

	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()
	if err := waitForReport(ctx, &stdout, checker, true, 0, 5*time.Millisecond); err != nil {
		t.Fatalf("waitForReport() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "已中断等待") {
		t.Errorf("output = %q", stdout.String())
	}
}

// cancelledChecker 模拟 Check 的网络往返执行到一半时 ctx 被取消，
// 返回包装过的 context.Canceled（真实场景是 *url.Error）。
type cancelledChecker struct{}

func (cancelledChecker) Check(context.Context) (status.Report, error) {
	return status.Report{}, fmt.Errorf("列表 Issue: %w", context.Canceled)
}

func TestWaitForReportStopsWhenCheckIsCancelled(t *testing.T) {
	var stdout bytes.Buffer

	if err := waitForReport(t.Context(), &stdout, cancelledChecker{}, true, 0, 10*time.Millisecond); err != nil {
		t.Fatalf("waitForReport() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "已中断等待") {
		t.Errorf("output = %q", stdout.String())
	}
}
