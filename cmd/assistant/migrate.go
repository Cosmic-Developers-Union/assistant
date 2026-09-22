package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"assistant/internal/instances"
	"assistant/internal/runcfg"

	"github.com/spf13/cobra"
)

// configMigrateOptions 是 `assistant config migrate` 的参数。
type configMigrateOptions struct {
	RunPath string
	DryRun  bool
}

// newConfigMigrateCommand 导入旧 run.yaml：运行参数进 runtimes、monitor 站点
// 并入 gitea 通道、provider 简写转 providers。run.yaml 已并入 config.json，
// 这是旧文件的唯一读取入口。
func newConfigMigrateCommand(configFlag *string) *cobra.Command {
	options := &configMigrateOptions{}
	command := &cobra.Command{
		Use:   "migrate",
		Short: "导入旧 run.yaml 到 config.json（运行参数→runtime、monitor→gitea 通道）",
		Long: "run.yaml 已并入 config.json，本命令把旧文件迁进来后即可删除：\n" +
			"  1. root/repos-dir/review-*/sessions-*/state-* → runtimes.<名> 的运行参数；\n" +
			"  2. monitor.<host>.repos/token → gitea 通道（token 缺省回退凭据库 purpose=review）；\n" +
			"  3. provider.type/token → providers 条目 + default_provider。\n\n" +
			"旧文件改名 run.yaml.migrated 保留备查，config.json 备份为 config.json.bak。\n" +
			"没有 run.yaml 时无操作（instances 的迁移在载入时自动完成）。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runConfigMigrate(command, *configFlag, options)
		},
	}
	command.Flags().StringVar(&options.RunPath, "run", "", "旧 run.yaml 路径（缺省 ASSISTANT_RUN 或标准位置）")
	command.Flags().BoolVar(&options.DryRun, "dry-run", false, "只打印导入计划，不写任何文件")
	return command
}

func runConfigMigrate(command *cobra.Command, configPath string, options *configMigrateOptions) error {
	stdout := command.OutOrStdout()
	path, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return err
	}
	if file == nil {
		path, err = setupConfigWritePath(configPath)
		if err != nil {
			return err
		}
		file = &instances.File{}
	}

	// run.yaml 与 config.json 同目录共存：只在显式参数 / 环境变量 / config.json
	// 同目录找，绝不跨到其他配置目录（导入错根比找不到更糟）
	runPath := strings.TrimSpace(options.RunPath)
	if runPath == "" {
		runPath = strings.TrimSpace(os.Getenv("ASSISTANT_RUN"))
	}
	if runPath == "" {
		candidate := filepath.Join(filepath.Dir(path), runcfg.FileName)
		if _, statErr := os.Stat(candidate); statErr == nil {
			runPath = candidate
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
	}
	if runPath == "" {
		fmt.Fprintln(stdout, "没有找到旧 run.yaml：无需迁移（instances 的迁移在载入时自动完成）")
		return nil
	}
	legacy, err := runcfg.Load(runPath)
	if err != nil {
		return err
	}

	changes, err := mergeLegacyRun(file, legacy)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Fprintln(stdout, "旧 run.yaml 没有带来新内容：无需迁移")
		return nil
	}
	for _, change := range changes {
		fmt.Fprintf(stdout, "  %s\n", change)
	}
	if options.DryRun {
		fmt.Fprintln(stdout, "dry-run：未写入任何文件")
		return nil
	}
	if err := saveConfig(file, path); err != nil {
		return err
	}
	migratedPath := runPath + ".migrated"
	if err := os.Rename(runPath, migratedPath); err != nil {
		return fmt.Errorf("config.json 已写入，但 run.yaml 改名失败: %w", err)
	}
	fmt.Fprintf(stdout, "已导入 %s → %s（备份 %s.bak）；旧文件改名 %s\n", runPath, path, path, migratedPath)
	return nil
}

