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
