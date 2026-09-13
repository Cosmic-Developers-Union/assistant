package main

import (
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

	server, err := auditServer(command, options, requiredApprovals, allowAdminOverride)
	if err != nil {
		return err
	}
	if server == nil {
		fmt.Fprintln(stdout, "SKIPPED   服务端 — 未找到实例配置或凭据（需要 config.json 或 GITEA_HOST/GITEA_ACCESS_TOKEN）")
	} else {
		problems += printServerFindings(stdout, server)
	}
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

// auditServer 定位当前仓库对应的实例与凭据后执行服务端体检；无法定位时返回
// (nil, nil) 表示跳过。
func auditServer(
	command *cobra.Command,
	options *repoToolOptions,
	requiredApprovals int64,
	allowAdminOverride bool,
) ([]status.AuditFinding, error) {
	ctx := command.Context()
	configPath, err := command.Flags().GetString("config")
	if err != nil {
		return nil, err
	}
	repositoryFlag, err := command.Flags().GetString("repo")
	if err != nil {
		return nil, err
	}

	// 仓库与服务端：--repo 优先，否则从 origin remote 推断
	fullName, host := repositoryFlag, ""
	if fullName == "" {
		url, ok := dispatcher.OriginRemote(options.Dir)
		if !ok {
			return nil, nil
		}
		remote, ok := dispatcher.ParseGitRemoteURL(url)
		if !ok {
			return nil, nil
		}
		fullName, host = remote.Repository, remote.Host
	}
	repository, err := parseRepository(fullName)
	if err != nil {
		return nil, err
	}

	// 凭据：实例配置优先（管理员令牌可做全部检查），否则 env 单实例模式
	var token, reviewer, merger string
	_, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return nil, err
	}
	if file != nil {
		instance, ok := findInstance(file, host, fullName)
		if !ok {
			return nil, nil
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
			return nil, nil
		}
		host = configuration.Host
		token = configuration.AccessToken
	}
	if token == "" {
		return nil, nil
	}
	client, err := status.NewClient(host, token)
	if err != nil {
		return nil, err
	}
	return client.AuditRepository(ctx, repository, status.AuditOptions{
		Reviewer:           reviewer,
		Merger:             merger,
		RequiredApprovals:  requiredApprovals,
		AllowAdminOverride: allowAdminOverride,
	})
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
