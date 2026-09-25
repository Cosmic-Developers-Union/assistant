package dispatcher

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// guardHarness 聚合守卫测试的桩：FollowUpMessages 可注入返回，调用留痕。
type guardHarness struct {
	deps     Deps
	guards   *guardState
	messages []string
	comments int // FollowUpMessages 调用次数
}

func newGuardHarness(t *testing.T, messages []string) *guardHarness {
	t.Helper()
	h := &guardHarness{guards: newGuardState(), messages: messages}
	h.deps = Deps{
		Log:        func(string) {},
		LogDebug:   func(string) {},
		LogVerbose: func(string) {},
		FollowUpMessages: func(_ context.Context, _ WorkItem, _ time.Time) ([]string, error) {
			h.comments++
			return h.messages, nil
		},
	}
	return h
}

// TestGuardStateReleaseByChannel 验证两通道守卫独立解除：settled 只看请求类
// 清单（review 请求 / 分诊标签），handled 只看 mention 清单——条目从一路消失
// 不影响另一路的守卫。
func TestGuardStateReleaseByChannel(t *testing.T) {
	guards := newGuardState()
	guards.apply("issue#6", ProcessResult{Settled: true, Responded: true}, time.Date(2026, 9, 17, 14, 36, 1, 0, time.UTC))

	guards.release([]WorkItem{{Kind: KindIssue, Number: 6, Mention: true}}, func(string) {})
	if _, handled := guards.get("issue#6"); handled.IsZero() {
		t.Error("mention 清单仍在，水位线必须保留")
	}
	if settled, _ := guards.get("issue#6"); settled {
		t.Error("条目已离开标签清单，settled 必须解除")
	}

	guards.release([]WorkItem{{Kind: KindIssue, Number: 6, Labeled: true}}, func(string) {})
	if _, handled := guards.get("issue#6"); !handled.IsZero() {
		t.Error("条目已离开 mention 清单，水位线必须清除（重新 mention 视为全新请求）")
	}
	if settled, _ := guards.get("issue#6"); settled {
		t.Error("标签清单仍在，settled 必须保留")
	}
}

// TestSelectDispatchByChannel 覆盖双通道的派发判定：标签 settled 压制不阻断
// mention 新消息；mention 水位线只吸收重复信号、放行新评论（追问轮）。
func TestSelectDispatchByChannel(t *testing.T) {
	handledAt := time.Date(2026, 9, 17, 14, 36, 1, 0, time.UTC)
	fresh := handledAt.Add(3 * time.Minute)
	stale := handledAt.Add(-5 * time.Minute)

	newHarness := func(messages []string, handled bool) *guardHarness {
		h := newGuardHarness(t, messages)
		if handled {
			h.guards.apply("issue#6", ProcessResult{Settled: true, Responded: true}, handledAt)
		}
		return h
	}

	cases := []struct {
		name     string
		item     WorkItem
		handled  bool // 预置 issue#6 已 settled + 已回应（水位线 = handledAt）
		messages []string
		wantOK   bool
		wantMode string // "" = 不派发；"full" = 全量；"followup" = 追问
	}{
		{
			name:     "分诊标签未 settled：全量派发",
			item:     WorkItem{Kind: KindIssue, Number: 6, Labeled: true},
			wantOK:   true,
			wantMode: "full",
		},
		{
			name:     "review 请求未 settled：全量派发",
			item:     WorkItem{Kind: KindPull, Number: 9, Requested: true},
			wantOK:   true,
			wantMode: "full",
		},
		{
			name:     "标签已 settled 且不在 mention 清单：压制",
			item:     WorkItem{Kind: KindIssue, Number: 6, Labeled: true},
			handled:  true,
			messages: []string{"新消息"},
		},
		{
			name:     "mention 从未回应：全量派发",
			item:     WorkItem{Kind: KindIssue, Number: 7, Mention: true},
			wantOK:   true,
			wantMode: "full",
		},
		{
			name:     "mention 已回应且无新动态：压制（水位线吸收重复信号）",
			item:     WorkItem{Kind: KindIssue, Number: 6, Mention: true, Updated: stale},
			handled:  true,
			messages: []string{"新消息"},
		},
		{
			name:     "有动态但全是自身活动：不重新入队",
			item:     WorkItem{Kind: KindIssue, Number: 6, Mention: true, Updated: fresh},
			handled:  true,
			messages: nil,
		},
		{
			name:     "水位线后有他人新评论：追问轮入队",
			item:     WorkItem{Kind: KindIssue, Number: 6, Mention: true, Updated: fresh},
			handled:  true,
			messages: []string{"@Ge：追问", "@Ge：再问一条"},
			wantOK:   true,
			wantMode: "followup",
		},
		{
			name:     "两路同时命中且标签未 settled：标签优先（全量）",
			item:     WorkItem{Kind: KindIssue, Number: 8, Mention: true, Labeled: true, Updated: fresh},
			messages: []string{"新消息"},
			wantOK:   true,
			wantMode: "full",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(tc.messages, tc.handled)
			dispatch, ok := h.deps.selectDispatch(context.Background(), h.guards, tc.item)
			if ok != tc.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tc.wantOK)
			}
			switch tc.wantMode {
			case "":
				return
			case "full":
				if dispatch.FollowUp {
					t.Errorf("dispatch = %+v, want 全量模式", dispatch)
				}
			case "followup":
				if !dispatch.FollowUp {
					t.Fatalf("dispatch = %+v, want 追问模式", dispatch)
				}
				if !dispatch.Since.Equal(handledAt) {
					t.Errorf("Since = %v, want 水位线 %v", dispatch.Since, handledAt)
				}
			}
		})
	}
}

