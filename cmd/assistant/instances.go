package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"assistant/internal/credentials"
	"assistant/internal/instances"
	"assistant/internal/status"
)

// resolveInstanceFile 解析多实例配置文件，按优先级查找：
//  1. --config
//  2. ASSISTANT_CONFIG
//  3. 平台标准配置目录 <UserConfigDir>/Cosmic-Developers-Union/assistant/config.json
//
// 不读取当前目录 config.json：仓库检出里同名文件（示例、脚本产物）会被意外
// 当成运行配置，行为随 cwd 漂移。都没有时返回 nil，调用方退回环境变量单实例模式。
func resolveInstanceFile(options commandOptions) (string, *instances.File, error) {
	path := strings.TrimSpace(options.ConfigPath)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("ASSISTANT_CONFIG"))
	}
	if path == "" {
		standard, err := instances.DefaultConfigPath()
		if err != nil {
			return "", nil, nil
		}
		if _, err := os.Stat(standard); err == nil {
			path = standard
		} else if !os.IsNotExist(err) {
			return "", nil, err
		}
	}
	if path == "" {
		return "", nil, nil
	}
	file, err := instances.Load(path)
	if err != nil {
		// 空骨架（config new 生成、尚未填写）当作没有配置文件：退回环境变量
		// 单实例模式——token 走 GITEA_HOST/GITEA_ACCESS_TOKEN 即可跑，填好
		// instances 后配置文件自然接管。其余错误（语法坏、语义错）照报。
		if skeleton, parseErr := instances.Parse(path); parseErr == nil &&
			len(skeleton.Instances) == 0 && skeleton.Weixin == nil {
			return "", nil, nil
		}
		return "", nil, err
	}
	return path, file, nil
}

// setupConfigWritePath 决定 setup/login 写配置的落点：--config / ASSISTANT_CONFIG
// 显式指定优先；否则用平台标准配置目录（不存在则创建）。不落当前目录，避免
// 检出内出现运行配置。
func setupConfigWritePath(configPath string) (string, error) {
	if path := strings.TrimSpace(configPath); path != "" {
		return path, nil
	}
	if path := strings.TrimSpace(os.Getenv("ASSISTANT_CONFIG")); path != "" {
		return path, nil
	}
	return instances.DefaultConfigPath()
}

// instanceManagers 按 instance × repo 构造 Manager。repoFilter 为空时，仓库
// 清单为空的 instance 不限定仓库（同步令牌可见的全部仓库）；否则逐个仓库限定。
func instanceManagers(
	ctx context.Context,
	configPath string,
	file *instances.File,
	repoFilter string,
	stderr io.Writer,
) ([]*status.Manager, error) {
	var managers []*status.Manager
	matchedFilter := repoFilter == ""
	for _, instance := range file.Instances {
		logf := func(format string, arguments ...any) {
			fmt.Fprintf(stderr, "[%s] %s\n", instance.Host, fmt.Sprintf(format, arguments...))
		}
		repos := slices.Clone(instance.Repos)
		if repoFilter != "" {
			repos = slices.DeleteFunc(repos, func(repo instances.Repo) bool { return repo.Name != repoFilter })
			if len(repos) == 0 {
				continue
			}
			matchedFilter = true
		}
		if len(repos) == 0 {
			manager, err := newInstanceManager(ctx, configPath, instance, instances.Repo{}, logf)
			if err != nil {
				return nil, err
			}
			managers = append(managers, manager)
			continue
		}
		for _, repo := range repos {
			manager, err := newInstanceManager(ctx, configPath, instance, repo, logf)
			if err != nil {
				return nil, err
			}
			managers = append(managers, manager)
		}
	}
	if !matchedFilter {
		return nil, fmt.Errorf("仓库 %s 不在配置文件的 instances[].repos 中", repoFilter)
	}
	if len(managers) == 0 {
		return nil, fmt.Errorf("配置文件没有可执行的仓库")
	}
	return managers, nil
}

func newInstanceManager(
	ctx context.Context,
	configPath string,
	instance instances.Instance,
	repo instances.Repo,
	logf func(string, ...any),
) (*status.Manager, error) {
	review, err := tokenForPurpose(configPath, instance.Host, credentials.PurposeReview)
	if err != nil {
		return nil, err
	}
	client, err := status.NewClient(instance.Host, review.Token)
	if err != nil {
		return nil, err
	}
	// 分支保护端点要求 repo admin：有 admin 用途令牌时单独配给这一处读取；
	// 没有时读取被拒会按严格模式降级（不阻断其余能力）
	if admin, ok, credentialErr := credentialFor(configPath, instance.Host, credentials.PurposeAdmin); credentialErr != nil {
		return nil, credentialErr
	} else if ok && admin.Token != review.Token {
		if err := client.UseBranchProtectionToken(admin.Token); err != nil {
			return nil, err
		}
		logf("分支保护读取使用 admin 令牌（@%s）", admin.User)
	} else if !ok {
		logf("没有 admin 用途令牌：分支保护读取将回退严格模式（管理员账号运行 assistant login 可补）")
	}
	managerOptions := []status.ManagerOption{status.WithProgress(logf)}
	if instance.Reviewer.Name != "" {
		managerOptions = append(managerOptions, status.WithContentReviewer(instance.Reviewer.Name))
	}
	// 状态评审（门禁驳回）以 merge 账号提交才是 official review
	if merge, ok, credentialErr := credentialFor(configPath, instance.Host, credentials.PurposeMerge); credentialErr != nil {
		return nil, credentialErr
	} else if ok {
		if err := client.UseStateReviewerToken(merge.Token); err != nil {
			return nil, err
		}
		managerOptions = append(managerOptions, status.WithStateReviewer(instance.Merger.Name))
	} else {
		logf("没有 merge 用途令牌：状态驳回将以基础令牌身份提交（运行 assistant setup 可补）")
	}
	if repo.Name != "" {
		owner, name, err := instances.ParseRepoName(repo.Name)
		if err != nil {
			return nil, err
		}
		managerOptions = append(managerOptions, status.WithRepository(status.Repository{Owner: owner, Name: name}))
	}
	return status.NewManager(client, managerOptions...), nil
}

// runManagerAction 对配置中的每个 instance × repo 执行同一动作，错误聚合返回。
func runManagerAction(
	ctx context.Context,
	stderr io.Writer,
	options commandOptions,
	file *instances.File,
	action func(context.Context, *status.Manager) error,
) error {
	managers, err := instanceManagers(ctx, options.ConfigPath, file, options.Repository, stderr)
	if err != nil {
		return err
	}
	var runErrors []error
	for _, manager := range managers {
		if err := action(ctx, manager); err != nil {
			runErrors = append(runErrors, err)
		}
	}
	return errors.Join(runErrors...)
}

// instanceChecker 把多个 Manager 的 Check 聚合成一份报告，供 check --wait 轮询。
type instanceChecker struct {
	managers []*status.Manager
}

func (c *instanceChecker) Check(ctx context.Context) (status.Report, error) {
	var report status.Report
	var checkErrors []error
	for _, manager := range c.managers {
		item, err := manager.Check(ctx)
		if err != nil {
			checkErrors = append(checkErrors, err)
			continue
		}
		report.NeedsTriage = append(report.NeedsTriage, item.NeedsTriage...)
		report.NeedsReview = append(report.NeedsReview, item.NeedsReview...)
	}
	return report, errors.Join(checkErrors...)
}
