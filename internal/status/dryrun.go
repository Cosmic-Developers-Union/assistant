package status

import (
	"context"
	"fmt"
)

// DryRunAPI 转发全部读取，拦截每个写操作，执行同一门禁而不修改平台。
type DryRunAPI struct {
	API
	log  func(string, ...any)
	next int64
}

// NewDryRunAPI 让 CI 动作支持可审阅的只读演练。
func NewDryRunAPI(api API, log func(string, ...any)) *DryRunAPI {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &DryRunAPI{API: api, log: log, next: -1}
}
func (d *DryRunAPI) report(repo Repository, index int64, action string) {
	d.log("演练 %s#%d: %s", repo.FullName(), index, action)
}

// CloseIssue 只报告将关闭的条目。
func (d *DryRunAPI) CloseIssue(_ context.Context, r Repository, n int64) error {
	d.report(r, n, "关闭 Issue")
	return nil
}

// MergePullRequest 只报告门禁通过的 squash 合并。
func (d *DryRunAPI) MergePullRequest(_ context.Context, r Repository, n int64) error {
	d.report(r, n, "squash 合并")
	return nil
}

// ArmAutoMerge 报告武装，返回正常武装结果，不计实际合并。
func (d *DryRunAPI) ArmAutoMerge(_ context.Context, r Repository, n int64, head string) (AutoMergeArmResult, error) {
	d.report(r, n, "武装 auto-merge（head "+head+"）")
	return AutoMergeArmed, nil
}

// DisarmAutoMerge 只报告取消排定。
func (d *DryRunAPI) DisarmAutoMerge(_ context.Context, r Repository, n int64) (bool, error) {
	d.report(r, n, "撤销 auto-merge")
	return false, nil
}

// CreateIssueComment 只报告写评论，不输出正文。
func (d *DryRunAPI) CreateIssueComment(_ context.Context, r Repository, n int64, _ string) error {
	d.report(r, n, "写入评论")
	return nil
}

// SetLabelExclusive 只报告互斥设置。
func (d *DryRunAPI) SetLabelExclusive(_ context.Context, r Repository, n int64) error {
	d.report(r, n, "设置互斥标签")
	return nil
}

// CreateLabel 提供虚拟 id，让后续演练能继续推导标签。
func (d *DryRunAPI) CreateLabel(_ context.Context, r Repository, v LabelDefinition) (Label, error) {
	d.report(r, 0, "创建标签 "+v.Name)
	id := d.next
	d.next--
	return Label{ID: id, Name: v.Name, Exclusive: v.Exclusive}, nil
}

// DeleteLabel 只报告删除标签。
func (d *DryRunAPI) DeleteLabel(_ context.Context, r Repository, n int64) error {
	d.report(r, n, "删除仓库标签")
	return nil
}

// AddLabel 只报告添加标签。
func (d *DryRunAPI) AddLabel(_ context.Context, r Repository, n, id int64) error {
	d.report(r, n, fmt.Sprintf("添加标签 %d", id))
	return nil
}

// RemoveLabel 只报告摘除标签。
func (d *DryRunAPI) RemoveLabel(_ context.Context, r Repository, n, id int64) error {
	d.report(r, n, fmt.Sprintf("移除标签 %d", id))
	return nil
}

// CreatePullReview 只报告会签，不伪造平台结论。
func (d *DryRunAPI) CreatePullReview(_ context.Context, r Repository, n int64, v ReviewInput) error {
	d.report(r, n, "提交会签 "+string(v.State))
	return nil
}

// CreateReviewRequests 只报告请求登记。
func (d *DryRunAPI) CreateReviewRequests(_ context.Context, r Repository, n int64, _ []string) error {
	d.report(r, n, "登记评审请求")
	return nil
}

// DeleteReviewRequests 只报告请求撤回。
func (d *DryRunAPI) DeleteReviewRequests(_ context.Context, r Repository, n int64, _ []string) error {
	d.report(r, n, "撤回评审请求")
	return nil
}
