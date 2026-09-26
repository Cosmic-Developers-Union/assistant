package claude

import "strings"

// EnvVar 是一个环境变量键值对。
type EnvVar struct {
	Key   string
	Value string
}

// EnvConfig 是一次会话要注入的进程环境。
//
// 三处来源合并到这一个结构体（此前散在两个包各自的函数里）：
//   - ConfigDir / ProjectDirName：CLI 自己的会话配置根与文本记录目录名。这两项
//     **必须**走进程环境，settings.env 对它们无效；
//   - Credentials：assistant 自己的身份与配置来源（Gitea 令牌、config.json
//     路径），让会话内的 MCP 以与调度器相同的身份工作；
//   - Extra：调用方的其它注入。
type EnvConfig struct {
	ConfigDir      string
	ProjectDirName string
	Credentials    []EnvVar
	Extra          []EnvVar
	// PinProjectDir 为真时同时钉 CLAUDE_CODE_PROJECT_DIR_NAME（评审会话需要：
	// 文本记录必须落在固定项目目录名下，不能随 worktree 路径漂移）；为假时只钉
	// CLAUDE_CONFIG_DIR（对话会话需要：用户在该目录里 `claude --continue` 能接上
	// 最近的会话）。
	PinProjectDir bool
}

// SessionEnv 组装注入子进程的环境变量。
//
// 返回的顺序稳定（配置根 → 项目目录名 → 凭据 → 额外项），便于测试与日志断言。
// 键为空或值为空白的项被丢弃：注入一个空值比不注入更糟——CLI 会把它当成显式
// 配置的空值。
func SessionEnv(config EnvConfig) []string {
	env := make([]string, 0, 2+len(config.Credentials)+len(config.Extra))
	if dir := strings.TrimSpace(config.ConfigDir); dir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+dir)
		if config.PinProjectDir {
			if project := strings.TrimSpace(config.ProjectDirName); project != "" {
				env = append(env, "CLAUDE_CODE_PROJECT_DIR_NAME="+project)
			}
		}
	}
	for _, variable := range append(append([]EnvVar(nil), config.Credentials...), config.Extra...) {
		key := strings.TrimSpace(variable.Key)
		if key == "" || strings.TrimSpace(variable.Value) == "" {
			continue
		}
		env = append(env, key+"="+variable.Value)
	}
	return env
}

// EnvKeys 从环境变量列表里取键名（容器形态下 `docker -e KEY` 只给键名，值由
// docker 从宿主环境继承——此时宿主已由 SessionEnv 注入过这些变量）。
func EnvKeys(env []string) []string {
	keys := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// EnvValue 从环境变量列表里取值（未设置时返回空串与 false）。
func EnvValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}
