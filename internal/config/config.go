package config

import (
	"fmt"
	"net/url"
	"strings"
)

type Config struct {
	Host        string
	AccessToken string
	// BranchProtectionToken 是可选的仓库管理员令牌，仅用于读取分支保护配置
	// （该端点要求 repo admin，Actions 内置令牌无法满足）。留空则回退为
	// AccessToken，读取被拒时按严格模式降级。
	BranchProtectionToken string
	// StateReviewer 是状态评审者的账号名（merge 令牌身份）。配置后 sync 按
	// 作者角色区分内容/状态两条 review 通道：该账号的 review 是状态结论
	// （会签/门禁驳回），不参与内容判定；其余账号的 review 才是内容结论。
	// 留空则不做角色区分（历史行为），automerge 也不做会签。
	StateReviewer string
	// StateToken 是状态评审者令牌，sync 提交门禁驳回时用它，使驳回成为
	// official review（被 block_on_rejected_reviews / required approvals
	// 承认）。留空则驳回以 AccessToken 身份提交（不计数，仅时间线记录）。
	StateToken string
	Repository string
}

func Load(getenv func(string) string) (Config, error) {
	host := strings.TrimSpace(getenv("GITEA_HOST"))
	accessToken := strings.TrimSpace(getenv("GITEA_ACCESS_TOKEN"))
	branchProtectionToken := strings.TrimSpace(getenv("GITEA_BRANCH_PROTECTION_TOKEN"))
	stateReviewer := strings.TrimSpace(getenv("GITEA_STATE_REVIEWER"))
	stateToken := strings.TrimSpace(getenv("GITEA_STATE_TOKEN"))
	repository := strings.TrimSpace(getenv("GITEA_REPOSITORY"))

	if host == "" {
		return Config{}, fmt.Errorf("GITEA_HOST is required")
	}
	if accessToken == "" {
		return Config{}, fmt.Errorf("GITEA_ACCESS_TOKEN is required")
	}

	parsedHost, err := url.Parse(host)
	if err != nil || parsedHost.Host == "" ||
		(parsedHost.Scheme != "http" && parsedHost.Scheme != "https") ||
		parsedHost.RawQuery != "" || parsedHost.Fragment != "" {
		return Config{}, fmt.Errorf("GITEA_HOST must be an absolute HTTP or HTTPS URL")
	}

	return Config{
		Host:                  strings.TrimRight(host, "/"),
		AccessToken:           accessToken,
		BranchProtectionToken: branchProtectionToken,
		StateReviewer:         stateReviewer,
		StateToken:            stateToken,
		Repository:            repository,
	}, nil
}
