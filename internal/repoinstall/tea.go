package repoinstall

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// teaConfigPath 返回 tea CLI 配置文件路径：TEA_CONFIG 覆盖，否则
// <UserConfigDir>/tea/config.yml（Linux ~/.config/tea/config.yml）。
func teaConfigPath(getenv func(string) string) string {
	if path := strings.TrimSpace(getenv("TEA_CONFIG")); path != "" {
		return path
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(configDir, "tea", "config.yml")
}

type teaLogin struct {
	User       string `yaml:"user"`
	Name       string `yaml:"name"`
	URL        string `yaml:"url"`
	Token      string `yaml:"token"`
	AuthMethod string `yaml:"auth_method"`
}

type teaConfig struct {
	Logins []teaLogin `yaml:"logins"`
}

func loadTeaConfig(getenv func(string) string) (teaConfig, string, bool) {
	if getenv == nil {
		getenv = os.Getenv
	}
	path := teaConfigPath(getenv)
	if path == "" {
		return teaConfig{}, "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return teaConfig{}, "", false
	}
	var config teaConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return teaConfig{}, "", false
	}
	return config, path, true
}

// TeaLoginStatus 描述 tea CLI 对某站点的登录现状（只读本地配置）。
type TeaLoginStatus struct {
	// Path 是 tea 配置文件路径。
	Path string
	// Found 表示存在 URL 匹配本站点的登录条目。
	Found bool
	// HasToken 表示存在可用（令牌非空）的登录条目。
	HasToken bool
	// Name/User/AuthMethod 是第一条匹配登录的名称、用户名与认证方式。
	Name       string
	User       string
	AuthMethod string
}

// InspectTeaLogin 检查 tea CLI 配置里本站点的登录现状：用于区分「没有登录」
// 与「有登录但没有令牌」（tea 的 OAuth 可能失败，只留下条目）。
func InspectTeaLogin(host string, getenv func(string) string) TeaLoginStatus {
	config, path, ok := loadTeaConfig(getenv)
	if !ok {
		return TeaLoginStatus{}
	}
	status := TeaLoginStatus{Path: path}
	for _, login := range config.Logins {
		if !sameTeaHost(login.URL, host) {
			continue
		}
		if !status.Found {
			status.Found = true
			status.Name = login.Name
			status.User = login.User
			status.AuthMethod = login.AuthMethod
		}
		if strings.TrimSpace(login.Token) != "" {
			status.HasToken = true
		}
	}
	return status
}

// DetectTeaToken 从 tea CLI 配置中检测 host 对应登录的令牌：按配置顺序取第一条
// URL 匹配且令牌非空的登录。source 返回配置文件路径；没有可用登录时 ok=false。
// 只读本地配置，不做任何网络请求。
func DetectTeaToken(host string, getenv func(string) string) (token, source string, ok bool) {
	config, path, found := loadTeaConfig(getenv)
	if !found {
		return "", "", false
	}
	for _, login := range config.Logins {
		if !sameTeaHost(login.URL, host) {
			continue
		}
		if value := strings.TrimSpace(login.Token); value != "" {
			return value, path, true
		}
	}
	return "", "", false
}

// sameTeaHost 比较 tea 配置里的 url 与目标 host（忽略尾斜杠与大小写）。
func sameTeaHost(configured, host string) bool {
	return strings.EqualFold(
		strings.TrimRight(strings.TrimSpace(configured), "/"),
		strings.TrimRight(strings.TrimSpace(host), "/"),
	)
}
