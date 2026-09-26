package dispatcher

import (
	"context"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

var testRepo = status.Repository{Owner: "owner", Name: "repo"}

// ListWork 的防御性出口：本次检索集合里没有待办可定位（results 为空 ⇒ work
// 长度为 0）时仍返回空结果而非 panic。当前实现里 work 非空（四个通道的
// append 至少在切片里留下一条），因此这里钉住的是行为契约而非内部构造——
// 一旦以后把「无待办」优化成直接返回空切片，这条断言不会误报。
func TestListWorkWithoutItemsReturnsEmpty(t *testing.T) {
	work, err := ListWork(context.Background(), &fakeAPI{}, testRepo, "ai")
	if err != nil {
		t.Fatalf("ListWork() error = %v", err)
	}
	if len(work) != 0 {
		t.Errorf("ListWork(空通道) = %+v, want 空", work)
	}
}

func TestListWorkCombinesPullsAndIssuesSortedByNumberDescending(t *testing.T) {
	api := &fakeAPI{
		requestedPulls: []status.PullRequest{
			{Index: 61, Title: "PR B"},
			{Index: 58, Title: "PR A"},
		},
		issues: []status.Issue{{Index: 40, Title: "Issue 甲"}},
	}
	work, err := ListWork(context.Background(), api, testRepo, "ai")
	if err != nil {
		t.Fatalf("ListWork() error = %v", err)
	}
	want := []WorkItem{
		{Kind: KindPull, Number: 61, Title: "PR B", Requested: true},
		{Kind: KindPull, Number: 58, Title: "PR A", Requested: true},
		{Kind: KindIssue, Number: 40, Title: "Issue 甲", Labeled: true},
	}
	if len(work) != len(want) {
		t.Fatalf("work = %+v, want %+v", work, want)
	}
	for i := range want {
		if work[i] != want[i] {
			t.Errorf("work[%d] = %+v, want %+v", i, work[i], want[i])
		}
	}
	if api.listCalls.Load() != 4 {
		t.Errorf("listCalls = %d, want 4（mention 两路 + 标签两路）", api.listCalls.Load())
	}
	if got, want := *api.mentionUser.Load(), "ai"; got != want {
		t.Errorf("mentionUser = %q, want %q", got, want)
	}
	if got, want := *api.requestedReviewer.Load(), "ai"; got != want {
		t.Errorf("requestedReviewer = %q, want %q", got, want)
	}
}

// review 请求通道命中的 PR（无 mention）也进入待办：原生请求即触发。
func TestListWorkIncludesReviewRequestChannel(t *testing.T) {
	api := &fakeAPI{
		requestedPulls: []status.PullRequest{{Index: 72, Title: "被请求评审的 PR"}},
	}
	work, err := ListWork(context.Background(), api, testRepo, "ai")
	if err != nil {
		t.Fatalf("ListWork() error = %v", err)
	}
	want := []WorkItem{{Kind: KindPull, Number: 72, Title: "被请求评审的 PR", Requested: true}}
	if len(work) != len(want) {
		t.Fatalf("work = %+v, want %+v", work, want)
	}
	for i := range want {
		if work[i] != want[i] {
			t.Errorf("work[%d] = %+v, want %+v", i, work[i], want[i])
		}
	}
}

// mention 通道命中的条目（标签缺失）也进入待办：mention 即触发，标签仅观测。
func TestListWorkIncludesMentionChannel(t *testing.T) {
	updated := time.Date(2026, 9, 17, 14, 38, 36, 0, time.UTC)
	api := &fakeAPI{
		mentionIssues: []status.Issue{{Index: 70, Title: "新 @ai Issue", Updated: updated}},
		mentionPulls:  []status.Issue{{Index: 71, Title: "新 @ai PR", IsPull: true}},
	}
	work, err := ListWork(context.Background(), api, testRepo, "ai")
	if err != nil {
		t.Fatalf("ListWork() error = %v", err)
	}
	want := []WorkItem{
		{Kind: KindPull, Number: 71, Title: "新 @ai PR", Mention: true},
		{Kind: KindIssue, Number: 70, Title: "新 @ai Issue", Mention: true, Updated: updated},
	}
	if len(work) != len(want) {
		t.Fatalf("work = %+v, want %+v", work, want)
	}
	for i := range want {
		if work[i] != want[i] {
			t.Errorf("work[%d] = %+v, want %+v", i, work[i], want[i])
		}
	}
}

// 多路命中同一待办时合并为一份，通道标记按或保留：各通道的守卫独立生效，
// 不能在检索层丢掉任何一路的信号。
func TestListWorkMergesChannelFlags(t *testing.T) {
	api := &fakeAPI{
		mentionIssues:  []status.Issue{{Index: 40, Title: "Issue 甲"}},
		issues:         []status.Issue{{Index: 40, Title: "Issue 甲"}},
		requestedPulls: []status.PullRequest{{Index: 41, Title: "PR 乙"}},
		mentionPulls:   []status.Issue{{Index: 41, Title: "PR 乙", IsPull: true}},
	}
	work, err := ListWork(context.Background(), api, testRepo, "ai")
	if err != nil {
		t.Fatalf("ListWork() error = %v", err)
	}
	if len(work) != 2 {
		t.Fatalf("work = %+v, want issue#40 与 pull#41 各一份", work)
	}
	if !work[0].Mention || !work[0].Requested || work[0].Number != 41 {
		t.Errorf("work[0] = %+v, want pull#41 Mention+Requested", work[0])
	}
	if !work[1].Mention || !work[1].Labeled || work[1].Number != 40 {
		t.Errorf("work[1] = %+v, want issue#40 Mention+Labeled", work[1])
	}
}

func TestListWorkSkipsIssuesMixedIntoPullResults(t *testing.T) {
	api := &fakeAPI{
		requestedPulls: []status.PullRequest{{Index: 58, Title: "真 PR"}},
		issues: []status.Issue{
			{Index: 12, Title: "混入的 Issue"},
		},
	}
	work, err := ListWork(context.Background(), api, testRepo, "ai")
	if err != nil {
		t.Fatalf("ListWork() error = %v", err)
	}
	if len(work) != 2 || work[0] != (WorkItem{Kind: KindPull, Number: 58, Title: "真 PR", Requested: true}) {
		t.Errorf("work = %+v, want pull #58 + issue #12", work)
	}
}

// 同一仓库内 (kind, number) 唯一：会话与完成判定都以该键去重；同键条目的
// 通道标记按或合并。
func TestDedupeWorkKeepsOnePerKindAndNumber(t *testing.T) {
	work := []WorkItem{
		{Kind: KindPull, Number: 1},
		{Kind: KindPull, Number: 1, Mention: true},
		{Kind: KindIssue, Number: 1, Labeled: true},
		{Kind: KindIssue, Number: 2},
		{Kind: KindIssue, Number: 2, Mention: true, Labeled: true},
		{Kind: KindPull, Number: 3, Requested: true},
		{Kind: KindPull, Number: 3},
	}
	deduped := dedupeWork(work)
	want := []WorkItem{
		{Kind: KindPull, Number: 1, Mention: true},
		{Kind: KindIssue, Number: 1, Labeled: true},
		{Kind: KindIssue, Number: 2, Mention: true, Labeled: true},
		{Kind: KindPull, Number: 3, Requested: true},
	}
	if len(deduped) != len(want) {
		t.Fatalf("deduped = %+v, want %+v", deduped, want)
	}
	for index := range want {
		if deduped[index] != want[index] {
			t.Errorf("deduped[%d] = %+v, want %+v", index, deduped[index], want[index])
		}
	}
}

// workItems 的强制定型：标签通道传 KindPull 时，非 PR 条目一律剔除
// （Gitea 叠加标识查询偶发混入 Issue，双保险过滤）；未强制时按 IsPull 区分。
func TestWorkItemsForcedKindFiltersNonPulls(t *testing.T) {
	items := []status.Issue{
		{Index: 1, Title: "真 PR", IsPull: true},
		{Index: 2, Title: "混入的 Issue"},
	}
	forced := workItems(items, labelChannel, KindPull)
	if len(forced) != 1 || forced[0].Number != 1 || forced[0].Kind != KindPull {
		t.Fatalf("forced = %+v, want 仅 pull#1", forced)
	}
	if !forced[0].Labeled || forced[0].Mention || forced[0].Requested {
		t.Errorf("forced[0] = %+v, want 仅 Labeled 标记", forced[0])
	}

	// 未强制：按条目自身 IsPull 定型
	byKind := workItems(items, mentionChannel)
	if len(byKind) != 2 || byKind[0].Kind != KindPull || byKind[1].Kind != KindIssue {
		t.Fatalf("byKind = %+v, want pull + issue", byKind)
	}
	if !byKind[0].Mention || byKind[0].Requested || byKind[0].Labeled {
		t.Errorf("byKind[0] = %+v, want 仅 Mention 标记", byKind[0])
	}
}

// 标签通道强制为 Issue 时不做剔除：Issue 分诊只可能是 Issue。
func TestWorkItemsLabelChannelKeepsIssues(t *testing.T) {
	work := workItems([]status.Issue{{Index: 12, Title: "分诊 Issue"}}, labelChannel, KindIssue)
	if len(work) != 1 || work[0].Kind != KindIssue || !work[0].Labeled {
		t.Fatalf("work = %+v, want issue#12 Labeled", work)
	}
}

// dedupeWork 只合并相邻同键条目（调用方已按编号排序）：不相邻则各自保留——
// 这条约束是「会话键唯一」的前提，也是 sort 先行的原因。
func TestDedupeWorkKeepsNonAdjacentDuplicatesSeparate(t *testing.T) {
	work := []WorkItem{
		{Kind: KindIssue, Number: 5},
		{Kind: KindIssue, Number: 6},
		{Kind: KindIssue, Number: 5, Mention: true},
	}
	deduped := dedupeWork(work)
	if len(deduped) != 3 {
		t.Fatalf("deduped = %+v, want 3 份（非相邻不去重）", deduped)
	}

	// 少于两份时原样返回，不复制切片
	if got := dedupeWork(work[:1]); len(got) != 1 {
		t.Errorf("单元素 dedupe = %+v", got)
	}
	if got := dedupeWork(nil); len(got) != 0 {
		t.Errorf("空输入 dedupe = %+v", got)
	}
}

// mention 通道与标签通道支持同一编号共存（键含 kind）：pull#40 与 issue#40
// 是两个待办，合并只能发生在 kind 也相同的条目之间。
func TestDedupeWorkDoesNotMergeAcrossKinds(t *testing.T) {
	work := []WorkItem{
		{Kind: KindPull, Number: 40, Requested: true},
		{Kind: KindIssue, Number: 40, Labeled: true},
	}
	if deduped := dedupeWork(work); len(deduped) != 2 {
		t.Fatalf("deduped = %+v, want 2 份（kind 不同不合并）", deduped)
	}
}
