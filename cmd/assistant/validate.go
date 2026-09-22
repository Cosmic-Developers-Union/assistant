package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"assistant/internal/claudecfg"
	"assistant/internal/credentials"
	"assistant/internal/instances"
	"assistant/internal/provider"

	"github.com/spf13/cobra"
)

// newValidateCommand 校验用户配置：config.json（平台、仓库、provider、微信桥）与
// 同目录的 credentials.json（登录身份与用途令牌）。只读、不联网、不改文件——改完
// 配置、重启 daemon 之前先跑一遍就知道能不能起来。
func newValidateCommand(configFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "校验 config.json 与 credentials.json（平台/仓库/provider/凭据），只读不联网",
		Long: "逐项校验用户配置，逐行给出结论与修复建议：\n" +
			"  - config.json：JSON 合法性、平台与仓库、默认 provider、微信桥；\n" +
			"  - provider：被选中的 provider 能否解析、有没有凭据（api_key/auth_token）、\n" +
			"    端点与模型、以及**定义了却没被引用**的 provider（最常见的配置失误）；\n" +
			"  - credentials.json：每个平台是否有登录身份，review/merge/admin/mcp 用途令牌是否齐。\n" +
			"不联网、不修改任何文件；有问题时以退出码 1 结束。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runValidate(command.OutOrStdout(), *configFlag)
		},
	}
}

// validateFinding 是一行校验结论：ERROR 计入退出码，WARN/SKIP 只提示。
type validateFinding struct {
	status  string
	subject string
	detail  string
}

func runValidate(stdout io.Writer, configPath string) error {
	path, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return err // instances.Load 的报错已带文件与字段位置
	}
	if file == nil {
		return fmt.Errorf("没有 config.json（--config / ASSISTANT_CONFIG / 平台标准配置目录）：先 assistant login <host>")
	}
	return reportValidate(stdout, validateConfigFile(path, file), "", path)
}

// reportValidate 打印校验结论：有 ERROR 时返回错误（written 非空表示配置刚刚写入，
// 措辞里说明「已写入但有问题」）。WARN/SKIP 只提示，不影响退出码。
func reportValidate(stdout io.Writer, findings []validateFinding, written, path string) error {
	problems, warnings := 0, 0
	for _, finding := range findings {
		switch finding.status {
		case "ERROR":
			problems++
		case "WARN":
			warnings++
		}
		printValidateFinding(stdout, finding)
	}
	if problems > 0 {
		if written != "" {
			return fmt.Errorf("配置已写入 %s，但校验发现 %d 处问题（见上面的 ERROR 行）", written, problems)
		}
		return fmt.Errorf("发现 %d 处配置问题（见上面的 ERROR 行）", problems)
	}
	summary := fmt.Sprintf("校验通过：%s 与 credentials.json 可用于 assistant run", path)
	if warnings > 0 {
		summary += fmt.Sprintf("（另有 %d 条提示，不阻断启动）", warnings)
	}
	fmt.Fprintf(stdout, "%-9s %s\n", "OK", summary)
	return nil
}

func printValidateFinding(stdout io.Writer, finding validateFinding) {
	if finding.detail == "" {
		fmt.Fprintf(stdout, "%-9s %s\n", finding.status, finding.subject)
		return
	}
	fmt.Fprintf(stdout, "%-9s %s — %s\n", finding.status, finding.subject, finding.detail)
}

