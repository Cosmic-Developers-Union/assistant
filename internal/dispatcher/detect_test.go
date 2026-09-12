package dispatcher

import (
	"context"
	"testing"

	"assistant/internal/status"
)

var testRepo = status.Repository{Owner: "owner", Name: "repo"}

func TestListWorkCombinesPullsAndIssuesSortedByNumber(t *testing.T) {
	api := &fakeAPI{
		pulls: []status.Issue{
			{Index: 61, Title: "PR B", IsPull: true},
			{Index: 58, Title: "PR A", IsPull: true},
		},
		issues: []status.Issue{{Index: 40, Title: "Issue 甲"}},
	}
	work, err := ListWork(context.Background(), api, testRepo)
	if err != nil {
		t.Fatalf("ListWork() error = %v", err)
	}
	want := []WorkItem{
		{Kind: KindIssue, Number: 40, Title: "Issue 甲"},
		{Kind: KindPull, Number: 58, Title: "PR A"},
		{Kind: KindPull, Number: 61, Title: "PR B"},
	}
	if len(work) != len(want) {
		t.Fatalf("work = %+v, want %+v", work, want)
	}
	for i := range want {
		if work[i] != want[i] {
			t.Errorf("work[%d] = %+v, want %+v", i, work[i], want[i])
		}
	}
	if api.listCalls.Load() != 2 {
		t.Errorf("listCalls = %d, want 2", api.listCalls.Load())
	}
}

func TestListWorkSkipsIssuesMixedIntoPullResults(t *testing.T) {
	api := &fakeAPI{
		pulls: []status.Issue{
			{Index: 58, Title: "真 PR", IsPull: true},
			{Index: 12, Title: "混入的 Issue"},
		},
	}
	work, err := ListWork(context.Background(), api, testRepo)
	if err != nil {
		t.Fatalf("ListWork() error = %v", err)
	}
	if len(work) != 1 || work[0] != (WorkItem{Kind: KindPull, Number: 58, Title: "真 PR"}) {
		t.Errorf("work = %+v, want only pull #58", work)
	}
}

// 同一仓库内 (kind, number) 唯一：会话与完成判定都以该键去重。
func TestDedupeWorkKeepsOnePerKindAndNumber(t *testing.T) {
	work := []WorkItem{
		{Kind: KindPull, Number: 1},
		{Kind: KindPull, Number: 1},
		{Kind: KindIssue, Number: 1},
		{Kind: KindIssue, Number: 2},
		{Kind: KindIssue, Number: 2},
	}
	deduped := dedupeWork(work)
	want := []WorkItem{
		{Kind: KindPull, Number: 1},
		{Kind: KindIssue, Number: 1},
		{Kind: KindIssue, Number: 2},
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