// mergeLegacyRun 把旧 run.yaml 的内容并进规范配置，返回变更清单。
func mergeLegacyRun(file *instances.File, legacy *runcfg.File) ([]string, error) {
	var changes []string

	// 运行参数 → runtime（缺省 main；已有 runtime 时只补空缺字段）
	runtime, err := file.ResolveRuntime("")
	if err != nil {
		return nil, err
	}
	target, hadRuntime := file.Runtimes[instances.DefaultRuntimeName]
	merge := func(label, current, value string) (string, bool) {
		if value == "" || (hadRuntime && current != "") {
			return current, false
		}
		return value, true
	}
	if runtime.MainAgent == "" {
		runtime.MainAgent = instances.DefaultMainAgent
	}
	if value, changed := merge("root", target.Root, legacy.Root); changed {
		target.Root, runtime.Root = value, value
		changes = append(changes, "+ runtimes.main.root = "+value)
	}
	if value, changed := merge("repos_dir", target.ReposDir, legacy.ReposDir); changed {
		target.ReposDir = value
		changes = append(changes, "+ runtimes.main.repos_dir = "+value)
	}
	if value, changed := merge("review_root", target.ReviewRoot, legacy.ReviewRoot); changed {
		target.ReviewRoot = value
		changes = append(changes, "+ runtimes.main.review_root = "+value)
	}
	if value, changed := merge("review_name_template", target.ReviewNameTemplate, legacy.ReviewNameTemplate); changed {
		target.ReviewNameTemplate = value
		changes = append(changes, "+ runtimes.main.review_name_template")
	}
	if value, changed := merge("sessions_dir", target.SessionsDir, legacy.SessionsDir); changed {
		target.SessionsDir = value
		changes = append(changes, "+ runtimes.main.sessions_dir = "+value)
	}
	if value, changed := merge("sessions_name_template", target.SessionsNameTemplate, legacy.SessionsNameTemplate); changed {
		target.SessionsNameTemplate = value
		changes = append(changes, "+ runtimes.main.sessions_name_template")
	}
	if value, changed := merge("state_dir", target.StateDir, legacy.StateDir); changed {
		target.StateDir = value
		changes = append(changes, "+ runtimes.main.state_dir = "+value)
	}
	if value, changed := merge("state_file", target.StateFile, legacy.StateFile); changed {
		target.StateFile = value
		changes = append(changes, "+ runtimes.main.state_file = "+value)
	}
	if file.Runtimes == nil {
		file.Runtimes = map[string]instances.Runtime{}
	}
	if _, exists := file.Runtimes[instances.DefaultRuntimeName]; !exists {
		file.Runtimes[instances.DefaultRuntimeName] = target
		if file.DefaultRuntime == "" {
			file.DefaultRuntime = instances.DefaultRuntimeName
		}
	}

	// monitor 站点 → gitea 通道（token 补缺、repos 并集）
	for host, monitor := range legacy.Monitor {
		channel := upsertGiteaChannel(file, host)
		if strings.TrimSpace(channel.Token) == "" && strings.TrimSpace(monitor.Token) != "" {
			channel.Token = strings.TrimSpace(monitor.Token)
			changes = append(changes, "+ channels["+channel.Key()+"].token（run.yaml monitor."+host+".token）")
		}
		known := map[string]bool{}
		for _, repo := range channel.Repos {
			known[repo.Name] = true
		}
		for _, repoName := range monitor.Repos {
			if known[repoName] {
				continue
			}
			channel.Repos = append(channel.Repos, instances.Repo{Name: repoName})
			changes = append(changes, "+ channels["+channel.Key()+"].repos "+repoName)
		}
	}

	// provider 简写 → providers + default_provider
	if legacy.Provider != nil && legacy.Provider.Type != "" {
		if _, exists := file.Providers[legacy.Provider.Type]; !exists {
			if file.Providers == nil {
				file.Providers = map[string]instances.Provider{}
			}
			entry := instances.Provider{APIKey: legacy.Provider.Token}
			file.Providers[legacy.Provider.Type] = entry
			changes = append(changes, "+ providers."+legacy.Provider.Type+"（run.yaml provider 简写）")
		}
		if file.DefaultProvider == "" {
			file.DefaultProvider = legacy.Provider.Type
			changes = append(changes, "+ default_provider = "+legacy.Provider.Type)
		}
	}
	return changes, nil
}
