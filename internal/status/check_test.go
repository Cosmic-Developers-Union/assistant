package status

import (
	"bytes"
	"strings"
	"testing"
)

func TestCheckReportsLabelledWork(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	api := newFakeAPI(repository, nil)
	api.triageIssues[repository.FullName()] = []Issue{
		{Index: 9, Title: "Need details", HTMLURL: "https://gitea.example.com/acme/video/issues/9"},
	}
	api.reviewPulls[repository.FullName()] = []Issue{
		{Index: 12, Title: "Fix rendering", HTMLURL: "https://gitea.example.com/acme/video/pulls/12"},
	}

	report, err := NewManager(api).Check(t.Context())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(report.NeedsTriage) != 1 || report.NeedsTriage[0].Index != 9 {
		t.Errorf("NeedsTriage = %+v", report.NeedsTriage)
	}
	if len(report.NeedsReview) != 1 || report.NeedsReview[0].Index != 12 {
		t.Errorf("NeedsReview = %+v", report.NeedsReview)
	}
}

func TestCheckNeverWrites(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	api := newFakeAPI(repository, nil)
	api.triageIssues[repository.FullName()] = []Issue{{Index: 9}}
	api.reviewPulls[repository.FullName()] = []Issue{{Index: 12}}

	if _, err := NewManager(api).Check(t.Context()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(api.createdLabels) != 0 || len(api.exclusive) != 0 ||
		len(api.addedLabels) != 0 || len(api.removedLabels) != 0 ||
		len(api.createdReviews) != 0 || len(api.closedIssues) != 0 {
		t.Fatalf(
			"Check() mutated state: created=%+v exclusive=%v added=%+v removed=%+v reviews=%+v closed=%v",
			api.createdLabels, api.exclusive, api.addedLabels, api.removedLabels, api.createdReviews, api.closedIssues,
		)
	}
}

func TestCheckContinuesAfterRepositoryError(t *testing.T) {
	broken := Repository{Owner: "acme", Name: "broken"}
	healthy := Repository{Owner: "acme", Name: "healthy"}
	api := newFakeAPI(healthy, nil)
	api.repositories = []Repository{broken, healthy}
	api.issueListError[broken.FullName()] = errUnavailable
	api.triageIssues[healthy.FullName()] = []Issue{{Index: 7}}

	report, err := NewManager(api).Check(t.Context())
	if err == nil || !strings.Contains(err.Error(), broken.FullName()) {
		t.Fatalf("Check() error = %v", err)
	}
	if len(report.NeedsTriage) != 1 || report.NeedsTriage[0].Index != 7 {
		t.Fatalf("NeedsTriage = %+v", report.NeedsTriage)
	}
}

func TestCheckValidatesTargetRepository(t *testing.T) {
	visible := Repository{Owner: "acme", Name: "visible"}
	target := Repository{Owner: "acme", Name: "target"}
	api := newFakeAPI(visible, nil)

	_, err := NewManager(api, WithRepository(target)).Check(t.Context())
	if err == nil || !strings.Contains(err.Error(), target.FullName()) {
		t.Fatalf("Check() error = %v", err)
	}
}

func TestWriteReport(t *testing.T) {
	var output bytes.Buffer
	if err := WriteReport(&output, Report{}); err != nil {
		t.Fatalf("WriteReport() error = %v", err)
	}
	if output.String() != "没有 Issue 或 PR 需要处理\n" {
		t.Errorf("output = %q", output.String())
	}

	output.Reset()
	repository := Repository{Owner: "acme", Name: "video"}
	report := Report{
		NeedsTriage: []IssueSummary{{
			Repository: repository,
			Index:      6,
			Title:      "Missing details",
			HTMLURL:    "https://gitea.example.com/acme/video/issues/6",
		}},
		NeedsReview: []PullRequestSummary{{
			Repository: repository,
			Index:      7,
			Title:      "Fix rendering",
			HTMLURL:    "https://gitea.example.com/acme/video/pulls/7",
		}},
	}
	if err := WriteReport(&output, report); err != nil {
		t.Fatalf("WriteReport() error = %v", err)
	}
	want := "需要 triage 的 Issue:\n" +
		"- acme/video#6 Missing details https://gitea.example.com/acme/video/issues/6\n" +
		"需要 review 的 PR:\n" +
		"- acme/video#7 Fix rendering https://gitea.example.com/acme/video/pulls/7\n"
	if output.String() != want {
		t.Errorf("output = %q, want %q", output.String(), want)
	}
}
