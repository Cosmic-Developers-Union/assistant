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

type teaConfig struct {
	Logins []struct {
		Name  string `yaml:"name"`
		URL   string `yaml:"url"`
		Token string `yaml:"token"`
	} `yaml:"logins"`
}

// DetectTeaToken 从 tea CLI 配置中检测 host 对应登录的令牌：按配置顺序取第一条
// URL 匹配且令牌非空的登录。source 返回配置文件路径；没有可用登录时 ok=false。
// 只读本地配置，不做任何网络请求。
func DetectTeaToken(host string, getenv func(string) string) (token, source string, ok bool) {
	if getenv == nil {
		getenv = os.Getenv
	}
	path := teaConfigPath(getenv)
	if path == "" {
		return "", "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	var config teaConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
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
