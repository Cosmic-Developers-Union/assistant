package status

import (
	"errors"
	"slices"
	"testing"
)

var errUnavailable = errors.New("unavailable")

// issueWithRepositoryLabels 用仓库真实标签（含正确 ID）构造 Issue；
// 体系外的标签（如自定义标签）分配 900 起的占位 ID。
func issueWithRepositoryLabels(t *testing.T, repositoryLabels []Label, index int64, names ...string) Issue {
	t.Helper()
	issue := Issue{Index: index, Title: "Issue", HTMLURL: "https://gitea.example.com/acme/video/issues/1"}
	for _, name := range names {
		label, ok := findLabel(repositoryLabels, name)
		if !ok {
			label = Label{ID: 900 + int64(len(issue.Labels)), Name: name}
		}
		issue.Labels = append(issue.Labels, label)
	}
	return issue
}

func TestSyncClosesIssuesWithClosureReason(t *testing.T) {
	for _, reason := range []string{duplicateLabelName, wontfixLabelName} {
		t.Run(reason, func(t *testing.T) {
			repository := Repository{Owner: "acme", Name: "video"}
			labels := completeLabels()
			api := newFakeAPI(repository, labels)
			api.openIssues[repository.FullName()] = []Issue{
				issueWithRepositoryLabels(t, labels, 5, reason, triageLabelName),
			}

			if err := NewManager(api).Sync(t.Context()); err != nil {
				t.Fatalf("Sync() error = %v", err)
			}
			if !slices.Equal(api.closedIssues, []int64{5}) {
				t.Errorf("closed issues = %v", api.closedIssues)
			}
			if len(api.addedLabels) != 0 || len(api.removedLabels) != 0 {
				t.Errorf("added = %+v, removed = %+v", api.addedLabels, api.removedLabels)
			}
		})
	}
}

func TestSyncRemovesPullRequestStatusLabelsFromIssues(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	api := newFakeAPI(repository, labels)
	api.openIssues[repository.FullName()] = []Issue{
		issueWithRepositoryLabels(
			t, labels, 5, reviewLabelName, approvedLabelName, awaitingAuthorLabelName, confirmedLabelName,
		),
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wantRemoved := []labelChange{
		{Item: 5, Label: labelByName(t, labels, reviewLabelName).ID},
		{Item: 5, Label: labelByName(t, labels, approvedLabelName).ID},
		{Item: 5, Label: labelByName(t, labels, awaitingAuthorLabelName).ID},
	}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
	if len(api.addedLabels) != 0 {
		t.Errorf("added labels = %+v", api.addedLabels)
	}
}

func TestSyncRemovesTriageWhenNeedsInfo(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	api := newFakeAPI(repository, labels)
	api.openIssues[repository.FullName()] = []Issue{
		issueWithRepositoryLabels(t, labels, 5, needsInfoLabelName, triageLabelName),
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wantRemoved := []labelChange{{Item: 5, Label: labelByName(t, labels, triageLabelName).ID}}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
	if len(api.addedLabels) != 0 {
		t.Errorf("added labels = %+v", api.addedLabels)
	}
}

func TestSyncDoesNotAutoTriageIssueWaitingForInfo(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	api := newFakeAPI(repository, labels)
	api.openIssues[repository.FullName()] = []Issue{
		issueWithRepositoryLabels(t, labels, 5, needsInfoLabelName),
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.addedLabels) != 0 || len(api.removedLabels) != 0 || len(api.closedIssues) != 0 {
		t.Errorf("added = %+v, removed = %+v, closed = %v", api.addedLabels, api.removedLabels, api.closedIssues)
	}
}

func TestSyncAddsTriageToIssuesWithoutStatus(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	api := newFakeAPI(repository, labels)
	api.openIssues[repository.FullName()] = []Issue{
		issueWithRepositoryLabels(t, labels, 5, "type/bug"),
		issueWithRepositoryLabels(t, labels, 6, confirmedLabelName),
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wantAdded := []labelChange{{Item: 5, Label: labelByName(t, labels, triageLabelName).ID}}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
}
