package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"assistant/internal/config"
	"assistant/internal/dispatcher"
	"assistant/internal/instances"
	"assistant/internal/repoinstall"
	"assistant/internal/status"

	"github.com/spf13/cobra"
)

// runDoctor 执行两级体检：本地检出（repoinstall 管理产物）与仓库服务端
// （分支保护/标签/协作者/merge 令牌）。服务端检查在拿不到实例配置或凭据时跳过。
func runDoctor(
	command *cobra.Command,
	options *repoToolOptions,
	requiredApprovals int64,
	allowAdminOverride bool,
) error {
	stdout := command.OutOrStdout()
	local, err := repoinstall.Doctor(options.repoOptions(command))
	if err != nil {
		return err
	}
	problems := printLocalFindings(stdout, local)

	server, target, err := auditServer(command, options, requiredApprovals, allowAdminOverride)
	if err != nil {
		return err
	}
	if target != "" {
		fmt.Fprintf(stdout, "# 服务端检查目标：%s\n", target)
	}
	problems += printServerFindings(stdout, server)
	if problems > 0 {
		return fmt.Errorf("发现 %d 处配置问题（本地：assistant install；服务端：assistant setup / actions）", problems)
	}
	return nil
}

func printLocalFindings(stdout io.Writer, findings []repoinstall.Finding) int {
	problems := 0
	for _, finding := range findings {
		if !finding.OK() {
			problems++
		}
		if finding.Detail != "" {
			fmt.Fprintf(stdout, "%-9s %s — %s\n", strings.ToUpper(finding.Status), finding.Path, finding.Detail)
		} else {
			fmt.Fprintf(stdout, "%-9s %s\n", strings.ToUpper(finding.Status), finding.Path)
		}
	}
	return problems
}

func printServerFindings(stdout io.Writer, findings []status.AuditFinding) int {
	problems := 0
	for _, finding := range findings {
		if !finding.OK() {
			problems++
		}
		path := "server: " + finding.Path
		if finding.Detail != "" {
			fmt.Fprintf(stdout, "%-9s %s — %s\n", strings.ToUpper(finding.Status), path, finding.Detail)
		} else {
			fmt.Fprintf(stdout, "%-9s %s\n", strings.ToUpper(finding.Status), path)
		}
	}
	return problems
}

// auditServer 定位当前仓库对应的实例与凭据后执行服务端体检。返回 findings 与
// 检查目标（host/owner/repo，空表示未定位到）；未定位到时以 SKIPPED finding
// 说明原因，而不是报错。
func auditServer(
	command *cobra.Command,
	options *repoToolOptions,
	requiredApprovals int64,
	allowAdminOverride bool,
) ([]status.AuditFinding, string, error) {
	ctx := command.Context()
	configPath, err := command.Flags().GetString("config")
	if err != nil {
		return nil, "", err
	}
	repositoryFlag, err := command.Flags().GetString("repo")
	if err != nil {
		return nil, "", err
	}
	_, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return nil, "", err
	}

	// 目标定位：配置优先（--repo 或 remote host 匹配实例）；否则逐个探测 remote
	// 是否为 Gitea，GitHub/GitLab 等会被跳过
	host, fullName, fromConfig := resolveServerTarget(ctx, file, repositoryFlag, options.Dir)
	if fullName == "" || host == "" {
		return []status.AuditFinding{{
			Path:   "服务端",
			Status: status.AuditStatusSkipped,
			Detail: "未定位到 Gitea 实例（多个 remote 会逐个探测 /api/v1/version；可用 --repo 或 config.json 指定）",
		}}, "", nil
	}
	repository, err := parseRepository(fullName)
	if err != nil {
		return nil, "", err
	}

	// 凭据：实例配置优先（管理员令牌可做全部检查），否则 env 单实例模式
	var token, reviewer, merger string
	if fromConfig {
		instance, ok := findInstance(file, host, fullName)
		if !ok {
			return nil, "", nil
		}
		host = instance.Host
		reviewer, merger = instance.Reviewer.Name, instance.Merger.Name
		var adminErr error
		token, adminErr = adminTokenForInstance(ctx, instance, func(string, ...any) {})
		if adminErr != nil {
			token = instance.Reviewer.Token
		}
	} else {
		configuration, loadErr := config.Load(os.Getenv)
		if loadErr != nil {
			return []status.AuditFinding{{
				Path:   "服务端",
				Status: status.AuditStatusSkipped,
				Detail: fmt.Sprintf("已定位 %s/%s，但缺少凭据（GITEA_HOST/GITEA_ACCESS_TOKEN 或 config.json）", host, fullName),
			}}, host + "/" + fullName, nil
		}
		host = configuration.Host
		token = configuration.AccessToken
	}
	if token == "" {
		return []status.AuditFinding{{
			Path:   "服务端",
			Status: status.AuditStatusSkipped,
			Detail: fmt.Sprintf("已定位 %s/%s，但缺少可用令牌", host, fullName),
		}}, host + "/" + fullName, nil
	}
	client, err := status.NewClient(host, token)
	if err != nil {
		return nil, "", err
	}
	findings, err := client.AuditRepository(ctx, repository, status.AuditOptions{
		Reviewer:           reviewer,
		Merger:             merger,
		RequiredApprovals:  requiredApprovals,
		AllowAdminOverride: allowAdminOverride,
	})
	if err != nil {
		return nil, "", err
	}
	return findings, host + "/" + fullName, nil
}