// 端到端：mention 条目处理后，同一条目的追问派发走 processFollowUp——起续聊
// 会话（不跑全量协议、不做完成判定），成功即回报 Responded。
func TestProcessItemFollowUpRunsResumeSessionWithoutVerify(t *testing.T) {
	logDir := t.TempDir()
	sessions := 0
	fetched := false
	var prompts []string
	deps := Deps{
		Config:      testConfig(func(config *Config) { config.LogDir = logDir }),
		API:         &fakeAPI{labels: []status.Label{{ID: 1, Name: "status/triage"}}},
		RepoDir:     "/repo",
		Log:         func(string) {},
		LogVerbose:  func(string) {},
		LogDebug:    func(string) {},
		BuildPrompt: BuildPrompt,
		PrepareIssue: func(string) (string, error) {
			// 追问轮与全量会话同路径建基线 worktree（cwd 隔离）
			return t.TempDir(), nil
		},
		RemoveWorktree: func(string) error { return nil },
		FollowUpMessages: func(_ context.Context, _ WorkItem, _ time.Time) ([]string, error) {
			// 消息只在首次检查时存在：追问会话结束后 followUp 兜底轮取不到新消息
			if fetched {
				return nil, nil
			}
			fetched = true
			return []string{"@Ge：使用 mcp 获取你令牌的身份"}, nil
		},
		RunSession: func(request SessionRequest) SessionOutcome {
			sessions++
			prompts = append(prompts, request.Prompt)
			return SessionOutcome{Subtype: "success", NumTurns: 2, CostUSD: 0.05, DurationMS: 10, Errors: []string{}}
		},
	}
	handledAt := time.Date(2026, 9, 17, 14, 36, 1, 0, time.UTC)
	result := ProcessItem(context.Background(), deps, WorkItem{
		Kind: KindIssue, Number: 6, Title: "冒烟 #6", Mention: true,
		FollowUp: true, Since: handledAt,
	})
	if !result.Responded {
		t.Error("追问会话成功后必须回报 Responded（水位线推进）")
	}
	if result.Settled {
		t.Error("追问轮没有标签式完成判定，不得回报 Settled")
	}
	if sessions != 1 {
		t.Fatalf("sessions = %d, want 1", sessions)
	}
	if !strings.Contains(prompts[0], "使用 mcp 获取你令牌的身份") {
		t.Errorf("prompt = %q, want 包含新消息原文", prompts[0])
	}
	if strings.Contains(prompts[0], "triage issue #6") {
		t.Errorf("prompt = %q, 追问轮不得重跑全量协议", prompts[0])
	}
	detail := readLogFile(t, logDir, "issue-6-")
	for _, want := range []string{"[follow-up]", "[follow-up prompt]"} {
		if !strings.Contains(detail, want) {
			t.Errorf("待办日志 missing %q:\n%s", want, detail)
		}
	}
}
