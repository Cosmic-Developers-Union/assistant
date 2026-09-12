package status

import (
	"context"
	"path"
	"slices"
	"strings"
)

// selfCheckContextPrefix 是 assistant 运行工作流（.gitea/workflows/assistant.yml 的
// `name: Gitea Assistant`）产生的检查 context 前缀（"<workflow 名> /"）。重命名该
// 工作流时必须同步修改此常量。
const selfCheckContextPrefix = "Gitea Assistant /"

// checkStates 返回 head commit 上失败与仍在运行（pending）的必要检查。
// 分支保护配置了状态检查（enable_status_check + contexts）时，只统计必要 context；
// 未配置时覆盖所有 context——但排除 assistant 自己的 sync 工作流：bot 的运行结果
// 不是 PR 的质量信号，排队中的 sync 运行被 Gitea 取消会留下 "failure" 状态
// （"Canceled after 0s"），不排除则一次取消就触发自我驳回循环。
func (m *Manager) checkStates(
	ctx context.Context,
	repository Repository,
	pullRequest PullRequest,
	protections []BranchProtection,
) (failed, pending []CheckStatus, err error) {
	if pullRequest.HeadSHA == "" {
		return nil, nil, nil
	}
	statuses, err := m.api.GetCombinedStatus(ctx, repository, pullRequest.HeadSHA)
	if err != nil {
		return nil, nil, err
	}
	required := requiredContexts(protections, pullRequest.BaseRef)

	for _, check := range statuses {
		if required != nil && !slices.Contains(required, check.Context) {
			continue
		}
		if required == nil && strings.HasPrefix(check.Context, selfCheckContextPrefix) {
			continue
		}
		switch check.State {
		case "failure", "error":
			failed = append(failed, check)
		case "pending":
			pending = append(pending, check)
		}
	}
	if len(failed) > 0 {
		m.logf("%s#%d: 必要检查未通过: %v", repository.FullName(), pullRequest.Index, checkContexts(failed))
	}
	return failed, pending, nil
}

func checkContexts(checks []CheckStatus) []string {
	contexts := make([]string, 0, len(checks))
	for _, check := range checks {
		contexts = append(contexts, check.Context)
	}
	return contexts
}

// requiredContexts 返回保护 baseRef 分支的规则配置的必检 context 集合；
// 返回 nil 表示没有启用的状态检查规则，调用方回退为「任何失败即阻塞」。
// 规则名支持 Gitea 的 glob 模式（如 main、release/*、*）。
func requiredContexts(protections []BranchProtection, baseRef string) []string {
	var contexts []string
	for _, protection := range protections {
		if !protection.EnableStatusCheck || len(protection.Contexts) == 0 {
			continue
		}
		if !branchRuleMatches(protection.RuleName, baseRef) {
			continue
		}
		for _, context := range protection.Contexts {
			if !slices.Contains(contexts, context) {
				contexts = append(contexts, context)
			}
		}
	}
	return contexts
}

func branchRuleMatches(rule, branch string) bool {
	if rule == branch {
		return true
	}
	matched, err := path.Match(rule, branch)
	return err == nil && matched
}