// resolveServerTarget 定位服务端体检的 (host, fullName)：配置优先（--repo 直接
// 命中仓库，或 remote host+仓库命中实例）；配置未命中时逐个探测 remote 是否为
// Gitea。返回 fromConfig 表示目标来自 config.json。
func resolveServerTarget(
	ctx context.Context,
	file *instances.File,
	repositoryFlag, dir string,
) (host, fullName string, fromConfig bool) {
	return resolveServerTargetWithProbe(file, repositoryFlag, dir, func(candidate string) bool {
		return status.ProbeGitea(ctx, candidate)
	})
}

// resolveServerTargetWithProbe 是 resolveServerTarget 的可注入实现（测试用）。
func resolveServerTargetWithProbe(
	file *instances.File,
	repositoryFlag, dir string,
	probe func(host string) bool,
) (host, fullName string, fromConfig bool) {
	remotes := dispatcher.ListRemotes(dir)
	if file != nil {
		if repositoryFlag != "" {
			for _, instance := range file.Instances {
				for _, repo := range instance.Repos {
					if repo.Name == repositoryFlag {
						return instance.Host, repositoryFlag, true
					}
				}
			}
		}
		for _, remote := range remotes {
			for _, instance := range file.Instances {
				if !sameHost(instance.Host, remote.Host) {
					continue
				}
				for _, repo := range instance.Repos {
					if repo.Name == remote.Repository {
						return instance.Host, remote.Repository, true
					}
				}
			}
		}
	}

	remote, ok := dispatcher.SelectGiteaRemote(dir, probe)
	if !ok {
		return "", repositoryFlag, false
	}
	if repositoryFlag != "" {
		return remote.Host, repositoryFlag, false
	}
	return remote.Host, remote.Repository, false
}

// sameHost 比较站点地址（忽略尾斜杠与大小写）。
func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
}

// findInstance 按站点（可空）与仓库全名定位实例配置。
func findInstance(file *instances.File, host, fullName string) (instances.Instance, bool) {
	normalizedHost := strings.TrimRight(host, "/")
	for _, instance := range file.Instances {
		if normalizedHost != "" && strings.TrimRight(instance.Host, "/") != normalizedHost {
			continue
		}
		for _, repo := range instance.Repos {
			if repo.Name == fullName {
				return instance, true
			}
		}
	}
	return instances.Instance{}, false
}
