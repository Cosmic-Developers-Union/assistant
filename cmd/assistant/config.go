package main

import (
	"assistant/internal/config"
	"assistant/internal/credentials"
	"assistant/internal/instances"
	"assistant/schema"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// configInitOptions 是 `assistant config init` 的参数。
type configInitOptions struct {
	DryRun      bool
	Provider    string
	APIKeyStdin bool
}

// newConfigCommand 是 config.json 的维护入口：骨架生成（config new）、模板补全
// （config init）与随后的校验。配置文件的边界只有两个：config.json（用户手写）
// 与 credentials.json（assistant 管理），所以这里只做「生成 + 补全 + 校验」，
// 不做交互式编辑。
func newConfigCommand(configFlag *string) *cobra.Command {
	command := &cobra.Command{
		Use:   "config",
		Short: "维护用户配置 config.json（生成骨架、模板补全、校验）",
		Args:  cobra.NoArgs,
	}
	options := &configInitOptions{}
	initCommand := &cobra.Command{
		Use:   "init",
		Short: "把内置模板与当前 config.json 合并写回（只补缺失项，绝不覆盖已有值）",
		Long: "帮助你把 config.json 补成完整可用的形态：\n" +
			"  1. 补 $schema 并把 schema 写到 config.json 旁边（编辑器补全与悬停文档，离线可用）；\n" +
			"  2. 用 credentials.json 里已登录的平台预填 instances（reviewer=ai / merger=merge）；\n" +
			"  3. --provider <名字> 写入 vendors 桩与 default_provider，--api-key-stdin 顺带写密钥；\n" +
			"  4. 旧文件备份为 config.json.bak，原子写入 0600，写完立即校验。\n\n" +
			"已有值一律不动（只补缺失的键/段），重复执行无改动。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runConfigInit(command, *configFlag, options)
		},
	}
	flags := initCommand.Flags()
	flags.BoolVar(&options.DryRun, "dry-run", false, "只打印差异与合并结果，不写任何文件")
	flags.StringVar(&options.Provider, "provider", "", "写入该供应商并设为 default_provider（如 minimax）")
	flags.BoolVar(&options.APIKeyStdin, "api-key-stdin", false,
		"从 stdin 读取 --provider 的 api_key（终端下隐藏输入，不进 shell 历史）")
	command.AddCommand(initCommand)

	// newOptions 是 `assistant config new` 的参数。
	newOptions := &configNewOptions{}
	newCommand := &cobra.Command{
		Use:   "new",
		Short: "生成空配置骨架 config.json（$schema + 空 channels），首次部署起步用",
		Long: "在目标位置写一份最小可编辑的 config.json 骨架：\n" +
			"  {\"$schema\": \"./config.schema.json\", \"channels\": []}\n" +
			"  并把 schema 写到旁边（编辑器补全与悬停文档，离线可用）。\n\n" +
			"骨架还不能直接运行（channels 为空）；随后手工编辑 providers/channels/\n" +
			"runtimes，或 assistant login <host> / setup 之后用 config init 补全。\n" +
			"已有文件不覆盖（--force 才覆盖）。落点与 config init 一致：--config /\n" +
			"ASSISTANT_CONFIG / 当前目录。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runConfigNew(command, *configFlag, newOptions)
		},
	}
	newCommand.Flags().BoolVar(&newOptions.Force, "force", false, "覆盖已存在的 config.json")
	command.AddCommand(newCommand)
	command.AddCommand(newConfigMigrateCommand(configFlag))
	return command
}

// configNewOptions 是 `assistant config new` 的参数。
type configNewOptions struct {
	Force bool
}