// validateConfigFile 逐项校验配置本体（不联网、不写文件）。
func validateConfigFile(path string, file *instances.File) []validateFinding {
	findings := []validateFinding{{
		status:  "OK",
		subject: path,
		detail: fmt.Sprintf("%d 个平台、%d 个仓库、%d 个 provider 定义",
			len(file.Instances), configRepoCount(file), len(definedProviders(file))),
	}}

	// provider 引用：default_provider / instance.provider / repo.provider / weixin.provider
	referenced := map[string]string{}
	addReference := func(name, source string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := referenced[name]; !ok {
			referenced[name] = source
		}
	}
	addReference(file.DefaultProvider, "default_provider")
	for _, instance := range file.Instances {
		addReference(instance.Provider, "instance "+instance.Host)
		for _, repo := range instance.Repos {
			addReference(repo.Provider, "repo "+repo.Name)
		}
	}
	if file.Weixin != nil {
		addReference(file.Weixin.Provider, "weixin.provider")
	}
	for _, name := range file.AgentNames() {
		if agent, ok := file.Agents[name]; ok {
			addReference(agent.Provider, "agents."+name)
		}
	}
	for _, name := range sortedKeys(referenced) {
		source := referenced[name]
		overrides, err := file.EffectiveOverrides(name)
		if err != nil {
			findings = append(findings, validateFinding{"ERROR", "providers." + name,
				fmt.Sprintf("%v（被 %s 引用）", err, source)})
			continue
		}
		detail := providerSummary(name, overrides)
		if credential := claudecfg.CredentialSource(overrides); credential != "" {
			findings = append(findings, validateFinding{"OK", "providers." + name,
				detail + "；凭据：" + credential + "（被 " + source + " 引用）"})
			continue
		}
		findings = append(findings, validateFinding{"ERROR", "providers." + name,
			detail + "；没有 api_key/auth_token——" + claudecfg.MissingCredentialHint})
	}
	for _, name := range definedProviders(file) {
		if _, ok := referenced[name]; ok {
			continue
		}
		findings = append(findings, validateFinding{"WARN", "providers." + name,
			"已定义但没有任何地方引用它（" + providerReferenceHint() + "），当前不生效"})
	}
	if len(referenced) == 0 {
		if credential := claudecfg.CredentialSource(claudecfg.Overrides{}); credential != "" {
			findings = append(findings, validateFinding{"OK", "providers",
				"没有选中 provider，使用进程环境凭据（" + credential + "）"})
		} else {
			findings = append(findings, validateFinding{"ERROR", "providers",
				"没有选中任何 provider，进程环境也没有 AI 凭据——" + claudecfg.MissingCredentialHint})
		}
	}

	// 平台与仓库
	for _, instance := range file.Instances {
		detail := fmt.Sprintf("%d 个仓库", len(instance.Repos))
		status := "OK"
		if len(instance.Repos) == 0 {
			status = "WARN"
			detail = "没有配置仓库（assistant setup --repos 或 config.json 的 repos）"
		}
		if instance.Provider != "" {
			detail += "；provider=" + instance.Provider
		}
		findings = append(findings, validateFinding{status, "instance " + instance.Host, detail})
		for _, repo := range instance.Repos {
			dir := strings.TrimSpace(repo.Dir)
			if dir == "" {
				continue
			}
			info, err := os.Stat(dir)
			switch {
			case err != nil || !info.IsDir():
				findings = append(findings, validateFinding{"WARN", "repo " + repo.Name,
					fmt.Sprintf("dir=%s 在本地不是目录（容器部署请确认该路径同样挂载）", dir)})
			default:
				if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
					findings = append(findings, validateFinding{"WARN", "repo " + repo.Name,
						fmt.Sprintf("dir=%s 不是 git 检出（评审需要能 fetch/worktree 的检出）", dir)})
				}
			}
		}
	}

	// $schema：相对路径按 config.json 自身位置解析（编辑器也这么解析）
	if reference := strings.TrimSpace(file.Schema); reference != "" {
		if strings.Contains(reference, "://") {
			findings = append(findings, validateFinding{"OK", "$schema", reference})
		} else if _, err := os.Stat(filepath.Join(filepath.Dir(path), reference)); err != nil {
			findings = append(findings, validateFinding{"WARN", "$schema",
				reference + " 指向的 schema 文件不存在：编辑器补全与悬停文档不可用（assistant config init 会写一份）"})
		} else {
			findings = append(findings, validateFinding{"OK", "$schema", reference + "（编辑器补全与悬停文档）"})
		}
	}

	// 微信桥
	if file.Weixin == nil {
		findings = append(findings, validateFinding{"SKIP", "weixin",
			"未配置：微信对话桥不会启动（需要时 assistant weixin login）"})
	} else {
		weixin := file.Weixin
		detail := "enabled=" + fmt.Sprintf("%t", weixin.Enabled)
		if weixin.Provider != "" {
			detail += "；provider=" + weixin.Provider
		}
		if weixin.Agent != "" {
			detail += "；agent=" + weixin.Agent
		}
		if strings.TrimSpace(weixin.BotToken) == "" {
			findings = append(findings, validateFinding{"ERROR", "weixin",
				detail + "；缺少 bot_token——先 assistant weixin login"})
		} else if !weixin.Enabled {
			findings = append(findings, validateFinding{"WARN", "weixin",
				detail + "；enabled=false：assistant run 不会启动对话桥（--weixin 可强制开启）"})
		} else {
			findings = append(findings, validateFinding{"OK", "weixin", detail + "；凭据已写入"})
		}
	}

	// QQ 通道
	if file.QQ == nil {
		findings = append(findings, validateFinding{"SKIP", "qq",
			"未配置：QQ 对话桥不会启动（在 q.qq.com 开放平台创建机器人后把 app_id/app_secret 写入 qq 节）"})
	} else {
		qqConfig := file.QQ
		detail := "enabled=" + fmt.Sprintf("%t", qqConfig.Enabled)
		if qqConfig.Agent != "" {
			detail += "；agent=" + qqConfig.Agent
		}
		switch {
		case qqConfig.AppID == "" || strings.TrimSpace(qqConfig.AppSecret) == "":
			findings = append(findings, validateFinding{"ERROR", "qq",
				detail + "；缺少 app_id/app_secret——在 q.qq.com 开放平台创建机器人后填入"})
		case !qqConfig.Enabled:
			findings = append(findings, validateFinding{"WARN", "qq",
				detail + "；enabled=false：assistant run 不会启动 QQ 通道（--qq 可强制开启）"})
		default:
			findings = append(findings, validateFinding{"OK", "qq", detail + "；凭据已写入"})
		}
	}

	findings = append(findings, validateProviderDirectory(path)...)
	return append(findings, validateCredentials(path, file)...)
}

