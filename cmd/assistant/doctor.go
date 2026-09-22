package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"assistant/internal/claudecfg"
	"assistant/internal/config"
	"assistant/internal/credentials"
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
	configPath string,
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
	problems += printSessionFindings(stdout, configPath)

	server, target, err := auditServer(command, configPath, options, requiredApprovals, allowAdminOverride)
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

// printSessionFindings 体检「会话能不能跑起来」的前提（与仓库内容无关）：claude
// 可执行文件与版本、assistant 托管的会话配置根、config.json 里生效 provider 的
// AI 凭据来源。凭据不再来自 ~/.claude 登录态，所以缺了必须显式报出来。
func printSessionFindings(stdout io.Writer, configPath string) int {
	problems := 0
	bin := "claude"
	if resolved, err := exec.LookPath(bin); err != nil {
		fmt.Fprintf(stdout, "%-9s 会话运行时 claude — 找不到 %q：装 claude，或用 --claude-bin / DISPATCH_CLAUDE_BIN 指绝对路径\n",
			"MISSING", bin)
		problems++
	} else if version := claudecfg.ClaudeVersion(bin); version != "" {
		fmt.Fprintf(stdout, "%-9s 会话运行时 claude — %s（%s）\n", "OK", version, resolved)
	} else {
		fmt.Fprintf(stdout, "%-9s 会话运行时 claude — %s（读不出版本，可能不是 Claude Code CLI）\n", "OK", resolved)
	}
	if directory, err := instances.ClaudeDir(); err != nil {
		fmt.Fprintf(stdout, "%-9s 会话运行时配置根 — %v\n", "MISSING", err)
		problems++
	} else if err := os.MkdirAll(directory, 0o755); err != nil {
		fmt.Fprintf(stdout, "%-9s 会话运行时配置根 — %s 不可写：%v\n", "MISSING", directory, err)
		problems++
	} else {
		fmt.Fprintf(stdout, "%-9s 会话运行时配置根 — %s（会话记录与 claude 全局配置；不是 ~/.claude）\n", "OK", directory)
	}
	_, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil || file == nil {
		fmt.Fprintf(stdout, "%-9s 会话 AI 凭据 — 没有 config.json，跳过（先 assistant login）\n", "SKIPPED")
		return problems
	}
	name := file.DefaultProvider
	label := name
	if label == "" {
		label = "内置缺省"
	}
	overrides, err := file.EffectiveOverrides(name)
	if err != nil {
		fmt.Fprintf(stdout, "%-9s 会话 AI 凭据 — provider %s 解析失败：%v\n", "UNMANAGED", label, err)
		return problems + 1
	}
	if source := claudecfg.CredentialSource(overrides); source != "" {
		fmt.Fprintf(stdout, "%-9s 会话 AI 凭据 — provider %s（%s）\n", "OK", label, source)
		return problems
	}
	fmt.Fprintf(stdout, "%-9s 会话 AI 凭据 — provider %s 没有 api_key：%s\n",
		"MISSING", label, claudecfg.MissingCredentialHint)
	return problems + 1
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
	configPath string,
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

	// 凭据：config.json 模式用该实例的 admin 用途令牌（凭据库）；env 单实例模式
	// 用 GITEA_HOST/GITEA_ACCESS_TOKEN
	var token, reviewer, merger string
	if fromConfig {
		instance, ok := findInstanceByHost(file, host)
		if !ok {
			return nil, "", nil
		}
		host = instance.Host
		reviewer, merger = instance.Reviewer.Name, instance.Merger.Name
		admin, found, credentialErr := credentialFor(configPath, host, credentials.PurposeAdmin)
		if credentialErr != nil {
			return nil, "", credentialErr
		}
		if found {
			token = admin.Token
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
		views := giteaViews(file)
		if repositoryFlag != "" {
			for _, instance := range views {
				if _, ok := instance.FindRepo(repositoryFlag); ok {
					return instance.Host, repositoryFlag, true
				}
			}
		}
		// remote 与通道 host 匹配：优先仓库也已登记的通道，其次 host 命中即可
		// （login 只表达作者身份，当前仓库未必在 repos[] 里）
		for _, remote := range remotes {
			for _, instance := range views {
				if !sameHost(instance.Host, remote.Host) {
					continue
				}
				if _, ok := instance.FindRepo(remote.Repository); ok {
					return instance.Host, remote.Repository, true
				}
			}
		}
		for _, remote := range remotes {
			for _, instance := range views {
				if sameHost(instance.Host, remote.Host) {
					return instance.Host, remote.Repository, true
				}
			}
		}
		if repositoryFlag != "" && len(views) == 1 {
			return views[0].Host, repositoryFlag, true
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

// findInstanceByHost 按站点定位 gitea 通道（折算成旧 Instance 视图）：当前仓库
// 可以不在 repos[] 中——login/setup 写入的 admin 凭据表达的是平台（作者）身份，
// doctor 用它检查当前仓库的服务端配置。
func findInstanceByHost(file *instances.File, host string) (instances.Instance, bool) {
	if channel, ok := findGiteaChannel(file, host); ok {
		return channelInstance(channel), true
	}
	return instances.Instance{}, false
}