// runConfigNew 写入空配置骨架：只有 $schema 与空 instances。这是新机器/新服务
// 用户的起步文件——不校验（空 instances 本就不可运行）、不合并、不备份。
func runConfigNew(command *cobra.Command, configPath string, options *configNewOptions) error {
	path, err := configTargetPath(configPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil && !options.Force {
		return fmt.Errorf("%s 已存在（--force 覆盖，或 assistant config init 在其基础上补全）", path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	// 骨架手工构造：struct 序列化会带出空的 optimizations 段，"空"名不副实
	skeleton, err := json.MarshalIndent(map[string]any{
		"$schema":  schema.Reference,
		"channels": []any{},
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := instances.SaveBytes(path, append(skeleton, '\n')); err != nil {
		return err
	}
	schemaPath, err := writeSchemaFile(path)
	if err != nil {
		return err
	}
	stdout := command.OutOrStdout()
	fmt.Fprintf(stdout, "已写入空配置骨架: %s（instances 为空，尚不能运行）\n", path)
	fmt.Fprintf(stdout, "schema: %s\n", schemaPath)
	fmt.Fprintf(stdout, "下一步：编辑 providers/instances，或 assistant login <host> / setup 后 "+
		"assistant config init 补全；完成用 assistant validate 校验\n")
	return nil
}

func runConfigInit(command *cobra.Command, configPath string, options *configInitOptions) error {
	path, err := configTargetPath(configPath)
	if err != nil {
		return err
	}
	stdout := command.OutOrStdout()
	existing, err := loadOptionalConfig(path)
	if err != nil {
		return fmt.Errorf("现有配置无法解析（先修好或手动备份）：%w", err)
	}
	file := existing
	if file == nil {
		file = &instances.File{}
	}

	changes := make([]string, 0, 4)
	if strings.TrimSpace(file.Schema) == "" {
		file.Schema = schema.Reference
		changes = append(changes, "+ $schema: "+schema.Reference)
	}
	prefilled, err := prefillInstances(path, file)
	if err != nil {
		return err
	}
	changes = append(changes, prefilled...)
	if name := strings.TrimSpace(options.Provider); name != "" {
		providerChanges, err := applyProviderOption(command, file, name, options)
		if err != nil {
			return err
		}
		changes = append(changes, providerChanges...)
	}
	file.Normalize()
	// 合并结果必须是可加载的：宁可不写，也不写坏
	if err := file.Validate(); err != nil {
		return fmt.Errorf("合并结果不合法，未写入：%w（先 assistant login <host>，或用 --provider 指定供应商）", err)
	}

	if options.DryRun {
		fmt.Fprintf(stdout, "dry-run：将写入 %s\n", path)
		printConfigChanges(stdout, changes)
		encoded, err := json.MarshalIndent(file, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s\n", encoded)
		return nil
	}

	if len(changes) > 0 {
		if existing != nil {
			if err := backupConfig(path); err != nil {
				return err
			}
		}
		if err := instances.Save(path, file); err != nil {
			return err
		}
		note := ""
		if existing != nil {
			note = "（旧文件备份为 " + filepath.Base(path) + ".bak）"
		}
		fmt.Fprintf(stdout, "已写入 %s%s\n", path, note)
		printConfigChanges(stdout, changes)
	} else {
		fmt.Fprintf(stdout, "配置已是最新（%s），无需补全\n", path)
	}
	if strings.TrimSpace(file.Schema) == schema.Reference {
		schemaPath, err := writeSchemaFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "schema：%s（$schema 指向它）\n", schemaPath)
	}
	return reportValidate(stdout, validateConfigFile(path, file), path, path)
}

func printConfigChanges(stdout io.Writer, changes []string) {
	if len(changes) == 0 {
		fmt.Fprintln(stdout, "  （无改动）")
		return
	}
	for _, change := range changes {
		fmt.Fprintln(stdout, "  "+change)
	}
}

// configTargetPath 返回要写入的配置文件路径：--config / ASSISTANT_CONFIG 优先，
// 否则平台标准位置（不存在也返回该位置——首次生成就写在那里）。
func configTargetPath(configPath string) (string, error) {
	if path := strings.TrimSpace(configPath); path != "" {
		return path, nil
	}
	if path := strings.TrimSpace(os.Getenv("ASSISTANT_CONFIG")); path != "" {
		return path, nil
	}
	return instances.DefaultConfigPath()
}

// loadOptionalConfig 读取现有配置；文件不存在返回 nil（首次生成）。空骨架
// （config new 的产物，instances 为空）不是可运行配置，但对补全有效：语义
// 校验失败时退回宽松解析，语法/结构错误仍然报出。
func loadOptionalConfig(path string) (*instances.File, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if _, err := config.LoadBeside(path); err != nil {
		return nil, err
	}
	file, err := instances.Load(path)
	if err == nil {
		return file, nil
	}
	return instances.Parse(path)
}

// prefillInstances 用 credentials.json 里已登录的平台补 instances：登录过就该出现在
// 配置里（reviewer/merger 是约定值 ai/merge）；已登记的平台不动。
func prefillInstances(configPath string, file *instances.File) ([]string, error) {
	storePath, err := credentials.Path()
	if err != nil {
		return nil, err
	}
	store, err := credentials.Load(storePath)
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", storePath, err)
	}
	if len(store.Identity) == 0 {
		return nil, nil
	}
	known := make(map[string]bool, len(file.Channels))
	for _, channel := range giteaChannels(file) {
		known[credentials.NormalizeHost(channel.Host)] = true
	}
	changes := make([]string, 0, len(store.Identity))
	for _, identity := range store.Identity {
		host := credentials.NormalizeHost(identity.Host)
		if host == "" || known[host] {
			continue
		}
		known[host] = true
		channel := upsertGiteaChannel(file, host)
		channel.Repos = []instances.Repo{}
		who := identity.User
		if identity.IsAdmin {
			who += "（管理员）"
		}
		changes = append(changes, fmt.Sprintf("+ gitea 通道 %s（credentials.json 里 %s 的登录身份）", host, who))
	}
	return changes, nil
}

// applyProviderOption 处理 --provider / --api-key-stdin：写 provider 桩、切默认
// provider、按需写入密钥（显式参数优先，所以 default_provider 会被切过去）。
func applyProviderOption(
	command *cobra.Command,
	file *instances.File,
	name string,
	options *configInitOptions,
) ([]string, error) {
	changes := make([]string, 0, 3)
	if file.Providers == nil {
		file.Providers = map[string]instances.Provider{}
	}
	provider, defined := file.Providers[name]
	if !defined {
		changes = append(changes, "+ providers."+name)
	}
	if file.DefaultProvider != name {
		changes = append(changes, fmt.Sprintf("~ default_provider: %q → %q", file.DefaultProvider, name))
		file.DefaultProvider = name
	}
	if options.APIKeyStdin {
		key, err := readAPIKey(command)
		if err != nil {
			return nil, err
		}
		if provider.APIKey != key {
			changes = append(changes, "+ providers."+name+".api_key（来自 stdin，不进 shell 历史）")
		}
		provider.APIKey = key
	}
	file.Providers[name] = provider
	return changes, nil
}

// readAPIKey 从 stdin 读密钥：终端下隐藏输入，管道下读一行。
func readAPIKey(command *cobra.Command) (string, error) {
	in := command.InOrStdin()
	if file, ok := in.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		fmt.Fprint(command.ErrOrStderr(), "api_key（输入不回显）: ")
		data, err := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(command.ErrOrStderr())
		if err != nil {
			return "", err
		}
		if key := strings.TrimSpace(string(data)); key != "" {
			return key, nil
		}
		return "", fmt.Errorf("--api-key-stdin 没有读到 api_key")
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	if key := strings.TrimSpace(line); key != "" {
		return key, nil
	}
	return "", fmt.Errorf("--api-key-stdin 没有读到 api_key（用法：echo -n <key> | assistant config init --provider minimax --api-key-stdin）")
}

// backupConfig 把现有配置原样备份为 <path>.bak（0600）。
func backupConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path+".bak", data, 0o600)
}

// writeSchemaFile 把随二进制分发的 schema 写到 config.json 旁边（0644）：离线可用，
// 且与当前版本一致。
func writeSchemaFile(configPath string) (string, error) {
	path := filepath.Join(filepath.Dir(configPath), schema.FileName)
	if err := os.WriteFile(path, schema.Config, 0o644); err != nil {
		return "", fmt.Errorf("写入 %s: %w", path, err)
	}
	return path, nil
}