// validateProviderDirectory 指出遗留的 <配置目录>/providers/*.json：provider 现在
// 只有 config.json 一个落点，这些文件不再被读取——必须把定义搬进去，否则会被忽略。
func validateProviderDirectory(configPath string) []validateFinding {
	directory := filepath.Join(filepath.Dir(configPath), "providers")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(name, ".json"))
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return []validateFinding{{"ERROR", directory, fmt.Sprintf(
		"%s 已不再被读取：把定义挪进 config.json 的 providers（provider 只有 config.json 这一个落点）",
		strings.Join(names, "、"))}}
}

// validateCredentials 检查每个平台的登录身份与用途令牌（只看有无，不打印令牌）。
func validateCredentials(configPath string, file *instances.File) []validateFinding {
	if len(file.Instances) == 0 {
		return nil
	}
	path, err := credentials.PathFor(configPath)
	if err != nil {
		return []validateFinding{{"ERROR", "credentials.json", err.Error()}}
	}
	store, err := credentials.Load(path)
	if err != nil {
		return []validateFinding{{"ERROR", "credentials.json", "解析失败：" + err.Error()}}
	}
	findings := make([]validateFinding, 0, len(file.Instances))
	for _, instance := range file.Instances {
		host := instance.Host
		identity, ok := store.IdentityFor(host)
		if !ok {
			findings = append(findings, validateFinding{"ERROR", "credentials " + host,
				"没有登录身份：assistant login " + host})
			continue
		}
		who := identity.User
		if identity.IsAdmin {
			who += "（管理员）"
		}
		have := []string{}
		missingRequired := []string{}
		missingOptional := []string{}
		for _, purpose := range credentials.Purposes() {
			if _, ok, _ := store.CredentialFor(host, purpose); ok {
				have = append(have, purpose)
				continue
			}
			switch purpose {
			case "review", "merge":
				// 有仓库才会派生机器人令牌；只登录过的平台（还没 setup）不算错
				if len(instance.Repos) > 0 {
					missingRequired = append(missingRequired, purpose)
				} else {
					missingOptional = append(missingOptional, purpose)
				}
			default:
				missingOptional = append(missingOptional, purpose)
			}
		}
		detail := "身份 " + who
		if len(have) > 0 {
			detail += "；令牌：" + strings.Join(have, "/")
		}
		switch {
		case len(missingRequired) > 0:
			findings = append(findings, validateFinding{"ERROR", "credentials " + host,
				detail + "；缺 " + strings.Join(missingRequired, "/") + " 用途令牌——assistant setup --host " + host})
		case len(missingOptional) > 0:
			findings = append(findings, validateFinding{"WARN", "credentials " + host,
				detail + "；缺 " + strings.Join(missingOptional, "/") +
					"（admin：setup/doctor；mcp：本机编辑器 MCP；review/merge：setup 派生）"})
		default:
			findings = append(findings, validateFinding{"OK", "credentials " + host, detail})
		}
	}
	return findings
}

// providerSummary 汇总生效覆盖里可读的几项（不打印值以外的任何密钥）。
func providerSummary(name string, overrides claudecfg.Overrides) string {
	parts := []string{}
	if provider.HasPreset(name) {
		parts = append(parts, "内置预设")
	}
	if base := strings.TrimSpace(overrides.Env["ANTHROPIC_BASE_URL"]); base != "" {
		parts = append(parts, "端点 "+base)
	}
	if model := strings.TrimSpace(overrides.Env["ANTHROPIC_MODEL"]); model != "" {
		parts = append(parts, "模型 "+model)
	}
	env, settings, mcp := overrides.Counts()
	parts = append(parts, fmt.Sprintf("env %d / settings %d / mcp %d", env, settings, mcp))
	return strings.Join(parts, "；")
}

func providerReferenceHint() string {
	return "default_provider / instance.provider / repo.provider / weixin.provider"
}

// definedProviders 返回 config.json 里定义过的 provider 名，排序。
func definedProviders(file *instances.File) []string {
	names := make([]string, 0, len(file.Providers))
	for name := range file.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func configRepoCount(file *instances.File) int {
	count := 0
	for _, instance := range file.Instances {
		count += len(instance.Repos)
	}
	return count
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
