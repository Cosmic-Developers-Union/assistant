package claude

import (
	"path/filepath"
	"strconv"
	"time"
)

// OutputFormat 是 stream-json：逐行事件流，会话进度可实时解析（而非等整段结束）。
const OutputFormat = "stream-json"

// Session 描述会话标识与续接意图。
type Session struct {
	// ID 是稳定会话 ID（同一待办/同一对话用户重试时复用）。
	ID string
	// Resume 为真时用 --resume 续接已有记录，否则用 --session-id 新建。
	Resume bool
}

// Container 是容器形态的运行参数：会话在 docker 里跑而不是宿主。
type Container struct {
	// Name 是容器名（超时兜底按名 kill）。
	Name string
	// Cwd 是宿主工作目录，按相同路径挂进容器并作为工作目录。
	Cwd string
	// Image 是镜像。
	Image string
	// MountDirs 是额外按相同绝对路径挂载的目录（会话配置、文本记录目录）。
	MountDirs []string
	// AssistantBin 是 assistant 可执行文件的绝对路径，只读挂进容器——会话内的
	// gitea MCP 由它启动，评审镜像不必自带 assistant。非绝对路径时忽略。
	AssistantBin string
	// ConfigDir 非空时显式注入 CLAUDE_CONFIG_DIR（容器内不继承宿主环境）。
	ConfigDir string
	// ProjectDirName 非空时显式注入 CLAUDE_CODE_PROJECT_DIR_NAME。
	ProjectDirName string
	// Network 非空时加 --network。
	Network string
	// PassthroughEnv 是按名透传的宿主环境变量（值由 docker 从宿主继承）。
	PassthroughEnv []string
}

// ArgsOptions 是一次会话调用的参数面。
//
// dispatcher（评审会话）与 daemon（对话会话）的差异体现在本结构体的字段上：
// dispatcher 传 Autocompact 与容器、用 300 的 MaxTurns；daemon 传 AgentsJSON 与
// Bare、不传 Autocompact。这样两者的 argv 由同一个纯函数产出，flag 的增删只改一处。
type ArgsOptions struct {
	Prompt             string
	SettingsPath       string
	MCPConfigPath      string
	SettingSources     string
	AppendSystemPrompt string
	Name               string
	Model              string
	PermissionMode     string
	Session            Session
	MaxTurns           int
	// Autocompact 非空时加 --autocompact（评审会话用：长会话自动压缩上下文）。
	Autocompact string
	// Bare 为真时加 --bare（对话会话的最小模式：不加载 hooks/插件/CLAUDE.md）。
	Bare bool
	// AgentsJSON 非空时加 --agents（对话会话注入子代理定义）。
	AgentsJSON string
	// StrictMCP 为真时加 --strict-mcp-config（不读项目级 MCP 授权状态）。
	StrictMCP bool
	// Verbose 为真时加 --verbose（stream-json 输出的前提）。
	Verbose bool
	// Container 非空时产出 `docker run …` 形式的 argv。
	Container *Container
}

// BuildArgs 组装一次 claude 调用的 argv（容器形态下是容器内 claude 的 argv）。
// 纯函数：不碰文件系统、不启动进程，因此每个 flag 的存在/缺席/顺序都可直接断言。
func BuildArgs(options ArgsOptions) []string {
	args := []string{"-p", options.Prompt}
	if options.Verbose {
		args = append(args, "--verbose")
	}
	if options.PermissionMode != "" {
		args = append(args, "--permission-mode", options.PermissionMode)
	}
	if options.Autocompact != "" {
		args = append(args, "--autocompact", options.Autocompact)
	}
	args = append(args, "--output-format", OutputFormat)
	if options.StrictMCP {
		args = append(args, "--strict-mcp-config")
	}
	if options.MCPConfigPath != "" {
		args = append(args, "--mcp-config", options.MCPConfigPath)
	}
	if options.SettingsPath != "" {
		args = append(args, "--settings", options.SettingsPath)
	}
	if options.SettingSources != "" {
		args = append(args, "--setting-sources", options.SettingSources)
	}
	if options.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", options.AppendSystemPrompt)
	}
	if options.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(options.MaxTurns))
	}
	if options.AgentsJSON != "" {
		args = append(args, "--agents", options.AgentsJSON)
	}
	if options.Bare {
		args = append(args, "--bare")
	}
	if options.Session.ID != "" {
		flag := "--session-id"
		if options.Session.Resume {
			flag = "--resume"
		}
		args = append(args, flag, options.Session.ID)
	}
	if options.Name != "" {
		args = append(args, "--name", options.Name)
	}
	if options.Model != "" {
		args = append(args, "--model", options.Model)
	}
	return args
}

// BuildSpec 把参数面折成 Runner 的输入：非容器形态直接用宿主 bin 与组装好的
// argv；容器形态 Bin 换 docker、argv 前置 `run …` 与挂载，claude 的 argv 追加在
// 镜像与容器内路径之后。
func BuildSpec(options ArgsOptions, bin, dir string, env []string, timeout time.Duration) Spec {
	claudeArgs := BuildArgs(options)
	container := options.Container
	if container == nil {
		return Spec{Bin: bin, Args: claudeArgs, Dir: dir, Env: env, Timeout: timeout}
	}

	name := container.Name
	args := []string{
		"run", "--rm", "-i",
		"--name", name,
		"-v", container.Cwd + ":" + container.Cwd,
		"-w", container.Cwd,
	}
	// 会话输入文件（MCP 配置、独立 settings）与文本记录目录按相同绝对路径挂载
	for _, mountDir := range container.MountDirs {
		args = append(args, "-v", mountDir+":"+mountDir)
	}
	// assistant 二进制只读挂进去：会话里的 gitea MCP 由它启动
	if filepath.IsAbs(container.AssistantBin) {
		args = append(args, "-v", container.AssistantBin+":"+container.AssistantBin+":ro")
	}
	// 文本记录的固定项目目录名靠进程环境传递，容器内显式注入
	if container.ConfigDir != "" {
		args = append(args, "-e", "CLAUDE_CONFIG_DIR="+container.ConfigDir)
	}
	if container.ProjectDirName != "" {
		args = append(args, "-e", "CLAUDE_CODE_PROJECT_DIR_NAME="+container.ProjectDirName)
	}
	if container.Network != "" {
		args = append(args, "--network", container.Network)
	}
	// 透传变量只给键名：值由 docker 从宿主环境继承
	for _, key := range container.PassthroughEnv {
		args = append(args, "-e", key)
	}
	args = append(args, container.Image, bin)
	args = append(args, claudeArgs...)
	return Spec{Bin: "docker", Args: args, Dir: dir, Env: env, Timeout: timeout, Container: name}
}
