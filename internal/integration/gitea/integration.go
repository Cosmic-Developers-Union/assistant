// Package gitea 是 Gitea 站点集成：把「仓库里的待办」（请求评审的 PR、被 @ 的
// 条目、挂着分诊标签的 Issue）接进调度引擎。
//
// 它与其他集成形态不同：qq/weixin/telegram 是对话型（收消息 → 回复，循环由
// integration.RunChat 提供）；gitea 是**调度型**——它的循环是「检测待办 →
// 执行会话 → 验证结论 → 清理」，由 internal/dispatcher 的常驻循环驱动
// （integration.DispatchIntegration 表达的就是这条区别）。
//
// 本包只提供 Gitea 侧的两件事，其余交给调度引擎：
//   - ListWork（在调度引擎侧，因为它产出的是调度引擎的 WorkItem）：这一轮有
//     哪些待办，走 Gitea 的 mentioned_by / requested_reviewers / status/triage
//     三个检索面；
//   - Verify：某个待办做完了没有（HEAD 锚定的评审完成判定 / 分诊标签是否移除）。
//
// **本包不 import 调度引擎**：Verify 收的是裸参数（kind/number），返回本包自己的
// 判定类型；把它折进调度引擎的类型由装配处（cmd/assistant）做。这样两边的依赖
// 都是单向的——平台实现不知道调度引擎的内部类型，调度引擎不知道 Gitea 的存在。
package gitea

import (
	"context"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// KindPull / KindIssue 是待办类型（与调度引擎的取值一致；此处只作参数约定）。
const (
	KindPull  = "pull"
	KindIssue = "issue"
)

// Verdict 是完成判定结论。
type Verdict struct {
	// Completed 为真表示该待办已完成（可标 settled）
	Completed bool
	// HeadMoved 为真表示会话开工时钉定的 head 已推进，本轮作废（不烧重试会话）
	HeadMoved bool
	// Reason 是判定依据（写进日志，操作者据此理解为什么算/不算完成）
	Reason string
}

// Verify 是完成判定的统一入口：PR 走 head 锚定的评审判定，Issue 走分诊标签判定。
//
// 「看 Gitea 原生状态而不是解析会话输出」是本系统的硬约定——模型说自己做完了
// 不算数，reviewer 名下真的出现新 review、或分诊标签真的被移除才算数。
func Verify(
	ctx context.Context,
	api API,
	repository status.Repository,
	reviewer string,
	kind string,
	number int64,
	since time.Time,
	expectedHead string,
) (Verdict, error) {
	if kind == KindPull {
		verdict, err := VerifyPullReview(ctx, api, repository, reviewer, number, since, expectedHead)
		if err != nil {
			return Verdict{}, err
		}
		return Verdict{Completed: verdict.Completed, HeadMoved: verdict.HeadMoved, Reason: verdict.Reason}, nil
	}
	verdict, err := VerifyIssueTriage(ctx, api, repository, number)
	if err != nil {
		return Verdict{}, err
	}
	return Verdict{Completed: verdict.Completed, Reason: verdict.Reason}, nil
}
