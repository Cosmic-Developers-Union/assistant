package dispatcher

import (
	"context"
	"testing"
	"time"

	"assistant/internal/status"
)

var testRepo = status.Repository{Owner: "owner", Name: "repo"}

func TestListWorkCombinesPullsAndIssuesSortedByNumberDescending(t *testing.T) {
	api := &fakeAPI{
		pulls: []status.Issue{
			{Index: 61, Title: "PR B", IsPull: true},
			{Index: 58, Title: "PR A", IsPull: true},
		},
		issues: []status.Issue{{Index: 40, Title: "Issue 甲"}},
	}
	work, err := ListWork(context.Background(), api, testRepo, "ai")
	if err != nil {
		t.Fatalf("ListWork() error = %v", err)
	}
	want := []WorkItem{
		{Kind: KindPull, Number: 61, Title: "PR B", Labeled: true},
		{Kind: KindPull, Number: 58, Title: "PR A", Labeled: true},
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

// mention 与标签两路命中同一待办时合并为一份，通道标记按或保留：两通道的
// 守卫各自独立生效，不能在检索层丢掉任何一路的信号。
func TestListWorkMergesMentionAndLabelChannels(t *testing.T) {
	api := &fakeAPI{
		mentionIssues: []status.Issue{{Index: 40, Title: "Issue 甲"}},
		issues:        []status.Issue{{Index: 40, Title: "Issue 甲"}},
	}
	work, err := ListWork(context.Background(), api, testRepo, "ai")
	if err != nil {
		t.Fatalf("ListWork() error = %v", err)
	}
	if len(work) != 1 {
		t.Fatalf("work = %+v, want 仅 issue#40 一份", work)
	}
	if !work[0].Mention || !work[0].Labeled {
		t.Errorf("work[0] = %+v, want Mention 与 Labeled 同时为真", work[0])
	}
}

func TestListWorkSkipsIssuesMixedIntoPullResults(t *testing.T) {
	api := &fakeAPI{
		pulls: []status.Issue{
			{Index: 58, Title: "真 PR", IsPull: true},
			{Index: 12, Title: "混入的 Issue"},
		},
	}
	work, err := ListWork(context.Background(), api, testRepo, "ai")
	if err != nil {
		t.Fatalf("ListWork() error = %v", err)
	}
	if len(work) != 1 || work[0] != (WorkItem{Kind: KindPull, Number: 58, Title: "真 PR", Labeled: true}) {
		t.Errorf("work = %+v, want only pull #58", work)
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
	}
	deduped := dedupeWork(work)
	want := []WorkItem{
		{Kind: KindPull, Number: 1, Mention: true},
		{Kind: KindIssue, Number: 1, Labeled: true},
		{Kind: KindIssue, Number: 2, Mention: true, Labeled: true},
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
