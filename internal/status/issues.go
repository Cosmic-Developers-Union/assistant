package status

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// issueStatusLabels 是 Issue 生命周期标签集合，与 PR 状态标签互斥。
var issueStatusLabels = []string{
	triageLabelName,
	confirmedLabelName,
	inProcessLabelName,
	blockedLabelName,
}

// pullRequestOnlyLabels 是 PR 专用标签集合（status/* 评审状态与 awaiting/* 行动方），
// 不应出现在 Issue 上。
var pullRequestOnlyLabels = []string{
	inProgressLabelName,
	reviewLabelName,
	changesRequestedLabelName,
	approvedLabelName,
	awaitingAuthorLabelName,
	awaitingReviewerLabelName,
	awaitingMergeLabelName,
}

// reconcileIssues 对仓库内所有 open Issue 执行标签规范检查（Sync 的一部分）。
func (m *Manager) reconcileIssues(
	ctx context.Context,
	repository Repository,
	repositoryLabels map[string]Label,
) error {
	issues, err := m.api.ListOpenIssues(ctx, repository)
	if err != nil {
		return fmt.Errorf("list open issues: %w", err)
	}
	m.logf("%s: 检查 %d 个 open Issue 的标签规范", repository.FullName(), len(issues))

	var reconcileErrors []error
	for _, issue := range issues {
		if err := m.reconcileIssue(ctx, repository, issue, repositoryLabels); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("issue #%d: %w", issue.Index, err))
		}
	}
	return errors.Join(reconcileErrors...)
}

// reconcileIssue 按顺序应用规范规则：带关闭原因标签的 Issue 直接关闭，不再调整其他标签。
func (m *Manager) reconcileIssue(
	ctx context.Context,
	repository Repository,
	issue Issue,
	repositoryLabels map[string]Label,
) error {
	if hasLabel(issue.Labels, duplicateLabelName) || hasLabel(issue.Labels, wontfixLabelName) {
		m.logf("%s#%d: 带 duplicate/wontfix 关闭原因标签，自动关闭", repository.FullName(), issue.Index)
		return m.api.CloseIssue(ctx, repository, issue.Index)
	}

	for _, name := range pullRequestOnlyLabels {
		label, ok := findLabel(issue.Labels, name)
		if !ok {
			continue
		}
		if err := m.api.RemoveLabel(ctx, repository, issue.Index, label.ID); err != nil {
			return err
		}
		m.logf("%s#%d: 移除 PR 专用标签 %s", repository.FullName(), issue.Index, name)
	}

	// needs-info 表示等待报告者补充信息：与 status/triage 互斥，且此时不自动补 triage，
	// 待报告者补齐信息后重新添加 status/triage 触发分诊。
	if hasLabel(issue.Labels, needsInfoLabelName) {
		if label, ok := findLabel(issue.Labels, triageLabelName); ok {
			if err := m.api.RemoveLabel(ctx, repository, issue.Index, label.ID); err != nil {
				return err
			}
			m.logf("%s#%d: needs-info 与 status/triage 共存，移除 status/triage", repository.FullName(), issue.Index)
		}
		return nil
	}

	if !hasAnyLabel(issue.Labels, issueStatusLabels) {
		triageLabel, ok := repositoryLabels[triageLabelName]
		if !ok {
			return fmt.Errorf("required label %q is missing", triageLabelName)
		}
		if err := m.api.AddLabel(ctx, repository, issue.Index, triageLabel.ID); err != nil {
			return err
		}
		m.logf("%s#%d: 无状态标签，添加 status/triage", repository.FullName(), issue.Index)
	}
	return nil
}

func findLabel(labels []Label, name string) (Label, bool) {
	index := slices.IndexFunc(labels, func(label Label) bool {
		return label.Name == name
	})
	if index < 0 {
		return Label{}, false
	}
	return labels[index], true
}

func hasAnyLabel(labels []Label, names []string) bool {
	return slices.ContainsFunc(names, func(name string) bool {
		return hasLabel(labels, name)
	})
}
